package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// attachUsageAnomaly records, for admins only, a successful text reply whose
// usage cannot be right: no usage at all, a reply billed with no prompt side,
// or a Claude stream that ended declaring a smaller prompt than message_start.
// It only audits. The quota has already been settled and is left untouched.
//
// Only chat-shaped endpoints are audited. Embeddings, moderations, rerank,
// images and audio can legitimately settle with no prompt or no output tokens.
//
// The prompt side is input + cache read + cache creation in both usage
// semantics. For OpenAI usage the cache figures are part of prompt_tokens
// already, so adding them only matters when prompt_tokens is zero and the
// cache figures are not, a shape this audit deliberately leaves alone.
//
// Detection is conservative. A Claude stream is flagged only when the final
// prompt side shrank below what message_start declared: message_delta only
// overrides fields it reports as positive, so an honest stream ends equal to
// or larger than its start (a placeholder start is not an anomaly).
func attachUsageAnomaly(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, originUsage *dto.Usage, billingUsage *dto.Usage, other map[string]interface{}) {
	if relayInfo == nil || other == nil {
		return
	}
	switch relayInfo.RelayFormat {
	case types.RelayFormatClaude, types.RelayFormatOpenAIResponses:
	case types.RelayFormatOpenAI:
		if relayInfo.RelayMode != relayconstant.RelayModeChatCompletions && relayInfo.RelayMode != relayconstant.RelayModeCompletions {
			return
		}
	default:
		return
	}

	kind := ""
	promptTokens, completionTokens, streamStartPromptTokens := 0, 0, 0
	if billingUsage != nil {
		promptTokens = billingUsage.PromptTokens + billingUsage.PromptTokensDetails.CachedTokens + billingUsage.PromptTokensDetails.CacheCreationTokensTotal()
		completionTokens = billingUsage.CompletionTokens
	}
	if originUsage != nil {
		streamStartPromptTokens = originUsage.StreamStartPromptTokens
	}
	switch {
	case originUsage == nil:
		kind = dto.UsageAnomalyZeroUsage
	case originUsage.UpstreamUsageAnomaly != "":
		kind = originUsage.UpstreamUsageAnomaly
	case promptTokens == 0 && completionTokens == 0:
		kind = dto.UsageAnomalyZeroUsage
	case promptTokens == 0:
		kind = dto.UsageAnomalyZeroPromptTokens
	case streamStartPromptTokens > promptTokens:
		kind = dto.UsageAnomalyClaudeStreamUsageMismatch
	default:
		return
	}

	anomaly := map[string]interface{}{
		"kind":              kind,
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
	}
	if streamStartPromptTokens > 0 {
		anomaly["stream_start_prompt_tokens"] = streamStartPromptTokens
	}
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	if !ok || adminInfo == nil {
		adminInfo = map[string]interface{}{}
		other["admin_info"] = adminInfo
	}
	adminInfo["usage_anomaly"] = anomaly
	logger.LogWarn(ctx, fmt.Sprintf("usage anomaly on consume log: kind=%s prompt=%d completion=%d stream_start_prompt=%d channel=%d model=%s",
		kind, promptTokens, completionTokens, streamStartPromptTokens, relayInfo.ChannelId, relayInfo.OriginModelName))
}
