package oaichat

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseOpenAI2ClaudeToolUseInputIsObject(t *testing.T) {
	tests := []struct {
		name string
		args string
		want map[string]interface{}
	}{
		{name: "object", args: `{"q":"x"}`, want: map[string]interface{}{"q": "x"}},
		{name: "empty", args: "", want: map[string]interface{}{}},
		{name: "invalid", args: "{", want: map[string]interface{}{}},
		{name: "null", args: "null", want: map[string]interface{}{}},
		{name: "array", args: `["x"]`, want: map[string]interface{}{}},
		{name: "string", args: `"x"`, want: map[string]interface{}{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := dto.Message{Role: "assistant"}
			msg.SetToolCalls([]dto.ToolCallRequest{
				{
					ID:   "call_1",
					Type: "function",
					Function: dto.FunctionRequest{
						Name:      "lookup",
						Arguments: tt.args,
					},
				},
			})

			resp := ResponseOpenAI2Claude(&dto.OpenAITextResponse{
				Id:    "chatcmpl_1",
				Model: "gpt-test",
				Choices: []dto.OpenAITextResponseChoice{
					{Message: msg, FinishReason: "tool_calls"},
				},
			}, nil)

			require.Len(t, resp.Content, 1)
			assert.Equal(t, "tool_use", resp.Content[0].Type)
			assert.Equal(t, tt.want, resp.Content[0].Input)
		})
	}
}

func TestResponseOpenAI2ClaudeUsageCarriesOpenAIBillingUsage(t *testing.T) {
	resp := ResponseOpenAI2Claude(&dto.OpenAITextResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.OpenAITextResponseChoice{
			{Message: dto.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
		},
		Usage: dto.Usage{
			PromptTokens:     11,
			CompletionTokens: 5,
			TotalTokens:      16,
		},
	}, nil)

	require.NotNil(t, resp.Usage)
	assert.Equal(t, 11, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.OutputTokens)
	require.NotNil(t, resp.Usage.BillingUsage)
	require.NotNil(t, resp.Usage.BillingUsage.OpenAIUsage)
	assert.Equal(t, dto.BillingUsageSourceOAIChat, resp.Usage.BillingUsage.Source)
	assert.Equal(t, dto.BillingUsageSemanticOpenAI, resp.Usage.BillingUsage.Semantic)
	assert.Equal(t, 11, resp.Usage.BillingUsage.OpenAIUsage.PromptTokens)
	assert.Equal(t, 5, resp.Usage.BillingUsage.OpenAIUsage.CompletionTokens)
	assert.Equal(t, 16, resp.Usage.BillingUsage.OpenAIUsage.TotalTokens)
	assert.Nil(t, resp.Usage.BillingUsage.OpenAIUsage.BillingUsage)
}

func TestBuildClaudeUsageFromOpenAICacheWriteUsage(t *testing.T) {
	usage := buildClaudeUsageFromOpenAIUsage(&dto.Usage{
		PromptTokens:     3619,
		CompletionTokens: 36,
		TotalTokens:      3655,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens:     2921,
			CacheWriteTokens: 3616,
		},
	})

	require.NotNil(t, usage)
	// Claude semantics reports input_tokens excluding cache read/write; the
	// overlapping unadjusted prefixes drive the remainder negative, clamp to 0.
	assert.Equal(t, 0, usage.InputTokens)
	assert.Equal(t, 2921, usage.CacheReadInputTokens)
	assert.Equal(t, 3616, usage.CacheCreationInputTokens)
	assert.Equal(t, 36, usage.OutputTokens)
	require.NotNil(t, usage.BillingUsage)
	require.NotNil(t, usage.BillingUsage.OpenAIUsage)
	assert.Equal(t, dto.BillingUsageSemanticOpenAI, usage.BillingUsage.Semantic)
	assert.Equal(t, 3616, usage.BillingUsage.OpenAIUsage.PromptTokensDetails.CacheWriteTokens)
}

