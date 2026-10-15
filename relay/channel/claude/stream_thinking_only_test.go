package claude

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeStreamThinkingOnlyWarningPreservesContentAndCompletion(t *testing.T) {
	thinking := `data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"diagnostic reasoning"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"fixture-signature"}}` + "\n\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n"
	for _, tc := range []struct {
		name    string
		content string
		warning bool
	}{
		{"thinking only", thinking, true},
		{"omitted thinking", `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"fixture-signature"}}` + "\n\n", true},
		{"redacted thinking", `data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"fixture"}}` + "\n\n", true},
		{"empty text is not an answer", thinking + `data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}` + "\n\n", true},
		{"text delta", thinking + claudeTextDelta, false},
		{"text in start", thinking + `data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"answer"}}` + "\n\n", false},
		{"tool call without arguments", thinking + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_fixture","name":"Read","input":{}}}` + "\n\n", false},
		{"server tool", thinking + `data: {"type":"content_block_start","index":2,"content_block":{"type":"server_tool_use","id":"tool_fixture","name":"web_search","input":{}}}` + "\n\n", false},
		{"unknown content is not thinking", thinking + `data: {"type":"content_block_start","index":2,"content_block":{"type":"future_content"}}` + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, resp, info := newClaudeStreamTest(t, claudeMessageStart+tc.content+claudeMessageDelta+claudeMessageStop)
			recorder := httptest.NewRecorder()
			request := c.Request
			c, _ = gin.CreateTestContext(recorder)
			c.Request = request
			usage, apiErr := ClaudeStreamHandler(c, resp, info)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 7, usage.CompletionTokens)
			assert.True(t, info.StreamStatus.IsNormalEnd())
			assert.Nil(t, info.StreamStatus.FailureError(), "an output warning must not trigger retries or demote a channel")

			other := map[string]interface{}{}
			service.AppendRelayErrorStreamInfo(info, other)
			stream, ok := other["stream_status"].(map[string]interface{})
			require.True(t, ok)
			if tc.warning {
				assert.Equal(t, true, stream["thinking_only"])
				assert.Equal(t, "warning", stream["status"])
			} else {
				assert.NotContains(t, stream, "thinking_only")
				assert.Equal(t, "ok", stream["status"])
			}

			for _, line := range strings.Split(tc.content, "\n") {
				if strings.HasPrefix(line, "data:") {
					assert.Contains(t, recorder.Body.String(), line, "content events must reach the caller unchanged")
				}
			}
			assert.Contains(t, recorder.Body.String(), `"type":"message_stop"`)
		})
	}
}
