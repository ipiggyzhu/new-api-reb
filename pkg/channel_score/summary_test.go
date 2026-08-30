package channel_score

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSummarizeByChannelCountsAdjustedRatherThanAveraging is the property the
// channel list depends on. A range alone cannot distinguish one demoted key from
// forty, and an average of tier offsets would be meaningless because tiers are
// ordinal and request-local. The cell needs "how many of how many".
func TestSummarizeByChannelCountsAdjustedRatherThanAveraging(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		// Keep the weight factor neutral so this test speaks only about offsets.
		s.MinSampleForWeight = 1000
		s.FaultsToDemote = 1
		s.MaxDemoteTiers = 3
	})

	// One demoted key among four on the same channel.
	Report(7, "default", "model-demoted", OutcomeFault)
	Report(7, "default", "model-ok-a", OutcomeSuccess)
	Report(7, "default", "model-ok-b", OutcomeSuccess)
	Report(7, "default", "model-ok-c", OutcomeSuccess)

	summaries, snapshot := SummarizeByChannel()
	require.True(t, snapshot.Enabled)
	require.Contains(t, summaries, 7)
	summary := summaries[7]

	assert.Equal(t, 4, summary.Total)
	assert.Equal(t, 4, summary.Active)
	assert.Equal(t, 1, summary.Adjusted, "only the faulted key moved")
	assert.Equal(t, -1, summary.MinOffset)
	assert.Equal(t, -1, summary.MaxOffset)
	assert.Equal(t, 0, summary.Idle)
}

// TestSummarizeByChannelSpansOffsetRange pins that the range covers every
// adjusted key rather than the last one seen. Map iteration order is random, so a
// naive implementation that assigns instead of comparing passes intermittently.
func TestSummarizeByChannelSpansOffsetRange(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.MinSampleForWeight = 1000
		s.FaultsToDemote = 1
		s.MaxDemoteTiers = 3
		s.SuccessesToPromote = 2
		s.MaxPromoteTiers = 1
	})

	// Down three tiers on one key.
	for i := 0; i < 3; i++ {
		Report(9, "default", "model-sinking", OutcomeFault)
	}
	// Up one tier on another.
	Report(9, "default", "model-rising", OutcomeSuccess)
	Report(9, "default", "model-rising", OutcomeSuccess)

	summaries, _ := SummarizeByChannel()
	summary := summaries[9]

	assert.Equal(t, 2, summary.Adjusted)
	assert.Equal(t, -3, summary.MinOffset, "range must reach the most demoted key")
	assert.Equal(t, 1, summary.MaxOffset, "range must reach the most promoted key")
}

// TestSummarizeByChannelExcludesIdleFromActiveAggregate is the same distinction
// Snapshot draws: an idle demotion exists but the selection path is not applying
// it, so reporting it as an adjustment would describe routing that is not
// happening. It still has to be visible as a count.
func TestSummarizeByChannelExcludesIdleFromActiveAggregate(t *testing.T) {
	now := useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.MinSampleForWeight = 1000
		s.FaultsToDemote = 1
		s.IdleResetSeconds = 100
	})

	Report(3, "default", "model-stale", OutcomeFault)

	summaries, _ := SummarizeByChannel()
	require.Equal(t, 1, summaries[3].Adjusted)
	require.Equal(t, 1, summaries[3].Active)
	require.Equal(t, 0, summaries[3].Idle)

	*now += 100

	summaries, _ = SummarizeByChannel()
	summary := summaries[3]
	assert.Equal(t, 1, summary.Total, "the key still exists")
	assert.Equal(t, 1, summary.Idle)
	assert.Equal(t, 0, summary.Active)
	assert.Equal(t, 0, summary.Adjusted, "an idle offset is not being applied")
	assert.Equal(t, 0, summary.MinOffset)
	assert.Equal(t, 0, summary.MaxOffset)
}

// TestSummarizeByChannelReportsWeightOnlyChange covers the case the offset
// columns cannot show: a channel whose keys all sit at tier 0 but whose success
// rate has scaled its weight. Its position is unchanged while it is handed a
// fraction of the traffic, so a cell that only watched offsets would render it as
// untouched.
func TestSummarizeByChannelReportsWeightOnlyChange(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.MinSampleForWeight = 2
		// No demotion however many faults land, isolating the weight effect.
		s.FaultsToDemote = 0
		s.MaxDemoteTiers = 0
	})

	// One of four succeeds. The factor is 0.5+rate, so a 50% rate lands exactly on
	// the neutral 1.0 and would prove nothing; 25% gives 0.75.
	Report(4, "default", "model-flaky", OutcomeSuccess)
	Report(4, "default", "model-flaky", OutcomeFault)
	Report(4, "default", "model-flaky", OutcomeFault)
	Report(4, "default", "model-flaky", OutcomeFault)

	summaries, _ := SummarizeByChannel()
	summary := summaries[4]

	require.Equal(t, 0, summary.Adjusted, "no tier movement with FaultsToDemote disabled")
	assert.Equal(t, 1, summary.Weighted, "the weight factor left 1.0 and must be reported")
	assert.Equal(t, 0.75, summary.MinWeightFactor)
	assert.Equal(t, 0.75, summary.MaxWeightFactor)
}

// TestSummarizeByChannelSeparatesChannels guards against the accumulator leaking
// between channels, which would attribute one channel's demotion to another.
func TestSummarizeByChannelSeparatesChannels(t *testing.T) {
	useScoreSettingForTest(t, func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.MinSampleForWeight = 1000
		s.FaultsToDemote = 1
	})

	Report(1, "default", "model-x", OutcomeFault)
	Report(2, "default", "model-x", OutcomeSuccess)

	summaries, _ := SummarizeByChannel()

	assert.Equal(t, 1, summaries[1].Adjusted)
	assert.Equal(t, -1, summaries[1].MinOffset)
	assert.Equal(t, 0, summaries[2].Adjusted, "channel 2 never faulted")
	assert.Equal(t, 0, summaries[2].MinOffset)
}

// TestSummarizeByChannelOmitsChannelsWithoutScores pins that the map is keyed by
// what has traffic, not by the channel table: the caller merges onto a page of
// channels, so a missing key already means "nothing to show" and materializing
// zero rows would only grow the payload.
func TestSummarizeByChannelOmitsChannelsWithoutScores(t *testing.T) {
	useScoreSettingForTest(t, nil)

	summaries, snapshot := SummarizeByChannel()
	assert.Empty(t, summaries)
	assert.Empty(t, snapshot.Rows)

	Report(42, "default", "model-x", OutcomeSuccess)

	summaries, _ = SummarizeByChannel()
	assert.Len(t, summaries, 1)
	assert.NotContains(t, summaries, 41)
}
