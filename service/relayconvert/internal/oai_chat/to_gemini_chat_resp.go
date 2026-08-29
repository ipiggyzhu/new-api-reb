package oaichat

import (
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// The conversion registry invokes StreamResponseOpenAI2Gemini once per upstream
// chunk with no per-stream state, while OpenAI streams tool-call arguments as
// JSON fragments across chunks and the Gemini protocol has no argument
// streaming at all. Fragments are therefore accumulated here, keyed by the
// request's RelayInfo (stable for the lifetime of one relay request), and
// flushed as complete functionCall parts when the candidate's finish_reason
// arrives. Entries are removed on flush; streams aborted before finishing are
// reclaimed by a TTL sweep.
var geminiStreamToolStates sync.Map

const geminiStreamToolStateTTL = 30 * time.Minute

type geminiPendingToolCall struct {
	name string
	args strings.Builder
}

type geminiPendingToolCalls struct {
	byIndex map[int]*geminiPendingToolCall
	order   []int
}

type geminiStreamToolState struct {
	streamID   string
	candidates map[int]*geminiPendingToolCalls
	lastSeen   time.Time
}

func newGeminiStreamToolState(streamID string) *geminiStreamToolState {
	return &geminiStreamToolState{
		streamID:   streamID,
		candidates: make(map[int]*geminiPendingToolCalls),
		lastSeen:   time.Now(),
	}
}

func geminiStreamToolStateFor(info *relaycommon.RelayInfo, streamID string, create bool) *geminiStreamToolState {
	if info == nil {
		// No stable key to accumulate across chunks; a throwaway state still
		// assembles calls delivered whole within a single chunk.
		if !create {
			return nil
		}
		return newGeminiStreamToolState(streamID)
	}
	if value, ok := geminiStreamToolStates.Load(info); ok {
		state := value.(*geminiStreamToolState)
		if streamID != "" && state.streamID != "" && state.streamID != streamID {
			// The upstream stream restarted (retry on the same RelayInfo);
			// stale fragments must not bleed into the new attempt.
			state = newGeminiStreamToolState(streamID)
			geminiStreamToolStates.Store(info, state)
		}
		return state
	}
	if !create {
		return nil
	}
	now := time.Now()
	geminiStreamToolStates.Range(func(key, value any) bool {
		if stale, ok := value.(*geminiStreamToolState); ok && now.Sub(stale.lastSeen) > geminiStreamToolStateTTL {
			geminiStreamToolStates.Delete(key)
		}
		return true
	})
	state := newGeminiStreamToolState(streamID)
	geminiStreamToolStates.Store(info, state)
	return state
}

func (s *geminiStreamToolState) accumulate(candidateIndex int, calls []dto.ToolCallResponse) {
	pending := s.candidates[candidateIndex]
	if pending == nil {
		pending = &geminiPendingToolCalls{byIndex: make(map[int]*geminiPendingToolCall)}
		s.candidates[candidateIndex] = pending
	}
	for i := range calls {
		call := &calls[i]
		idx := i
		if call.Index != nil {
			idx = *call.Index
		}
		acc := pending.byIndex[idx]
		if acc == nil {
			acc = &geminiPendingToolCall{}
			pending.byIndex[idx] = acc
			pending.order = append(pending.order, idx)
		}
		if acc.name == "" {
			acc.name = call.Function.Name
		}
		acc.args.WriteString(call.Function.Arguments)
	}
	s.lastSeen = time.Now()
}

func (s *geminiStreamToolState) flush(candidateIndex int) []dto.GeminiPart {
	pending := s.candidates[candidateIndex]
	if pending == nil {
		return nil
	}
	delete(s.candidates, candidateIndex)
	parts := make([]dto.GeminiPart, 0, len(pending.order))
	for _, idx := range pending.order {
		call := pending.byIndex[idx]
		rawArgs := call.args.String()
		var args map[string]interface{}
		if strings.TrimSpace(rawArgs) == "" {
			args = make(map[string]interface{})
		} else if err := common.Unmarshal([]byte(rawArgs), &args); err != nil {
			args = map[string]interface{}{"arguments": rawArgs}
		}
		parts = append(parts, dto.GeminiPart{
			FunctionCall: &dto.FunctionCall{
				FunctionName: call.name,
				Arguments:    args,
			},
		})
	}
	return parts
}

// ResponseOpenAI2Gemini 将 OpenAI 响应转换为 Gemini 格式
func ResponseOpenAI2Gemini(openAIResponse *dto.OpenAITextResponse, info *relaycommon.RelayInfo) *dto.GeminiChatResponse {
	totalTokens := openAIResponse.TotalTokens
	if totalTokens == 0 {
		totalTokens = openAIResponse.PromptTokens + openAIResponse.CompletionTokens
	}
	geminiResponse := &dto.GeminiChatResponse{
		Candidates:       make([]dto.GeminiChatCandidate, 0, len(openAIResponse.Choices)),
		HasUsageMetadata: true,
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:     openAIResponse.PromptTokens,
			CandidatesTokenCount: openAIResponse.CompletionTokens,
			TotalTokenCount:      totalTokens,
			BillingUsage:         openAIBillingUsageFromUsage(&openAIResponse.Usage),
		},
	}
	if metadata, ok := geminiBillingMetadataFromOpenAIUsage(&openAIResponse.Usage); ok {
		geminiResponse.UsageMetadata = metadata
	}

	for _, choice := range openAIResponse.Choices {
		candidate := dto.GeminiChatCandidate{
			Index:         int64(choice.Index),
			SafetyRatings: []dto.GeminiChatSafetyRating{},
		}

		// 设置结束原因
		var finishReason string
		switch choice.FinishReason {
		case "stop":
			finishReason = "STOP"
		case "length":
			finishReason = "MAX_TOKENS"
		case "content_filter":
			finishReason = "SAFETY"
		case "tool_calls":
			finishReason = "STOP"
		default:
			finishReason = "STOP"
		}
		candidate.FinishReason = &finishReason

		// 转换消息内容
		content := dto.GeminiChatContent{
			Role:  "model",
			Parts: make([]dto.GeminiPart, 0),
		}

		textContent := choice.Message.StringContent()
		if textContent != "" {
			part := dto.GeminiPart{
				Text: textContent,
			}
			content.Parts = append(content.Parts, part)
		}

		toolCalls := choice.Message.ParseToolCalls()
		for _, toolCall := range toolCalls {
			var args map[string]interface{}
			if toolCall.Function.Arguments != "" {
				if err := common.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
					args = map[string]interface{}{"arguments": toolCall.Function.Arguments}
				}
			} else {
				args = make(map[string]interface{})
			}

			part := dto.GeminiPart{
				FunctionCall: &dto.FunctionCall{
					FunctionName: toolCall.Function.Name,
					Arguments:    args,
				},
			}
			content.Parts = append(content.Parts, part)
		}

		candidate.Content = content
		geminiResponse.Candidates = append(geminiResponse.Candidates, candidate)
	}

	return geminiResponse
}

