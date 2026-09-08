package openai

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/types"
	"github.com/tidwall/gjson"
)

func emptyOutputError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New("upstream returned no output"), types.ErrorCodeEmptyResponse, http.StatusBadGateway)
}

// Keep ordinary role/lifecycle frames until output makes the attempt usable.
// An upstream sending only metadata must not make this buffer grow indefinitely.
type outputPreamble struct {
	chunks []string
	bytes  int
}

func (p *outputPreamble) add(data string) bool {
	if len(p.chunks) >= 64 || len(data) > (64<<10)-p.bytes {
		return false
	}
	p.chunks = append(p.chunks, data)
	p.bytes += len(data)
	return true
}

func hasOutputString(value gjson.Result) bool {
	return value.Type == gjson.String && value.Str != ""
}

// The DTOs intentionally cover only the fields needed for conversion and usage.
// Inspect the original JSON as well so a native refusal, audio reply, or opaque
// reasoning item is not mistaken for an empty text response.
func hasOutputContent(value gjson.Result) bool {
	if hasOutputString(value) {
		return true
	}
	if value.IsArray() {
		for _, part := range value.Array() {
			if hasOutputContent(part) {
				return true
			}
		}
		return false
	}
	if !value.IsObject() {
		return false
	}
	for _, path := range []string{"text", "text.value", "refusal", "transcript", "image_url", "image_url.url", "image.data", "image.url", "audio.data", "audio.id", "audio.transcript"} {
		if hasOutputString(value.Get(path)) {
			return true
		}
	}
	switch value.Get("type").String() {
	case "audio", "output_audio", "image", "output_image", "video":
		return hasOutputString(value.Get("data")) || hasOutputString(value.Get("url")) || hasOutputString(value.Get("id"))
	}
	return false
}

func hasReasoningOutput(value gjson.Result) bool {
	if hasOutputContent(value) {
		return true
	}
	if value.IsArray() {
		for _, part := range value.Array() {
			if hasReasoningOutput(part) {
				return true
			}
		}
	}
	return value.IsObject() && (hasOutputString(value.Get("encrypted_content")) ||
		hasOutputString(value.Get("data")) || hasOutputContent(value.Get("summary")))
}

func hasCallOutput(call gjson.Result) bool {
	return call.IsObject() && (hasOutputString(call.Get("name")) ||
		hasOutputString(call.Get("arguments")) || hasOutputString(call.Get("input")))
}

func chatMessageHasOutput(message gjson.Result) bool {
	if !message.IsObject() {
		return false
	}
	if hasOutputContent(message.Get("content")) || hasOutputString(message.Get("refusal")) ||
		hasOutputContent(message.Get("images")) || hasCallOutput(message.Get("function_call")) {
		return true
	}
	for _, path := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
		if hasReasoningOutput(message.Get(path)) {
			return true
		}
	}
	for _, path := range []string{"audio.data", "audio.id", "audio.transcript"} {
		if hasOutputString(message.Get(path)) {
			return true
		}
	}
	toolCalls := message.Get("tool_calls")
	if !toolCalls.IsArray() {
		return false
	}
	for _, call := range toolCalls.Array() {
		if hasCallOutput(call.Get("function")) || hasCallOutput(call.Get("custom")) {
			return true
		}
	}
	return false
}

type chatOutputState struct {
	hasOutput       bool
	choices         map[int64]bool
	lengthFinished  bool
	reasoningTokens bool
}

func (s *chatOutputState) observe(response gjson.Result) {
	s.observeChoices(response, "delta")
}

