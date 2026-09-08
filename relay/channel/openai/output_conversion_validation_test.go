package openai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertedResponseRejectsEmptyOutput(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		stream  bool
		handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{
			name:    "responses to chat",
			body:    `{"id":"resp_empty","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
			handler: OaiResponsesToChatHandler,
		},
		{
			name:    "chat to responses",
			body:    `{"id":"chat_empty","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			handler: OaiChatToResponsesHandler,
		},
		{
			name:    "buffered responses to chat",
			body:    "data: " + `{"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n",
			handler: OaiResponsesToChatBufferedStreamHandler,
		},
		{
			name: "responses to chat SSE", stream: true,
			body: "data: " + `{"type":"response.created","response":{"id":"resp_empty","status":"in_progress"}}` + "\n\n" +
				"data: " + `{"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n",
			handler: OaiResponsesToChatStreamHandler,
		},
		{
			name: "chat to responses SSE", stream: true,
			body: "data: " + `{"id":"chat_empty","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
				"data: " + `{"id":"chat_empty","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\ndata: [DONE]\n\n",
			handler: OaiChatToResponsesStreamHandler,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, tc.body, tc.stream)
			usage, apiErr := tc.handler(c, info, resp)
			require.NotNil(t, apiErr)
			assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
			assert.Nil(t, usage)
			if !tc.stream {
				assert.False(t, c.Writer.Written())
			}
			assert.NotContains(t, recorder.Body.String(), "[DONE]")
			assert.NotContains(t, recorder.Body.String(), "response.completed")
		})
	}
}

func TestConvertedStreamDoesNotCompleteTruncatedOutput(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{
			name:    "responses to chat",
			body:    "data: " + `{"type":"response.output_text.delta","delta":"partial answer"}` + "\n\n",
			handler: OaiResponsesToChatStreamHandler,
		},
		{
			name:    "chat to responses",
			body:    "data: " + `{"id":"chat_partial","choices":[{"index":0,"delta":{"content":"partial answer"},"finish_reason":null}]}` + "\n\n",
			handler: OaiChatToResponsesStreamHandler,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, tc.body, true)
			usage, apiErr := tc.handler(c, info, resp)
			require.Nil(t, apiErr, "already delivered output retains its usage for settlement")
			require.NotNil(t, usage)
			assert.True(t, info.StreamStatus.MissingTerminator())
			assert.NotNil(t, info.StreamStatus.FailureError())
			assert.Contains(t, recorder.Body.String(), "partial answer")
			assert.NotContains(t, recorder.Body.String(), "[DONE]")
			assert.NotContains(t, recorder.Body.String(), "response.completed")
		})
	}
}

func TestBufferedConvertedStreamRejectsMissingTerminator(t *testing.T) {
	body := "data: " + `{"type":"response.output_text.delta","delta":"partial answer"}` + "\n\n"
	c, recorder, resp, info := newResponsesChatTestContext(t, body, false)
	usage, apiErr := OaiResponsesToChatBufferedStreamHandler(c, info, resp)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeBadResponse, apiErr.GetErrorCode())
	assert.Nil(t, usage)
	assert.False(t, c.Writer.Written(), "a buffered reply has delivered nothing and can still fail or retry")
	assert.Empty(t, recorder.Body.String())
}

