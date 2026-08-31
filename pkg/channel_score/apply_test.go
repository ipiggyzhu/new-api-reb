package channel_score

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useScoreSettingForTest installs a setting snapshot and a fixed clock, and
// restores both afterwards. The clock is injected rather than slept on so window
// rotation and idle reset are exact.
func useScoreSettingForTest(t *testing.T, mutate func(*operation_setting.ChannelDynamicScoreSetting)) *int64 {
	t.Helper()

	setting := operation_setting.ChannelDynamicScoreSetting{
		Enabled:              true,
		SuccessesToPromote:   5,
		FaultsToDemote:       1,
		MaxPromoteTiers:      1,
		MaxDemoteTiers:       3,
		MinSampleForWeight:   20,
		SuccessWindowSeconds: 300,
		IdleResetSeconds:     1800,
	}
	if mutate != nil {
		mutate(&setting)
	}
	operation_setting.SetChannelDynamicScoreSettingForTest(setting)

	now := int64(1_700_000_000)
	originalNow := nowFunc
	nowFunc = func() time.Time { return time.Unix(now, 0) }

	ResetAll()
	t.Cleanup(func() {
		nowFunc = originalNow
		operation_setting.ResetChannelDynamicScoreSettingForTest()
		ResetAll()
	})
	return &now
}

const (
	testGroup = "default"
	testModel = "gpt-test"
)

func reportN(channelId int, outcome Outcome, times int) {
	for i := 0; i < times; i++ {
		Report(channelId, testGroup, testModel, outcome)
	}
}

func offsetOf(t *testing.T, channelId int) int {
	t.Helper()
	setting := operation_setting.GetChannelDynamicScoreSetting()
	snap := lookup(scoreKey(channelId, testGroup, testModel), setting.IdleResetSeconds)
	if snap == nil {
		return 0
	}
	return snap.tierOffset
}

// TestPromotionRequiresConsecutiveSuccesses is the user-facing threshold: five
// successes buy one tier, and the fifth is what does it — not the fourth.
func TestPromotionRequiresConsecutiveSuccesses(t *testing.T) {
	useScoreSettingForTest(t, nil)

	reportN(1, OutcomeSuccess, 4)
	assert.Equal(t, 0, offsetOf(t, 1), "four successes must not promote")

	Report(1, testGroup, testModel, OutcomeSuccess)
	assert.Equal(t, 1, offsetOf(t, 1), "the fifth success promotes one tier")
}

// TestOneFaultDemotesImmediately is the asymmetry the design is built on: a real
// fault costs a tier at once, while recovery is gradual.
func TestOneFaultDemotesImmediately(t *testing.T) {
	useScoreSettingForTest(t, nil)

	Report(2, testGroup, testModel, OutcomeFault)
	assert.Equal(t, -1, offsetOf(t, 2), "a single fault demotes")
}

// TestFaultResetsTheSuccessStreak keeps a flapping channel from accumulating a
// promotion out of successes interleaved with faults.
func TestFaultResetsTheSuccessStreak(t *testing.T) {
	useScoreSettingForTest(t, nil)

	reportN(3, OutcomeSuccess, 4)
	Report(3, testGroup, testModel, OutcomeFault)
	require.Equal(t, -1, offsetOf(t, 3))

	// Four more successes would reach five in total, but not five in a row.
	reportN(3, OutcomeSuccess, 4)
	assert.Equal(t, -1, offsetOf(t, 3), "the streak restarted, so no promotion yet")

	Report(3, testGroup, testModel, OutcomeSuccess)
	assert.Equal(t, 0, offsetOf(t, 3), "five consecutive successes recover one tier")
}

func TestOffsetsAreClampedBothWays(t *testing.T) {
	useScoreSettingForTest(t, nil)

	reportN(4, OutcomeFault, 10)
	assert.Equal(t, -3, offsetOf(t, 4), "demotion stops at MaxDemoteTiers")

	reportN(5, OutcomeSuccess, 100)
	assert.Equal(t, 1, offsetOf(t, 5), "promotion stops at MaxPromoteTiers")
}

