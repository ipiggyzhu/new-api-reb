package channel_score

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// The projection collapses a channel's per-route scores to one number, so the
// tests here pin WHICH route survives the collapse and that the collapsed value is
// the same one the selection path would compute.

func TestProjectByChannelReportsTheWorstRoute(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.MinSampleForWeight = 1
	})

	// One healthy route and one failing route on the same channel. A channel-level
	// number built from the healthy one would report a channel that is half broken
	// as promoted.
	for i := 0; i < 5; i++ {
		Report(7, "default", "claude-opus-5", OutcomeSuccess)
	}
	Report(7, "default", "gpt-5.6", OutcomeFault)

	projections, snapshot := ProjectByChannel()
	require.True(t, snapshot.Enabled)
	require.Contains(t, projections, 7)

	projection := projections[7]
	assert.Equal(t, "gpt-5.6", projection.Model, "the demoted route is the one reported")
	assert.Equal(t, -1, projection.TierOffset)
	assert.Equal(t, 2, projection.Active, "both routes are counted even though one is reported")
	assert.Equal(t, 2, projection.Adjusted, "one promoted and one demoted route both moved")
}

func TestProjectByChannelSkipsIdleRoutes(t *testing.T) {
	now := useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.IdleResetSeconds = 60
		s.MinSampleForWeight = 1
	})

	Report(9, "default", "gpt-5.6", OutcomeFault)
	require.Contains(t, mustProject(t), 9)

	// Past the idle window the selection path stops applying the demotion, so a
	// projection that still reported it would show an adjustment not in force.
	*now += 61
	assert.NotContains(t, mustProject(t), 9,
		"an all-idle channel must be absent so the caller restores the baseline")
}

// TestProjectedPriorityMatchesTheSelectionPath is the contract that makes the
// mirror trustworthy: the number written to the column must be the number routing
// would land on, computed by the same function from the same baseline.
func TestProjectedPriorityMatchesTheSelectionPath(t *testing.T) {
	candidates := []Candidate{
		{ChannelId: 1, Priority: 100, Weight: 10},
		{ChannelId: 2, Priority: 50, Weight: 10},
		{ChannelId: 3, Priority: 0, Weight: 10},
	}
	tiers := DistinctBaselineTiers([]int64{100, 50, 0})

	for _, offset := range []int{-2, -1, 0, 1, 2} {
		for _, candidate := range candidates {
			expected := shiftByTiers(distinctTiersDesc(candidates), candidate.Priority, offset)
			actual := EffectivePriority(tiers, candidate.Priority, offset)
			assert.Equal(t, expected, actual,
				"projected priority diverged from the selector for channel %d offset %d",
				candidate.ChannelId, offset)
		}
	}
}

// TestRepeatedProjectionFromBaselineIsStable is the direct answer to
// TestWritingShiftedPriorityBackCompounds: the same inputs projected repeatedly
// must produce the same output, because nothing is fed back.
func TestRepeatedProjectionFromBaselineIsStable(t *testing.T) {
	const baseline = int64(0)
	tiers := DistinctBaselineTiers([]int64{0, 0, 0})

	results := make([]int64, 0, 6)
	for round := 0; round < 6; round++ {
		// The baseline is re-read every round, never replaced by the result.
		results = append(results, EffectivePriority(tiers, baseline, 1))
	}
	assert.Equal(t, []int64{1, 1, 1, 1, 1, 1}, results,
		"absolute recomputation from a frozen baseline must not walk")
}

// TestProjectionOscillatesIfTiersComeFromProjectedValues records the failure mode
// Codex identified, so the "tiers must come from the baseline column" rule in
// project.go has a test behind it rather than only a comment.
func TestProjectionOscillatesIfTiersComeFromProjectedValues(t *testing.T) {
	priority := int64(0)
	seen := []int64{priority}
	for round := 0; round < 4; round++ {
		// The bug: rebuilding the ladder from the value just written.
		tiers := DistinctBaselineTiers([]int64{priority, 0})
		priority = EffectivePriority(tiers, priority, 1)
		seen = append(seen, priority)
	}
	// 0 -> 1, then 1 is the top of a two-tier ladder so promoting walks it up
	// again: the projection never settles.
	assert.Equal(t, []int64{0, 1, 2, 3, 4}, seen,
		"tiers derived from projected values make the column walk")
}

func TestEffectiveWeightKeepsAConfiguredZeroAtZero(t *testing.T) {
	// A configured 0 means "baseline share"; flooring it to 1 would make a
	// penalized channel outdraw an untouched sibling.
	assert.Equal(t, 0, EffectiveWeight(0, 0.5))
	assert.Equal(t, 5, EffectiveWeight(10, 0.5))
	assert.Equal(t, 15, EffectiveWeight(10, 1.5))
	assert.Equal(t, 1, EffectiveWeight(1, 0.5), "a penalized channel stays reachable")
}

func mustProject(t *testing.T) map[int]ChannelProjection {
	t.Helper()
	projections, _ := ProjectByChannel()
	return projections
}
