package common

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/types"
)

type StreamEndReason string

const (
	StreamEndReasonNone        StreamEndReason = ""
	StreamEndReasonDone        StreamEndReason = "done"
	StreamEndReasonTimeout     StreamEndReason = "timeout"
	StreamEndReasonClientGone  StreamEndReason = "client_gone"
	StreamEndReasonScannerErr  StreamEndReason = "scanner_error"
	StreamEndReasonHandlerStop StreamEndReason = "handler_stop"
	StreamEndReasonEOF         StreamEndReason = "eof"
	StreamEndReasonPanic       StreamEndReason = "panic"
	StreamEndReasonPingFail    StreamEndReason = "ping_fail"
	// StreamEndReasonNoStreamBody: the upstream answered a streaming request
	// with a body that was never a stream, and the scanner forwarded nothing.
	StreamEndReasonNoStreamBody StreamEndReason = "no_stream_body"
)

const maxStreamErrorEntries = 20

type StreamErrorEntry struct {
	Message   string
	Timestamp time.Time
}

type StreamStatus struct {
	EndReason StreamEndReason
	EndError  error
	endOnce   sync.Once

	mu         sync.Mutex
	Errors     []StreamErrorEntry
	ErrorCount int
	// missingTerminator records that the upstream stopped mid-message: it sent
	// content and then closed without the frame that ends a message
	// (message_delta for Anthropic, which is where claudeInfo.Done is set;
	// message_stop is a trailing frame some upstreams omit even on a complete
	// reply, so it cannot be the signal). EndReason cannot carry this, because at the
	// transport level the close really was a clean EOF — the fault is only
	// visible to the protocol handler, which knows what a finished message looks
	// like. Guarded by mu because the scanner goroutine reads the verdict
	// through IsNormalEnd while the handler writes it.
	missingTerminator bool
}

func NewStreamStatus() *StreamStatus {
	return &StreamStatus{}
}

func (s *StreamStatus) SetEndReason(reason StreamEndReason, err error) {
	if s == nil {
		return
	}
	s.endOnce.Do(func() {
		s.EndReason = reason
		s.EndError = err
	})
}

func (s *StreamStatus) RecordError(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ErrorCount++
	if len(s.Errors) < maxStreamErrorEntries {
		s.Errors = append(s.Errors, StreamErrorEntry{
			Message:   msg,
			Timestamp: time.Now(),
		})
	}
}

// MarkMissingTerminator records that the upstream ended the stream without
// closing the message it had started.
//
// A client that hung up is refused here rather than at each call site: the
// stream ends the same way — content delivered, no terminator — but the upstream
// is not at fault and must not be demoted for it.
func (s *StreamStatus) MarkMissingTerminator() {
	if s == nil || s.EndReason == StreamEndReasonClientGone {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingTerminator = true
}

func (s *StreamStatus) MissingTerminator() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.missingTerminator
}

func (s *StreamStatus) HasErrors() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ErrorCount > 0
}

func (s *StreamStatus) TotalErrorCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ErrorCount
}

