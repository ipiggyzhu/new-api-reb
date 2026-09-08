package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newOutputValidationTest(t *testing.T, body string, responses, stream bool) (*gin.Context, *relaycommon.RelayInfo, *http.Response, *httptest.ResponseRecorder) {
	t.Helper()

	path := "/v1/chat/completions"
	format := types.RelayFormatOpenAI
	mode := relayconstant.RelayModeChatCompletions
	if responses {
		path = "/v1/responses"
		format = types.RelayFormatOpenAIResponses
		mode = relayconstant.RelayModeResponses
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: format,
		RelayMode:   mode,
		IsStream:    stream,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "gpt-4o",
		},
	}
	info.SetEstimatePromptTokens(10)
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	return c, info, resp, recorder
}

func TestNativeResponseRejectsEmptyOutputBeforeWriting(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		responses bool
	}{
		{name: "chat null", body: `null`},
		{name: "chat empty object", body: `{}`},
		{name: "chat empty choices with billed tokens", body: `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`},
		{name: "chat empty message with billed tokens", body: `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"completion_tokens_details":{"text_tokens":1,"audio_tokens":1,"image_tokens":1}}}`},
		{name: "chat budget without reasoning evidence", body: `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`},
		{name: "chat empty call shells", body: `{"choices":[{"message":{"tool_calls":[{}],"function_call":{}},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":1}}`},
		{name: "chat tool calls must be an array", body: `{"choices":[{"message":{"tool_calls":{"type":"function","function":{"name":"lookup","arguments":"{}"}}},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":1}}`},
		{name: "chat stream delta in nonstream response", body: `{"choices":[{"delta":{"content":"not a complete response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`},
		{name: "chat upstream cannot assert validation", body: `{"choices":[],"usage":{"ResponseValidated":true,"response_validated":true}}`},
		{name: "responses null", body: `null`, responses: true},
		{name: "responses empty object", body: `{}`, responses: true},
		{name: "responses empty output with billed tokens", body: `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`, responses: true},
		{name: "responses empty message shells", body: `{"id":"resp_1","status":"completed","output":[null,{}, {"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":""}]}],"usage":{"output_tokens":1}}`, responses: true},
		{name: "responses budget without reasoning evidence", body: `{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"output_tokens":1}}`, responses: true},
		{name: "responses unidentified queued job", body: `{"status":"queued","background":true,"output":[]}`, responses: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info, resp, recorder := newOutputValidationTest(t, tc.body, tc.responses, false)
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			require.NotPanics(t, func() {
				if tc.responses {
					usage, apiErr = OaiResponsesHandler(c, info, resp)
				} else {
					usage, apiErr = OpenaiHandler(c, info, resp)
				}
			})
			require.NotNil(t, apiErr)
			assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
			assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
			assert.Nil(t, usage)
			assert.False(t, c.Writer.Written(), "the failed attempt must remain retryable")
			assert.Empty(t, recorder.Body.String())
		})
	}
}

