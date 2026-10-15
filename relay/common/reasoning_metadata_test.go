package common

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestReasoningAcrossProtocols(t *testing.T) {
	for _, tc := range []struct {
		name, body, effort, mode string
		request                  dto.Request
		budget                   *int
	}{
		{"chat", `{"reasoning_effort":"high"}`, "high", "", &dto.GeneralOpenAIRequest{}, nil},
		{"responses none", `{"reasoning":{"effort":"none"}}`, "none", "", &dto.OpenAIResponsesRequest{}, nil},
		{"claude adaptive", `{"output_config":{"effort":"max"},"thinking":{"type":"adaptive"}}`, "max", "adaptive", &dto.ClaudeRequest{}, nil},
		{"claude per turn", `{"output_config":{"effort":"high"},"messages":[{"role":"system","content":[],"output_config":{"effort":"low"}},{"role":"user","content":"Hi"},{"role":"system","content":[],"output_config":{"effort":"max"}}]}`, "low", "", &dto.ClaudeRequest{}, nil},
		{"claude budget", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, "", "enabled", &dto.ClaudeRequest{}, intPointer(1024)},
		{"gemini level", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`, "HIGH", "", &dto.GeminiChatRequest{}, nil},
		{"gemini config aliases", `{"generationConfig":{"thinking_config":{"thinking_level":"low"}}}`, "low", "", &dto.GeminiChatRequest{}, nil},
		{"gemini disabled budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, "", "", &dto.GeminiChatRequest{}, intPointer(0)},
		{"gemini dynamic budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`, "", "", &dto.GeminiChatRequest{}, intPointer(-1)},
		{"deepseek", `{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"max"}`, "max", "enabled", &dto.GeneralOpenAIRequest{}, nil},
		{"qwen explicit false", `{"enable_thinking":false}`, "", "disabled", &dto.GeneralOpenAIRequest{}, nil},
		{"kimi mode", `{"thinking":{"type":"disabled"}}`, "", "disabled", &dto.GeneralOpenAIRequest{}, nil},
		{"unspecified", `{}`, "", "", &dto.GeneralOpenAIRequest{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, common.UnmarshalJsonStr(tc.body, tc.request))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			info := GenRelayInfoOpenAI(c, tc.request)
			require.NotNil(t, info.RequestReasoning)
			assert.Equal(t, &RequestReasoning{Effort: tc.effort, Mode: tc.mode, Budget: tc.budget}, info.RequestReasoning)
			info.ResetResponseState()
			assert.Equal(t, tc.effort, info.RequestReasoning.Effort, "retry keeps original client settings")
		})
	}
}

func intPointer(value int) *int { return &value }

func TestUpstreamReasoningDoesNotInferOrLeakAcrossRetries(t *testing.T) {
	for _, body := range []string{
		`{"reasoning_effort":"low"}`, `{"reasoning":{"effort":"low"}}`,
		`{"message":{"output_config":{"effort":"low"}}}`, `{"output_config":{"effort":"low"}}`,
		`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`,
	} {
		info := &RelayInfo{ReasoningEffort: "max"}
		info.CaptureUpstreamReasoning(body)
		assert.Equal(t, "low", info.UpstreamReasoningEffort)
		info.CaptureUpstreamReasoning(`{"usage":{"reasoning_tokens":100},"input":[{"reasoning_effort":"high"}],"reasoning_effort":null}`)
		assert.Equal(t, "low", info.UpstreamReasoningEffort)
		info.ResetResponseState()
		info.CaptureUpstreamReasoning(`{"choices":[{"delta":{"reasoning_content":"high"}}]}`)
		assert.Empty(t, info.UpstreamReasoningEffort)
		assert.Equal(t, "max", info.ReasoningEffort)
	}
}