// TestIdleDecayReturnsToBaselineOneTierAtATime pins that idleness forgives a
// demotion gradually rather than all at once.
//
// This deliberately does not assert a return to baseline after a single idle
// window, which is what it used to check. A demoted channel receives no traffic —
// that is what demotion does — so it is idle by construction, and forgiving the
// whole verdict on that silence meant the verdict expired every time it was
// applied. In production that returned channels failing 100% of requests to the
// top tier repeatedly; 208 requests spent their first attempt on one.
func TestIdleDecayReturnsToBaselineOneTierAtATime(t *testing.T) {
	now := useScoreSettingForTest(t, nil)

	reportN(6, OutcomeFault, 2)
	require.Equal(t, -2, offsetOf(t, 6))

	*now += 1799
	assert.Equal(t, -2, offsetOf(t, 6), "still inside the idle window")

	*now += 2
	assert.Equal(t, -1, offsetOf(t, 6), "one idle period forgives one tier")

	*now += 1800
	assert.Equal(t, 0, offsetOf(t, 6), "a second period returns it to baseline")
}

func TestScoresAreIsolatedPerGroupAndModel(t *testing.T) {
	useScoreSettingForTest(t, nil)
	setting := operation_setting.GetChannelDynamicScoreSetting()

	reportN(7, OutcomeFault, 2)
	require.Equal(t, -2, offsetOf(t, 7))

	// A different model on the same channel must be unaffected: a channel that is
	// out of quota for one model is usually fine for the others.
	otherModel := lookup(scoreKey(7, testGroup, "some-other-model"), setting.IdleResetSeconds)
	assert.Nil(t, otherModel, "another model must not inherit the demotion")

	// And a different group, so one group's traffic cannot reroute another's.
	otherGroup := lookup(scoreKey(7, "vip", testModel), setting.IdleResetSeconds)
	assert.Nil(t, otherGroup, "another group must not inherit the demotion")
}

// TestApplyToCandidatesNeverDropsACandidate is the safety invariant: this package
// may reorder and reweight, but the candidate set it returns must always be the
// one it was given, or selection could be left with nothing to choose.
func TestApplyToCandidatesNeverDropsACandidate(t *testing.T) {
	useScoreSettingForTest(t, nil)

	candidates := []Candidate{
		{ChannelId: 10, Priority: 100, Weight: 5},
		{ChannelId: 11, Priority: 50, Weight: 5},
		{ChannelId: 12, Priority: 0, Weight: 5},
	}
	// Demote every channel as hard as the bounds allow.
	for _, candidate := range candidates {
		reportN(candidate.ChannelId, OutcomeFault, 10)
	}

	out := ApplyToCandidates(testGroup, testModel, candidates)
	require.Len(t, out, len(candidates), "the set must keep its size")

	ids := make([]int, 0, len(out))
	for _, candidate := range out {
		ids = append(ids, candidate.ChannelId)
	}
	assert.Equal(t, []int{10, 11, 12}, ids, "order and membership are preserved")
}

// TestApplyToCandidatesDemotesAcrossAHugePriorityGap is the end-to-end version of
// the fixed-offset defect: a channel configured at 100 must actually fall below
// one configured at 0.
func TestApplyToCandidatesDemotesAcrossAHugePriorityGap(t *testing.T) {
	useScoreSettingForTest(t, nil)

	candidates := []Candidate{
		{ChannelId: 20, Priority: 100, Weight: 5},
		{ChannelId: 21, Priority: 0, Weight: 5},
	}
	reportN(20, OutcomeFault, 1)

	out := ApplyToCandidates(testGroup, testModel, candidates)
	require.Len(t, out, 2)
	assert.EqualValues(t, 0, out[0].Priority, "the faulted channel dropped a tier")
	assert.EqualValues(t, 0, out[1].Priority, "its healthy sibling is untouched")
	assert.LessOrEqual(t, out[0].Priority, out[1].Priority,
		"a demotion across an arbitrary numeric gap must actually take effect")
}

// TestApplyToCandidatesIsRequestLocal documents the deliberate consequence of
// expressing movement in tiers: the same channel with the same score resolves to
// a different absolute priority depending on who else is eligible.
func TestApplyToCandidatesIsRequestLocal(t *testing.T) {
	useScoreSettingForTest(t, nil)
	reportN(30, OutcomeFault, 1)

	twoTiers := ApplyToCandidates(testGroup, testModel, []Candidate{
		{ChannelId: 30, Priority: 100, Weight: 5},
		{ChannelId: 31, Priority: 0, Weight: 5},
	})
	threeTiers := ApplyToCandidates(testGroup, testModel, []Candidate{
		{ChannelId: 30, Priority: 100, Weight: 5},
		{ChannelId: 32, Priority: 50, Weight: 5},
		{ChannelId: 31, Priority: 0, Weight: 5},
	})

	assert.EqualValues(t, 0, twoTiers[0].Priority)
	assert.EqualValues(t, 50, threeTiers[0].Priority,
		"one tier down means one rank down among THIS request's candidates")
}

