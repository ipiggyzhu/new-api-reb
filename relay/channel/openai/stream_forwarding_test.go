package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// sseRecorder captures every SSE payload flushed to the client, so a test can
// drive the upstream one chunk at a time and observe what the caller has
// actually received before the next chunk exists.
type sseRecorder struct {
	gin.ResponseWriter
	pending strings.Builder
	mu      sync.Mutex
	payload []string
}

func (w *sseRecorder) Write(b []byte) (int, error) {
	w.pending.Write(b)
	return len(b), nil
}

func (w *sseRecorder) WriteString(s string) (int, error) {
	w.pending.WriteString(s)
	return len(s), nil
}

func (w *sseRecorder) Flush() {
	data := strings.TrimSpace(w.pending.String())
	w.pending.Reset()
	if data == "" {
		return
	}
	w.mu.Lock()
	w.payload = append(w.payload, strings.TrimPrefix(data, "data: "))
	w.mu.Unlock()
}

func (w *sseRecorder) received(substr string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.payload {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// runOaiStream feeds chunks into OaiStreamHandler and returns what the client
// received. When lockstep is set the upstream withholds the next chunk until
// the content of the current one has reached the client, so a handler that
// waits for the next chunk before forwarding the current one deadlocks instead
// of merely being slow. Chunks with no content or refusal (finish, usage,
// role-only) never wait.
func runOaiStream(t *testing.T, format types.RelayFormat, includeUsage, lockstep bool, chunks ...string) []string {
	t.Helper()

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	writer := &sseRecorder{ResponseWriter: c.Writer}
	c.Writer = writer

	errStalled := errors.New("client never received the previous chunk")
	stalled := make(chan struct{})
	pr, pw := io.Pipe()
	go func() {
		for _, chunk := range chunks {
			fmt.Fprintf(pw, "data: %s\n\n", chunk)
			delta := gjson.Get(chunk, "choices.0.delta")
			marker := delta.Get("content").String()
			if marker == "" {
				marker = delta.Get("refusal").String()
			}
			if !lockstep || marker == "" {
				continue
			}
			deadline := time.Now().Add(3 * time.Second)
			for !writer.received(marker) {
				if time.Now().After(deadline) {
					close(stalled)
					_ = pw.CloseWithError(errStalled)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		fmt.Fprint(pw, "data: [DONE]\n\n")
		_ = pw.Close()
	}()

	info := &relaycommon.RelayInfo{
		RelayMode:          relayconstant.RelayModeChatCompletions,
		RelayFormat:        format,
		ShouldIncludeUsage: includeUsage,
		ChannelMeta:        &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o"},
	}
	if format == types.RelayFormatClaude {
		info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{LastMessagesType: relaycommon.LastMessageTypeNone}
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(pr),
	}

	_, apiErr := OaiStreamHandler(c, info, resp)
	require.Nil(t, apiErr)

	select {
	case <-stalled:
		require.FailNow(t, "upstream stalled", "a chunk was withheld from the client until the next chunk arrived")
	default:
	}
	return writer.payload
}

func delta(content string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":%q}}]}`, content)
}

const (
	finishChunk    = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usageOnlyChunk = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`
)

// A content delta must reach the client as soon as the upstream emits it. The
// handler used to hold every chunk back until the next one arrived, which cost
// the caller a full upstream inter-chunk interval on the very first token.
func TestOaiStreamForwardsContentWithoutWaitingForNextChunk(t *testing.T) {
	payload := runOaiStream(t, types.RelayFormatOpenAI, true, true, delta("tok1"), delta("tok2"), finishChunk)

	assert.Equal(t, []string{delta("tok1"), delta("tok2"), finishChunk}, payload[:3])
}

// The caller asked for usage, so the upstream's usage chunk is passed through
// verbatim — exactly once, even though the tail path also inspects it.
func TestOaiStreamPassesUpstreamUsageThroughExactlyOnce(t *testing.T) {
	payload := runOaiStream(t, types.RelayFormatOpenAI, true, false, delta("tok1"), finishChunk, usageOnlyChunk)

	assert.Equal(t, []string{delta("tok1"), finishChunk, usageOnlyChunk, "[DONE]"}, payload)
}

// The caller did not ask for usage, so the trailing usage-only chunk must still
// be swallowed. This is the case the one-chunk lookahead exists for, and after
// the change it is the only case that still pays for it.
func TestOaiStreamSuppressesUsageChunkWhenCallerDidNotAskForIt(t *testing.T) {
	payload := runOaiStream(t, types.RelayFormatOpenAI, false, false, delta("tok1"), finishChunk, usageOnlyChunk)

	assert.Equal(t, []string{delta("tok1"), finishChunk, "[DONE]"}, payload)
}

// A usage-bearing chunk that also carries content is not suppressible, so it is
// forwarded rather than dropped.
func TestOaiStreamKeepsUsageChunkThatAlsoCarriesContent(t *testing.T) {
	withContent := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"tail"}}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`
	payload := runOaiStream(t, types.RelayFormatOpenAI, false, false, delta("tok1"), withContent)

	assert.Equal(t, []string{delta("tok1"), withContent, "[DONE]"}, payload)
}

// When the upstream never reports usage, the final content chunk is still sent
// once and new-api appends its own usage chunk.
func TestOaiStreamGeneratesUsageWhenUpstreamOmitsIt(t *testing.T) {
	payload := runOaiStream(t, types.RelayFormatOpenAI, true, false, delta("tok1"), finishChunk)

	require.Len(t, payload, 4)
	assert.Equal(t, delta("tok1"), payload[0])
	assert.Equal(t, finishChunk, payload[1])
	assert.Contains(t, payload[2], `"usage"`, "new-api must append a usage chunk")
	assert.Equal(t, "[DONE]", payload[3])
}

// OpenAI sends "usage":null on every chunk when stream_options.include_usage is
// set; a null usage is not a usage and must not make a delta wait.
func deltaWithNullUsage(content string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":%q}}],"usage":null}`, content)
}

// A Claude-format caller behind an OpenAI upstream must get each delta at once
// too. Only a chunk that can close the message (finish reason or usage) is held
// for HandleFinalResponse, which converts the tail exactly once with the
// settled usage.
func TestOaiStreamForwardsContentToClaudeClientWithoutWaitingForNextChunk(t *testing.T) {
	payload := runOaiStream(t, types.RelayFormatClaude, false, true, deltaWithNullUsage("tok1"), deltaWithNullUsage("tok2"), finishChunk, usageOnlyChunk)
	joined := strings.Join(payload, "\n")

	assert.Equal(t, 1, strings.Count(joined, `"text":"tok1"`))
	assert.Equal(t, 1, strings.Count(joined, `"text":"tok2"`))
	assert.Equal(t, 1, strings.Count(joined, `"type":"message_stop"`))
	assert.Contains(t, joined, `"stop_reason":"end_turn"`)
	assert.Contains(t, joined, `"output_tokens":22`)
}

func TestOaiStreamForwardsContentToGeminiClientWithoutWaitingForNextChunk(t *testing.T) {
	payload := runOaiStream(t, types.RelayFormatGemini, false, true, deltaWithNullUsage("tok1"), deltaWithNullUsage("tok2"), finishChunk)
	joined := strings.Join(payload, "\n")

	assert.Equal(t, 1, strings.Count(joined, `"text":"tok1"`))
	assert.Equal(t, 1, strings.Count(joined, `"text":"tok2"`))
	assert.Equal(t, 1, strings.Count(joined, `"finishReason":"STOP"`))
}

// A stream that ends on a plain delta has already forwarded its tail, so the
// final conversion must not replay it.
func TestOaiStreamDoesNotReplayForwardedTailWhenStreamEndsWithoutFinishReason(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatGemini} {
		payload := runOaiStream(t, format, false, false, delta("tok1"), delta("tok2"))
		joined := strings.Join(payload, "\n")

		assert.Equal(t, 1, strings.Count(joined, `"text":"tok1"`), string(format))
		assert.Equal(t, 1, strings.Count(joined, `"text":"tok2"`), string(format))
	}
}
