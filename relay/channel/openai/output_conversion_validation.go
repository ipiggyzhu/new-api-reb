package openai

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"
	"github.com/tidwall/gjson"
)

// Conversion can omit fields that are valid in the source protocol. Validate
// the generated response before trusting it as output delivered to the caller.
func convertedResponseHasOutput(response gjson.Result, format types.RelayFormat, usage *dto.Usage) bool {
	hasHiddenReasoning := usage != nil && usage.GetOutputTokenDetails().ReasoningTokens > 0
	switch format {
	case types.RelayFormatOpenAI:
		return chatResponseHasOutput(response)
	case types.RelayFormatOpenAIResponses:
		return responsesResponseHasOutput(response) || responsesStreamEventHasOutput(response)
	case types.RelayFormatClaude:
		if hasOutputString(response.Get("completion")) || response.Get("stop_reason").String() == "refusal" ||
			response.Get("delta.stop_reason").String() == "refusal" {
			return true
		}
		if hasHiddenReasoning && (response.Get("stop_reason").String() == "max_tokens" || response.Get("delta.stop_reason").String() == "max_tokens") {
			return true
		}
		parts := response.Get("content").Array()
		parts = append(parts, response.Get("content_block"), response.Get("delta"))
		for _, part := range parts {
			if hasOutputString(part.Get("text")) || hasOutputString(part.Get("thinking")) ||
				hasOutputString(part.Get("partial_json")) || hasOutputString(part.Get("refusal")) {
				return true
			}
			switch part.Get("type").String() {
			case "tool_use", "server_tool_use":
				if hasOutputString(part.Get("name")) {
					return true
				}
			case "redacted_thinking":
				if hasOutputString(part.Get("data")) {
					return true
				}
			}
		}
	case types.RelayFormatGemini:
		if hasOutputString(response.Get("promptFeedback.blockReason")) {
			return true
		}
		for _, candidate := range response.Get("candidates").Array() {
			finishReason := candidate.Get("finishReason").String()
			if finishReason == "SAFETY" || (finishReason == "MAX_TOKENS" && hasHiddenReasoning) {
				return true
			}
			for _, part := range candidate.Get("content.parts").Array() {
				if hasOutputString(part.Get("text")) || hasOutputString(part.Get("functionCall.name")) ||
					hasOutputString(part.Get("inlineData.data")) || hasOutputString(part.Get("fileData.fileUri")) {
					return true
				}
			}
		}
	}
	return false
}

type convertedOutputState struct {
	hasOutput bool
	chat      chatOutputState
}

func (s *convertedOutputState) observe(result relayconvert.ResponseResult) error {
	value := result.Value
	if event, ok := value.(relayconvert.ChatToResponsesStreamEvent); ok {
		value = event.Payload
	}
	body, err := common.Marshal(value)
	if err != nil {
		return err
	}
	response := gjson.ParseBytes(body)
	if result.To == types.RelayFormatOpenAI {
		// Usage is optional on the wire. A length finish can still represent
		// a budget exhausted on hidden reasoning when usage was not requested.
		if result.Usage != nil && result.Usage.CompletionTokenDetails.ReasoningTokens > 0 {
			s.chat.reasoningTokens = true
		}
		s.chat.observe(response)
		s.hasOutput = s.hasOutput || s.chat.hasOutput
	} else {
		s.hasOutput = s.hasOutput || convertedResponseHasOutput(response, result.To, result.Usage)
	}
	return nil
}