func TestApplyToCandidatesIsInertWhenDisabled(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.Enabled = false
	})

	candidates := []Candidate{{ChannelId: 40, Priority: 100, Weight: 5}}
	Report(40, testGroup, testModel, OutcomeFault)

	out := ApplyToCandidates(testGroup, testModel, candidates)
	require.Len(t, out, 1)
	assert.EqualValues(t, 100, out[0].Priority, "a disabled feature changes nothing")
	assert.Equal(t, 5, out[0].Weight)
	assert.False(t, Enabled())
}

// TestWeightUnchangedBelowSampleThreshold checks the volume gate end to end: a
// channel with a handful of requests keeps exactly its configured weight.
func TestWeightUnchangedBelowSampleThreshold(t *testing.T) {
	useScoreSettingForTest(t, nil)

	reportN(50, OutcomeSuccess, 5)
	out := ApplyToCandidates(testGroup, testModel, []Candidate{
		{ChannelId: 50, Priority: 0, Weight: 8},
	})
	require.Len(t, out, 1)
	assert.Equal(t, 8, out[0].Weight, "five samples is not enough to move weight")
}

// TestHigherSuccessRateEarnsMoreWeight is the requirement that within one
// priority tier, the channel succeeding more often takes a bigger share.
func TestHigherSuccessRateEarnsMoreWeight(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		// Keep both channels in the same tier so only weight can differ.
		s.MaxPromoteTiers = 0
		s.MaxDemoteTiers = 0
	})

	// Channel 60: 30 requests, all successful.
	reportN(60, OutcomeSuccess, 30)
	// Channel 61: 30 requests, half faulted.
	for i := 0; i < 15; i++ {
		Report(61, testGroup, testModel, OutcomeSuccess)
		Report(61, testGroup, testModel, OutcomeFault)
	}

	out := ApplyToCandidates(testGroup, testModel, []Candidate{
		{ChannelId: 60, Priority: 0, Weight: 10},
		{ChannelId: 61, Priority: 0, Weight: 10},
	})
	require.Len(t, out, 2)
	assert.Equal(t, 15, out[0].Weight, "a perfect success rate scales weight up")
	assert.Equal(t, 10, out[1].Weight, "a 50% success rate leaves weight alone")
	assert.Greater(t, out[0].Weight, out[1].Weight,
		"same tier, better success rate, larger share")
}

func TestResetClearsOneChannelOnly(t *testing.T) {
	useScoreSettingForTest(t, nil)

	reportN(70, OutcomeFault, 2)
	reportN(71, OutcomeFault, 2)
	require.Equal(t, -2, offsetOf(t, 70))
	require.Equal(t, -2, offsetOf(t, 71))

	Reset(70)
	assert.Equal(t, 0, offsetOf(t, 70), "the reset channel is back to baseline")
	assert.Equal(t, -2, offsetOf(t, 71), "its neighbour is untouched")
}

// TestResetIsNotResurrectedByAnInFlightReport covers the window between a report
// reaching the shared store and mirroring its result locally. The Lua script has
// already run, so the value in hand still describes the pre-reset streak and
// window; publishing it after an admin reset the channel would put back the exact
// tier offset the reset was meant to clear, and the admin would see their reset
// do nothing.
func TestResetIsNotResurrectedByAnInFlightReport(t *testing.T) {
	useScoreSettingForTest(t, nil)

	const channelId = 73
	key := scoreKey(channelId, testGroup, testModel)

	// A report starts and captures the generation it is operating under.
	generation := entryGeneration(key)

	// The admin resets the channel while that report is still in flight.
	Reset(channelId)

	// The report now comes back with its pre-reset result.
	publishLocal(key, snapshot{tierOffset: -3, total: 20, success: 2}, generation)

	assert.Nil(t, lookup(key, 0),
		"a result computed before the reset must not repopulate the record")
	assert.Equal(t, 0, offsetOf(t, channelId), "the channel stays at baseline")

	// A report that starts after the reset is the normal case and must still land.
	publishLocal(key, snapshot{tierOffset: -1, total: 5, success: 4}, entryGeneration(key))
	assert.Equal(t, -1, offsetOf(t, channelId),
		"a report started after the reset is recorded as usual")
}

