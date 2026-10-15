package dto

import (
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
