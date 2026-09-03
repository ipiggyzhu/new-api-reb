package channel_score

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// The tests below pin the rule that a demotion is not erased by the silence the
// demotion itself causes.
//
// Production evidence for why this matters: on a gateway where 8 of 21 channels
// were failing 100% of requests, 208 requests spent their FIRST attempt on a
// 0%-success channel. Every one of those channels had already been demoted. The
// wholesale idle reset returned each to the top tier one idle period later, where
// it was picked again, failed again, and was demoted again — a loop nothing the
// channel did could break, because being demoted is what made it idle.

func TestIdleDoesNotEraseADemotionWholesale(t *testing.T) {
	state := scoreState{tierOffset: -3, updatedAt: 1000}

	// Exactly one idle period has elapsed.
	state.decayIdle(1000+300, 300)

	assert.Equal(t, -2, state.tierOffset,
		"one idle period must forgive one tier, not the whole demotion")
}

func TestIdleDecayIsProportionalToElapsedPeriods(t *testing.T) {
	for _, tc := range []struct {
		name     string
		offset   int
		elapsed  int64
		expected int
	}{
		{"one period of three", -3, 300, -2},
		{"two periods of three", -3, 600, -1},
		{"three periods clears it", -3, 900, 0},
		{"more periods than depth floors at neutral", -3, 6000, 0},
		{"a single blip is forgiven in one period", -1, 300, 0},
		{"a promotion decays the same way", 1, 300, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := scoreState{tierOffset: tc.offset, updatedAt: 1000}
			state.decayIdle(1000+tc.elapsed, 300)
			assert.Equal(t, tc.expected, state.tierOffset)
		})
	}
}

// A deeper demotion must buy a longer exile, because that is what makes the
// mechanism self-limiting: a channel that fails repeatedly sinks further and
// therefore stays out of rotation longer, while one that failed once returns
// quickly.
func TestDeeperDemotionMeansLongerExile(t *testing.T) {
	shallow := scoreState{tierOffset: -1, updatedAt: 1000}
	deep := scoreState{tierOffset: -3, updatedAt: 1000}

	// After a single idle period the shallow one is already neutral and the deep
	// one is still demoted.
	shallow.decayIdle(1300, 300)
	deep.decayIdle(1300, 300)

	assert.Equal(t, 0, shallow.tierOffset)
	assert.Less(t, deep.tierOffset, 0,
		"a channel demoted three tiers must not be back in rotation after one idle period")
}

// The sample window measures current behaviour, so unlike the offset it must be
// cleared outright: a success rate recorded before a long silence says nothing
// about now, and carrying it forward would let a stale rate scale weight.
func TestIdleClearsTheSampleWindowButKeepsTheOffset(t *testing.T) {
	state := scoreState{
		tierOffset:         -2,
		consecutiveSuccess: 4,
		faultCount:         1,
		curStart:           900,
		curTotal:           40,
		curSuccess:         40,
		prevTotal:          10,
		prevSuccess:        10,
		updatedAt:          1000,
	}

	state.decayIdle(1000+300, 300)

	total, success := state.sampleTotals()
	assert.Zero(t, total, "stale sample volume must not survive the silence")
	assert.Zero(t, success)
	assert.Zero(t, state.consecutiveSuccess, "a streak cannot span a silence")
	assert.Zero(t, state.faultCount)
	assert.Equal(t, -1, state.tierOffset, "the demotion decays by one tier and survives")
}

// Guards the arithmetic against a clock that does not advance, which would
// otherwise divide into a zero or negative period count.
func TestIdleDecayIsInertWithoutElapsedTime(t *testing.T) {
	state := scoreState{tierOffset: -2, updatedAt: 1000}
	state.decayIdle(1000, 300)
	assert.Equal(t, -2, state.tierOffset)

	unset := scoreState{tierOffset: -2}
	unset.decayIdle(5000, 300)
	assert.Equal(t, -2, unset.tierOffset, "a record never written has no lapse to apply")

	zeroPeriod := scoreState{tierOffset: -2, updatedAt: 1000}
	zeroPeriod.decayIdle(5000, 0)
	assert.Equal(t, -2, zeroPeriod.tierOffset, "idle reset disabled must not move the offset")
}

// TestIdleDemotionStillReordersCandidates is the end-to-end regression for the
// production loop: a channel that is idle because it was demoted must remain
// beneath a healthy sibling. If lookup returned nil at the idle boundary, both
// candidates would read as priority zero and the random selection would put a
// request straight back onto the 0%-success channel.
func TestIdleDemotionStillReordersCandidates(t *testing.T) {
	now := useScoreSettingForTest(t, func(setting *operation_setting.ChannelDynamicScoreSetting) {
		setting.IdleResetSeconds = 300
		setting.MaxDemoteTiers = 3
	})

	candidates := []Candidate{
		{ChannelId: 41, Priority: 0, Weight: 10},
		{ChannelId: 42, Priority: 0, Weight: 10},
	}
	reportN(41, OutcomeFault, 3)
	require.Equal(t, -3, offsetOf(t, 41))

	*now += 301
	out := ApplyToCandidates(testGroup, testModel, candidates)

	require.Len(t, out, 2)
	assert.EqualValues(t, -2, out[0].Priority,
		"after one idle period a three-tier demotion must still keep the dead channel below baseline")
	assert.EqualValues(t, 0, out[1].Priority)
	assert.EqualValues(t, 10, out[0].Weight,
		"stale rate samples must not keep scaling weight while the tier verdict survives")
}
