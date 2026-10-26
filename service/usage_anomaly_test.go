package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func claudeAnomalyUsage(input, cacheRead, cacheCreation, output, streamStart int) *dto.Usage {
	usage := &dto.Usage{
		PromptTokens:            input,
		CompletionTokens:        output,
		UsageSemantic:           "anthropic",
		StreamStartPromptTokens: streamStart,
	}
	usage.PromptTokensDetails.CachedTokens = cacheRead
	usage.PromptTokensDetails.CachedCreationTokens = cacheCreation
	return usage
}

func TestAttachUsageAnomaly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamZero := claudeAnomalyUsage(0, 0, 0, 0, 0)
	upstreamZero.UpstreamUsageAnomaly = dto.UsageAnomalyZeroUsage
	// What the Claude stream handler hands over after its local estimate filled
	// the zero usage in: the billed numbers look fine, only the marker remembers.
	estimated := claudeAnomalyUsage(120, 0, 0, 30, 0)
	estimated.UpstreamUsageAnomaly = dto.UsageAnomalyZeroUsage

	tests := []struct {
		name       string
		format     types.RelayFormat
		mode       int
		usage      *dto.Usage
		wantKind   string
		wantPrompt int
		wantStart  int
	}{
		{name: "claude stream whose final usage matches message_start", format: types.RelayFormatClaude,
			usage: claudeAnomalyUsage(10, 50000, 2000, 300, 52010)},
		{name: "claude stream whose message_start was a placeholder", format: types.RelayFormatClaude,
			usage: claudeAnomalyUsage(10, 50000, 2000, 300, 1)},
		{name: "fully cached claude reply has a prompt side", format: types.RelayFormatClaude,
			usage: claudeAnomalyUsage(0, 50000, 0, 300, 50000)},
		{name: "openai chat with normal usage", format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions,
			usage: &dto.Usage{PromptTokens: 900, CompletionTokens: 40}},
		{name: "embeddings may settle without output", format: types.RelayFormatEmbedding,
			usage: &dto.Usage{PromptTokens: 0, CompletionTokens: 0}},
		{name: "moderations share the openai format but are not chat", format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeModerations,
			usage: &dto.Usage{PromptTokens: 0, CompletionTokens: 0}},

		{name: "claude stream ended with a smaller prompt than message_start", format: types.RelayFormatClaude,
			usage: claudeAnomalyUsage(10, 49000, 0, 300, 50010), wantKind: dto.UsageAnomalyClaudeStreamUsageMismatch, wantPrompt: 49010, wantStart: 50010},
		{name: "claude reply billed with no prompt side", format: types.RelayFormatClaude,
			usage: claudeAnomalyUsage(0, 0, 0, 300, 0), wantKind: dto.UsageAnomalyZeroPromptTokens},
		{name: "responses reply billed with no prompt side", format: types.RelayFormatOpenAIResponses,
			usage: &dto.Usage{PromptTokens: 0, CompletionTokens: 25}, wantKind: dto.UsageAnomalyZeroPromptTokens},
		{name: "openai chat with all-zero usage", format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions,
			usage: &dto.Usage{}, wantKind: dto.UsageAnomalyZeroUsage},
		{name: "claude reply with no usage at all", format: types.RelayFormatClaude,
			usage: nil, wantKind: dto.UsageAnomalyZeroUsage},
		{name: "upstream zero usage hidden by the local estimate", format: types.RelayFormatClaude,
			usage: estimated, wantKind: dto.UsageAnomalyZeroUsage, wantPrompt: 120},
		{name: "upstream zero usage left as reported", format: types.RelayFormatClaude,
			usage: upstreamZero, wantKind: dto.UsageAnomalyZeroUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(nil)
			relayInfo := &relaycommon.RelayInfo{
				RelayFormat: tt.format,
				RelayMode:   tt.mode,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 30},
			}
			var before dto.Usage
			if tt.usage != nil {
				before = *tt.usage
			}
			other := map[string]interface{}{"admin_info": map[string]interface{}{"admin_username": "root"}}

			attachUsageAnomaly(ctx, relayInfo, tt.usage, tt.usage, other)

			adminInfo := other["admin_info"].(map[string]interface{})
			assert.Equal(t, "root", adminInfo["admin_username"], "existing admin_info is kept")
			if tt.usage != nil {
				assert.Equal(t, before, *tt.usage, "the audit must not touch the usage it settles from")
			}
			if tt.wantKind == "" {
				assert.NotContains(t, adminInfo, "usage_anomaly")
				return
			}
			anomaly, ok := adminInfo["usage_anomaly"].(map[string]interface{})
			require.True(t, ok, "usage_anomaly should be nested under admin_info")
			assert.Equal(t, tt.wantKind, anomaly["kind"])
			assert.Equal(t, tt.wantPrompt, anomaly["prompt_tokens"])
			if tt.wantStart > 0 {
				assert.Equal(t, tt.wantStart, anomaly["stream_start_prompt_tokens"])
			} else {
				assert.NotContains(t, anomaly, "stream_start_prompt_tokens")
			}
		})
	}
}
