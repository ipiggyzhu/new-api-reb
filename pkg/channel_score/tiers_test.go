package channel_score

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDistinctTiersDesc(t *testing.T) {
	cases := []struct {
		name       string
		candidates []Candidate
		want       []int64
	}{
		{
			name:       "empty",
			candidates: nil,
			want:       []int64{},
		},
		{
			name:       "dedupes and sorts descending",
			candidates: []Candidate{{Priority: 0}, {Priority: 100}, {Priority: 50}, {Priority: 100}},
			want:       []int64{100, 50, 0},
		},
		{
			name:       "single tier",
			candidates: []Candidate{{Priority: 7}, {Priority: 7}},
			want:       []int64{7},
		},
		{
			name:       "negative priorities keep ordering",
			candidates: []Candidate{{Priority: -5}, {Priority: 0}, {Priority: -20}},
			want:       []int64{0, -5, -20},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, distinctTiersDesc(tc.candidates))
		})
	}
}

// TestShiftByTiersHandlesArbitraryPriorityScales is the regression test for the
// design defect that a fixed numeric offset cannot express demotion: a channel
// at priority 100 must be able to fall below a sibling at 0, whatever numbers
// the operator chose.
func TestShiftByTiersHandlesArbitraryPriorityScales(t *testing.T) {
	cases := []struct {
		name     string
		tiers    []int64
		priority int64
		offset   int
		want     int64
	}{
		{
			name:     "zero offset is identity",
			tiers:    []int64{100, 0},
			priority: 100,
			offset:   0,
			want:     100,
		},
		{
			name:     "demote one tier across a huge gap",
			tiers:    []int64{100, 0},
			priority: 100,
			offset:   -1,
			want:     0,
		},
		{
			name:     "demote one tier lands on the intermediate tier",
			tiers:    []int64{100, 50, 0},
			priority: 100,
			offset:   -1,
			want:     50,
		},
		{
			name:     "demote two tiers walks past the intermediate tier",
			tiers:    []int64{100, 50, 0},
			priority: 100,
			offset:   -2,
			want:     0,
		},
		{
			name:     "demote past the bottom goes below every candidate",
			tiers:    []int64{100, 0},
			priority: 0,
			offset:   -1,
			want:     -1,
		},
		{
			name:     "demote far past the bottom keeps descending",
			tiers:    []int64{10},
			priority: 10,
			offset:   -3,
			want:     7,
		},
		{
			name:     "promote one tier",
			tiers:    []int64{100, 50, 0},
			priority: 0,
			offset:   1,
			want:     50,
		},
		{
			name:     "promote past the top goes above every candidate",
			tiers:    []int64{100, 0},
			priority: 100,
			offset:   1,
			want:     101,
		},
		{
			name:     "priority absent from tiers is left alone",
			tiers:    []int64{100, 0},
			priority: 42,
			offset:   -1,
			want:     42,
		},
		{
			name:     "empty tiers is identity",
			tiers:    nil,
			priority: 5,
			offset:   -2,
			want:     5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shiftByTiers(tc.tiers, tc.priority, tc.offset))
		})
	}
}

// TestSuccessRateFactorIgnoresSmallSamples pins the rule that a success rate
// measured over too few requests must not move weight at all. Without the
// volume gate, one early failure would halve a channel's share on no evidence.
func TestSuccessRateFactorIgnoresSmallSamples(t *testing.T) {
	cases := []struct {
		name      string
		total     int64
		success   int64
		minSample int
		want      float64
	}{
		{name: "no samples", total: 0, success: 0, minSample: 20, want: 1.0},
		{name: "below threshold all failures", total: 19, success: 0, minSample: 20, want: 1.0},
		{name: "below threshold all successes", total: 19, success: 19, minSample: 20, want: 1.0},
		{name: "at threshold all successes", total: 20, success: 20, minSample: 20, want: 1.5},
		{name: "at threshold all failures", total: 20, success: 0, minSample: 20, want: 0.5},
		{name: "at threshold half", total: 20, success: 10, minSample: 20, want: 1.0},
		{name: "quantized down to a step", total: 100, success: 60, minSample: 20, want: 1.0},
		{name: "quantized up to a step", total: 100, success: 70, minSample: 20, want: 1.25},
		{name: "success beyond total clamps", total: 20, success: 40, minSample: 20, want: 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, successRateFactor(tc.total, tc.success, tc.minSample), 1e-9)
		})
	}
}

