package claude

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The usage audit (service.attachUsageAnomaly) can only judge what the stream
// handler leaves for it: the prompt side message_start declared, and whether
// the upstream reported no usage before the local estimate replaced it.
func TestClaudeStreamHandlerRecordsUsageForTheAudit(t *testing.T) {
	const (
		zeroMessageStart = `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n"
		zeroMessageDelta = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}` + "\n\n"
		cacheStart       = `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"cache_read_input_tokens":500,"cache_creation_input_tokens":20,"output_tokens":1}}}` + "\n\n"
	)
	tests := []struct {
		name        string
		body        string
		wantAnomaly string
		wantStart   int
	}{
		{name: "normal stream", body: cacheStart + claudeTextDelta + claudeMessageDelta + claudeMessageStop,
			wantStart: 530},
		{name: "finished stream that answered but reported no usage", body: zeroMessageStart + claudeTextDelta + zeroMessageDelta + claudeMessageStop,
			wantAnomaly: dto.UsageAnomalyZeroUsage},
		// Cut off before message_delta: the missing usage is the truncation's,
		// which StreamStatus already records, not an accounting fault.
		{name: "truncated stream", body: zeroMessageStart + claudeTextDelta},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, resp, info := newClaudeStreamTest(t, tt.body)

			usage, apiErr := ClaudeStreamHandler(c, resp, info)

			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tt.wantAnomaly, usage.UpstreamUsageAnomaly)
			assert.Equal(t, tt.wantStart, usage.StreamStartPromptTokens)
			if tt.wantAnomaly != "" {
				assert.Positive(t, usage.CompletionTokens, "the local estimate still bills the answer; the marker only records why")
			}
		})
	}
}
