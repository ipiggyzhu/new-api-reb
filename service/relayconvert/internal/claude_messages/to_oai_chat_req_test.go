package claudemessages

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeMessagesRequestToOpenAIChatAssistantTextWithToolUse(t *testing.T) {
	requestJSON := `{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "What is the weather in Paris?"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me check that for you."},
				{"type": "tool_use", "id": "toolu_01", "name": "get_weather", "input": {"city": "Paris"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_01", "content": "18C, sunny"}
			]}
		]
	}`
	var claudeRequest dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(requestJSON, &claudeRequest))

	openAIRequest, err := ClaudeMessagesRequestToOpenAIChat(claudeRequest, nil)
	require.NoError(t, err)
	require.Len(t, openAIRequest.Messages, 3)

	assistantMessage := openAIRequest.Messages[1]
	require.Equal(t, "assistant", assistantMessage.Role)

	toolCalls := assistantMessage.ParseToolCalls()
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "toolu_01", toolCalls[0].ID)
	assert.Equal(t, "get_weather", toolCalls[0].Function.Name)

	content := assistantMessage.ParseContent()
	require.Len(t, content, 1)
	assert.Equal(t, "text", content[0].Type)
	assert.Equal(t, "Let me check that for you.", content[0].Text)

	assert.Equal(t, "tool", openAIRequest.Messages[2].Role)
}

func TestClaudeMessagesRequestToOpenAIChatToolChoice(t *testing.T) {
	tests := []struct {
		name           string
		toolChoiceJSON string
		wantToolChoice any
		wantParallel   *bool
	}{
		{
			name:           "auto",
			toolChoiceJSON: `{"type":"auto"}`,
			wantToolChoice: "auto",
		},
		{
			name:           "any maps to required",
			toolChoiceJSON: `{"type":"any"}`,
			wantToolChoice: "required",
		},
		{
			name:           "none",
			toolChoiceJSON: `{"type":"none"}`,
			wantToolChoice: "none",
		},
		{
			name:           "forced tool",
			toolChoiceJSON: `{"type":"tool","name":"get_weather"}`,
			wantToolChoice: map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_weather"},
			},
		},
		{
			name:           "disable parallel tool use",
			toolChoiceJSON: `{"type":"any","disable_parallel_tool_use":true}`,
			wantToolChoice: "required",
			wantParallel:   common.GetPointer(false),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requestJSON := fmt.Sprintf(`{
				"model": "claude-sonnet-4",
				"max_tokens": 64,
				"messages": [{"role": "user", "content": "hi"}],
				"tools": [{"name": "get_weather", "description": "d", "input_schema": {"type": "object"}}],
				"tool_choice": %s
			}`, tc.toolChoiceJSON)
			var claudeRequest dto.ClaudeRequest
			require.NoError(t, common.UnmarshalJsonStr(requestJSON, &claudeRequest))

			openAIRequest, err := ClaudeMessagesRequestToOpenAIChat(claudeRequest, nil)
			require.NoError(t, err)

			assert.Equal(t, tc.wantToolChoice, openAIRequest.ToolChoice)
			if tc.wantParallel == nil {
				assert.Nil(t, openAIRequest.ParallelTooCalls)
			} else {
				require.NotNil(t, openAIRequest.ParallelTooCalls)
				assert.Equal(t, *tc.wantParallel, *openAIRequest.ParallelTooCalls)
			}
		})
	}
}

func TestClaudeMessagesRequestToOpenAIChatToolChoiceAbsent(t *testing.T) {
	requestJSON := `{
		"model": "claude-sonnet-4",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}]
	}`
	var claudeRequest dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(requestJSON, &claudeRequest))

	openAIRequest, err := ClaudeMessagesRequestToOpenAIChat(claudeRequest, nil)
	require.NoError(t, err)

	assert.Nil(t, openAIRequest.ToolChoice)
	assert.Nil(t, openAIRequest.ParallelTooCalls)
}