// TestSuccessRateFactorIsStableInsideABucket is the anti-oscillation property: a
// continuous factor recomputed per request made weight twitch on every sample,
// so the factor must only move when the rate crosses a quantization boundary.
//
// With factor = 0.5 + rate snapped to 0.25 steps, the boundaries sit at rates
// 0.125, 0.375, 0.625 and 0.875. Rates from 0.63 to 0.87 therefore share one
// bucket, which is the span this test walks.
func TestSuccessRateFactorIsStableInsideABucket(t *testing.T) {
	base := successRateFactor(100, 70, 20)
	require.InDelta(t, 1.25, base, 1e-9)
	for success := int64(63); success <= 87; success++ {
		assert.InDelta(t, base, successRateFactor(100, success, 20), 1e-9,
			"success=%d should stay in the same bucket", success)
	}
	assert.NotEqualValues(t, base, successRateFactor(100, 90, 20),
		"crossing a bucket boundary must change the factor")
	assert.NotEqualValues(t, base, successRateFactor(100, 50, 20),
		"crossing the other way must change it too")
}

func TestApplyWeightFactorNeverDropsBelowOne(t *testing.T) {
	assert.Equal(t, 10, applyWeightFactor(10, 1.0), "identity factor returns the weight untouched")
	assert.Equal(t, 15, applyWeightFactor(10, 1.5))
	assert.Equal(t, 5, applyWeightFactor(10, 0.5))
	assert.Equal(t, 1, applyWeightFactor(1, 0.5), "a weight of 1 cannot be scaled away")
}

// TestApplyWeightFactorLeavesZeroAlone pins the one case where the floor must not
// apply. effectiveChannelWeight adds channelWeightBaseline to every candidate, so
// a configured weight of 0 already draws its baseline share; flooring it to 1
// would lift a penalized channel ABOVE an untouched sibling at 0, turning a
// demotion into a promotion.
func TestApplyWeightFactorLeavesZeroAlone(t *testing.T) {
	assert.Equal(t, 0, applyWeightFactor(0, 0.5), "a penalty must not invent weight")
	assert.Equal(t, 0, applyWeightFactor(0, 1.5), "a reward must not invent weight either")
	assert.Equal(t, 0, applyWeightFactor(0, 1.0))
}

// TestShiftByTiersSaturatesInsteadOfWrapping covers an admin-configured priority
// sitting at the int64 extreme. Moving past the end of the tier list adds to the
// boundary tier, which would wrap a MaxInt64 priority to a large negative number
// — inverting a promotion into the harshest possible demotion. Saturation keeps
// the sign of the intended movement, which is all the ranking depends on.
func TestShiftByTiersSaturatesInsteadOfWrapping(t *testing.T) {
	promoted := shiftByTiers([]int64{math.MaxInt64}, math.MaxInt64, 3)
	assert.Equal(t, int64(math.MaxInt64), promoted,
		"promoting past the top tier must not wrap to a negative priority")

	demoted := shiftByTiers([]int64{math.MinInt64}, math.MinInt64, -3)
	assert.Equal(t, int64(math.MinInt64), demoted,
		"demoting past the bottom tier must not wrap to a positive priority")

	// Saturation must not leak into the ordinary case: a promotion from a normal
	// priority still moves.
	assert.Equal(t, int64(101), shiftByTiers([]int64{100, 50}, 100, 1),
		"promoting past the top of a normal list still extends by one")
}

// TestRotateSlidingWindow drives the window with an injected clock rather than
// sleeping, so the boundaries are exact.
func TestRotateSlidingWindow(t *testing.T) {
	const half = int64(150)

	t.Run("first observation opens the window", func(t *testing.T) {
		state := &scoreState{}
		state.rotate(1000, half)
		assert.EqualValues(t, 1000, state.curStart)
	})

	t.Run("inside the half nothing rotates", func(t *testing.T) {
		state := &scoreState{curStart: 1000, curTotal: 5, curSuccess: 4}
		state.rotate(1000+half-1, half)
		assert.EqualValues(t, 5, state.curTotal)
		assert.EqualValues(t, 1000, state.curStart)
	})

	t.Run("one half elapsed shifts current into previous", func(t *testing.T) {
		state := &scoreState{curStart: 1000, curTotal: 5, curSuccess: 4}
		state.rotate(1000+half, half)
		assert.EqualValues(t, 5, state.prevTotal)
		assert.EqualValues(t, 4, state.prevSuccess)
		assert.EqualValues(t, 0, state.curTotal)
		assert.EqualValues(t, 1000+half, state.curStart)
	})

	t.Run("two halves elapsed discards everything", func(t *testing.T) {
		state := &scoreState{curStart: 1000, curTotal: 5, curSuccess: 4, prevTotal: 9, prevSuccess: 9}
		state.rotate(1000+2*half, half)
		total, success := state.sampleTotals()
		assert.EqualValues(t, 0, total, "stale data must not shift forward")
		assert.EqualValues(t, 0, success)
	})
}

func TestSampleTotalsSumsBothHalves(t *testing.T) {
	state := &scoreState{curTotal: 3, curSuccess: 2, prevTotal: 7, prevSuccess: 5}
	total, success := state.sampleTotals()
	require.EqualValues(t, 10, total)
	require.EqualValues(t, 7, success)
}
