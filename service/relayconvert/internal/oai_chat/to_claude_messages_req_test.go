package oaichat

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIChatToClaudeMessagesReasoningEffortClampsBudgetAndClearsSampling(t *testing.T) {
	settings := model_setting.GetClaudeSettings()
	prevPercentage := settings.ThinkingAdapterBudgetTokensPercentage
	settings.ThinkingAdapterBudgetTokensPercentage = 0.8
	t.Cleanup(func() {
		settings.ThinkingAdapterBudgetTokensPercentage = prevPercentage
	})

	req := dto.GeneralOpenAIRequest{
		Model:           "claude-sonnet-4-5",
		ReasoningEffort: "low",
		Temperature:     lo.ToPtr(0.3),
		TopP:            lo.ToPtr(0.9),
		TopK:            lo.ToPtr(40),
		MaxTokens:       lo.ToPtr(uint(1000)),
		Messages:        []dto.Message{{Role: "user", Content: "hi"}},
	}

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	require.NotNil(t, got.Thinking)

	assert.Equal(t, "enabled", got.Thinking.Type)
	require.NotNil(t, got.MaxTokens)
	assert.Equal(t, uint(1280), *got.MaxTokens)
	require.NotNil(t, got.Thinking.BudgetTokens)
	assert.Equal(t, 1024, *got.Thinking.BudgetTokens)
	assert.Less(t, *got.Thinking.BudgetTokens, int(*got.MaxTokens))
	assert.Nil(t, got.Temperature)
	assert.Nil(t, got.TopP)
	assert.Nil(t, got.TopK)
}

func TestOpenAIChatToClaudeMessagesReasoningEffortKeepsBudgetBelowMaxTokens(t *testing.T) {
	req := dto.GeneralOpenAIRequest{
		Model:           "claude-sonnet-4-5",
		ReasoningEffort: "high",
		MaxTokens:       lo.ToPtr(uint(8000)),
		Messages:        []dto.Message{{Role: "user", Content: "hi"}},
	}

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	require.NotNil(t, got.Thinking)

	assert.Equal(t, "enabled", got.Thinking.Type)
	require.NotNil(t, got.Thinking.BudgetTokens)
	assert.Equal(t, 4096, *got.Thinking.BudgetTokens)
	require.NotNil(t, got.MaxTokens)
	assert.Equal(t, uint(8000), *got.MaxTokens)
}

func TestOpenAIChatToClaudeMessagesMergePreservesQuotes(t *testing.T) {
	req := dto.GeneralOpenAIRequest{
		Model: "claude-sonnet-4-5",
		Messages: []dto.Message{
			{Role: "user", Content: `"To be or not to be"`},
			{Role: "user", Content: `— explain this quote`},
		},
	}

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)

	assert.Equal(t, "user", got.Messages[0].Role)
	assert.Equal(t, `"To be or not to be" — explain this quote`, got.Messages[0].Content)
}

func TestOpenAIChatToClaudeMessagesToolSchemaTypeArray(t *testing.T) {
	req := dto.GeneralOpenAIRequest{
		Model: "claude-sonnet-4-5",
		Tools: []dto.ToolCallRequest{{
			Type: "function",
			Function: dto.FunctionRequest{
				Name: "f",
				Parameters: map[string]any{
					"type":       []any{"object", "null"},
					"properties": map[string]any{"x": map[string]any{"type": "string"}},
				},
			},
		}},
		Messages: []dto.Message{{Role: "user", Content: "hi"}},
	}

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	tools, ok := got.Tools.([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)

	tool, ok := tools[0].(*dto.Tool)
	require.True(t, ok)
	assert.Equal(t, "f", tool.Name)
	assert.Equal(t, []any{"object", "null"}, tool.InputSchema["type"])
}

func TestOpenAIChatToClaudeMessagesStopArraySkipsNonStrings(t *testing.T) {
	req := dto.GeneralOpenAIRequest{
		Model:    "claude-sonnet-4-5",
		Stop:     []any{"end", float64(1), true},
		Messages: []dto.Message{{Role: "user", Content: "hi"}},
	}

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"end"}, got.StopSequences)
}
