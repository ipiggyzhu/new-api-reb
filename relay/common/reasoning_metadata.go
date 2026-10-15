package common

import (
	"github.com/QuantumNous/new-api/dto"
	"github.com/tidwall/gjson"
)

// RequestReasoning records client settings, not inferred model defaults or the
// settings an adaptor may later rewrite. A nil budget differs from zero.
type RequestReasoning struct {
	Effort string `json:"effort,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Budget *int   `json:"budget,omitempty"`
}

func requestReasoning(request dto.Request) *RequestReasoning {
	r := &RequestReasoning{}
	switch request := request.(type) {
	case *dto.ClaudeRequest:
		if request == nil {
			return nil
		}
		r.Effort = request.GetEfforts()
		// Per-message system settings supersede the top-level effort only once
		// a subsequent user turn makes them effective.
		pending := r.Effort
		for _, message := range request.Messages {
			if message.Role == "system" {
				if effort := gjson.GetBytes(message.OutputConfig, "effort"); effort.Type == gjson.String {
					pending = effort.String()
				}
			}
			if message.Role == "user" {
				r.Effort = pending
			}
		}
		if request.Thinking != nil {
			r.Mode = request.Thinking.Type
			r.Budget = request.Thinking.BudgetTokens
		}
	case *dto.GeminiChatRequest:
		if request == nil {
			return nil
		}
		if thinking := request.GenerationConfig.ThinkingConfig; thinking != nil {
			r.Effort = thinking.ThinkingLevel
			r.Budget = thinking.ThinkingBudget
		}
	case *dto.GeneralOpenAIRequest:
		if request == nil {
			return nil
		}
		r.Effort = request.ReasoningEffort
		if r.Effort == "" {
			r.Effort = gjson.GetBytes(request.Reasoning, "effort").String()
		}
		r.Mode = gjson.GetBytes(request.THINKING, "type").String()
		if r.Mode == "" && len(request.EnableThinking) != 0 {
			switch gjson.ParseBytes(request.EnableThinking).Type {
			case gjson.True:
				r.Mode = "enabled"
			case gjson.False:
				r.Mode = "disabled"
			}
		}
	case *dto.OpenAIResponsesRequest:
		if request == nil {
			return nil
		}
		if request.Reasoning != nil {
			r.Effort = request.Reasoning.Effort
		}
	default:
		return nil
	}
	// Do not retain pointers into mutable request DTOs.
	if r.Budget != nil {
		budget := *r.Budget
		r.Budget = &budget
	}
	return r
}

// CaptureUpstreamReasoning accepts only explicit response metadata. Thinking
// text, token counts, request echoes nested inside input, and model names do
// not establish an upstream-reported effort. Missing later frames don't erase it.
func (info *RelayInfo) CaptureUpstreamReasoning(data string) {
	if info == nil {
		return
	}
	root := gjson.Parse(data)
	for _, path := range []string{
		"reasoning.effort", "reasoning_effort", "output_config.effort",
		"response.reasoning.effort", "response.reasoning_effort", "message.output_config.effort",
		"generationConfig.thinkingConfig.thinkingLevel",
		"generation_config.thinking_config.thinking_level",
	} {
		value := root.Get(path)
		if value.Type == gjson.String && value.String() != "" {
			info.UpstreamReasoningEffort = value.String()
			return
		}
	}
}
