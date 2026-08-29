package claudemessages

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseClaude2OpenAIJoinsAllTextBlocks(t *testing.T) {
	claudeResponse := &dto.ClaudeResponse{
		Id:         "msg_01",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4",
		StopReason: "end_turn",
		Content: []dto.ClaudeMediaMessage{
			{Type: "text", Text: common.GetPointer("Based on my search, ")},
			{Type: "server_tool_use", Id: "srvtoolu_01", Name: "web_search"},
			{Type: "text", Text: common.GetPointer("here is the summary. ")},
			{Type: "text", Text: common.GetPointer("Sources: example.com")},
		},
	}

	openAIResponse := ResponseClaude2OpenAI(claudeResponse)
	require.NotNil(t, openAIResponse)
	require.Len(t, openAIResponse.Choices, 1)
	assert.Equal(t, "Based on my search, here is the summary. Sources: example.com",
		openAIResponse.Choices[0].Message.StringContent())
}

func TestResponseClaude2OpenAITextWithToolUse(t *testing.T) {
	claudeResponse := &dto.ClaudeResponse{
		Id:         "msg_02",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4",
		StopReason: "tool_use",
		Content: []dto.ClaudeMediaMessage{
			{Type: "text", Text: common.GetPointer("Let me check.")},
			{Type: "tool_use", Id: "toolu_01", Name: "get_weather", Input: map[string]any{"city": "Paris"}},
		},
	}

	openAIResponse := ResponseClaude2OpenAI(claudeResponse)
	require.NotNil(t, openAIResponse)
	require.Len(t, openAIResponse.Choices, 1)
	choice := openAIResponse.Choices[0]
	assert.Equal(t, "Let me check.", choice.Message.StringContent())

	toolCalls := choice.Message.ParseToolCalls()
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "toolu_01", toolCalls[0].ID)
	assert.Equal(t, "get_weather", toolCalls[0].Function.Name)
}

func TestStreamResponseClaude2OpenAIToolCallIndexIsZeroBased(t *testing.T) {
	claudeInfo := &ClaudeResponseInfo{Usage: &dto.Usage{}}

	convert := func(event *dto.ClaudeResponse) *dto.ChatCompletionsStreamResponse {
		response := StreamResponseClaude2OpenAI(event)
		require.NotNil(t, response)
		require.True(t, FormatClaudeResponseInfo(event, response, claudeInfo))
		return response
	}

	textStart := convert(&dto.ClaudeResponse{
		Type:         "content_block_start",
		Index:        common.GetPointer(0),
		ContentBlock: &dto.ClaudeMediaMessage{Type: "text", Text: common.GetPointer("")},
	})
	require.Len(t, textStart.Choices, 1)
	assert.Empty(t, textStart.Choices[0].Delta.ToolCalls)

	textDelta := convert(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Index: common.GetPointer(0),
		Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: common.GetPointer("Let me check.")},
	})
	require.Len(t, textDelta.Choices, 1)
	assert.Empty(t, textDelta.Choices[0].Delta.ToolCalls)

	firstToolStart := convert(&dto.ClaudeResponse{
		Type:         "content_block_start",
		Index:        common.GetPointer(1),
		ContentBlock: &dto.ClaudeMediaMessage{Type: "tool_use", Id: "toolu_01", Name: "get_weather"},
	})
	require.Len(t, firstToolStart.Choices, 1)
	require.Len(t, firstToolStart.Choices[0].Delta.ToolCalls, 1)
	firstToolCall := firstToolStart.Choices[0].Delta.ToolCalls[0]
	require.NotNil(t, firstToolCall.Index)
	assert.Equal(t, 0, *firstToolCall.Index)
	assert.Equal(t, "toolu_01", firstToolCall.ID)

	firstToolArgs := convert(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Index: common.GetPointer(1),
		Delta: &dto.ClaudeMediaMessage{Type: "input_json_delta", PartialJson: common.GetPointer(`{"city":"Paris"}`)},
	})
	require.Len(t, firstToolArgs.Choices, 1)
	require.Len(t, firstToolArgs.Choices[0].Delta.ToolCalls, 1)
	firstArgsCall := firstToolArgs.Choices[0].Delta.ToolCalls[0]
	require.NotNil(t, firstArgsCall.Index)
	assert.Equal(t, 0, *firstArgsCall.Index)
	assert.Equal(t, `{"city":"Paris"}`, firstArgsCall.Function.Arguments)

	secondToolStart := convert(&dto.ClaudeResponse{
		Type:         "content_block_start",
		Index:        common.GetPointer(2),
		ContentBlock: &dto.ClaudeMediaMessage{Type: "tool_use", Id: "toolu_02", Name: "get_time"},
	})
	require.Len(t, secondToolStart.Choices, 1)
	require.Len(t, secondToolStart.Choices[0].Delta.ToolCalls, 1)
	secondToolCall := secondToolStart.Choices[0].Delta.ToolCalls[0]
	require.NotNil(t, secondToolCall.Index)
	assert.Equal(t, 1, *secondToolCall.Index)

	secondToolArgs := convert(&dto.ClaudeResponse{
		Type:  "content_block_delta",
		Index: common.GetPointer(2),
		Delta: &dto.ClaudeMediaMessage{Type: "input_json_delta", PartialJson: common.GetPointer(`{}`)},
	})
	require.Len(t, secondToolArgs.Choices, 1)
	require.Len(t, secondToolArgs.Choices[0].Delta.ToolCalls, 1)
	secondArgsCall := secondToolArgs.Choices[0].Delta.ToolCalls[0]
	require.NotNil(t, secondArgsCall.Index)
	assert.Equal(t, 1, *secondArgsCall.Index)
}
