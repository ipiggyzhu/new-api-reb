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

func TestRelayRetryExhaustionPreservesUpstreamError(t *testing.T) {
	const modelName = "claude-fable-5-1"
	const initialQuota = 100_000
	cases := []struct {
		name              string
		upstreamStatus    int
		channelCount      int
		saturateFallback  bool
		wantStatus        int
		wantType          string
		wantMessage       string
		wantUpstreamCalls int32
	}{
		{
			name: "only channel returns 503", upstreamStatus: http.StatusServiceUnavailable,
			channelCount: 1, wantStatus: http.StatusServiceUnavailable,
			wantType: "upstream_unavailable", wantMessage: "Service Unavailable", wantUpstreamCalls: 1,
		},
		{
			name: "only channel returns 520", upstreamStatus: 520,
			channelCount: 1, wantStatus: 520,
			wantType: "upstream_unavailable", wantMessage: "Service Unavailable", wantUpstreamCalls: 1,
		},
		{
			name: "no channel before the first attempt", upstreamStatus: http.StatusServiceUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantType: "new_api_error",
		},
		{
			name: "remaining channel is saturated", upstreamStatus: http.StatusServiceUnavailable,
			channelCount: 2, saturateFallback: true, wantStatus: http.StatusInternalServerError,
			wantType: "new_api_error", wantMessage: "渠道均已达到并发上限（retry）", wantUpstreamCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			originalDB, originalLogDB := model.DB, model.LOG_DB
			originalMode := gin.Mode()
			originalRedis, originalCache := common.RedisEnabled, common.MemoryCacheEnabled
			originalBatch, originalDisable := common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled
			originalRetry, originalCount := common.RetryTimes, constant.CountToken
			originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
			originalScores := operation_setting.GetChannelDynamicScoreSetting()
			originalRatios := ratio_setting.ModelRatio2JSONString()
			t.Cleanup(func() {
				model.DB, model.LOG_DB = originalDB, originalLogDB
				gin.SetMode(originalMode)
				common.RedisEnabled, common.MemoryCacheEnabled = originalRedis, originalCache
				common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = originalBatch, originalDisable
				common.RetryTimes, constant.CountToken = originalRetry, originalCount
				common.SetDatabaseTypes(originalMainType, originalLogType)
				operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalRatios))
			})
			common.MemoryCacheEnabled = true
			common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = false, false
			common.RetryTimes, constant.CountToken = 3, false
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
			user := model.User{Id: 7, Username: "relay-retry-user", Group: "default", Quota: initialQuota, Status: common.UserStatusEnabled}
			token := model.Token{Id: 9, UserId: user.Id, Key: "local-retry-test-token", Name: "relay-retry-token", RemainQuota: initialQuota}
			require.NoError(t, db.Create(&user).Error)
			require.NoError(t, db.Create(&token).Error)

			type reservation struct {
				walletQuota, tokenQuota int
				walletErr, tokenErr     error
			}
			reservations := make(chan reservation, common.RetryTimes+1)
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				var currentUser model.User
				var currentToken model.Token
				walletErr := db.First(&currentUser, user.Id).Error
				tokenErr := db.First(&currentToken, token.Id).Error
				reservations <- reservation{currentUser.Quota, currentToken.RemainQuota, walletErr, tokenErr}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"Service Unavailable","type":"upstream_error","code":"upstream_unavailable"}}`)
			}))
			t.Cleanup(upstream.Close)
			for id := 1; id <= tc.channelCount; id++ {
				channel := model.Channel{
					Id: id, Type: constant.ChannelTypeAnthropic, Name: "retry-upstream", Key: "local-upstream-test-key",
					Status: common.ChannelStatusEnabled, Models: modelName, Group: "default",
					BaseURL: &upstream.URL, Priority: common.GetPointer(int64(5)), Weight: common.GetPointer(uint(10)),
				}
				if id == 2 && tc.saturateFallback {
					channel.MaxConcurrency = common.GetPointer(1)
					require.True(t, model.AcquireChannelSlot(id, 1))
					t.Cleanup(func() { model.ReleaseChannelSlot(2) })
				}
				require.NoError(t, db.Create(&channel).Error)
			}
			model.InitChannelCache()

			router := gin.New()
			router.POST("/v1/messages", func(c *gin.Context) {
				// Supply the identity normally established by TokenAuth; routing,
				// upstream I/O, quota reservation and refunds remain the real path.
				common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
				common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
				common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
				c.Set("token_name", token.Name)
				c.Set("token_quota", initialQuota)
				c.Set(common.RequestIdKey, "relay-retry-test")
				c.Next()
			}, middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatClaude) })
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
				`{"model":"claude-fable-5-1","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept-Language", "zh")
			request.Header.Set("anthropic-beta", "context-1m-2025-08-07")
			recorder := httptest.NewRecorder()
			// Wallet refunds enqueue a nested cache update. Balances can already
			// be restored while that task still reads the Redis setting, so this
			// must run before earlier cleanups close the DB or restore globals.
			t.Cleanup(func() {
				require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 },
					2*time.Second, time.Millisecond, "relay background tasks must finish before fixture cleanup")
			})
			router.ServeHTTP(recorder, request)

			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
			var body struct {
				Type  string            `json:"type"`
				Error types.OpenAIError `json:"error"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
			assert.Equal(t, tc.wantType, body.Error.Type)
			assert.Equal(t, tc.wantUpstreamCalls, upstreamCalls.Load(), "channel exhaustion must not resend upstream")
			if tc.wantUpstreamCalls == 0 {
				assert.Equal(t, string(types.ErrorCodeModelNotFound), body.Error.Code)
				assert.Contains(t, body.Error.Message, modelName)
			} else {
				assert.Equal(t, "error", body.Type, "Claude clients require the error envelope")
				assert.Contains(t, body.Error.Message, tc.wantMessage)
				assert.NotContains(t, body.Error.Message, "no channel found")
				select {
				case reserved := <-reservations:
					require.NoError(t, reserved.walletErr)
					require.NoError(t, reserved.tokenErr)
					assert.Less(t, reserved.walletQuota, initialQuota, "the request must exercise real pre-consumption")
					assert.Equal(t, reserved.walletQuota, reserved.tokenQuota)
				default:
					t.Fatal("the upstream attempt did not observe its quota reservation")
				}
			}
			// Refund runs asynchronously; await the observable account balances,
			// not an implementation-specific goroutine or a fixed sleep.
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				var currentUser model.User
				var currentToken model.Token
				if !assert.NoError(collect, db.First(&currentUser, user.Id).Error) ||
					!assert.NoError(collect, db.First(&currentToken, token.Id).Error) {
					return
				}
				assert.Equal(collect, initialQuota, currentUser.Quota)
				assert.Zero(collect, currentUser.UsedQuota)
				assert.Equal(collect, initialQuota, currentToken.RemainQuota)
				assert.Zero(collect, currentToken.UsedQuota)
			}, 2*time.Second, time.Millisecond, "failed requests must refund both balances without a second charge")
			var logs []model.Log
			require.NoError(t, db.Find(&logs).Error)
			require.Len(t, logs, int(tc.wantUpstreamCalls), "only actual upstream failures create attempt logs; none may be consume logs")
			for _, entry := range logs {
				assert.Equal(t, model.LogTypeError, entry.Type)
				assert.Zero(t, entry.Quota)
				assert.Equal(t, 1, entry.ChannelId)
				other, err := common.StrToMap(entry.Other)
				require.NoError(t, err)
				assert.EqualValues(t, tc.upstreamStatus, other["status_code"])
			}
			assert.Zero(t, model.ChannelInFlight(1), "the failed request must release its concurrency slot")
		})
	}
}