func TestStreamResponseOpenAI2ClaudeClosesTextThinkingAndToolBlocks(t *testing.T) {
	info := &relaycommon.RelayInfo{
		ClaudeConvertInfo: &relaycommon.ClaudeConvertInfo{
			LastMessagesType: relaycommon.LastMessageTypeNone,
		},
	}

	info.SendResponseCount = 1
	textResponses := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
					Content: ptr("hello"),
				},
			},
		},
	}, info)
	require.Len(t, textResponses, 3)
	assert.Equal(t, "message_start", textResponses[0].Type)
	assert.Equal(t, "content_block_start", textResponses[1].Type)
	assert.Equal(t, 0, textResponses[1].GetIndex())
	assert.Equal(t, "content_block_delta", textResponses[2].Type)

	info.SendResponseCount = 2
	thinkingResponses := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
					ReasoningContent: ptr("thinking"),
				},
			},
		},
	}, info)
	require.Len(t, thinkingResponses, 3)
	assert.Equal(t, "content_block_stop", thinkingResponses[0].Type)
	assert.Equal(t, 0, thinkingResponses[0].GetIndex())
	assert.Equal(t, "content_block_start", thinkingResponses[1].Type)
	assert.Equal(t, 1, thinkingResponses[1].GetIndex())
	assert.Equal(t, "thinking", thinkingResponses[1].ContentBlock.Type)
	assert.Equal(t, "content_block_delta", thinkingResponses[2].Type)

	info.SendResponseCount = 3
	toolResponses := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
					ToolCalls: []dto.ToolCallResponse{
						{
							Index: ptr(0),
							ID:    "call_1",
							Type:  "function",
							Function: dto.FunctionResponse{
								Name:      "lookup",
								Arguments: `{"q":"x"}`,
							},
						},
					},
				},
			},
		},
	}, info)
	require.Len(t, toolResponses, 3)
	assert.Equal(t, "content_block_stop", toolResponses[0].Type)
	assert.Equal(t, 1, toolResponses[0].GetIndex())
	assert.Equal(t, "content_block_start", toolResponses[1].Type)
	assert.Equal(t, 2, toolResponses[1].GetIndex())
	assert.Equal(t, "tool_use", toolResponses[1].ContentBlock.Type)
	assert.Equal(t, "content_block_delta", toolResponses[2].Type)

	info.SendResponseCount = 4
	finishResponses := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{FinishReason: ptr("tool_calls")},
		},
		Usage: &dto.Usage{
			PromptTokens:     7,
			CompletionTokens: 3,
			TotalTokens:      10,
		},
	}, info)
	require.Len(t, finishResponses, 3)
	assert.Equal(t, "content_block_stop", finishResponses[0].Type)
	assert.Equal(t, 2, finishResponses[0].GetIndex())
	assert.Equal(t, "message_delta", finishResponses[1].Type)
	assert.Equal(t, "tool_use", *finishResponses[1].Delta.StopReason)
	require.NotNil(t, finishResponses[1].Usage)
	require.NotNil(t, finishResponses[1].Usage.BillingUsage)
	require.NotNil(t, finishResponses[1].Usage.BillingUsage.OpenAIUsage)
	assert.Equal(t, 7, finishResponses[1].Usage.BillingUsage.OpenAIUsage.PromptTokens)
	assert.Equal(t, 3, finishResponses[1].Usage.BillingUsage.OpenAIUsage.CompletionTokens)
	assert.Equal(t, "message_stop", finishResponses[2].Type)
}

func TestBuildClaudeUsageFromOpenAICachedTokensExcludedFromInput(t *testing.T) {
	usage := buildClaudeUsageFromOpenAIUsage(&dto.Usage{
		PromptTokens:     4000,
		CompletionTokens: 20,
		TotalTokens:      4020,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 3000,
		},
	})

	require.NotNil(t, usage)
	// OpenAI prompt_tokens includes cached_tokens even when no cache-write
	// count is reported; Claude input_tokens must exclude the cached share.
	assert.Equal(t, 1000, usage.InputTokens)
	assert.Equal(t, 3000, usage.CacheReadInputTokens)
	assert.Equal(t, 0, usage.CacheCreationInputTokens)
	assert.Equal(t, 20, usage.OutputTokens)
}

func TestStreamResponseOpenAI2ClaudeFirstChunkParallelToolCalls(t *testing.T) {
	info := &relaycommon.RelayInfo{
		SendResponseCount: 1,
		ClaudeConvertInfo: &relaycommon.ClaudeConvertInfo{
			LastMessagesType: relaycommon.LastMessageTypeNone,
		},
	}

	responses := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
					ToolCalls: []dto.ToolCallResponse{
						{
							Index: ptr(0),
							ID:    "call_1",
							Type:  "function",
							Function: dto.FunctionResponse{
								Name:      "lookup",
								Arguments: `{"q":"x"}`,
							},
						},
						{
							Index: ptr(1),
							ID:    "call_2",
							Type:  "function",
							Function: dto.FunctionResponse{
								Name:      "fetch",
								Arguments: `{"url":"y"}`,
							},
						},
					},
				},
				FinishReason: ptr("tool_calls"),
			},
		},
		Usage: &dto.Usage{
			PromptTokens:     7,
			CompletionTokens: 3,
			TotalTokens:      10,
		},
	}, info)

	require.Len(t, responses, 9)
	assert.Equal(t, "message_start", responses[0].Type)

	assert.Equal(t, "content_block_start", responses[1].Type)
	assert.Equal(t, 0, responses[1].GetIndex())
	assert.Equal(t, "call_1", responses[1].ContentBlock.Id)
	assert.Equal(t, "lookup", responses[1].ContentBlock.Name)
	assert.Equal(t, "content_block_delta", responses[2].Type)
	assert.Equal(t, 0, responses[2].GetIndex())
	assert.Equal(t, `{"q":"x"}`, *responses[2].Delta.PartialJson)

	assert.Equal(t, "content_block_start", responses[3].Type)
	assert.Equal(t, 1, responses[3].GetIndex())
	assert.Equal(t, "call_2", responses[3].ContentBlock.Id)
	assert.Equal(t, "fetch", responses[3].ContentBlock.Name)
	assert.Equal(t, "content_block_delta", responses[4].Type)
	assert.Equal(t, 1, responses[4].GetIndex())
	assert.Equal(t, `{"url":"y"}`, *responses[4].Delta.PartialJson)

	assert.Equal(t, "content_block_stop", responses[5].Type)
	assert.Equal(t, 0, responses[5].GetIndex())
	assert.Equal(t, "content_block_stop", responses[6].Type)
	assert.Equal(t, 1, responses[6].GetIndex())

	assert.Equal(t, "message_delta", responses[7].Type)
	assert.Equal(t, "tool_use", *responses[7].Delta.StopReason)
	assert.Equal(t, "message_stop", responses[8].Type)
	assert.True(t, info.ClaudeConvertInfo.Done)
}

