package service_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReasoningResponseHandlersRecordOnlyReportedEffort(t *testing.T) {
	for _, protocol := range []string{"chat", "claude", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, reported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/reported=%v", protocol, stream, reported), func(t *testing.T) {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
					info := &relaycommon.RelayInfo{IsStream: stream, DisablePing: true, RelayMode: relayconstant.RelayModeChatCompletions, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "fixture"}, RequestReasoning: &relaycommon.RequestReasoning{Effort: "max"}}
					field := ""
					if reported {
						field = `,"reasoning_effort":"low"`
					}
					body := `{"id":"chat-test","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}` + field + `}`
					info.RelayFormat = types.RelayFormatOpenAI
					if stream {
						body = "data: " + strings.ReplaceAll(body, `"message":`, `"delta":`) + "\n\ndata: [DONE]\n\n"
					}
					if protocol == "claude" {
						info.RelayFormat = types.RelayFormatClaude
						field = ""
						if reported {
							field = `,"output_config":{"effort":"low"}`
						}
						body = `{"id":"msg-test","type":"message","role":"assistant","model":"fixture","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}` + field + `}`
						if stream {
							body = "data: " + `{"type":"message_start","message":` + body + "}\n\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}` + "\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
						}
					}
					if protocol == "gemini" {
						info.RelayFormat = types.RelayFormatGemini
						field = ""
						if reported {
							field = `,"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}`
						}
						body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}` + field + `}`
						if stream {
							body = "data: " + body + "\n\n"
						}
					}
					response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
					var apiErr *types.NewAPIError
					var usage *dto.Usage
					switch protocol {
					case "chat":
						if stream {
							usage, apiErr = openai.OaiStreamHandler(c, info, response)
						} else {
							usage, apiErr = openai.OpenaiHandler(c, info, response)
						}
					case "claude":
						if stream {
							usage, apiErr = claude.ClaudeStreamHandler(c, response, info)
						} else {
							usage, apiErr = claude.ClaudeHandler(c, response, info)
						}
					case "gemini":
						if stream {
							usage, apiErr = gemini.GeminiTextGenerationStreamHandler(c, info, response)
						} else {
							usage, apiErr = gemini.GeminiTextGenerationHandler(c, info, response)
						}
					}
					require.Nil(t, apiErr)
					require.NotNil(t, usage)
					assert.Contains(t, recorder.Body.String(), "OK")
					log := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 0, 1)
					assert.Equal(t, "max", log["reasoning_effort"])
					if reported {
						assert.Equal(t, "low", log["upstream_reasoning_effort"])
					} else {
						assert.NotContains(t, log, "upstream_reasoning_effort")
					}
				})
			}
		}
	}
}
