package common

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A message the upstream started and never closed is the shape that made a
// truncated reply look like a success: the connection closes cleanly, so
// EndReason is eof and every verdict downstream reads "normal". These cases pin
// the one signal that separates the two, and they live apart from
// stream_status_test.go because that file covers EndReason alone.
func TestStreamStatus_MissingTerminator_WithdrawsSuccess(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonEOF, nil)
	require.True(t, s.IsNormalEnd(), "a clean eof is the precondition this test needs")
	require.Nil(t, s.FailureError())

	s.MarkMissingTerminator()

	assert.True(t, s.MissingTerminator())
	assert.False(t, s.IsNormalEnd(),
		"a truncated message must not read as a normal end, however cleanly the connection closed")
	assert.Equal(t, StreamEndReasonEOF, s.EndReason,
		"the transport-level reason is still eof and must not be rewritten")

	failure := s.FailureError()
	require.NotNil(t, failure, "a truncated message must produce a failure verdict")
	assert.Equal(t, http.StatusBadGateway, failure.StatusCode)
	assert.Contains(t, failure.Error(), "without a terminator")
	assert.Contains(t, failure.Error(), string(StreamEndReasonEOF),
		"the end reason has to survive: a timeout that cut a message short is a different problem")
	// ErrorCodeBadResponse: IsChannelRoutingFaultError demotes on the 502 via the
	// retry status ranges, while ShouldDisableChannel cannot disable on it
	// because the code carries no "channel:" prefix.
	assert.Equal(t, types.ErrorCodeBadResponse, failure.GetErrorCode())
	assert.False(t, operation_setting.IsAlwaysSkipRetryCode(failure.GetErrorCode()),
		"a truncated message must not map to a never-retry error code")
}

// The caller hanging up produces the same shape — content delivered, no
// terminator — but the upstream did nothing wrong. Refusing the mark inside
// MarkMissingTerminator keeps that rule in one place instead of at each handler.
func TestStreamStatus_MissingTerminator_IgnoredForClientGone(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonClientGone, errors.New("context canceled"))

	s.MarkMissingTerminator()

	assert.False(t, s.MissingTerminator(),
		"a client disconnect must not be recorded as an upstream truncation")
	assert.Nil(t, s.FailureError(),
		"blaming the channel for an abandoned request would poison channel selection")
}

// A truncation discovered on an already-abnormal end must keep reporting the
// reason, because "the upstream timed out mid-message" and "the upstream closed
// mid-message" call for different operational responses.
func TestStreamStatus_MissingTerminator_OnAbnormalEnd(t *testing.T) {
	t.Parallel()

	for _, reason := range []StreamEndReason{
		StreamEndReasonTimeout,
		StreamEndReasonScannerErr,
		StreamEndReasonPingFail,
	} {
		s := NewStreamStatus()
		s.SetEndReason(reason, nil)
		s.MarkMissingTerminator()

		failure := s.FailureError()
		require.NotNil(t, failure, "reason %q", reason)
		assert.Contains(t, failure.Error(), "without a terminator", "reason %q", reason)
		assert.Contains(t, failure.Error(), string(reason),
			"reason %q must stay visible in the message", reason)
	}
}

// A body that was never a stream is reported ahead of everything else, because
// it is the one abnormal end still retryable on another channel. A truncation
// mark must not shadow it.
func TestStreamStatus_MissingTerminator_DoesNotShadowNoStreamBody(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonNoStreamBody, errors.New(`{"error":{"message":"Upstream request failed"}}`))
	s.MarkMissingTerminator()

	failure := s.FailureError()
	require.NotNil(t, failure)
	assert.Equal(t, types.ErrorCodeEmptyResponse, failure.GetErrorCode(),
		"a non-stream body must keep its own retryable verdict")
	assert.Contains(t, failure.Error(), "Upstream request failed")
}

func TestStreamStatus_MissingTerminator_Summary(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonEOF, nil)
	assert.NotContains(t, s.Summary(), "missing_terminator")

	s.MarkMissingTerminator()
	summary := s.Summary()
	assert.Contains(t, summary, "reason=eof")
	assert.Contains(t, summary, "missing_terminator=true")
}

func TestStreamStatus_MissingTerminator_NilSafe(t *testing.T) {
	t.Parallel()

	var s *StreamStatus
	s.MarkMissingTerminator()
	assert.False(t, s.MissingTerminator())
	assert.True(t, s.IsNormalEnd())
	assert.Nil(t, s.FailureError())
}

// The scanner goroutine reads the verdict through IsNormalEnd and Summary while
// the handler writes the mark, so the field needs the same mutex as the soft
// errors beside it. Run under -race.
func TestStreamStatus_MissingTerminator_Concurrent(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonEOF, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.MarkMissingTerminator()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.IsNormalEnd()
			_ = s.Summary()
			_ = s.FailureError()
			s.RecordError("soft")
		}()
	}
	wg.Wait()

	assert.True(t, s.MissingTerminator())
}