func TestNativeStreamRejectsEmptyOutputBeforeTerminator(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		responses bool
	}{
		{name: "chat empty body"},
		{name: "chat only done", body: "data: [DONE]\n\n"},
		{name: "chat only usage", body: "data: " + usageOnlyChunk + "\n\ndata: [DONE]\n\n"},
		{name: "chat only finish", body: "data: " + finishChunk + "\n\ndata: [DONE]\n\n"},
		{name: "chat full message in stream chunk", body: "data: " + `{"choices":[{"index":0,"message":{"role":"assistant","content":"not a stream delta"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"},
		{name: "chat choices must be an array", body: "data: " + `{"choices":{"index":0,"delta":{"content":"invalid choices"},"finish_reason":"stop"},"usage":{"prompt_tokens":10,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"},
		{name: "chat role finish and usage", body: "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\ndata: " + finishChunk + "\n\ndata: " + usageOnlyChunk + "\n\ndata: [DONE]\n\n"},
		{name: "responses empty body", responses: true},
		{name: "responses only completed usage", body: "data: " + `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\n", responses: true},
		{name: "responses lifecycle before empty completed", body: "data: " + `{"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}` + "\n\ndata: " + `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n", responses: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info, resp, recorder := newOutputValidationTest(t, tc.body, tc.responses, true)
			info.ShouldIncludeUsage = true
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			if tc.responses {
				usage, apiErr = OaiResponsesStreamHandler(c, info, resp)
			} else {
				usage, apiErr = OaiStreamHandler(c, info, resp)
			}
			require.NotNil(t, apiErr)
			assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
			assert.Nil(t, usage)
			assert.False(t, c.Writer.Written(), "empty lifecycle frames must leave the attempt retryable")
			assert.Empty(t, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "[DONE]")
			assert.NotContains(t, recorder.Body.String(), "response.completed")
		})
	}
}

func TestNativeResponsePreservesNonTextAndDeferredOutput(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		responses bool
	}{
		{name: "chat tool without usage", body: `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		{name: "chat legacy function call without usage", body: `{"choices":[{"message":{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{}"}},"finish_reason":"function_call"}]}`},
		{name: "chat audio without usage", body: `{"choices":[{"message":{"role":"assistant","content":null,"audio":{"id":"audio_1","data":"YXVkaW8=","transcript":""}},"finish_reason":"stop"}]}`},
		{name: "chat opaque reasoning details", body: `{"choices":[{"message":{"role":"assistant","content":null,"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]},"finish_reason":"stop"}]}`},
		{name: "chat reasoning without usage", body: `{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"Consider the inputs."},"finish_reason":"stop"}]}`},
		{name: "chat refusal without usage", body: `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I cannot help with that."},"finish_reason":"stop"}]}`},
		{name: "chat content filter", body: `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"content_filter"}]}`},
		{name: "chat hidden reasoning at token limit", body: `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":1}}}`},
		{name: "responses function call without usage", body: `{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}`, responses: true},
		{name: "responses opaque reasoning", body: `{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}]}`, responses: true},
		{name: "responses hidden reasoning item", body: `{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[]}]}`, responses: true},
		{name: "responses compaction output", body: `{"id":"resp_1","output":[{"type":"compaction","id":"cmp_1","encrypted_content":"opaque"}]}`, responses: true},
		{name: "responses built in tool output", body: `{"id":"resp_1","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"weather"}}]}`, responses: true},
		{name: "responses refusal", body: `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]}]}`, responses: true},
		{name: "responses content filter", body: `{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`, responses: true},
		{name: "responses queued job", body: `{"id":"resp_1","status":"queued","background":true,"output":[]}`, responses: true},
		{name: "responses running job", body: `{"id":"resp_1","status":"in_progress","background":true,"output":[]}`, responses: true},
		{name: "responses hidden reasoning at token limit", body: `{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":1}}}`, responses: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info, resp, recorder := newOutputValidationTest(t, tc.body, tc.responses, false)
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			if tc.responses {
				usage, apiErr = OaiResponsesHandler(c, info, resp)
			} else {
				usage, apiErr = OpenaiHandler(c, info, resp)
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated, "the relay must accept the protocol output even if usage is absent")
			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.NotEmpty(t, recorder.Body.String())
			outputPath := "choices"
			if tc.responses {
				outputPath = "output"
			}
			assert.JSONEq(t, gjson.Get(tc.body, outputPath).Raw, gjson.Get(recorder.Body.String(), outputPath).Raw)
			if tc.responses && !strings.Contains(tc.body, `"usage"`) {
				assert.Zero(t, usage.CompletionTokens, "acceptance must not invent output tokens")
			}
		})
	}
}

func TestNativeStreamPreservesNonTextOutput(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`
	cases := []struct {
		name      string
		events    []string
		responses bool
	}{
		{
			name: "chat tool",
			events: []string{
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "[DONE]",
			},
		},
		{
			name: "chat refusal",
			events: []string{
				`{"choices":[{"index":0,"delta":{"refusal":"I cannot help with that."}}]}`, finishChunk, "[DONE]",
			},
		},
		{name: "chat legacy function call", events: []string{`{"choices":[{"index":0,"delta":{"function_call":{"name":"lookup","arguments":"{}"}}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`, "[DONE]"}},
		{name: "chat audio", events: []string{`{"choices":[{"index":0,"delta":{"audio":{"id":"audio_1","data":"YXVkaW8="}}}]}`, finishChunk, "[DONE]"}},
		{name: "chat hidden reasoning with later usage", events: []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"completion_tokens_details":{"reasoning_tokens":1}}}`, "[DONE]"}},
		{name: "chat content filter", events: []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`, "[DONE]"}},
		{
			name:      "responses function call",
			responses: true,
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}}`, completed,
			},
		},
		{name: "responses reasoning", responses: true, events: []string{`{"type":"response.reasoning_summary_text.delta","delta":"Consider the inputs."}`, completed}},
		{name: "responses refusal", responses: true, events: []string{`{"type":"response.refusal.delta","delta":"I cannot help with that."}`, completed}},
		{name: "responses opaque reasoning", responses: true, events: []string{`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}}`, completed}},
		{name: "responses hidden reasoning at token limit", responses: true, events: []string{`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":1}}}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: " + strings.Join(tc.events, "\n\ndata: ") + "\n\n"
			c, info, resp, recorder := newOutputValidationTest(t, body, tc.responses, true)
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			if tc.responses {
				usage, apiErr = OaiResponsesStreamHandler(c, info, resp)
			} else {
				usage, apiErr = OaiStreamHandler(c, info, resp)
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated)
			assert.False(t, info.StreamStatus.MissingTerminator())
			for _, event := range tc.events {
				// A usage-only tail is suppressed when it was not requested.
				if gjson.Get(event, "usage").Exists() && len(gjson.Get(event, "choices").Array()) == 0 {
					continue
				}
				assert.Contains(t, recorder.Body.String(), event)
			}
			if tc.responses {
				assert.True(t, strings.Contains(recorder.Body.String(), "response.completed") || strings.Contains(recorder.Body.String(), "response.incomplete"))
			} else {
				assert.Contains(t, recorder.Body.String(), "[DONE]")
			}
		})
	}
}

func TestNativeResponsePreservesReasoningUsageWhenEstimatingPrompt(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":1}}}`
	c, info, resp, recorder := newOutputValidationTest(t, body, false, false)
	usage, apiErr := OpenaiHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.True(t, usage.ResponseValidated)
	assert.Equal(t, info.GetEstimatePromptTokens(), usage.PromptTokens)
	assert.Equal(t, 1, usage.CompletionTokens)
	assert.Equal(t, 1, usage.CompletionTokenDetails.ReasoningTokens)
	assert.Equal(t, int64(1), gjson.Get(recorder.Body.String(), "usage.completion_tokens_details.reasoning_tokens").Int(),
		"the client must retain the reasoning-only output evidence when missing prompt usage is estimated")
}

func TestNativeStreamMarksMissingTerminator(t *testing.T) {
	cases := []struct {
		name        string
		events      []string
		responses   bool
		wantMissing bool
	}{
		{name: "chat loses both finish and done", events: []string{delta("partial answer")}, wantMissing: true},
		{name: "chat finish without done is complete", events: []string{delta("complete answer"), finishChunk}},
		{name: "chat one unfinished choice", events: []string{`{"choices":[{"index":0,"delta":{"content":"first"}},{"index":1,"delta":{"content":"second"}}]}`, finishChunk}, wantMissing: true},
		{name: "chat all choices finished", events: []string{`{"choices":[{"index":0,"delta":{"content":"first"}},{"index":1,"delta":{"content":"second"}}]}`, finishChunk, `{"choices":[{"index":1,"delta":{},"finish_reason":"length"}]}`}},
		{name: "responses loses terminal event", responses: true, events: []string{`{"type":"response.output_text.delta","delta":"partial answer"}`}, wantMissing: true},
		{name: "responses incomplete is a terminal event", responses: true, events: []string{`{"type":"response.output_text.delta","delta":"partial answer"}`, `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: " + strings.Join(tc.events, "\n\ndata: ") + "\n\n"
			c, info, resp, recorder := newOutputValidationTest(t, body, tc.responses, true)
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			if tc.responses {
				usage, apiErr = OaiResponsesStreamHandler(c, info, resp)
			} else {
				usage, apiErr = OaiStreamHandler(c, info, resp)
			}
			require.Nil(t, apiErr, "partial output is billed before the relay reports the stream failure")
			require.NotNil(t, usage)
			assert.Equal(t, tc.wantMissing, info.StreamStatus.MissingTerminator())
			if tc.wantMissing {
				assert.NotNil(t, info.StreamStatus.FailureError())
				assert.NotContains(t, recorder.Body.String(), "[DONE]")
				assert.NotContains(t, recorder.Body.String(), "response.completed")
			}
		})
	}
}

func TestNativeFormattingPreservesNonTextOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{name: "refusal", output: `"refusal":"I cannot help with that."`},
		{name: "legacy function call", output: `"function_call":{"name":"lookup","arguments":"{}"}`},
		{name: "audio", output: `"audio":{"id":"audio_1","data":"YXVkaW8="}`},
		{name: "opaque reasoning", output: `"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]`},
		{name: "custom tool", output: `"tool_calls":[{"id":"call_1","type":"custom","custom":{"name":"lookup","input":"value"}}]`},
	}
	for _, tc := range cases {
		for _, format := range []string{"response", "stream", "thinking stream"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				stream := format != "response"
				field := "message"
				if stream {
					field = "delta"
				}
				body := `{"choices":[{"index":0,"` + field + `":{"role":"assistant","content":null,` + tc.output + `},"finish_reason":"stop"}]}`
				if stream {
					body = "data: " + body + "\n\ndata: [DONE]\n\n"
				}
				c, info, resp, recorder := newOutputValidationTest(t, body, false, stream)
				info.ChannelSetting.ForceFormat = format != "thinking stream"
				info.ChannelSetting.ThinkingToContent = format == "thinking stream"
				var usage *dto.Usage
				var apiErr *types.NewAPIError
				if stream {
					usage, apiErr = OaiStreamHandler(c, info, resp)
				} else {
					usage, apiErr = OpenaiHandler(c, info, resp)
				}
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.True(t, usage.ResponseValidated)
				responseJSON := recorder.Body.String()
				if stream {
					responseJSON = strings.TrimPrefix(strings.SplitN(responseJSON, "\n", 2)[0], "data: ")
				}
				message := gjson.Get(responseJSON, "choices.0."+field)
				for key, expected := range gjson.Parse("{" + tc.output + "}").Map() {
					actual := message.Get(key)
					require.True(t, actual.Exists(), "formatting must preserve the %s output that was validated", key)
					assert.JSONEq(t, expected.Raw, actual.Raw)
				}
			})
		}
	}
}

func TestNativeCompletionsOutputValidation(t *testing.T) {
	for _, text := range []string{"", "answer"} {
		for _, stream := range []bool{false, true} {
			name := "response/" + text
			if stream {
				name = "stream/" + text
			}
			t.Run(name, func(t *testing.T) {
				body := `{"choices":[{"index":0,"text":"` + text + `","finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`
				if stream {
					body = "data: " + body + "\n\ndata: [DONE]\n\n"
				}
				c, info, resp, recorder := newOutputValidationTest(t, body, false, stream)
				info.RelayMode = relayconstant.RelayModeCompletions
				info.ChannelSetting.ForceFormat = true
				var usage *dto.Usage
				var apiErr *types.NewAPIError
				if stream {
					usage, apiErr = OaiStreamHandler(c, info, resp)
				} else {
					usage, apiErr = OpenaiHandler(c, info, resp)
				}
				if text == "" {
					require.NotNil(t, apiErr)
					assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
					assert.False(t, c.Writer.Written())
					return
				}
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.True(t, usage.ResponseValidated)
				assert.Contains(t, recorder.Body.String(), `"text":"answer"`)
			})
		}
	}
}

func TestNativeCompactionOutputValidation(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantError bool
	}{
		{name: "empty output with usage", body: `{"id":"resp_1","object":"response.compaction","output":[],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`, wantError: true},
		{name: "encrypted output without usage", body: `{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","id":"cmp_1","encrypted_content":"opaque"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, resp, recorder := newOutputValidationTest(t, tc.body, true, false)
			usage, apiErr := OaiResponsesCompactionHandler(c, resp)
			if tc.wantError {
				require.NotNil(t, apiErr)
				assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
				assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
				assert.Nil(t, usage)
				assert.False(t, c.Writer.Written())
				return
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.True(t, usage.ResponseValidated)
			assert.Zero(t, usage.CompletionTokens)
			assert.JSONEq(t, tc.body, recorder.Body.String())
		})
	}
}

func TestNativeStreamForwardsNonTextWithUsageImmediately(t *testing.T) {
	chunk := `{"choices":[{"index":0,"delta":{"refusal":"I cannot help with that."}}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`
	payload := runOaiStream(t, false, true, chunk, finishChunk)
	assert.Equal(t, []string{chunk, finishChunk, "[DONE]"}, payload)
}

func TestNativeStreamPreservesInitialRole(t *testing.T) {
	role := `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	payload := runOaiStream(t, false, false, role, delta("answer"), finishChunk)
	assert.Equal(t, []string{role, delta("answer"), finishChunk, "[DONE]"}, payload)
}

func TestNativeResponsesStreamPreservesUpstreamErrors(t *testing.T) {
	cases := []struct {
		name               string
		event              string
		errorType          string
		code               string
		message            string
		reportedCompletion int
	}{
		{
			name:      "response failed",
			event:     `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"server_error","code":"server_error","message":"The upstream failed while generating the response."},"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}`,
			errorType: "server_error", code: "server_error", message: "The upstream failed while generating the response.", reportedCompletion: 3,
		},
		{
			name:      "native response error without type",
			event:     `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"The upstream could not generate a response."}}}`,
			errorType: "upstream_error", code: "server_error", message: "The upstream could not generate a response.",
		},
		{
			name:      "response error",
			event:     `{"type":"response.error","error":{"type":"overloaded_error","code":"overloaded","message":"The upstream is overloaded."}}`,
			errorType: "overloaded_error", code: "overloaded", message: "The upstream is overloaded.",
		},
		{
			name:      "flat error event",
			event:     `{"type":"error","code":"server_error","message":"The upstream failed to process the request.","param":null}`,
			errorType: "error", code: "server_error", message: "The upstream failed to process the request.",
		},
		{
			name:      "top level error object",
			event:     `{"error":{"type":"invalid_request_error","code":"invalid_model","message":"The requested model is not available."}}`,
			errorType: "invalid_request_error", code: "invalid_model", message: "The requested model is not available.",
		},
	}
	for _, tc := range cases {
		for _, partial := range []bool{false, true} {
			name := tc.name + "/before output"
			if partial {
				name = tc.name + "/after output"
			}
			t.Run(name, func(t *testing.T) {
				events := []string{`{"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}`}
				if partial {
					events = append(events, `{"type":"response.output_text.delta","delta":"partial answer"}`)
				}
				events = append(events, tc.event)
				if partial {
					events = append(events, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"MUST_NOT_BE_SENT"}]}],"usage":{"input_tokens":10,"output_tokens":99,"total_tokens":109}}}`)
				}
				body := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
				c, info, resp, recorder := newOutputValidationTest(t, body, true, true)
				usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
				if !partial {
					require.NotNil(t, apiErr)
					assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
					assert.Equal(t, tc.errorType, apiErr.ToOpenAIError().Type)
					assert.Equal(t, tc.message, apiErr.ToOpenAIError().Message)
					assert.Equal(t, tc.code, apiErr.ToOpenAIError().Code)
					assert.Nil(t, usage, "the failed attempt must refund rather than settle")
					assert.False(t, c.Writer.Written(), "the original error must remain retryable")
					assert.Empty(t, recorder.Body.String())
					return
				}
				require.Nil(t, apiErr, "delivered output must be settled before the controller reports stream failure")
				require.NotNil(t, usage)
				assert.True(t, usage.ResponseValidated)
				assert.Positive(t, usage.CompletionTokens)
				if tc.reportedCompletion > 0 {
					assert.Equal(t, tc.reportedCompletion, usage.CompletionTokens)
				}
				assert.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
				require.NotNil(t, info.StreamStatus.FailureError())
				require.NotEmpty(t, info.StreamStatus.Errors)
				assert.Contains(t, info.StreamStatus.Errors[0].Message, tc.errorType)
				assert.Contains(t, info.StreamStatus.Errors[0].Message, tc.message)
				assert.Contains(t, recorder.Body.String(), "partial answer")
				assert.Contains(t, recorder.Body.String(), tc.event, "the client must retain the upstream's original error event")
				assert.NotContains(t, recorder.Body.String(), "response.completed")
				assert.NotContains(t, recorder.Body.String(), "MUST_NOT_BE_SENT")
				assert.NotContains(t, recorder.Body.String(), "[DONE]")
			})
		}
	}
}
