package common

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stop_reason describes how the message ended, EndReason how the connection did,
// and the pair is what separates a reply the model finished from one cut off at
// the output budget. Both arrive as a protocol-complete stream, so these cases
// pin that recording the reason never turns a complete stream into a fault —
// the whole point is a log row that can tell them apart, not a new verdict.
// They live apart from stream_truncation_test.go because that file covers the
// opposite case: a message with no terminator at all.
func TestStreamStatus_StopReason_RecordedWithoutAffectingVerdict(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"end_turn", "max_tokens", "tool_use", "stop_sequence", "refusal"} {
		s := NewStreamStatus()
		s.SetEndReason(StreamEndReasonEOF, nil)
		s.SetStopReason(reason)

		assert.Equal(t, reason, s.StopReason())
		assert.True(t, s.IsNormalEnd(),
			"reason %q ends a stream the protocol considers complete", reason)
		assert.Nil(t, s.FailureError(),
			"reason %q is not an upstream fault and must not demote a channel", reason)
		assert.False(t, s.MissingTerminator(),
			"a declared stop_reason proves a terminator arrived")
	}
}

// max_tokens is the case the field exists for: the upstream itself says the reply
// was cut short at the output budget, on a stream that is otherwise flawless.
// Without the reason in the log this row is identical to end_turn.
func TestStreamStatus_StopReason_MaxTokensIsVisibleButNotAFault(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonEOF, nil)
	s.SetStopReason("max_tokens")

	summary := s.Summary()
	assert.Contains(t, summary, "stop_reason=max_tokens")
	assert.NotContains(t, summary, "missing_terminator",
		"the message was closed off; only its content was cut")
	assert.Nil(t, s.FailureError(),
		"the output budget is usually the caller's own, so the channel is blameless")
}

// Empty is ignored so a terminator that omits the field cannot erase what an
// earlier one declared — the trailing message_stop carries no stop_reason, and it
// arrives after the message_delta that does.
func TestStreamStatus_StopReason_EmptyDoesNotErase(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	assert.Empty(t, s.StopReason(), "nothing declared yet")
	assert.NotContains(t, s.Summary(), "stop_reason",
		"absent rather than empty, so its presence alone is a usable log filter")

	s.SetStopReason("max_tokens")
	s.SetStopReason("")

	assert.Equal(t, "max_tokens", s.StopReason())
}

// Unlike SetEndReason this is deliberately not a sync.Once: that one guards
// against a later, less specific reason overwriting the real cause of an abnormal
// end, whereas a message has exactly one terminator. Pinned so the difference is
// a decision rather than an oversight.
func TestStreamStatus_StopReason_LastNonEmptyWins(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetStopReason("end_turn")
	s.SetStopReason("max_tokens")

	assert.Equal(t, "max_tokens", s.StopReason())
}

// A truncated stream has no terminator, so it has no stop_reason to carry: the
// two signals are mutually exclusive in practice and must not be confused for
// each other when reading a log row.
func TestStreamStatus_StopReason_AbsentOnTruncation(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonEOF, nil)
	s.SetStopReason("")
	s.MarkMissingTerminator()

	assert.Empty(t, s.StopReason())
	summary := s.Summary()
	assert.Contains(t, summary, "missing_terminator=true")
	assert.NotContains(t, summary, "stop_reason")
}

func TestStreamStatus_StopReason_NilSafe(t *testing.T) {
	t.Parallel()

	var s *StreamStatus
	s.SetStopReason("end_turn")
	assert.Empty(t, s.StopReason())
	assert.True(t, s.IsNormalEnd())
	assert.Nil(t, s.FailureError())
}

// Summary reads the field with mu already held, so it takes the value directly
// rather than through StopReason() — sync.Mutex is not reentrant and the accessor
// would deadlock. Run under -race.
func TestStreamStatus_StopReason_Concurrent(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonEOF, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetStopReason("max_tokens")
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.StopReason()
			_ = s.Summary()
			_ = s.IsNormalEnd()
			_ = s.FailureError()
		}()
	}
	wg.Wait()

	require.Equal(t, "max_tokens", s.StopReason())
}
