package service

import (
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A truncated message keeps end_reason "eof", because the connection really did
// close cleanly — the fault is only visible as the missing terminator. Without
// missing_terminator in the log the row shows status "error" beside a reason that
// looks healthy, and a truncated reply cannot be told apart from a transport
// fault. This is the field the whole observation depends on.
func TestAppendStreamStatus_MissingTerminator(t *testing.T) {
	t.Parallel()

	info := &relaycommon.RelayInfo{IsStream: true, StreamStatus: relaycommon.NewStreamStatus()}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	info.StreamStatus.MarkMissingTerminator()

	other := map[string]interface{}{}
	appendStreamStatus(info, other)

	streamInfo, ok := other["stream_status"].(map[string]interface{})
	require.True(t, ok, "stream_status must be recorded for a streamed request")
	assert.Equal(t, "error", streamInfo["status"],
		"a truncated message must not be logged as a success")
	assert.Equal(t, "eof", streamInfo["end_reason"],
		"the transport-level reason must be reported as it was")
	assert.Equal(t, true, streamInfo["missing_terminator"])
}

// The field has to stay absent on healthy streams, so its presence alone is a
// usable filter when counting truncations in production.
func TestAppendStreamStatus_CleanStreamOmitsMissingTerminator(t *testing.T) {
	t.Parallel()

	info := &relaycommon.RelayInfo{IsStream: true, StreamStatus: relaycommon.NewStreamStatus()}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)

	other := map[string]interface{}{}
	appendStreamStatus(info, other)

	streamInfo, ok := other["stream_status"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "ok", streamInfo["status"])
	assert.NotContains(t, streamInfo, "missing_terminator")
}

// max_tokens is the case this field exists for: the upstream closed the message
// properly, so every other field in the row says the request went fine, and the
// reply still stopped mid-sentence because it ran out of output budget. The
// status stays "ok" on purpose — the upstream did nothing wrong — which is
// exactly why the row needs stop_reason to be answerable at all.
func TestAppendStreamStatus_RecordsMaxTokensStopReason(t *testing.T) {
	t.Parallel()

	info := &relaycommon.RelayInfo{IsStream: true, StreamStatus: relaycommon.NewStreamStatus()}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	info.StreamStatus.SetStopReason("max_tokens")

	other := map[string]interface{}{}
	appendStreamStatus(info, other)

	streamInfo, ok := other["stream_status"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "max_tokens", streamInfo["stop_reason"])
	assert.Equal(t, "ok", streamInfo["status"],
		"a budget-limited reply is a complete stream, not an upstream fault")
	assert.NotContains(t, streamInfo, "missing_terminator",
		"the message was closed off; only its content was cut")
}

// A stream with no stop_reason must omit the key rather than carry an empty
// string, so that filtering on its presence in production is meaningful.
func TestAppendStreamStatus_OmitsAbsentStopReason(t *testing.T) {
	t.Parallel()

	info := &relaycommon.RelayInfo{IsStream: true, StreamStatus: relaycommon.NewStreamStatus()}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	info.StreamStatus.SetStopReason("")

	other := map[string]interface{}{}
	appendStreamStatus(info, other)

	streamInfo, ok := other["stream_status"].(map[string]interface{})
	require.True(t, ok)
	assert.NotContains(t, streamInfo, "stop_reason")
}
