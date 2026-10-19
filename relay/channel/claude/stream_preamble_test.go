package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newClaudeStreamRecorderTest(t *testing.T, body string, format types.RelayFormat) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()
	_, resp, info := newClaudeStreamTest(t, body)
	info.RelayFormat = format
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, recorder, resp, info
}

// An error the upstream reports before any content must reach the controller
// with nothing on the wire and with the status Anthropic documents for its
// type: shouldRetry refuses once anything was written, and the retry, cooldown
// and auto-disable rules all key on that status.
func TestClaudeStreamErrorBeforeContentWritesNothing(t *testing.T) {
	ping := `data: {"type":"ping"}` + "\n\n"
	for _, tc := range []struct {
		name      string
		format    types.RelayFormat
		errorType string
		status    int
	}{
		{"rate limit", types.RelayFormatClaude, "rate_limit_error", http.StatusTooManyRequests},
		{"overloaded", types.RelayFormatClaude, "overloaded_error", 529},
		{"invalid request", types.RelayFormatClaude, "invalid_request_error", http.StatusBadRequest},
		{"unknown type keeps 500", types.RelayFormatClaude, "future_error", http.StatusInternalServerError},
		{"openai format", types.RelayFormatOpenAI, "rate_limit_error", http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errorFrame := `data: {"type":"error","error":{"type":"` + tc.errorType + `","message":"Concurrency limit exceeded for account"}}` + "\n\n"
			c, recorder, resp, info := newClaudeStreamRecorderTest(t, claudeMessageStart+ping+errorFrame, tc.format)

			_, apiErr := ClaudeStreamHandler(c, resp, info)

			require.NotNil(t, apiErr)
			assert.Equal(t, tc.status, apiErr.StatusCode)
			assert.Empty(t, recorder.Body.String(), "frames held before content must be discarded with the error")
			assert.False(t, c.Writer.Written())
		})
	}
}

// A complete envelope with no content block — the shape a channel out of
// upstream credit returned — must fail as empty_response without writing the
// envelope, so the request can still move to another channel.
func TestClaudeStreamEmptyEnvelopeWritesNothing(t *testing.T) {
	c, recorder, resp, info := newClaudeStreamRecorderTest(t, claudeMessageStart+claudeMessageDelta+claudeMessageStop, types.RelayFormatClaude)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
	assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	assert.Empty(t, recorder.Body.String())
}

// Held frames are released ahead of the first content frame, in order, in both
// output formats.
func TestClaudeStreamPreambleReleasedBeforeContent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		format   types.RelayFormat
		preamble string
	}{
		{"claude", types.RelayFormatClaude, `"type":"message_start"`},
		{"openai", types.RelayFormatOpenAI, `"role":"assistant"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newClaudeStreamRecorderTest(t, claudeMessageStart+claudeTextDelta+claudeMessageDelta+claudeMessageStop, tc.format)

			_, apiErr := ClaudeStreamHandler(c, resp, info)

			require.Nil(t, apiErr)
			body := recorder.Body.String()
			preambleAt := strings.Index(body, tc.preamble)
			contentAt := strings.Index(body, "partial answer")
			require.GreaterOrEqual(t, preambleAt, 0, body)
			require.GreaterOrEqual(t, contentAt, 0, body)
			assert.Less(t, preambleAt, contentAt)
		})
	}
}

// Once content is on the wire the error still carries its mapped status, and
// what the caller already received stays in place.
func TestClaudeStreamErrorAfterContentKeepsDeliveredFrames(t *testing.T) {
	errorFrame := `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"
	c, recorder, resp, info := newClaudeStreamRecorderTest(t, claudeMessageStart+claudeTextDelta+errorFrame, types.RelayFormatClaude)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.NotNil(t, apiErr)
	assert.Equal(t, 529, apiErr.StatusCode)
	assert.Contains(t, recorder.Body.String(), `"type":"message_start"`)
	assert.Contains(t, recorder.Body.String(), "partial answer")
}

// A non-stream error body on a 200 maps the same way.
func TestClaudeNonStreamErrorBodyMapsStatus(t *testing.T) {
	_, _, info := newClaudeStreamTest(t, "")
	info.IsStream = false
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)),
	}

	_, apiErr := ClaudeHandler(c, resp, info)

	require.NotNil(t, apiErr)
	assert.Equal(t, 529, apiErr.StatusCode)
}