// FailureError reports an abnormal stream end as a relay-level error, or nil if
// the stream ended cleanly.
//
// Every relay handler used to return (usage, nil) unconditionally after
// StreamScannerHandler, so an upstream that dropped the connection mid-stream,
// timed out, or died in the handler goroutine was indistinguishable from a
// completed response: the truncated content was billed, no error log was
// written, and SetChannelAffinityRelayOutcome marked the channel healthy. A
// channel that reliably truncated long streams therefore never accumulated a
// single fault. The end reason is already recorded here, so the verdict is made
// from it in one place rather than in each of the 11 stream handlers.
//
// StreamEndReasonClientGone is deliberately excluded: the caller hanging up is
// not an upstream fault, and blaming the channel for it would let one abandoned
// request poison channel selection for everyone. StreamEndReasonNone is
// excluded for a different reason — it means no end reason was ever recorded,
// which is the state of a stream that never reached the scanner, not evidence
// that anything went wrong.
//
// Callers must settle billing before consulting this — the upstream really did
// produce the tokens that were delivered, so the user is charged for them; only
// the success verdict is withdrawn.
func (s *StreamStatus) FailureError() *types.NewAPIError {
	// Checked first, not alongside the reasons below: the calls in between are
	// nil-safe too, so a nil check further down reads as though they were
	// unreachable with a nil receiver when in fact they run.
	if s == nil {
		return nil
	}
	if streamErr := s.NoStreamBodyError(); streamErr != nil {
		return streamErr
	}
	if s.MissingTerminator() {
		// Reported separately from the reasons below because the transport-level
		// end was clean, so naming it alone ("ended abnormally (eof)") would
		// describe the opposite of what happened. The reason is still included:
		// a timeout that cut a message short is a different operational problem
		// from an upstream that closed on its own.
		return types.NewErrorWithStatusCode(
			fmt.Errorf("upstream ended the stream mid-message without a terminator (%s)", s.EndReason),
			types.ErrorCodeBadResponse,
			http.StatusBadGateway,
		)
	}
	if s.IsNormalEnd() ||
		s.EndReason == StreamEndReasonNone ||
		s.EndReason == StreamEndReasonClientGone {
		return nil
	}
	detail := string(s.EndReason)
	if s.EndError != nil {
		detail = fmt.Sprintf("%s: %s", s.EndReason, s.EndError.Error())
	}
	return types.NewErrorWithStatusCode(
		fmt.Errorf("upstream stream ended abnormally (%s)", detail),
		types.ErrorCodeBadResponse,
		http.StatusBadGateway,
	)
}

// NoStreamBodyError reports an upstream that answered a streaming request with
// a body that was never a stream — the shape a relay produces when it fails
// after having already committed to 200, usually a bare JSON error object.
//
// It is split out from FailureError because it is the one abnormal end a
// handler can still act on. Every other reason here is discovered with part of
// the response already on the wire, so all the caller can do is withdraw the
// success verdict. This one is discovered having forwarded nothing at all:
// returning it before the handler writes its terminator is what keeps the
// request retryable on another channel, instead of ending it as a clean but
// empty stream the caller reports as an empty response.
//
// ErrorCodeEmptyResponse rather than ErrorCodeBadResponse: the caller sees the
// same symptom as any other empty response, and that code is the one
// IsChannelRoutingFaultError demotes on while leaving ShouldDisableChannel
// unable to disable the channel over it.
func (s *StreamStatus) NoStreamBodyError() *types.NewAPIError {
	if s == nil || s.EndReason != StreamEndReasonNoStreamBody {
		return nil
	}
	err := errors.New("upstream returned no stream")
	if s.EndError != nil {
		err = fmt.Errorf("upstream returned no stream: %s", s.EndError.Error())
	}
	return types.NewErrorWithStatusCode(err, types.ErrorCodeEmptyResponse, http.StatusBadGateway)
}

func (s *StreamStatus) IsNormalEnd() bool {
	if s == nil {
		return true
	}
	// A truncated message is not a normal end however cleanly the connection
	// closed. This is what withdraws the success verdict in controller.Relay and
	// turns the log's stream_status to "error": both read the answer from here.
	if s.MissingTerminator() {
		return false
	}
	return s.EndReason == StreamEndReasonDone ||
		s.EndReason == StreamEndReasonEOF ||
		s.EndReason == StreamEndReasonHandlerStop
}

func (s *StreamStatus) Summary() string {
	if s == nil {
		return "StreamStatus<nil>"
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "reason=%s", s.EndReason)
	if s.EndError != nil {
		fmt.Fprintf(b, " end_error=%q", s.EndError.Error())
	}
	s.mu.Lock()
	// Read directly: mu is already held here and sync.Mutex is not reentrant, so
	// going through MissingTerminator() would deadlock.
	if s.missingTerminator {
		fmt.Fprint(b, " missing_terminator=true")
	}
	if s.ErrorCount > 0 {
		fmt.Fprintf(b, " soft_errors=%d", s.ErrorCount)
	}
	s.mu.Unlock()
	return b.String()
}
