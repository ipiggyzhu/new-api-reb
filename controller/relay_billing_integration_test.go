package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupCrossGroupBillingTest(t *testing.T, task bool, quota int, firstRatio float64, upstream http.Handler, billingPreference ...string) (*gin.Engine, *gorm.DB) {
	t.Helper()
	_, db := setupRelayTaskRoutingTest(t, upstream, 0.01, 1)
	originalGroups := setting.AutoGroups2JsonString()
	originalUsable := setting.UserUsableGroups2JSONString()
	originalRatios := ratio_setting.GroupRatio2JSONString()
	originalSpecial := ratio_setting.GroupGroupRatio2JSONString()
	originalCount := constant.CountToken
	originalPing := operation_setting.GetGeneralSetting().PingIntervalEnabled
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsable))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalRatios))
		require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(originalSpecial))
		constant.CountToken = originalCount
		operation_setting.GetGeneralSetting().PingIntervalEnabled = originalPing
	})
	constant.CountToken = false
	operation_setting.GetGeneralSetting().PingIntervalEnabled = false
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["default","vip"]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"default","vip":"vip"}`))
	ratios, err := common.Marshal(map[string]float64{"default": firstRatio, "vip": 3})
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(ratios)))
	require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(`{}`))
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 991).Update("quota", quota).Error)
	tokenQuota := max(quota, 100_000)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", 992).Update("remain_quota", tokenQuota).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 81).Update("key", "cross-group-first").Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 82).Updates(map[string]any{"group": "vip", "key": "cross-group-second"}).Error)
	model.InitChannelCache()

	preference := "wallet_only"
	if len(billingPreference) > 0 {
		preference = billingPreference[0]
	}
	router := gin.New()
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	router.Use(func(c *gin.Context) {
		defer common.CleanupBodyStorage(c)
		common.SetContextKey(c, constant.ContextKeyUserId, 991)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
		common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, true)
		common.SetContextKey(c, constant.ContextKeyTokenId, 992)
		common.SetContextKey(c, constant.ContextKeyTokenKey, "local-task-routing-token")
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: preference, QuotaWarningThreshold: -1_000_000_000})
		c.Set("token_name", "cross-group-token")
		c.Set("token_quota", tokenQuota)
		c.Set(common.RequestIdKey, "cross-group-test")
		c.Next()
	})
	if task {
		router.POST("/suno/submit/:action", middleware.Distribute(), RelayTask)
	} else {
		router.POST("/v1/chat/completions", middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	}
	return router, db
}

func crossGroupRequest(task bool) *http.Request {
	if task {
		return taskRoutingRequest(`{"prompt":"hello"}`)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"suno_lyrics","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestRelayCrossGroupRetryReservesBeforeMoreExpensiveUpstream(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "task"}[task], func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			router, db := setupCrossGroupBillingTest(t, task, 10_000, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer cross-group-first" {
					firstCalls.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "first group unavailable")
					return
				}
				secondCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if task {
					_, _ = io.WriteString(w, `{"code":"success","data":"paid-task"}`)
				} else {
					_, _ = io.WriteString(w, `{"id":"chatcmpl-local","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
				}
			}))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, crossGroupRequest(task))
			assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			assert.Equal(t, int32(1), firstCalls.Load())
			assert.Zero(t, secondCalls.Load(), "the more expensive group must be reserved before contacting it")
			var user model.User
			var token model.Token
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				require.NoError(collect, db.First(&user, 991).Error)
				require.NoError(collect, db.First(&token, 992).Error)
				assert.Equal(collect, 10_000, user.Quota)
				assert.Equal(collect, 100_000, token.RemainQuota)
			}, 2*time.Second, time.Millisecond)
			assert.Zero(t, user.UsedQuota)
			assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{ChannelID: 82}).Rows)
		})
	}
}

