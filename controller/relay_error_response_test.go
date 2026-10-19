package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRelayErrorContext drives Relay down the earliest failing path — a request
// body that never names a model — so the deferred error writer runs against a
// recorder without a database, a channel or an upstream. The header and
// already-written state the writer branches on is set with the same production
// helpers a real streaming attempt uses, so the branch it picks here is the
// branch it picks in production.
func newRelayErrorContext(t *testing.T, path string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	oldErrorLog := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.ErrorLogEnabled = oldErrorLog
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

// relay/channel/api_request.go calls helper.SetEventStreamHeaders before
// client.Do, so Content-Type: text/event-stream sits in the header map for every
// streaming attempt — including ones that never receive a response. gin's
// writeContentType only fills Content-Type when it is absent, so c.JSON could not
// replace that label and every failed streaming relay answered JSON that claimed
// to be an event stream. Clients pick the parser from Content-Type, so the error
// body was fed to an SSE parser and dropped.
func TestRelayErrorShedsEventStreamHeadersWhenNothingWasWritten(t *testing.T) {
	c, recorder := newRelayErrorContext(t, "/v1/chat/completions")
	helper.SetEventStreamHeaders(c)
	require.False(t, c.Writer.Written(), "the attempt must fail before any byte reaches the client")

	Relay(c, types.RelayFormatOpenAI)

	assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"),
		"a JSON error body must be labelled as JSON, not left with the streaming attempt's text/event-stream")
	assert.Empty(t, recorder.Header().Get("Transfer-Encoding"),
		"chunked framing belongs to the abandoned stream, not to a single JSON error")
	assert.Empty(t, recorder.Header().Get("X-Accel-Buffering"))
	assert.Equal(t, http.StatusBadRequest, recorder.Code,
		"the status must stay the error's own status")

	var body struct {
		Error types.OpenAIError `json:"error"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Contains(t, body.Error.Message, "model is required")
}

// Once chunks are on the wire the status and Content-Type are committed, so the
// only way to reach the client is another SSE frame. c.JSON here appended a bare
// JSON object to a half-delivered SSE body, which a spec-compliant parser reads
// as an unknown field and silently drops — the caller saw a truncated stream with
// no error at all.
func TestRelayErrorReportsMidStreamFailureAsSSEFrame(t *testing.T) {
	c, recorder := newRelayErrorContext(t, "/v1/chat/completions")
	helper.SetEventStreamHeaders(c)
	require.NoError(t, helper.StringData(c, `{"id":"chatcmpl-1","object":"chat.completion.chunk"}`))
	require.True(t, c.Writer.Written(), "this case only exists once bytes are committed")

	Relay(c, types.RelayFormatOpenAI)

	body := recorder.Body.String()
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"),
		"a committed stream stays a stream")
	assert.Equal(t, http.StatusOK, recorder.Code,
		"the 200 is already on the wire; a mid-stream failure cannot rewrite it")

	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		assert.True(t, strings.HasPrefix(line, "data: "),
			"every line of a committed SSE body must stay a framed event; a bare JSON object is dropped by a spec-compliant parser: %q", line)
	}

	frames := sseDataFrames(t, body)
	require.Len(t, frames, 3, "the delivered chunk, the error frame and the terminator")
	assert.Equal(t, "[DONE]", frames[2], "the stream must still be terminated after the error frame")

	var errorFrame struct {
		Error types.OpenAIError `json:"error"`
	}
	require.NoError(t, common.Unmarshal([]byte(frames[1]), &errorFrame))
	assert.Contains(t, errorFrame.Error.Message, "model is required",
		"the failure has to reach the client inside the frame, not as a trailing object")
}

// A non-streaming failure never had the streaming headers, so it must keep the
// plain c.JSON behaviour: the Del calls are conditional and must not strip a
// Content-Type the writer never set.
func TestRelayErrorKeepsPlainJSONWhenNotStreaming(t *testing.T) {
	c, recorder := newRelayErrorContext(t, "/v1/chat/completions")

	Relay(c, types.RelayFormatOpenAI)

	assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "data: ",
		"a request that never streamed must not be answered with SSE framing")

	var body struct {
		Error types.OpenAIError `json:"error"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Contains(t, body.Error.Message, "model is required")
}

// Claude clients parse a top-level {"type":"error","error":{...}} envelope, and
// they parse it in both transports. Shedding the streaming headers must not
// reshape the body, and the mid-stream frame must carry the same envelope the
// non-streaming body does.
func TestRelayErrorPreservesClaudeErrorEnvelope(t *testing.T) {
	type claudeErrorBody struct {
		Type  string            `json:"type"`
		Error types.ClaudeError `json:"error"`
	}

	t.Run("json", func(t *testing.T) {
		c, recorder := newRelayErrorContext(t, "/v1/messages")
		helper.SetEventStreamHeaders(c)

		Relay(c, types.RelayFormatClaude)

		assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))

		var body claudeErrorBody
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Equal(t, "error", body.Type)
		assert.Contains(t, body.Error.Message, "field messages is required")
	})

	// Anthropic SDKs dispatch on the SSE event name and silently drop unnamed
	// frames, so the error must arrive as `event: error` — an unnamed data frame
	// left Claude Code with a stream that just ended and a bare "API error".
	// [DONE] is an OpenAI terminator with no meaning in this protocol.
	t.Run("mid stream", func(t *testing.T) {
		c, recorder := newRelayErrorContext(t, "/v1/messages")
		helper.SetEventStreamHeaders(c)
		require.NoError(t, helper.StringData(c, `{"type":"message_start"}`))

		Relay(c, types.RelayFormatClaude)

		body := recorder.Body.String()
		frames := sseDataFrames(t, body)
		require.Len(t, frames, 2, "the delivered chunk and the error frame, with no OpenAI terminator")
		assert.NotContains(t, body, "[DONE]")

		errorEvent := strings.Index(body, "event: error\ndata: ")
		require.GreaterOrEqual(t, errorEvent, 0, "the error frame must be named so Anthropic SDKs raise it")
		assert.Equal(t, "data: "+frames[1], strings.SplitN(body[errorEvent:], "\n", 3)[1],
			"the named event must carry the error payload")

		var errorBody claudeErrorBody
		require.NoError(t, common.Unmarshal([]byte(frames[1]), &errorBody))
		assert.Equal(t, "error", errorBody.Type)
		assert.Contains(t, errorBody.Error.Message, "field messages is required")
	})
}

