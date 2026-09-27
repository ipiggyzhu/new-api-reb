package helper

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Response-side sensitive-word enforcement. The request-side check in
// controller/relay.go answers "what did the caller send us"; this answers "what
// is the upstream sending back", which is a different threat: a compromised or
// hostile upstream can inject instructions into the model's reply to steer the
// caller's agent, and it can echo back data it was never meant to surface.
//
// Two enforcement modes, selected by StopOnSensitiveEnabled:
//
//   - true  (default) — block. The response never reaches the caller.
//   - false           — audit. The hit is logged and the response is forwarded.
//
// Audit mode exists because the legacy alternative, redaction via
// service.SensitiveWordReplace, cannot be used here: that function indexes the
// Aho-Corasick hit positions (rune offsets) into a byte-sliced string, so on any
// non-ASCII text it cuts mid-codepoint and corrupts the body. Forwarding a
// logged hit is honest; shipping mojibake is not. Redaction can be revisited if
// that function is fixed.

// sensitiveResponseError builds the client-facing rejection. It is deliberately
// 502 rather than 400: the caller did nothing wrong, the upstream did, and the
// status code is what tells an SDK's retry logic which side to blame. SkipRetry
// is set because the words came from the response of a channel that already
// answered — replaying the same prompt on another channel is a fresh billable
// call that is likely to produce the same hit.
func sensitiveResponseError(words []string) *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		errors.New("upstream response blocked: sensitive words detected ("+service.DescribeSensitiveWords(words)+")"),
		types.ErrorCodeSensitiveWordsDetected,
		http.StatusBadGateway,
		types.ErrOptionWithSkipRetry(),
	)
}

// shouldScanResponse is the cheap gate in front of every scan. Both checks are
// plain variable reads, which matters because the streaming path consults this
// once per chunk.
func shouldScanResponse() bool {
	return setting.ShouldCheckCompletionSensitive() && len(setting.SensitiveWords) > 0
}

// CheckNonStreamResponseSensitive scans a fully-buffered response body. It must
// be called before service.IOCopyBytesGracefully: that function sets
// Content-Length and calls WriteHeader, after which the status code is committed
// and no error can be returned to the caller.
//
// Returning a non-nil error means the body must not be written.
func CheckNonStreamResponseSensitive(c *gin.Context, body []byte) *types.NewAPIError {
	if !shouldScanResponse() {
		return nil
	}
	contains, words := service.ScanResponseForSensitiveWords(body)
	if !contains {
		return nil
	}
	if !setting.StopOnSensitiveEnabled {
		logger.LogWarn(c, fmt.Sprintf("upstream response sensitive words detected (audit only, not blocked): %s", service.DescribeSensitiveWords(words)))
		return nil
	}
	logger.LogWarn(c, fmt.Sprintf("upstream response blocked, sensitive words detected: %s", service.DescribeSensitiveWords(words)))
	return sensitiveResponseError(words)
}

// blockStreamChunkOnSensitive scans one SSE payload before it is handed to the
// adapter that would write it to the caller. It reports whether the chunk was
// blocked.
//
// A stream cannot be un-sent. By the time a chunk is rejected the headers are
// committed at 200 and earlier chunks are already on the wire, so enforcement
// here truncates the stream rather than replacing it with an error document —
// sr.Stop records the reason, ends the stream, and (via shouldRetry seeing bytes
// already written) keeps the request from being replayed onto another channel.
// Stopping at the first offending chunk is still the point: the injected
// instruction never reaches the caller's agent.
func blockStreamChunkOnSensitive(c *gin.Context, data string, sr *StreamResult) bool {
	if !shouldScanResponse() {
		return false
	}
	contains, words := service.ScanResponseForSensitiveWords([]byte(data))
	if !contains {
		return false
	}
	if !setting.StopOnSensitiveEnabled {
		logger.LogWarn(c, fmt.Sprintf("upstream stream sensitive words detected (audit only, not blocked): %s", service.DescribeSensitiveWords(words)))
		return false
	}
	logger.LogWarn(c, fmt.Sprintf("upstream stream truncated, sensitive words detected: %s", service.DescribeSensitiveWords(words)))
	sr.Stop(sensitiveResponseError(words))
	return true
}
