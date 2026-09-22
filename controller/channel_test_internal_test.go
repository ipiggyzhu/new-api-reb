package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier: "base",
	})

	require.Equal(t, "tiered_expr", other["billing_mode"])
	require.Equal(t, "base", other["matched_tier"])
	require.NotEmpty(t, other["expr_b64"])
}

// TestRequireTestOutput pins the probe verdict that keeps unusable models off a
// channel: a 200 with a well-formed envelope and no content is a failure in every
// text format, a padded token count does not rescue a non-stream reply, and
// genuine content in any format passes.
func TestRequireTestOutput(t *testing.T) {
	testCases := []struct {
		name    string
		format  types.RelayFormat
		body    string
		usage   *dto.Usage
		stream  bool
		wantErr bool
	}{
		{"chat with content", types.RelayFormatOpenAI, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, &dto.Usage{}, false, false},
		{"chat with reasoning only", types.RelayFormatOpenAI, `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"thinking"}}]}`, &dto.Usage{}, false, false},
		{"chat with tool call", types.RelayFormatOpenAI, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"f"}}]}}]}`, &dto.Usage{}, false, false},
		{"chat empty content", types.RelayFormatOpenAI, `{"choices":[{"message":{"role":"assistant","content":""}}]}`, &dto.Usage{}, false, true},
		{"chat empty content with padded usage", types.RelayFormatOpenAI, `{"choices":[{"message":{"role":"assistant","content":""}}],"usage":{"completion_tokens":7}}`, &dto.Usage{CompletionTokens: 7}, false, true},
		{"chat no choices", types.RelayFormatOpenAI, `{"id":"x","object":"chat.completion","choices":[]}`, &dto.Usage{CompletionTokens: 1}, false, true},
		{"adaptor already validated", types.RelayFormatOpenAI, `{"choices":[]}`, &dto.Usage{ResponseValidated: true}, false, false},
		{"chat stream delta", types.RelayFormatOpenAI, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n", &dto.Usage{}, true, false},
		{"chat stream lifecycle only", types.RelayFormatOpenAI, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n", &dto.Usage{}, true, true},
		{"chat stream trusted usage", types.RelayFormatOpenAI, "data: {\"choices\":[{\"delta\":{}}]}\n", &dto.Usage{CompletionTokens: 3}, true, false},
		{"claude text block", types.RelayFormatClaude, `{"type":"message","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":1}}`, &dto.Usage{}, false, false},
		{"claude empty content with output tokens", types.RelayFormatClaude, `{"type":"message","content":[],"stop_reason":"end_turn","usage":{"output_tokens":1}}`, &dto.Usage{CompletionTokens: 1}, false, true},
		{"claude stream text delta", types.RelayFormatClaude, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n", &dto.Usage{}, true, false},
		{"claude stream envelope only", types.RelayFormatClaude, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n", &dto.Usage{}, true, true},
		{"gemini part", types.RelayFormatGemini, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`, &dto.Usage{}, false, false},
		{"gemini empty parts", types.RelayFormatGemini, `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`, &dto.Usage{CompletionTokens: 1}, false, true},
		{"responses message", types.RelayFormatOpenAIResponses, `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, &dto.Usage{}, false, false},
		{"responses reasoning only", types.RelayFormatOpenAIResponses, `{"output":[{"type":"reasoning","encrypted_content":"abc"}]}`, &dto.Usage{}, false, false},
		{"responses empty output", types.RelayFormatOpenAIResponses, `{"output":[],"usage":{"output_tokens":2}}`, &dto.Usage{CompletionTokens: 2}, false, true},
		{"responses stream text delta", types.RelayFormatOpenAIResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n", &dto.Usage{}, true, false},
		{"embedding is not judged", types.RelayFormatEmbedding, `{"data":[]}`, &dto.Usage{}, false, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireTestOutput(tc.format, []byte(tc.body), tc.usage, tc.stream)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有通道测试任务正在运行或等待中")
}
