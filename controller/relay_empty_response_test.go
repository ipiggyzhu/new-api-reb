package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayUpstreamOutputValidation(t *testing.T) {
	const modelName = "gpt-4o"
	const initialQuota = 100_000
	const channelID = 71
	cases := []struct {
		name         string
		upstreamBody string
		responses    bool
		stream       bool
		toolOutput   bool
	}{
		{name: "chat null", upstreamBody: `null`},
		{
			name: "chat empty choices with positive usage",
			upstreamBody: `{"id":"chatcmpl-empty","object":"chat.completion","model":"gpt-4o","choices":[],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
		{name: "chat empty SSE", stream: true},
		{name: "chat only DONE SSE", stream: true, upstreamBody: "data: [DONE]\n\n"},
		{
			name: "chat only usage SSE", stream: true,
			upstreamBody: "data: " + `{"id":"chatcmpl-empty","object":"chat.completion.chunk","model":"gpt-4o","choices":[],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\ndata: [DONE]\n\n",
		},
		{
			name: "responses completed empty output with positive usage", responses: true,
			upstreamBody: `{"id":"resp_empty","object":"response","model":"gpt-4o","status":"completed","output":[],` +
				`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
		},
		{
			name: "responses completed empty output SSE with positive usage", responses: true, stream: true,
			upstreamBody: "event: response.completed\ndata: " +
				`{"type":"response.completed","response":{"id":"resp_empty","object":"response","model":"gpt-4o","status":"completed","output":[],` +
				`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n",
		},
		{
			name: "chat tool output without usage", toolOutput: true,
			upstreamBody: `{"id":"chatcmpl-tool","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,` +
				`"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_weather","type":"function",` +
				`"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			originalDB, originalLogDB := model.DB, model.LOG_DB
			originalMode := gin.Mode()
			originalRedis, originalCache := common.RedisEnabled, common.MemoryCacheEnabled
			originalBatch, originalDisable := common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled
			originalRetry, originalCount := common.RetryTimes, constant.CountToken
			originalConsumeLog, originalPreConsume := common.LogConsumeEnabled, common.PreConsumedQuota
			originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
			originalScores := operation_setting.GetChannelDynamicScoreSetting()
			originalPing := operation_setting.GetGeneralSetting().PingIntervalEnabled
			originalRatios := ratio_setting.ModelRatio2JSONString()
			t.Cleanup(func() {
				model.DB, model.LOG_DB = originalDB, originalLogDB
				gin.SetMode(originalMode)
				common.RedisEnabled, common.MemoryCacheEnabled = originalRedis, originalCache
				common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = originalBatch, originalDisable
				common.RetryTimes, constant.CountToken = originalRetry, originalCount
				common.LogConsumeEnabled, common.PreConsumedQuota = originalConsumeLog, originalPreConsume
				common.SetDatabaseTypes(originalMainType, originalLogType)
				operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
				operation_setting.GetGeneralSetting().PingIntervalEnabled = originalPing
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalRatios))
			})
			common.MemoryCacheEnabled = true
			common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = false, false
			common.RetryTimes, constant.CountToken = 3, false
			common.LogConsumeEnabled, common.PreConsumedQuota = true, 500
			operation_setting.GetGeneralSetting().PingIntervalEnabled = false
			scores := originalScores
			scores.Enabled = false
			operation_setting.SetChannelDynamicScoreSettingForTest(scores)
			ratios := ratio_setting.GetModelRatioCopy()
			ratios[modelName] = 1
			ratioJSON, err := common.Marshal(ratios)
			require.NoError(t, err)
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(ratioJSON)))
			require.NoError(t, i18n.Init())
			service.InitHttpClient()

			initModelListColumnNames(t)
			db := setupErrorLogTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Token{}))
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() {
				require.NoError(t, db.Where("1 = 1").Delete(&model.Channel{}).Error)
				model.InitChannelCache()
			})
			user := model.User{Id: 73, Username: "relay-output-user", Group: "default", Quota: initialQuota, Status: common.UserStatusEnabled}
			token := model.Token{Id: 79, UserId: user.Id, Key: "local-output-test-token", Name: "relay-output-token", RemainQuota: initialQuota}
			require.NoError(t, db.Create(&user).Error)
			require.NoError(t, db.Create(&token).Error)

			type reservation struct {
				walletQuota, tokenQuota int
				walletErr, tokenErr     error
				inFlight                int
				path                    string
			}
			reservations := make(chan reservation, common.RetryTimes+1)
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				var currentUser model.User
				var currentToken model.Token
				walletErr := db.First(&currentUser, user.Id).Error
				tokenErr := db.First(&currentToken, token.Id).Error
				reservations <- reservation{
					walletQuota: currentUser.Quota, tokenQuota: currentToken.RemainQuota,
					walletErr: walletErr, tokenErr: tokenErr, inFlight: model.ChannelInFlight(channelID), path: r.URL.Path,
				}
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.upstreamBody)
			}))
			t.Cleanup(upstream.Close)
			channel := model.Channel{
				Id: channelID, Type: constant.ChannelTypeOpenAI, Name: "output-upstream", Key: "local-output-upstream-key",
				Status: common.ChannelStatusEnabled, Models: modelName, Group: "default",
				BaseURL: &upstream.URL, Priority: common.GetPointer(int64(5)), Weight: common.GetPointer(uint(10)),
				MaxConcurrency: common.GetPointer(1),
			}
			require.NoError(t, db.Create(&channel).Error)
			model.InitChannelCache()

			path := "/v1/chat/completions"
			format := types.RelayFormatOpenAI
			requestBody := map[string]any{
				"model": modelName, "max_tokens": 16, "stream": tc.stream,
				"messages": []map[string]string{{"role": "user", "content": "hello"}},
			}
			if tc.responses {
				path, format = "/v1/responses", types.RelayFormatOpenAIResponses
				requestBody = map[string]any{"model": modelName, "input": "hello", "max_output_tokens": 16, "stream": tc.stream}
			} else if tc.stream {
				requestBody["stream_options"] = map[string]bool{"include_usage": true}
			} else if tc.toolOutput {
				requestBody["tools"] = []map[string]any{{
					"type": "function", "function": map[string]any{
						"name": "get_weather", "parameters": map[string]any{
							"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}},
						},
					},
				}}
			}
			router := gin.New()
			router.POST(path, func(c *gin.Context) {
				// Only authentication is supplied by the fixture. Selection, upstream
				// I/O, quota reservation, settlement/refund and logging are real.
				common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
				common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
				common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
				c.Set("token_name", token.Name)
				c.Set("token_quota", initialQuota)
				c.Set(common.RequestIdKey, "relay-output-test")
				c.Next()
			}, middleware.Distribute(), func(c *gin.Context) { Relay(c, format) })
			bodyJSON, err := common.Marshal(requestBody)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(bodyJSON)))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			// Refunds enqueue nested cache updates. Drain them before any earlier
			// cleanup closes the DB or restores global settings, including on failure.
			t.Cleanup(func() {
				require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 },
					2*time.Second, time.Millisecond, "relay background tasks must finish before fixture cleanup")
			})
			require.NotPanics(t, func() { router.ServeHTTP(recorder, request) })

			assert.Equal(t, int32(1), upstreamCalls.Load(), "an exhausted channel pool must not resend the empty response")
			select {
			case reserved := <-reservations:
				require.NoError(t, reserved.walletErr)
				require.NoError(t, reserved.tokenErr)
				assert.Less(t, reserved.walletQuota, initialQuota, "the upstream must observe a real wallet reservation")
				assert.Equal(t, reserved.walletQuota, reserved.tokenQuota, "the token must reserve the same quota")
				assert.Equal(t, 1, reserved.inFlight, "the upstream attempt must hold the channel slot")
				assert.Equal(t, path, reserved.path)
			default:
				t.Fatal("the upstream did not observe its quota reservation")
			}
			assert.Zero(t, model.ChannelInFlight(channelID), "the request must release its concurrency slot")

			if !tc.toolOutput {
				assert.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
				assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
				assert.NotContains(t, recorder.Body.String(), "[DONE]", "an empty attempt must not terminate successfully before its error")
				assert.NotContains(t, recorder.Body.String(), "response.completed", "empty output must not be announced as completed")
				var body struct {
					Error types.OpenAIError `json:"error"`
				}
				assert.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
				assert.Equal(t, string(types.ErrorCodeEmptyResponse), body.Error.Code)
				assert.NotContains(t, body.Error.Message, "no channel found")
			} else {
				assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var body dto.OpenAITextResponse
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
				require.Len(t, body.Choices, 1)
				assert.Equal(t, "tool_calls", body.Choices[0].FinishReason)
				assert.JSONEq(t, `[{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]`,
					string(body.Choices[0].Message.ToolCalls))
				assert.NotContains(t, recorder.Body.String(), string(types.ErrorCodeEmptyResponse))
			}

			// Wait for observable accounting/logging to settle before inspecting the
			// completed request. Failure cases require a full refund, even when the
			// upstream advertised positive usage for an empty output.
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				var currentUser model.User
				var currentToken model.Token
				var logs []model.Log
				if !assert.NoError(collect, db.First(&currentUser, user.Id).Error) ||
					!assert.NoError(collect, db.First(&currentToken, token.Id).Error) ||
					!assert.NoError(collect, db.Find(&logs).Error) {
					return
				}
				if !assert.Len(collect, logs, 1, "only the actual upstream attempt may create a log") {
					return
				}
				entry := logs[0]
				assert.Equal(collect, channelID, entry.ChannelId)
				assert.Equal(collect, user.Id, entry.UserId)
				assert.Equal(collect, token.Id, entry.TokenId)
				if !tc.toolOutput {
					assert.Equal(collect, model.LogTypeError, entry.Type, "empty output must never create a consume log")
					assert.Zero(collect, entry.Quota)
					assert.Equal(collect, initialQuota, currentUser.Quota)
					assert.Zero(collect, currentUser.UsedQuota)
					assert.Zero(collect, currentUser.RequestCount)
					assert.Equal(collect, initialQuota, currentToken.RemainQuota)
					assert.Zero(collect, currentToken.UsedQuota)
					other, err := common.StrToMap(entry.Other)
					if assert.NoError(collect, err) {
						assert.Equal(collect, string(types.ErrorCodeEmptyResponse), other["error_code"])
						assert.EqualValues(collect, http.StatusBadGateway, other["status_code"])
					}
				} else {
					assert.Equal(collect, model.LogTypeConsume, entry.Type, "a tool output remains a successful request without usage")
					assert.GreaterOrEqual(collect, entry.Quota, 0)
					assert.Equal(collect, initialQuota-entry.Quota, currentUser.Quota)
					assert.Equal(collect, entry.Quota, currentUser.UsedQuota)
					assert.Equal(collect, initialQuota-entry.Quota, currentToken.RemainQuota)
					assert.Equal(collect, entry.Quota, currentToken.UsedQuota)
				}
			}, 2*time.Second, time.Millisecond, "the request must settle both balances and log its actual outcome")
		})
	}
}