func TestResetManyClearsEveryListedChannel(t *testing.T) {
	useScoreSettingForTest(t, nil)

	reportN(80, OutcomeFault, 1)
	reportN(81, OutcomeFault, 1)
	reportN(82, OutcomeFault, 1)

	ResetMany([]int{80, 81})
	assert.Equal(t, 0, offsetOf(t, 80))
	assert.Equal(t, 0, offsetOf(t, 81))
	assert.Equal(t, -1, offsetOf(t, 82), "channels outside the list keep their scores")
}

func TestReportIgnoresInvalidChannel(t *testing.T) {
	useScoreSettingForTest(t, nil)

	Report(0, testGroup, testModel, OutcomeFault)
	Report(-1, testGroup, testModel, OutcomeFault)
	assert.Equal(t, 0, offsetOf(t, 0))
}

// TestConcurrentReportsDoNotLoseCounts guards the state transition: the whole
// record moves under one lock, so parallel reports cannot interleave into a
// promotion that no sequence of five successes earned.
func TestConcurrentReportsDoNotLoseCounts(t *testing.T) {
	useScoreSettingForTest(t, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Report(90, testGroup, testModel, OutcomeSuccess)
		}()
	}
	wg.Wait()

	setting := operation_setting.GetChannelDynamicScoreSetting()
	snap := lookup(scoreKey(90, testGroup, testModel), setting.IdleResetSeconds)
	require.NotNil(t, snap)
	assert.EqualValues(t, 50, snap.total, "every report is counted exactly once")
	assert.EqualValues(t, 50, snap.success)
	assert.Equal(t, 1, snap.tierOffset, "clamped at MaxPromoteTiers, never above")
}

func TestSlidingWindowForgetsOldSamples(t *testing.T) {
	now := useScoreSettingForTest(t, nil)
	setting := operation_setting.GetChannelDynamicScoreSetting()

	reportN(100, OutcomeSuccess, 25)
	snap := lookup(scoreKey(100, testGroup, testModel), setting.IdleResetSeconds)
	require.NotNil(t, snap)
	require.EqualValues(t, 25, snap.total)

	// Advance a full window (two halves) and report once. The old samples must
	// have aged out rather than accumulating forever.
	*now += int64(setting.SuccessWindowSeconds)
	Report(100, testGroup, testModel, OutcomeSuccess)

	snap = lookup(scoreKey(100, testGroup, testModel), setting.IdleResetSeconds)
	require.NotNil(t, snap)
	assert.EqualValues(t, 1, snap.total, "a full window later, only the new sample counts")
}

func TestApplyToCandidatesHandlesEmptyInput(t *testing.T) {
	useScoreSettingForTest(t, nil)
	assert.Empty(t, ApplyToCandidates(testGroup, testModel, nil))
	assert.Empty(t, ApplyToCandidates(testGroup, testModel, []Candidate{}))
}

func TestScoreKeyIsStable(t *testing.T) {
	assert.Equal(t, fmt.Sprintf("%s5:7:default:gpt-test", scoreKeyPrefix), scoreKey(5, "default", "gpt-test"))
}

// TestScoreKeyIsUnambiguousAcrossColons covers the collision the length prefix
// exists to prevent. Both a group name and a model id may contain a colon —
// vendor-prefixed model ids routinely do — so joining them with a bare separator
// let two unrelated routes share one streak, one success window and one tier
// offset. Reporting a fault on one would then demote the other.
func TestScoreKeyIsUnambiguousAcrossColons(t *testing.T) {
	assert.NotEqual(t,
		scoreKey(5, "a", "b:c"),
		scoreKey(5, "a:b", "c"),
		"a colon in either segment must not make two routes share a key")

	// The same key must still be reproducible for identical inputs, since the
	// selection path rebuilds it on every request.
	assert.Equal(t, scoreKey(5, "a:b", "c"), scoreKey(5, "a:b", "c"))

	// An empty group is a real case (unset group on a test call) and must not
	// collide with a group that is literally the string the prefix would produce.
	assert.NotEqual(t, scoreKey(5, "", "m"), scoreKey(5, "m", ""))
}