func TestStreamResponseOpenAI2ClaudeFinishChunkWithoutInlineUsageClosesWithSettledUsage(t *testing.T) {
	info := &relaycommon.RelayInfo{
		SendResponseCount: 1,
		ClaudeConvertInfo: &relaycommon.ClaudeConvertInfo{
			LastMessagesType: relaycommon.LastMessageTypeNone,
		},
	}

	first := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: ptr("hel")}},
		},
	}, info)
	require.Len(t, first, 3)

	// HandleFinalResponse settles usage before converting the buffered finish
	// chunk; the converter must close the stream with it.
	info.ClaudeConvertInfo.Usage = &dto.Usage{
		PromptTokens:     9,
		CompletionTokens: 2,
		TotalTokens:      11,
	}
	info.SendResponseCount = 2
	final := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta:        dto.ChatCompletionsStreamResponseChoiceDelta{Content: ptr("lo")},
				FinishReason: ptr("stop"),
			},
		},
	}, info)

	require.Len(t, final, 4)
	assert.Equal(t, "content_block_delta", final[0].Type)
	assert.Equal(t, 0, final[0].GetIndex())
	assert.Equal(t, "lo", *final[0].Delta.Text)
	assert.Equal(t, "content_block_stop", final[1].Type)
	assert.Equal(t, 0, final[1].GetIndex())
	assert.Equal(t, "message_delta", final[2].Type)
	assert.Equal(t, "end_turn", *final[2].Delta.StopReason)
	require.NotNil(t, final[2].Usage)
	assert.Equal(t, 9, final[2].Usage.InputTokens)
	assert.Equal(t, 2, final[2].Usage.OutputTokens)
	assert.Equal(t, "message_stop", final[3].Type)
	assert.True(t, info.ClaudeConvertInfo.Done)
}

func TestStreamResponseOpenAI2ClaudeFinishChunkDeltaDeferredUntilUsageChunk(t *testing.T) {
	info := &relaycommon.RelayInfo{
		SendResponseCount: 1,
		ClaudeConvertInfo: &relaycommon.ClaudeConvertInfo{
			LastMessagesType: relaycommon.LastMessageTypeNone,
		},
	}

	first := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: ptr("hel")}},
		},
	}, info)
	require.Len(t, first, 3)

	// No usage anywhere yet: closing defers to the trailing usage-only chunk,
	// but the text riding on the finish chunk must still be delivered.
	info.SendResponseCount = 2
	finish := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta:        dto.ChatCompletionsStreamResponseChoiceDelta{Content: ptr("lo")},
				FinishReason: ptr("stop"),
			},
		},
	}, info)
	require.Len(t, finish, 1)
	assert.Equal(t, "content_block_delta", finish[0].Type)
	assert.Equal(t, 0, finish[0].GetIndex())
	assert.Equal(t, "lo", *finish[0].Delta.Text)
	assert.False(t, info.ClaudeConvertInfo.Done)

	info.SendResponseCount = 3
	final := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id:      "chatcmpl_1",
		Model:   "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{},
		Usage: &dto.Usage{
			PromptTokens:     9,
			CompletionTokens: 2,
			TotalTokens:      11,
		},
	}, info)
	require.Len(t, final, 3)
	assert.Equal(t, "content_block_stop", final[0].Type)
	assert.Equal(t, 0, final[0].GetIndex())
	assert.Equal(t, "message_delta", final[1].Type)
	assert.Equal(t, "end_turn", *final[1].Delta.StopReason)
	require.NotNil(t, final[1].Usage)
	assert.Equal(t, 9, final[1].Usage.InputTokens)
	assert.Equal(t, "message_stop", final[2].Type)
	assert.True(t, info.ClaudeConvertInfo.Done)
}

func TestNormalizeCacheCreationSplit(t *testing.T) {
	cache5m, cache1h := NormalizeCacheCreationSplit(10, 3, 2)
	assert.Equal(t, 8, cache5m)
	assert.Equal(t, 2, cache1h)

	cache5m, cache1h = NormalizeCacheCreationSplit(3, 5, 1)
	assert.Equal(t, 5, cache5m)
	assert.Equal(t, 1, cache1h)
}

func ptr[T any](value T) *T {
	return &value
}