// StreamResponseOpenAI2Gemini 将 OpenAI 流式响应转换为 Gemini 格式
func StreamResponseOpenAI2Gemini(openAIResponse *dto.ChatCompletionsStreamResponse, info *relaycommon.RelayInfo) *dto.GeminiChatResponse {
	// 检查是否有实际内容或结束标志
	hasContent := false
	hasFinishReason := false
	hasToolFragments := false
	for _, choice := range openAIResponse.Choices {
		if len(choice.Delta.GetContentString()) > 0 {
			hasContent = true
		}
		if len(choice.Delta.ToolCalls) > 0 {
			hasToolFragments = true
		}
		if choice.FinishReason != nil {
			hasFinishReason = true
		}
	}

	// 如果没有实际内容且没有结束标志，跳过。主要针对 openai 流响应开头的空数据
	if !hasContent && !hasFinishReason && !hasToolFragments {
		return nil
	}

	var toolState *geminiStreamToolState
	if hasToolFragments {
		toolState = geminiStreamToolStateFor(info, openAIResponse.Id, true)
		for _, choice := range openAIResponse.Choices {
			if len(choice.Delta.ToolCalls) > 0 {
				toolState.accumulate(choice.Index, choice.Delta.ToolCalls)
			}
		}
	} else if hasFinishReason {
		toolState = geminiStreamToolStateFor(info, openAIResponse.Id, false)
	}

	if !hasContent && !hasFinishReason {
		// 工具调用参数分片已聚合，等待 finish_reason 时一次性输出
		return nil
	}

	estimatePromptTokens := 0
	if info != nil {
		estimatePromptTokens = info.GetEstimatePromptTokens()
	}
	geminiResponse := &dto.GeminiChatResponse{
		Candidates:       make([]dto.GeminiChatCandidate, 0, len(openAIResponse.Choices)),
		HasUsageMetadata: true,
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:     estimatePromptTokens,
			CandidatesTokenCount: 0, // 流式响应中可能没有完整的 usage 信息
			TotalTokenCount:      estimatePromptTokens,
		},
	}

	if openAIResponse.Usage != nil {
		geminiResponse.UsageMetadata.PromptTokenCount = openAIResponse.Usage.PromptTokens
		geminiResponse.UsageMetadata.CandidatesTokenCount = openAIResponse.Usage.CompletionTokens
		geminiResponse.UsageMetadata.TotalTokenCount = openAIResponse.Usage.TotalTokens
		geminiResponse.UsageMetadata.BillingUsage = openAIBillingUsageFromUsage(openAIResponse.Usage)
		if metadata, ok := geminiBillingMetadataFromOpenAIUsage(openAIResponse.Usage); ok {
			geminiResponse.UsageMetadata = metadata
		}
	}

	for _, choice := range openAIResponse.Choices {
		candidate := dto.GeminiChatCandidate{
			Index:         int64(choice.Index),
			SafetyRatings: []dto.GeminiChatSafetyRating{},
		}

		// 设置结束原因
		if choice.FinishReason != nil {
			var finishReason string
			switch *choice.FinishReason {
			case "stop":
				finishReason = "STOP"
			case "length":
				finishReason = "MAX_TOKENS"
			case "content_filter":
				finishReason = "SAFETY"
			case "tool_calls":
				finishReason = "STOP"
			default:
				finishReason = "STOP"
			}
			candidate.FinishReason = &finishReason
		}

		// 转换消息内容
		content := dto.GeminiChatContent{
			Role:  "model",
			Parts: make([]dto.GeminiPart, 0),
		}

		// 处理文本内容
		textContent := choice.Delta.GetContentString()
		if textContent != "" {
			part := dto.GeminiPart{
				Text: textContent,
			}
			content.Parts = append(content.Parts, part)
		}

		// 工具调用参数分片在 finish_reason 到达时一次性输出完整 functionCall
		if choice.FinishReason != nil && toolState != nil {
			content.Parts = append(content.Parts, toolState.flush(choice.Index)...)
		}

		candidate.Content = content
		geminiResponse.Candidates = append(geminiResponse.Candidates, candidate)
	}

	if info != nil && toolState != nil && len(toolState.candidates) == 0 {
		geminiStreamToolStates.Delete(info)
	}

	return geminiResponse
}

