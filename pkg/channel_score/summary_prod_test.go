package channel_score

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSummaryUnderProductionSettings reproduces the live configuration
// (min_sample_for_weight=5, everything else default) against the two traffic
// shapes actually on the box, to pin what the channel list is expected to render.
//
// It exists because "the priority column still reads 0" is the expected steady
// state — the configured value is never rewritten — so the only way to tell the
// feature apart from a broken one is to know which badge each shape produces.
func TestSummaryUnderProductionSettings(t *testing.T) {
	prod := func(s *operation_setting.ChannelDynamicScoreSetting) {
		s.Enabled = true
		s.MinSampleForWeight = 5
		s.SuccessesToPromote = 5
		s.FaultsToDemote = 1
		s.MaxPromoteTiers = 1
		s.MaxDemoteTiers = 3
		s.SuccessWindowSeconds = 300
		s.IdleResetSeconds = 1800
	}

	t.Run("healthy high-volume channel earns a weight boost and one promotion", func(t *testing.T) {
		useScoreSettingForTest(t, prod)

		// Channel 31's shape: continuous successes on one model.
		for i := 0; i < 24; i++ {
			Report(31, "default", "gpt-5.6-sol", OutcomeSuccess)
		}

		summaries, snapshot := SummarizeByChannel()
		require.True(t, snapshot.Enabled)
		summary := summaries[31]

		// 24 successes of 24, above the 5-sample floor: factor is 0.5+1.0 = 1.5.
		assert.Equal(t, 1, summary.Weighted, "weight factor must have left 1.0")
		assert.Equal(t, 1.5, summary.MinWeightFactor)
		// Promotion is capped at MaxPromoteTiers=1 however long the streak runs.
		assert.Equal(t, 1, summary.Adjusted)
		assert.Equal(t, 1, summary.MaxOffset)

		// So the list shows a green +1 badge on priority AND a green 1.5x on weight.
		assert.True(t, summary.Adjusted > 0, "priority badge renders")
		assert.True(t, summary.Weighted > 0, "weight badge renders")
	})

	t.Run("a single fault demotes immediately", func(t *testing.T) {
		useScoreSettingForTest(t, prod)

		Report(5, "ClaudeCode", "claude-opus-5", OutcomeFault)

		summary := mustSummary(t, 5)
		// FaultsToDemote=1 in production: one fault is one tier, same second.
		assert.Equal(t, 1, summary.Adjusted)
		assert.Equal(t, -1, summary.MinOffset)
		// One sample is below the 5-sample floor, so weight stays neutral and the
		// weight badge is correctly absent.
		assert.Equal(t, 0, summary.Weighted)
	})

	t.Run("a channel with no traffic produces no summary at all", func(t *testing.T) {
		useScoreSettingForTest(t, prod)

		Report(31, "default", "gpt-5.6-sol", OutcomeSuccess)

		summaries, _ := SummarizeByChannel()
		// The 28 idle channels on the box are absent from the map, so their cells
		// render the configured priority alone. That is the correct "nothing to
		// show", not a bug.
		assert.NotContains(t, summaries, 19)
		assert.NotContains(t, summaries, 20)
		assert.Contains(t, summaries, 31)
	})
}

func mustSummary(t *testing.T, channelID int) ChannelScoreSummary {
	t.Helper()
	summaries, _ := SummarizeByChannel()
	summary, ok := summaries[channelID]
	require.True(t, ok, "expected a summary for channel %d", channelID)
	return summary
}