// Responses clients decode each frame as a typed event and skip frames without
// a `type`, so the OpenAI {"error":...} frame plus [DONE] never surfaced in
// Codex: the stream ended without a terminal event and it reported "stream
// closed before response.completed" and retried, even for a policy block.
// response.failed is the protocol's terminal failure, and Codex classifies it by
// a string error.code — a non-string code fails its decode and loses the message.
func TestRelayErrorReportsResponsesMidStreamFailureAsResponseFailed(t *testing.T) {
	c, recorder := newRelayErrorContext(t, "/v1/responses")
	helper.SetEventStreamHeaders(c)
	require.NoError(t, helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.created"}, `{"type":"response.created"}`))

	Relay(c, types.RelayFormatOpenAIResponses)

	body := recorder.Body.String()
	assert.NotContains(t, body, "[DONE]", "Responses streams have no [DONE] terminator")
	frames := sseDataFrames(t, body)
	require.Len(t, frames, 2, "the delivered event and the failure event")

	failedEvent := strings.Index(body, "event: response.failed\ndata: ")
	require.GreaterOrEqual(t, failedEvent, 0, "the failure must be a named response.failed event")
	assert.Equal(t, "data: "+frames[1], strings.SplitN(body[failedEvent:], "\n", 3)[1])

	var event struct {
		Type     string `json:"type"`
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Code    json.RawMessage `json:"code"`
				Message string          `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	require.NoError(t, common.Unmarshal([]byte(frames[1]), &event))
	assert.Equal(t, "response.failed", event.Type, "the type field is what Responses clients dispatch on")
	assert.Equal(t, "failed", event.Response.Status)
	assert.True(t, strings.HasPrefix(string(event.Response.Error.Code), `"`), "error.code must be a JSON string, got %s", event.Response.Error.Code)
	assert.Contains(t, event.Response.Error.Message, "is required")
}

// Task adaptors wrap upstream bodies from many call sites, several without the
// "post"/"dial"/"http" words TaskErrorWrapper masks on, so the response is the
// one place that masks the address of the channel the last attempt used.
func TestRespondTaskErrorMasksTheChannelAddress(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://relay.private.example")

	respondTaskError(c, &dto.TaskError{
		Code:       "unmarshal_response_body_failed",
		Message:    "body: upstream relay.private.example rejected the job, see https://docs.example.org/tasks",
		StatusCode: http.StatusBadGateway,
	})

	assert.Equal(t, http.StatusBadGateway, recorder.Code)
	var body dto.TaskError
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "body: upstream ***.***.example rejected the job, see https://docs.example.org/tasks", body.Message)
}

func sseDataFrames(t *testing.T, body string) []string {
	t.Helper()
	var frames []string
	for _, line := range strings.Split(body, "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			frames = append(frames, payload)
		}
	}
	return frames
}