func geminiBillingMetadataFromOpenAIUsage(usage *dto.Usage) (dto.GeminiUsageMetadata, bool) {
	if usage == nil || usage.BillingUsage == nil || usage.BillingUsage.GeminiUsageMetadata == nil {
		return dto.GeminiUsageMetadata{}, false
	}
	if usage.BillingUsage.Source != dto.BillingUsageSourceGeminiChat && usage.BillingUsage.Semantic != dto.BillingUsageSemanticGemini {
		return dto.GeminiUsageMetadata{}, false
	}
	billingUsage := dto.CloneBillingUsage(usage.BillingUsage)
	if billingUsage == nil || billingUsage.GeminiUsageMetadata == nil {
		return dto.GeminiUsageMetadata{}, false
	}
	return *billingUsage.GeminiUsageMetadata, true
}

func openAIBillingUsageFromUsage(usage *dto.Usage) *dto.BillingUsage {
	if usage == nil {
		return nil
	}
	if existingBillingUsage := dto.CloneBillingUsage(usage.BillingUsage); existingBillingUsage != nil && existingBillingUsage.OpenAIUsage != nil {
		if existingBillingUsage.Source == dto.BillingUsageSourceOAIChat ||
			existingBillingUsage.Source == dto.BillingUsageSourceOAIResponses ||
			existingBillingUsage.Semantic == dto.BillingUsageSemanticOpenAI {
			return existingBillingUsage
		}
	}
	return dto.NewOpenAIChatBillingUsage(usage)
}