func (s *chatOutputState) observeChoices(response gjson.Result, messageField string) {
	var choices []gjson.Result
	if value := response.Get("choices"); value.IsArray() {
		choices = value.Array()
	}
	for _, choice := range choices {
		if !choice.IsObject() {
			continue
		}
		if s.choices == nil {
			s.choices = make(map[int64]bool)
		}
		index := choice.Get("index").Int()
		finish := choice.Get("finish_reason")
		s.choices[index] = s.choices[index] || hasOutputString(finish)
		s.lengthFinished = s.lengthFinished || finish.String() == "length"
		if chatMessageHasOutput(choice.Get(messageField)) ||
			hasOutputString(choice.Get("text")) || finish.String() == "content_filter" {
			s.hasOutput = true
		}
	}
	reasoning := response.Get("usage.completion_tokens_details.reasoning_tokens")
	s.reasoningTokens = s.reasoningTokens || (reasoning.Type == gjson.Number && reasoning.Int() > 0)
	// A model may exhaust its budget on hidden reasoning before it can emit text.
	// Ordinary positive completion usage is not evidence that anything was output.
	s.hasOutput = s.hasOutput || (s.lengthFinished && s.reasoningTokens)
}

func (s *chatOutputState) finished() bool {
	if len(s.choices) == 0 {
		return false
	}
	for _, finished := range s.choices {
		if !finished {
			return false
		}
	}
	return true
}

func chatResponseHasOutput(response gjson.Result) bool {
	var output chatOutputState
	output.observeChoices(response, "message")
	return output.hasOutput
}

func responsesItemHasOutput(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	itemType := item.Get("type").String()
	switch itemType {
	case "message":
		return hasOutputContent(item.Get("content"))
	case "reasoning":
		return hasReasoningOutput(item) || hasOutputContent(item.Get("content")) || hasOutputString(item.Get("id"))
	case "compaction":
		return hasOutputString(item.Get("encrypted_content"))
	case "function_call", "custom_tool_call":
		return hasCallOutput(item)
	default:
		// Built-in tools have their own payloads. Their typed, identified output
		// item is meaningful even when they produce no text (for example search).
		if strings.HasSuffix(itemType, "_call") || strings.HasSuffix(itemType, "_call_output") ||
			itemType == "mcp_list_tools" || itemType == "mcp_approval_request" {
			return hasOutputString(item.Get("id")) || hasOutputString(item.Get("call_id")) || hasCallOutput(item) ||
				hasOutputString(item.Get("result")) || hasOutputContent(item.Get("output"))
		}
		return hasOutputContent(item)
	}
}

func responsesResponseHasOutput(response gjson.Result) bool {
	if output := response.Get("output"); output.IsArray() {
		for _, item := range output.Array() {
			if responsesItemHasOutput(item) {
				return true
			}
		}
	}
	if hasOutputString(response.Get("output_text")) {
		return true
	}
	if response.Get("status").String() == "incomplete" {
		switch response.Get("incomplete_details.reason").String() {
		case "content_filter":
			return true
		case "max_output_tokens":
			reasoning := response.Get("usage.output_tokens_details.reasoning_tokens")
			return reasoning.Type == gjson.Number && reasoning.Int() > 0
		}
	}
	return false
}

func responsesResponseIsDeferred(response gjson.Result) bool {
	if !hasOutputString(response.Get("id")) {
		return false
	}
	status := response.Get("status").String()
	return status == "queued" || status == "in_progress"
}

func isResponsesTerminalEvent(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.done" || eventType == "response.incomplete"
}

func responsesStreamEventHasOutput(event gjson.Result) bool {
	if responsesResponseHasOutput(event.Get("response")) || responsesItemHasOutput(event.Get("item")) || hasOutputContent(event.Get("part")) {
		return true
	}
	switch event.Get("type").String() {
	case "response.output_text.delta", "response.output_text.done",
		"response.refusal.delta", "response.refusal.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
		"response.audio.delta", "response.output_audio.delta", "response.audio_transcript.delta", "response.audio_transcript.done":
		for _, path := range []string{"delta", "text", "refusal", "arguments", "input", "data", "transcript"} {
			if hasOutputString(event.Get(path)) {
				return true
			}
		}
	case "response.image_generation_call.partial_image":
		return hasOutputString(event.Get("partial_image_b64"))
	}
	return false
}
