package zhipu

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gin.Context.Stream type-asserts the writer to http.CloseNotifier, which
// httptest.ResponseRecorder does not implement. The channel is never closed, so
// the handler streams to completion exactly as it does against a connected
// client.
type streamRecorder struct {
	*httptest.ResponseRecorder
	closed chan bool
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{ResponseRecorder: httptest.NewRecorder(), closed: make(chan bool)}
}

func (w *streamRecorder) CloseNotify() <-chan bool { return w.closed }

func newZhipuStreamTest(t *testing.T, body string) (*gin.Context, *streamRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	recorder := newStreamRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "glm-4"}}

	return c, recorder, resp, info
}

// Zhipu reports token counts in a trailing "meta:" frame. A client disconnect,
// an upstream truncation, or a model that simply omits meta never delivers one,
// and the handler used to return a nil *dto.Usage on that path. Its callers
// narrow the adaptor's `any` usage with a one-value type assertion, which
// succeeds on a typed nil and then panics on the first field access — and that
// panic unwinds past controller.Relay's deferred refund, so the pre-consumed
// quota is neither refunded nor settled and the user is charged with no consume
// log. A stream that delivered content must come back billable.
func TestZhipuStreamHandlerReturnsUsageWithoutMetaFrame(t *testing.T) {
	c, recorder, resp, info := newZhipuStreamTest(t, "data: 你好\ndata: 世界\n")

	usage, apiErr := zhipuStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage, "a stream that never sent meta must still yield a usage; its callers dereference this pointer")
	assert.Positive(t, usage.CompletionTokens,
		"content the client already received has to be counted locally, otherwise a truncated stream bills nothing")
	assert.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
	assert.Contains(t, recorder.Body.String(), "data: [DONE]", "the stream must still be terminated")
}

// The meta frame is authoritative when it arrives: upstream counts must win over
// the local estimate.
func TestZhipuStreamHandlerPrefersUpstreamMetaUsage(t *testing.T) {
	c, _, resp, info := newZhipuStreamTest(t,
		"data: 你好\nmeta: {\"request_id\":\"r-1\",\"usage\":{\"prompt_tokens\":31,\"completion_tokens\":47,\"total_tokens\":78}}\n")

	usage, apiErr := zhipuStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, &dto.Usage{PromptTokens: 31, CompletionTokens: 47, TotalTokens: 78}, usage,
		"upstream token counts must be billed as reported, not replaced by the local estimate")
}

// An upstream that cuts the connection before sending anything has no content to
// count, but it still must not hand a nil pointer to a caller that dereferences
// it.
func TestZhipuStreamHandlerReturnsUsageForEmptyStream(t *testing.T) {
	c, _, resp, info := newZhipuStreamTest(t, "")

	usage, apiErr := zhipuStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage, "an empty stream must return a zeroed usage, never nil")
	assert.Zero(t, usage.CompletionTokens, "nothing was delivered, so nothing may be billed as output")
}
