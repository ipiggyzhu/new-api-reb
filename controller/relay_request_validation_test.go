package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayRejectsInvalidRequestsBeforeRouting(t *testing.T) {
	originalScores := operation_setting.GetChannelDynamicScoreSetting()
	originalRedis := common.RedisEnabled
	t.Cleanup(func() {
		channel_score.ResetAll()
		operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
		common.RedisEnabled = originalRedis
	})
	common.RedisEnabled = false
	scores := originalScores
	scores.Enabled = true
	operation_setting.SetChannelDynamicScoreSettingForTest(scores)
	channel_score.ResetAll()

	cases := []struct {
		name       string
		path       string
		format     types.RelayFormat
		body       string
		bodyLimit  int64
		wantStatus int
	}{
		{
			name: "missing model", path: "/v1/chat/completions", format: types.RelayFormatOpenAI,
			body: `{"messages":[{"role":"user","content":"hello"}]}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "malformed JSON", path: "/v1/chat/completions", format: types.RelayFormatOpenAI,
			body: `{"model":`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid field type", path: "/v1/chat/completions", format: types.RelayFormatOpenAI,
			body: `{"model":"gpt-4o","messages":"not an array"}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "unbounded max tokens", path: "/v1/chat/completions", format: types.RelayFormatOpenAI,
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"max_tokens":18446744073686646784}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "wrapped validation error", path: "/v1/rerank", format: types.RelayFormatRerank,
			body: `{"model":"rerank-model","documents":["hello"]}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "Claude missing messages", path: "/v1/messages", format: types.RelayFormatClaude,
			body: `{"model":"claude-fable-5-1","max_tokens":1}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "Responses missing input", path: "/v1/responses", format: types.RelayFormatOpenAIResponses,
			body: `{"model":"gpt-4o"}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "oversized body", path: "/v1/chat/completions", format: types.RelayFormatOpenAI,
			body: `{"model":"gpt-4o"}`, bodyLimit: 1, wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newRelayErrorContext(t, tc.path)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.bodyLimit > 0 {
				c.Request.Body = http.MaxBytesReader(recorder, c.Request.Body, tc.bodyLimit)
			}
			t.Cleanup(func() { common.CleanupBodyStorage(c) })

			Relay(c, tc.format)

			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			var response map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Contains(t, response, "error")
			assert.Empty(t, c.GetStringSlice("use_channel"), "invalid client DTOs must not reach an upstream attempt")
			assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{}).Rows,
				"invalid client DTOs must not produce channel score samples")
		})
	}
}
