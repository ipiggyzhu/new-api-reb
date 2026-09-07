package claude

import (
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	claudeMessageDeltaMaxTokens = `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":7}}` + "\n\n"
	claudeMessageDeltaToolUse   = `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}` + "\n\n"
	claudeMessageDeltaNoReason  = `data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n"
)

// The case this whole field exists for: a reply cut off at the output budget. The
// stream is flawless — message_delta arrived, the connection closed cleanly — so
// every other signal reads exactly as it does for a reply the model finished on
// its own. Only stop_reason separates them, which is what makes "it stopped after
// two sentences" answerable from a log at all.
func TestClaudeStreamHandler_MaxTokensIsRecordedNotBlamed(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDeltaMaxTokens+claudeMessageStop)

	usage, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, "max_tokens", info.StreamStatus.StopReason(),
		"the upstream said the reply was cut short; that has to survive into the log")
	// The channel is blameless: the output budget is normally the caller's own
	// max_tokens. Demoting on this would punish upstreams for honouring the request.
	assert.False(t, info.StreamStatus.MissingTerminator())
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.Nil(t, info.StreamStatus.FailureError())
}

// The contrast case. Same wire shape, same clean end, different ending — and
// before stop_reason was recorded these two produced byte-identical log rows.
func TestClaudeStreamHandler_EndTurnIsRecorded(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDelta+claudeMessageStop)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	assert.Equal(t, "end_turn", info.StreamStatus.StopReason())
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

// message_stop arrives after the message_delta that carried the reason and has no
// stop_reason of its own. Capturing the terminator unconditionally would let this
// trailing frame blank out what was already declared, which would silently undo
// the fix for every upstream that sends the full sequence — the common case.
func TestClaudeStreamHandler_TrailingMessageStopDoesNotEraseStopReason(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDeltaMaxTokens+claudeMessageStop)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	assert.Equal(t, "max_tokens", info.StreamStatus.StopReason(),
		"message_stop carries no stop_reason and must not overwrite message_delta's")
}

// tool_use ends the message legitimately: the model is waiting on a tool result,
// not cut short. Recorded so a reply that stops to call a tool is not read as a
// truncation when the field is used to triage complaints.
func TestClaudeStreamHandler_ToolUseStopReason(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDeltaToolUse+claudeMessageStop)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	assert.Equal(t, "tool_use", info.StreamStatus.StopReason())
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

// A truncated stream has no terminator, so there is no stop_reason to record. The
// field must stay absent rather than empty, so that filtering a log on its
// presence is meaningful and the two signals are never confused for each other.
func TestClaudeStreamHandler_TruncationHasNoStopReason(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t, claudeMessageStart+claudeTextDelta)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	assert.Empty(t, info.StreamStatus.StopReason(),
		"no terminator arrived, so nothing declared how the message ended")
	assert.True(t, info.StreamStatus.MissingTerminator())
}

// An upstream that closes the message but omits the field is complete-but-silent:
// not a truncation, and not something to invent a reason for. The absent field is
// the honest record.
func TestClaudeStreamHandler_TerminatorWithoutStopReasonStaysEmpty(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDeltaNoReason)

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	assert.Empty(t, info.StreamStatus.StopReason())
	assert.False(t, info.StreamStatus.MissingTerminator(),
		"message_delta closed the message off, reason or no reason")
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

// The OpenAI-format path shares claudeInfo with the Claude path, which is why
// SetStopReason sits in the handler both formats funnel through rather than in the
// per-frame formatter. A client asking in OpenAI format gets the same diagnosis.
func TestClaudeStreamHandler_StopReasonOnOpenAIFormat(t *testing.T) {
	c, resp, info := newClaudeStreamTest(t,
		claudeMessageStart+claudeTextDelta+claudeMessageDeltaMaxTokens+claudeMessageStop)
	info.RelayFormat = types.RelayFormatOpenAI

	_, apiErr := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiErr)
	assert.Equal(t, "max_tokens", info.StreamStatus.StopReason(),
		"the OpenAI path converts the frame but shares the same claudeInfo")
}