func TestConvertedToolOutputWithoutUsageRemainsValid(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{
			name:    "responses to chat",
			body:    `{"id":"resp_tool","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}`,
			handler: OaiResponsesToChatHandler,
		},
		{
			name:    "chat to responses",
			body:    `{"id":"chat_tool","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			handler: OaiChatToResponsesHandler,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, tc.body, false)
			usage, apiErr := tc.handler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated, "a valid tool call must survive the shared zero-usage check")
			assert.Contains(t, recorder.Body.String(), `"name":"lookup"`)
			assert.NotContains(t, recorder.Body.String(), `response_validated`)
		})
	}
}

func TestConvertedOutputDoesNotAcceptDiscardedSourceFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		stream  bool
		handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{
			name:    "opaque responses reasoning cannot become chat text",
			body:    `{"id":"resp_opaque","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
			handler: OaiResponsesToChatHandler,
		},
		{
			name:    "chat audio cannot become responses text",
			body:    `{"choices":[{"message":{"role":"assistant","content":null,"audio":{"id":"audio_1","data":"YXVkaW8="}},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			handler: OaiChatToResponsesHandler,
		},
		{
			name: "opaque responses stream", stream: true,
			body: "data: " + `{"type":"response.created","response":{"id":"resp_opaque","status":"in_progress"}}` + "\n\n" +
				"data: " + `{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}}` + "\n\n" +
				"data: " + `{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n",
			handler: OaiResponsesToChatStreamHandler,
		},
		{
			name: "chat audio stream", stream: true,
			body: "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","audio":{"id":"audio_1","data":"YXVkaW8="}}}]}` + "\n\n" +
				"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
			handler: OaiChatToResponsesStreamHandler,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, tc.body, tc.stream)
			usage, apiErr := tc.handler(c, info, resp)
			require.NotNil(t, apiErr)
			assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
			assert.Nil(t, usage)
			assert.False(t, c.Writer.Written(), "metadata must not commit a successful stream before usable converted output")
			assert.Empty(t, recorder.Body.String())
		})
	}
}

func TestConvertedHiddenReasoningLimitIsNotEmpty(t *testing.T) {
	const responsesBody = `{"id":"resp_reasoning","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11,"output_tokens_details":{"reasoning_tokens":1}}}`
	const chatBody = `{"choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"completion_tokens_details":{"reasoning_tokens":1}}}`
	cases := []struct {
		name          string
		body          string
		format        types.RelayFormat
		stream        bool
		suppressUsage bool
		wantTerminal  string
		handler       func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{name: "responses to chat", body: responsesBody, wantTerminal: `"finish_reason":"length"`, handler: OaiResponsesToChatHandler},
		{name: "chat to responses", body: chatBody, wantTerminal: `"reason":"max_output_tokens"`, handler: OaiChatToResponsesHandler},
		{name: "buffered responses to chat", body: "data: " + `{"type":"response.incomplete","response":` + responsesBody + "}\n\n", wantTerminal: `"finish_reason":"length"`, handler: OaiResponsesToChatBufferedStreamHandler},
		{name: "responses to chat SSE", body: "data: " + `{"type":"response.incomplete","response":` + responsesBody + "}\n\n", stream: true, wantTerminal: `"finish_reason":"length"`, handler: OaiResponsesToChatStreamHandler},
		{name: "responses to chat SSE without requested usage", body: "data: " + `{"type":"response.incomplete","response":` + responsesBody + "}\n\n", stream: true, suppressUsage: true, wantTerminal: `"finish_reason":"length"`, handler: OaiResponsesToChatStreamHandler},
		{name: "chat to responses SSE", body: "data: " + strings.Replace(chatBody, `"message":`, `"delta":`, 1) + "\n\ndata: [DONE]\n\n", stream: true, wantTerminal: `"reason":"max_output_tokens"`, handler: OaiChatToResponsesStreamHandler},
		{name: "responses to Claude", body: responsesBody, format: types.RelayFormatClaude, wantTerminal: `"stop_reason":"max_tokens"`, handler: OaiResponsesToChatHandler},
		{name: "responses to Gemini", body: responsesBody, format: types.RelayFormatGemini, wantTerminal: `"finishReason":"MAX_TOKENS"`, handler: OaiResponsesToChatHandler},
		{name: "responses to Claude SSE", body: "data: " + `{"type":"response.incomplete","response":` + responsesBody + "}\n\n", format: types.RelayFormatClaude, stream: true, wantTerminal: `"stop_reason":"max_tokens"`, handler: OaiResponsesToChatStreamHandler},
		{name: "responses to Gemini SSE", body: "data: " + `{"type":"response.incomplete","response":` + responsesBody + "}\n\n", format: types.RelayFormatGemini, stream: true, wantTerminal: `"finishReason":"MAX_TOKENS"`, handler: OaiResponsesToChatStreamHandler},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, tc.body, tc.stream)
			if tc.format != "" {
				info.RelayFormat = tc.format
			}
			info.ShouldIncludeUsage = !tc.suppressUsage
			usage, apiErr := tc.handler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated)
			assert.Equal(t, 1, usage.CompletionTokenDetails.ReasoningTokens)
			assert.Contains(t, recorder.Body.String(), tc.wantTerminal)
			if !tc.suppressUsage && tc.format == "" {
				assert.Contains(t, recorder.Body.String(), `"reasoning_tokens":1`)
			}
			if tc.stream {
				assert.Nil(t, info.StreamStatus.FailureError())
			}
		})
	}
}

func TestConvertedStreamPreservesContentFilter(t *testing.T) {
	const body = "data: " + `{"type":"response.incomplete","response":{"id":"resp_filtered","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[],"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}` + "\n\n"
	for _, tc := range []struct {
		format types.RelayFormat
		marker string
	}{
		{types.RelayFormatOpenAI, `"finish_reason":"content_filter"`},
		{types.RelayFormatClaude, `"stop_reason":"refusal"`},
		{types.RelayFormatGemini, `"finishReason":"SAFETY"`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
			info.RelayFormat = tc.format
			usage, apiErr := OaiResponsesToChatStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated)
			assert.Contains(t, recorder.Body.String(), tc.marker)
			assert.Nil(t, info.StreamStatus.FailureError())
		})
	}
}

func TestConvertedStreamDoesNotTreatToolArgumentsAsMetadata(t *testing.T) {
	fragments := []string{`{"q":"`}
	for i := 0; i < 65; i++ {
		fragments = append(fragments, "x")
	}
	fragments = append(fragments, `"}`)
	for _, tc := range []struct {
		name      string
		arguments []string
	}{
		{name: "fragmented tool arguments", arguments: fragments},
		{name: "large tool argument", arguments: []string{`{"q":"` + strings.Repeat("x", 70<<10) + `"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body strings.Builder
			body.WriteString("data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}` + "\n\n")
			for _, argument := range tc.arguments {
				event, err := common.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": argument})
				require.NoError(t, err)
				body.WriteString("data: " + string(event) + "\n\n")
			}
			body.WriteString("data: " + `{"type":"response.completed","response":{"id":"resp_tool","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":100,"total_tokens":110}}}` + "\n\n")
			c, recorder, resp, info := newResponsesChatTestContext(t, body.String(), true)
			info.RelayFormat = types.RelayFormatGemini
			usage, apiErr := OaiResponsesToChatStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated)
			assert.Contains(t, recorder.Body.String(), `"name":"lookup"`)
			assert.Contains(t, recorder.Body.String(), strings.Join(tc.arguments, ""))
			assert.Nil(t, info.StreamStatus.FailureError())
		})
	}
}
