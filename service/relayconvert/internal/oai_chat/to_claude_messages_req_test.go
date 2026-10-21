package oaichat

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaymedia "github.com/QuantumNous/new-api/service/relayconvert/internal/media"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

// Anthropic only caches a prefix up to an explicit cache_control breakpoint, so
// breakpoints an OpenAI-format client puts on content parts (as OpenRouter
// documents) must reach the Claude request; dropping them turns every request
// of such a client into a full cache miss.
func TestOpenAIChatToClaudeMessagesKeepsContentCacheControl(t *testing.T) {
	relaymedia.SetMediaResolver(relaymedia.MediaResolver{
		GetBase64Data: func(c *gin.Context, source types.FileSource, reason ...string) (string, string, error) {
			return "aGVsbG8=", "image/png", nil
		},
	})
	t.Cleanup(func() { relaymedia.SetMediaResolver(relaymedia.MediaResolver{}) })

	body := `{
		"model": "claude-sonnet-4-5",
		"messages": [
			{"role": "system", "content": [
				{"type": "text", "text": "long stable instructions", "cache_control": {"type": "ephemeral"}}
			]},
			{"role": "user", "content": [
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}, "cache_control": {"type": "ephemeral", "ttl": "1h"}},
				{"type": "text", "text": "shared document", "cache_control": {"type": "ephemeral"}},
				{"type": "text", "text": "question"}
			]}
		]
	}`
	var req dto.GeneralOpenAIRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &req))

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	upstream, err := common.Marshal(got)
	require.NoError(t, err)

	system := gjson.GetBytes(upstream, "system.0")
	assert.Equal(t, "long stable instructions", system.Get("text").String())
	assert.JSONEq(t, `{"type":"ephemeral"}`, system.Get("cache_control").Raw)

	content := gjson.GetBytes(upstream, "messages.0.content")
	require.Len(t, content.Array(), 3)
	assert.Equal(t, "image", content.Get("0.type").String())
	assert.JSONEq(t, `{"type":"ephemeral","ttl":"1h"}`, content.Get("0.cache_control").Raw)
	assert.Equal(t, "shared document", content.Get("1.text").String())
	assert.JSONEq(t, `{"type":"ephemeral"}`, content.Get("1.cache_control").Raw)
	assert.Equal(t, "question", content.Get("2.text").String())
	assert.False(t, content.Get("2.cache_control").Exists(), "a part without a breakpoint must not gain one")
}

// Anthropic rejects a request with more than four cache_control breakpoints.
// Kept client breakpoints must never turn a working request into a 400; the
// latest ones cover the longest prefix, so they are the ones that stay.
func TestOpenAIChatToClaudeMessagesKeepsAtMostFourLatestCacheBreakpoints(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"messages": [
			{"role": "system", "content": [
				{"type": "text", "text": "one", "cache_control": {"type": "ephemeral", "ttl": "1h"}},
				{"type": "text", "text": "two", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "three", "cache_control": {"type": "ephemeral"}},
				{"type": "text", "text": "four", "cache_control": {"type": "ephemeral"}}
			]},
			{"role": "assistant", "content": "calling", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": [{"type": "text", "text": "five", "cache_control": {"type": "ephemeral"}}]},
			{"role": "user", "content": [{"type": "text", "text": "six", "cache_control": {"type": "ephemeral"}}]}
		]
	}`
	var req dto.GeneralOpenAIRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &req))

	got, err := OpenAIChatRequestToClaudeMessages(nil, req)
	require.NoError(t, err)
	upstream, err := common.Marshal(got)
	require.NoError(t, err)

	assert.Equal(t, 4, strings.Count(string(upstream), `"cache_control"`))
	assert.False(t, gjson.GetBytes(upstream, "system.0.cache_control").Exists())
	assert.False(t, gjson.GetBytes(upstream, "system.1.cache_control").Exists())
	assert.Equal(t, "two", gjson.GetBytes(upstream, "system.1.text").String(), "dropping a breakpoint keeps its block")
	for _, path := range []string{"messages.0.content.0", "messages.0.content.1", "messages.2.content.0.content.0", "messages.3.content.0"} {
		assert.Equal(t, "ephemeral", gjson.GetBytes(upstream, path+".cache_control.type").String(), path)
	}
}
