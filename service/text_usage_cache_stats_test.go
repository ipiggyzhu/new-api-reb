package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claudeUpstreamUsage builds usage the way the Claude adaptor does for a
// response that read 900 tokens from the prompt cache, wrote 50 and sent 50
// uncached (relay/channel/claude/relay-claude.go).
func claudeUpstreamUsage() *dto.Usage {
	claudeUsage := &dto.ClaudeUsage{
		InputTokens:              50,
		CacheReadInputTokens:     900,
		CacheCreationInputTokens: 50,
		OutputTokens:             20,
	}
	usage := &dto.Usage{
		PromptTokens:     claudeUsage.InputTokens,
		CompletionTokens: claudeUsage.OutputTokens,
		TotalTokens:      claudeUsage.InputTokens + claudeUsage.OutputTokens,
		UsageSemantic:    "anthropic",
		BillingUsage:     dto.NewClaudeMessagesBillingUsage(claudeUsage),
	}
	usage.PromptTokensDetails.CachedTokens = claudeUsage.CacheReadInputTokens
	usage.PromptTokensDetails.CachedCreationTokens = claudeUsage.CacheCreationInputTokens
	return usage
}

// openAIStyleUpstreamUsage is usage from an OpenAI-compatible upstream such as
// DeepSeek: prompt_tokens already include the 990 cache-hit tokens.
func openAIStyleUpstreamUsage() *dto.Usage {
	usage := &dto.Usage{
		PromptTokens:         1000,
		CompletionTokens:     20,
		TotalTokens:          1020,
		PromptCacheHitTokens: 990,
	}
	usage.PromptTokensDetails.CachedTokens = 990
	usage.BillingUsage = dto.NewOpenAIChatBillingUsage(usage)
	return usage
}

// The admin cache stats must count every processed input token in the prompt
// total, whatever the client format, so cached/prompt is the share of input
// served from the upstream cache (cache reads over reads + writes + uncached).
func TestObserveTextUsageCacheStats_RecordsCachedShareOfAllInputTokens(t *testing.T) {
	tests := []struct {
		name             string
		relayFormat      types.RelayFormat
		usage            *dto.Usage
		wantPromptTokens int64
		wantCachedTokens int64
		wantTotalTokens  int64
	}{
		{
			name:             "claude client on claude channel counts cache reads and writes",
			relayFormat:      types.RelayFormatClaude,
			usage:            claudeUpstreamUsage(),
			wantPromptTokens: 1000,
			wantCachedTokens: 900,
			wantTotalTokens:  1020,
		},
		{
			name:             "openai client on claude channel follows the anthropic usage semantic",
			relayFormat:      types.RelayFormatOpenAI,
			usage:            claudeUpstreamUsage(),
			wantPromptTokens: 1000,
			wantCachedTokens: 900,
			wantTotalTokens:  1020,
		},
		{
			name:             "claude client on openai-style channel keeps prompt tokens as reported",
			relayFormat:      types.RelayFormatClaude,
			usage:            openAIStyleUpstreamUsage(),
			wantPromptTokens: 1000,
			wantCachedTokens: 990,
			wantTotalTokens:  1020,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ruleName, keyFP := uniqueChannelAffinityTestKeys()
			ctx := buildChannelAffinityStatsContextForTest(ruleName, "default", keyFP)
			relayInfo := &relaycommon.RelayInfo{RelayFormat: tt.relayFormat}

			observeTextUsageCacheStats(ctx, relayInfo, effectiveBillingUsage(tt.usage))

			stats := GetChannelAffinityUsageCacheStats(ruleName, "default", keyFP)
			require.EqualValues(t, 1, stats.Total)
			assert.EqualValues(t, 1, stats.Hit)
			assert.Equal(t, cacheTokenRateModeCachedOverPrompt, stats.CachedTokenRateMode)
			assert.Equal(t, tt.wantPromptTokens, stats.PromptTokens)
			assert.Equal(t, tt.wantCachedTokens, stats.CachedTokens)
			assert.Equal(t, tt.wantTotalTokens, stats.TotalTokens)
			assert.EqualValues(t, 20, stats.CompletionTokens)
		})
	}
}

// Billing reads the same usage object after the stats are recorded, so the
// widening must not leak back into it.
func TestObserveTextUsageCacheStats_LeavesBillingUsageUntouched(t *testing.T) {
	ruleName, keyFP := uniqueChannelAffinityTestKeys()
	ctx := buildChannelAffinityStatsContextForTest(ruleName, "default", keyFP)
	relayInfo := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatClaude}
	usage := effectiveBillingUsage(claudeUpstreamUsage())

	observeTextUsageCacheStats(ctx, relayInfo, usage)

	assert.Equal(t, 50, usage.PromptTokens)
	assert.Equal(t, 900, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, "anthropic", usage.UsageSemantic)
}
