package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain owns the package-level globals the stream handler reads, because
// setting them per test is itself a data race: gin.SetMode writes gin's mode
// global while gin.New (via CreateTestContext) reads it, and the scanner
// goroutine reads constant.StreamingTimeout. Under -race the concurrent tests
// in this package tripped on both, which reports as a race in whichever test
// happened to be running rather than where the write is. Written once here,
// before any test starts, there is no concurrent access left.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	if constant.StreamingTimeout == 0 {
		constant.StreamingTimeout = 30
	}
	os.Exit(m.Run())
}

func newClaudeStreamTest(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}

	info := &relaycommon.RelayInfo{
		ChannelMeta:  &relaycommon.ChannelMeta{},
		RelayFormat:  types.RelayFormatClaude,
		IsStream:     true,
		StreamStatus: relaycommon.NewStreamStatus(),
	}
	info.UpstreamModelName = "claude-opus-5"

	return c, resp, info
}

const (
	claudeMessageStart = `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n"
	claudeTextDelta    = `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}` + "\n\n"
	claudeMessageDelta = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n"
	claudeMessageStop  = `data: {"type":"message_stop"}` + "\n\n"
)

// A reply closed by message_stop alone is complete, so it must not be recorded
// as truncated even though message_delta never arrived.
//
// This separates the truncation verdict from claudeInfo.Done, which means
// message_delta specifically because that is the frame carrying stop_reason and
// the final usage. Keying truncation off Done would demote a channel whose
// upstream simply omits message_delta — the reply reached the client whole, and
// the only real consequence is that usage falls back to the local estimate.
func TestClaudeStreamHandler_MessageStopAloneIsNotTruncation(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t, claudeMessageStart+claudeTextDelta+claudeMessageStop)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.False(t, info.StreamStatus.MissingTerminator(),
		"message_stop closes the message off; the reply was not cut short")
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.Nil(t, info.StreamStatus.FailureError(),
		"demoting a channel for a complete reply would be a false positive")
}

// The production shape: the upstream streams part of an answer and closes the
// connection without any terminating frame. The transport-level end is a clean
// EOF, so before the truncation signal existed this reached the caller as a
// well-formed but truncated stream, was billed at full price, logged as
// stream_status ok/eof, and credited to the channel as a success.
func TestClaudeStreamHandler_TruncatedMidMessage(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t, claudeMessageStart+claudeTextDelta)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	// The handler still returns the usage: the tokens were really delivered and
	// the caller must be billed for them. Only the success verdict is withdrawn.
	require.Nil(t, apiErr, "the partial content is already on the wire, so the handler must not fail the relay")
	require.NotNil(t, usage)
	assert.Greater(t, usage.CompletionTokens, 0,
		"content that reached the client has to be billed even though the message was truncated")

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason,
		"the connection closed cleanly; the reason must stay eof")
	assert.True(t, info.StreamStatus.MissingTerminator(),
		"a message with neither message_delta nor message_stop must be recorded as truncated")
	assert.False(t, info.StreamStatus.IsNormalEnd(),
		"controller.Relay reads this to decide whether to credit a success")

	failure := info.StreamStatus.FailureError()
	require.NotNil(t, failure, "the verdict controller.Relay consults must be a failure")
	assert.Equal(t, http.StatusBadGateway, failure.StatusCode)
	assert.Equal(t, types.ErrorCodeBadResponse, failure.GetErrorCode())
	assert.Contains(t, failure.Error(), "without a terminator")
}

// The complete shape has to stay untouched, otherwise every healthy stream would
// start reporting a fault.
func TestClaudeStreamHandler_CompleteMessageStaysClean(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDelta+claudeMessageStop)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 7, usage.CompletionTokens, "a closed message reports the upstream's own count")

	assert.False(t, info.StreamStatus.MissingTerminator())
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.Nil(t, info.StreamStatus.FailureError())
}

// message_delta closes the message off. An upstream that omits only the trailing
// message_stop has still told us how the message ended and how much it produced,
// so it is not a truncation — treating it as one would demote every upstream
// that trims the final no-op frame.
func TestClaudeStreamHandler_MessageDeltaWithoutStopIsNotTruncation(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDelta)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.False(t, info.StreamStatus.MissingTerminator(),
		"message_delta already carried stop_reason and usage")
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.Nil(t, info.StreamStatus.FailureError())
}

// A stream that carried nothing at all is the older fault, and it must keep its
// own retryable verdict rather than being reclassified as a truncation: nothing
// is on the wire yet, so the request can still move to another channel.
func TestClaudeStreamHandler_NoContentKeepsEmptyResponseVerdict(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t, claudeMessageStart)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.NotNil(t, apiErr, "an upstream that answered nothing must fail the relay")
	assert.Nil(t, usage)
	assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
	assert.Contains(t, apiErr.Error(), "no text, no thinking, no tool call")
	assert.False(t, info.StreamStatus.MissingTerminator(),
		"the empty-answer guard returns before the truncation mark is reached")
}

// A truncated tool call never reaches ResponseText, so the truncation signal has
// to come from the missing message_delta rather than from accumulated text.
func TestClaudeStreamHandler_TruncatedToolCall(t *testing.T) {
	body := claudeMessageStart +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\n"

	c, resp, info := newClaudeStreamTest(t, body)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.True(t, info.StreamStatus.MissingTerminator(),
		"a tool call cut mid-arguments is a truncated message")
	assert.False(t, info.StreamStatus.IsNormalEnd())
}

// The caller hanging up produces the same wire shape as an upstream truncation:
// content delivered, no message_delta. The channel must not be demoted for it.
//
// Driven through a real context cancellation rather than by pre-setting the end
// reason, because StreamScannerHandler replaces info.StreamStatus outright
// (stream_scanner.go:134) — a pre-set reason would be discarded and the test
// would pass for the wrong reason. This also pins the ordering the guard depends
// on: the scanner records client_gone before ClaudeStreamHandler marks anything.
func TestClaudeStreamHandler_ClientGoneIsNotTruncation(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	c, resp, info := newClaudeStreamTest(t, "")
	resp.Body = pr

	ctx, cancel := context.WithCancel(context.Background())
	c.Request = c.Request.WithContext(ctx)

	delivered := make(chan struct{})
	go func() {
		_, _ = io.WriteString(pw, claudeMessageStart)
		_, _ = io.WriteString(pw, claudeTextDelta)
		close(delivered)
		// Deliberately left open: the client goes away while the upstream is
		// still considered live, which is what makes this a disconnect rather
		// than an upstream EOF.
	}()

	done := make(chan *types.NewAPIError, 1)
	go func() {
		<-delivered
		cancel()
	}()
	go func() {
		_, apiErr := ClaudeStreamHandler(c, resp, info)
		done <- apiErr
	}()

	select {
	case apiErr := <-done:
		require.Nil(t, apiErr)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the handler to observe the client disconnect")
	}

	require.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason,
		"the scanner has to attribute the end to the client, not to the upstream")
	assert.False(t, info.StreamStatus.MissingTerminator(),
		"an abandoned request must not be recorded as an upstream fault")
	assert.Nil(t, info.StreamStatus.FailureError(),
		"blaming the channel here would let one abandoned request poison channel selection")
}
