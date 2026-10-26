package dto

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClaudeRequestPreservesPerMessageOutputConfig(t *testing.T) {
	const input = `{"model":"claude-opus-5-5","output_config":{"effort":"high"},"messages":[{"role":"user","content":"Continue the previous task."},{"role":"system","content":[],"output_config":{"effort":"medium"}}]}`
	var request ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(input), &request))
	copy, err := common.DeepCopy(&request)
	require.NoError(t, err)
	encoded, err := common.Marshal(copy)
	require.NoError(t, err)
	assert.Equal(t, "medium", gjson.GetBytes(encoded, "messages.1.output_config.effort").String())
	assert.Equal(t, "high", gjson.GetBytes(encoded, "output_config.effort").String())
	assert.False(t, gjson.GetBytes(encoded, "messages.0.output_config").Exists())
	assert.Equal(t, "[]", gjson.GetBytes(encoded, "messages.1.content").Raw)
}

// A native request's thinking object reaches the upstream whole: block_binding
// and any field the gateway does not model yet survive decode, DeepCopy and
// encode, and a known field still wins over a stale Extra entry of that name.
// Key order is part of the contract: CLI profiles send "type" first, so the
// modeled fields keep their order and unmodeled ones follow them.
func TestClaudeThinkingForwardsUnmodeledFields(t *testing.T) {
	const input = `{"model":"claude-opus-5-5","thinking":{"type":"adaptive","display":"summarized",` +
		`"block_binding":{"prefix_mismatch_behavior":"drop_block"},"future_field":{"a":1}},"messages":[]}`
	var request ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(input), &request))
	require.NotNil(t, request.Thinking)
	assert.JSONEq(t, `{"prefix_mismatch_behavior":"drop_block"}`, string(request.Thinking.BlockBinding))

	copied, err := common.DeepCopy(&request)
	require.NoError(t, err)
	encoded, err := common.Marshal(copied)
	require.NoError(t, err)
	assert.Equal(t, `{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"},"future_field":{"a":1}}`,
		gjson.GetBytes(encoded, "thinking").Raw)

	copied.Thinking.Type = "enabled"
	copied.Thinking.Extra["type"] = []byte(`"stale"`)
	encoded, err = common.Marshal(copied.Thinking)
	require.NoError(t, err)
	assert.Equal(t, `{"type":"enabled","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"},"future_field":{"a":1}}`, string(encoded))

	encoded, err = common.Marshal(Thinking{Type: "disabled"})
	require.NoError(t, err)
	assert.Equal(t, `{"type":"disabled"}`, string(encoded))

	encoded, err = common.Marshal(Thinking{Extra: map[string]json.RawMessage{"future_field": json.RawMessage(`1`)}})
	require.NoError(t, err)
	assert.Equal(t, `{"future_field":1}`, string(encoded))
}

// Claude Code auto mode sends a top-level safeguards object. A native request
// that is decoded and re-encoded (every non-passthrough /v1/messages request)
// must still carry it, and a request without it must not grow the key.
func TestClaudeRequestForwardsSafeguards(t *testing.T) {
	const withSafeguards = `{"model":"claude-opus-5-5","messages":[],"tools":[],"safeguards":{"mode":"auto","rules":[{"id":"r1"}]}}`
	var request ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(withSafeguards), &request))
	copied, err := common.DeepCopy(&request)
	require.NoError(t, err)
	encoded, err := common.Marshal(copied)
	require.NoError(t, err)
	assert.Equal(t, `{"mode":"auto","rules":[{"id":"r1"}]}`, gjson.GetBytes(encoded, "safeguards").Raw)

	var plain ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"claude-opus-5-5","messages":[]}`), &plain))
	encoded, err = common.Marshal(&plain)
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(encoded, "safeguards").Exists())
}