func TestRelayCrossGroupTieredBillingUsesServingGroupAndFrozenExpression(t *testing.T) {
	for _, tc := range []struct {
		name        string
		firstRatio  float64
		noMaxTokens bool
		reserved    int64
	}{
		{name: "free to paid", firstRatio: 0, reserved: 48},
		{name: "paid to expensive", firstRatio: 1, reserved: 48},
		{name: "free to paid without max tokens", firstRatio: 0, noMaxTokens: true, reserved: 49_152},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var db *gorm.DB
			var secondBalance atomic.Int64
			var calls atomic.Int32
			router, db := setupCrossGroupBillingTest(t, false, 100_000, tc.firstRatio, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") == "Bearer cross-group-first" {
					// Mid-request settings edits must not replace the frozen expression.
					if err := config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_expr": `{"suno_lyrics":"p * 200 + c * 400"}`}); err != nil {
						t.Error(err)
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "first group unavailable")
					return
				}
				var user model.User
				if err := db.First(&user, 991).Error; err != nil {
					t.Error(err)
				}
				secondBalance.Store(int64(user.Quota))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl-local","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			}))
			billingConfig := config.GlobalConfig.Get("billing_setting")
			saved, err := config.ConfigToMap(billingConfig)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, config.UpdateConfigFromMap(billingConfig, saved)) })
			require.NoError(t, config.UpdateConfigFromMap(billingConfig, map[string]string{
				"billing_mode": `{"suno_lyrics":"tiered_expr"}`,
				"billing_expr": `{"suno_lyrics":"p * 2 + c * 4"}`,
			}))
			recorder := httptest.NewRecorder()
			request := crossGroupRequest(false)
			if tc.noMaxTokens {
				request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"suno_lyrics","messages":[{"role":"user","content":"hello"}]}`))
				request.Header.Set("Content-Type", "application/json")
			}
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.Equal(t, int32(2), calls.Load())
			// The paid group must reserve completion cost even when the first group was free.
			assert.Equal(t, 100_000-tc.reserved, secondBalance.Load())
			var user model.User
			var token model.Token
			require.NoError(t, db.First(&user, 991).Error)
			require.NoError(t, db.First(&token, 992).Error)
			// Actual (10*2+5*4)/1M*500K*3 = 60, not the first group's cost.
			assert.Equal(t, 99_940, user.Quota)
			assert.Equal(t, 99_940, token.RemainQuota)
			assert.Equal(t, 60, user.UsedQuota)
		})
	}
}

func TestRelayCrossGroupTrustedRequestChecksHigherBudget(t *testing.T) {
	for _, tc := range []struct {
		name            string
		vipRatio        float64
		wantStatus      int
		wantSecondCalls int32
		wantFinalQuota  int
	}{
		{name: "reserve increased budget", vipRatio: 3, wantStatus: http.StatusOK, wantSecondCalls: 1, wantFinalQuota: 5_985_000},
		{name: "reject unaffordable budget", vipRatio: 2000, wantStatus: http.StatusForbidden, wantSecondCalls: 0, wantFinalQuota: 6_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var db *gorm.DB
			var firstBalance, secondBalance atomic.Int64
			var secondCalls atomic.Int32
			router, db := setupCrossGroupBillingTest(t, false, 6_000_000, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var user model.User
				if err := db.First(&user, 991).Error; err != nil {
					t.Error(err)
				}
				if r.Header.Get("Authorization") == "Bearer cross-group-first" {
					firstBalance.Store(int64(user.Quota))
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "first group unavailable")
					return
				}
				secondCalls.Add(1)
				secondBalance.Store(int64(user.Quota))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl-local","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			}))
			ratios, err := common.Marshal(map[string]float64{"default": 1, "vip": tc.vipRatio})
			require.NoError(t, err)
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(ratios)))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, crossGroupRequest(false))
			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			assert.Equal(t, int64(6_000_000), firstBalance.Load(), "the original affordable budget still uses the trust optimization")
			assert.Equal(t, tc.wantSecondCalls, secondCalls.Load())
			if tc.wantSecondCalls != 0 {
				assert.Equal(t, int64(tc.wantFinalQuota), secondBalance.Load(), "the increased budget must be reserved before upstream work")
			}
			var user model.User
			var token model.Token
			require.NoError(t, db.First(&user, 991).Error)
			require.NoError(t, db.First(&token, 992).Error)
			assert.Equal(t, tc.wantFinalQuota, user.Quota)
			assert.Equal(t, tc.wantFinalQuota, token.RemainQuota)
		})
	}
}

func TestRelayCrossGroupReserveRollsBackFundingAndTokenTogether(t *testing.T) {
	cases := []struct {
		name, preference string
		tokenQuota       int
		secondStatus     int
		wantStatus       int
	}{
		{"wallet final upstream failure", "wallet_only", 100_000, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"wallet token insufficient", "wallet_only", 10_000, http.StatusOK, http.StatusForbidden},
		{"subscription success", "subscription_only", 100_000, http.StatusOK, http.StatusOK},
		{"subscription final upstream failure", "subscription_only", 100_000, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"subscription token insufficient", "subscription_only", 10_000, http.StatusOK, http.StatusForbidden},
	}
	for _, task := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(map[bool]string{false: "chat", true: "task"}[task]+"/"+tc.name, func(t *testing.T) {
				var db *gorm.DB
				var secondCalls atomic.Int32
				var secondFunding, secondToken atomic.Int64
				router, db := setupCrossGroupBillingTest(t, task, 100_000, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") == "Bearer cross-group-first" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					secondCalls.Add(1)
					if tc.preference == "subscription_only" {
						var sub model.UserSubscription
						if err := db.First(&sub, 994).Error; err != nil {
							t.Error(err)
						}
						secondFunding.Store(sub.AmountUsed)
					} else {
						var user model.User
						if err := db.First(&user, 991).Error; err != nil {
							t.Error(err)
						}
						secondFunding.Store(int64(100_000 - user.Quota))
					}
					var token model.Token
					if err := db.First(&token, 992).Error; err != nil {
						t.Error(err)
					}
					secondToken.Store(int64(token.RemainQuota))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.secondStatus)
					if task {
						_, _ = io.WriteString(w, "{\"code\":\"success\",\"data\":\"reserved-task\"}")
					} else {
						_, _ = io.WriteString(w, "{\"id\":\"chatcmpl-local\",\"object\":\"chat.completion\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}")
					}
				}), tc.preference)
				require.NoError(t, db.Model(&model.Token{}).Where("id = ?", 992).Update("remain_quota", tc.tokenQuota).Error)
				if tc.preference == "subscription_only" {
					require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}))
					plan := model.SubscriptionPlan{Id: 993, Title: "local billing plan", Enabled: true, TotalAmount: 100_000, QuotaResetPeriod: model.SubscriptionResetNever}
					require.NoError(t, db.Create(&plan).Error)
					sub := model.UserSubscription{Id: 994, UserId: 991, PlanId: plan.Id, AmountTotal: 100_000, Status: "active", StartTime: time.Now().Unix() - 60, EndTime: time.Now().Unix() + 3600}
					require.NoError(t, db.Create(&sub).Error)
					model.InvalidateSubscriptionPlanCache(plan.Id)
					t.Cleanup(func() { model.InvalidateSubscriptionPlanCache(plan.Id) })
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, crossGroupRequest(task))
				assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
				if tc.wantStatus == http.StatusForbidden {
					assert.Zero(t, secondCalls.Load(), "insufficient token quota must stop the higher-priced upstream")
				} else {
					assert.Equal(t, int32(1), secondCalls.Load())
					assert.Equal(t, int64(15_000), secondFunding.Load(), "the complete target budget must be reserved before the retry")
					assert.Equal(t, int64(tc.tokenQuota-15_000), secondToken.Load())
				}
				charge := 0
				if tc.wantStatus == http.StatusOK {
					charge = 15_000
				}
				require.EventuallyWithT(t, func(collect *assert.CollectT) {
					var user model.User
					var token model.Token
					require.NoError(collect, db.First(&user, 991).Error)
					require.NoError(collect, db.First(&token, 992).Error)
					wantWallet := 100_000
					if tc.preference == "wallet_only" {
						wantWallet -= charge
					}
					assert.Equal(collect, wantWallet, user.Quota)
					assert.Equal(collect, tc.tokenQuota-charge, token.RemainQuota)
					assert.Equal(collect, charge, user.UsedQuota)
					if tc.preference == "subscription_only" {
						var sub model.UserSubscription
						require.NoError(collect, db.First(&sub, 994).Error)
						assert.Equal(collect, int64(charge), sub.AmountUsed)
						var records []model.SubscriptionPreConsumeRecord
						require.NoError(collect, db.Find(&records).Error)
						require.Len(collect, records, 1, "retries must reuse the subscription reservation")
						wantState := "refunded"
						if charge > 0 {
							wantState = "consumed"
						}
						assert.Equal(collect, wantState, records[0].Status)
					}
				}, 3*time.Second, time.Millisecond)
			})
		}
	}
}

func TestRelayCrossGroupRetryBudgetIsFinitePerGroup(t *testing.T) {
	var calls sync.Map
	router, db := setupCrossGroupBillingTest(t, false, 100_000, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		counter, _ := calls.LoadOrStore(key, new(atomic.Int32))
		counter.(*atomic.Int32).Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "fixture unavailable")
	}))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString("[\"default\",\"vip\",\"extra\"]"))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString("{\"default\":\"default\",\"vip\":\"vip\",\"extra\":\"extra\"}"))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString("{\"default\":1,\"vip\":1,\"extra\":1}"))
	prices := ratio_setting.GetModelPriceCopy()
	prices[taskRoutingModel] = 0
	priceJSON, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceJSON)))
	var template model.Channel
	require.NoError(t, db.First(&template, 81).Error)
	groups := []string{"default", "vip", "extra"}
	for groupIndex, group := range groups {
		for offset := 0; offset < 3; offset++ {
			channel := template
			channel.Id = 81 + groupIndex*3 + offset
			channel.Group = group
			channel.Key = fmt.Sprintf("%s:%d", group, channel.Id)
			require.NoError(t, db.Save(&channel).Error)
		}
	}
	model.InitChannelCache()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, crossGroupRequest(false))
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	groupCalls := map[string]int{}
	calls.Range(func(key, value any) bool {
		assert.Equal(t, int32(1), value.(*atomic.Int32).Load(), "a failed channel must not be retried within the request")
		group, _, _ := strings.Cut(key.(string), ":")
		groupCalls[group]++
		return true
	})
	assert.Equal(t, map[string]int{"default": 2, "vip": 2, "extra": 2}, groupCalls,
		"RetryTimes=1 permits two attempts in each group, not a global budget or an unbounded reset")
	for id := 81; id < 90; id++ {
		assert.Zero(t, model.ChannelInFlight(id), "every attempted channel must release its slot")
	}
}
