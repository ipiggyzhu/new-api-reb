package controller

import (
	"context"
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
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const taskRoutingModel = "suno_lyrics"
const taskRoutingPath = "/suno/submit/lyrics"

func setupRelayTaskRoutingTest(t *testing.T, upstream http.Handler, price float64, retryTimes int) (*gin.Engine, *gorm.DB) {
	t.Helper()
	originalMode := gin.Mode()
	originalRedis, originalCache := common.RedisEnabled, common.MemoryCacheEnabled
	originalBatch, originalDisable := common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled
	originalRetry, originalConsumeLog := common.RetryTimes, common.LogConsumeEnabled
	originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
	originalScores := operation_setting.GetChannelDynamicScoreSetting()
	originalPrices := ratio_setting.ModelPrice2JSONString()
	quotaSetting := operation_setting.GetQuotaSetting()
	originalFreePreConsume := quotaSetting.EnableFreeModelPreConsume
	affinity := operation_setting.GetChannelAffinitySetting()
	originalAffinity := *affinity
	t.Cleanup(func() {
		service.ClearChannelAffinityCacheAll()
		channel_score.ResetAll()
		gin.SetMode(originalMode)
		common.RedisEnabled, common.MemoryCacheEnabled = originalRedis, originalCache
		common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = originalBatch, originalDisable
		common.RetryTimes, common.LogConsumeEnabled = originalRetry, originalConsumeLog
		common.SetDatabaseTypes(originalMainType, originalLogType)
		operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
		quotaSetting.EnableFreeModelPreConsume = originalFreePreConsume
		*affinity = originalAffinity
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(originalPrices))
	})
	common.RedisEnabled, common.MemoryCacheEnabled = false, true
	common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = false, false
	common.RetryTimes, common.LogConsumeEnabled = retryTimes, false
	quotaSetting.EnableFreeModelPreConsume = false
	scores := originalScores
	scores.Enabled, scores.FaultsToDemote = true, 1
	operation_setting.SetChannelDynamicScoreSettingForTest(scores)
	channel_score.ResetAll()
	*affinity = operation_setting.ChannelAffinitySetting{
		Enabled: true, SwitchOnSuccess: true, MaxEntries: 100, DefaultTTLSeconds: 120,
		Rules: []operation_setting.ChannelAffinityRule{{
			Name: "task routing", ModelRegex: []string{"^suno_lyrics$"}, PathRegex: []string{"^/suno/submit/lyrics$"},
			KeySources:        []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Task-Session"}},
			IncludeUsingGroup: true, IncludeModelName: true, IncludeRuleName: true,
		}},
	}
	service.ClearChannelAffinityCacheAll()
	prices := ratio_setting.GetModelPriceCopy()
	prices[taskRoutingModel] = price
	priceJSON, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceJSON)))
	require.NoError(t, i18n.Init())
	service.InitHttpClient()

	initModelListColumnNames(t)
	db := setupErrorLogTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Token{}, &model.Task{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, db.Where("1 = 1").Delete(&model.Channel{}).Error)
		model.InitChannelCache()
	})
	user := model.User{Id: 991, Username: "task-routing-user", Group: "default", Status: common.UserStatusEnabled}
	token := model.Token{Id: 992, UserId: user.Id, Key: "local-task-routing-token", Name: "task-routing-token", RemainQuota: 100_000}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&token).Error)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	for _, id := range []int{81, 82} {
		channel := model.Channel{
			Id: id, Type: constant.ChannelTypeOpenAI, Name: "task-routing-upstream", Key: "local-task-routing-key",
			Status: common.ChannelStatusEnabled, Models: taskRoutingModel, Group: "default",
			BaseURL: &server.URL, Priority: common.GetPointer(int64(5)), Weight: common.GetPointer(uint(10)),
		}
		require.NoError(t, db.Create(&channel).Error)
	}
	model.InitChannelCache()

	router := gin.New()
	router.POST("/suno/submit/:action", func(c *gin.Context) {
		defer common.CleanupBodyStorage(c)
		common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
		common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
		c.Set("token_name", token.Name)
		c.Set("token_quota", token.RemainQuota)
		c.Set(common.RequestIdKey, "task-routing-test")
		c.Next()
	}, middleware.Distribute(), RelayTask)
	t.Cleanup(func() {
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 },
			2*time.Second, time.Millisecond, "task routing reports must finish before fixture cleanup")
	})
	return router, db
}

func taskRoutingRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, taskRoutingPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Task-Session", "task-routing-session")
	return request
}

func taskRoutingAffinityContext(t *testing.T) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = taskRoutingRequest(`{"prompt":"hello"}`)
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	return c
}

func seedTaskRoutingAffinity(t *testing.T) {
	t.Helper()
	c := taskRoutingAffinityContext(t)
	_, found := service.GetPreferredChannelByAffinity(c, taskRoutingModel, "default")
	require.False(t, found)
	service.SetChannelAffinityRelayOutcome(c, true)
	service.RecordChannelAffinity(c, 81)
	probe := taskRoutingAffinityContext(t)
	channelID, found := service.GetPreferredChannelByAffinity(probe, taskRoutingModel, "default")
	require.True(t, found, "fixture must start with a usable pin")
	require.Equal(t, 81, channelID)
}

func TestRelayTaskPublishesRoutingOutcomes(t *testing.T) {
	cases := []struct {
		name         string
		requestBody  string
		upstreamBody string
		status       int
		price        float64
		wantStatus   int
		wantCalls    int32
		wantTotal    int64
		wantSuccess  int64
		wantPin      bool
	}{
		{
			name: "success", requestBody: `{"prompt":"hello"}`,
			upstreamBody: `{"code":"success","message":"","data":"upstream-task"}`, status: http.StatusOK,
			wantStatus: http.StatusOK, wantCalls: 1, wantTotal: 1, wantSuccess: 1, wantPin: true,
		},
		{
			name: "upstream unavailable", requestBody: `{"prompt":"hello"}`, upstreamBody: "temporarily unavailable",
			status: http.StatusServiceUnavailable, wantStatus: http.StatusServiceUnavailable, wantCalls: 1, wantTotal: 1,
		},
		{
			name: "upstream timeout", requestBody: `{"prompt":"hello"}`, upstreamBody: "upstream timeout",
			status: http.StatusGatewayTimeout, wantStatus: http.StatusGatewayTimeout, wantCalls: 1, wantTotal: 1,
		},
		{
			name: "upstream application failure", requestBody: `{"prompt":"hello"}`,
			upstreamBody: `{"code":"task_unavailable","message":"generation unavailable","data":null}`, status: http.StatusOK,
			wantStatus: http.StatusInternalServerError, wantCalls: 1, wantTotal: 1,
		},
		{
			name: "upstream request rejection", requestBody: `{"prompt":"hello"}`, upstreamBody: "invalid prompt",
			status: http.StatusBadRequest, wantStatus: http.StatusBadRequest, wantCalls: 1, wantPin: true,
		},
		{
			name: "local validation rejection", requestBody: `{}`,
			wantStatus: http.StatusBadRequest, wantPin: true,
		},
		{
			name: "insufficient wallet quota", requestBody: `{"prompt":"hello"}`, price: 0.01,
			wantStatus: http.StatusForbidden, wantPin: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			router, db := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.upstreamBody)
			}), tc.price, 0)
			seedTaskRoutingAffinity(t)
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, taskRoutingRequest(tc.requestBody))

			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			assert.Equal(t, tc.wantCalls, calls.Load())
			require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 2*time.Second, time.Millisecond)
			snapshot := channel_score.Snapshot(channel_score.ScoreFilter{ChannelID: 81, Group: "default", Model: taskRoutingModel})
			if tc.wantTotal == 0 {
				assert.Empty(t, snapshot.Rows, "client and billing errors must not affect channel ranking")
			} else if assert.Len(t, snapshot.Rows, 1, "every real upstream attempt must publish an outcome") {
				assert.Equal(t, tc.wantTotal, snapshot.Rows[0].Total)
				assert.Equal(t, tc.wantSuccess, snapshot.Rows[0].Success)
				if tc.wantSuccess == 0 {
					assert.Equal(t, -1, snapshot.Rows[0].TierOffset, "a real upstream fault must demote the channel")
				}
			}
			probe := taskRoutingAffinityContext(t)
			channelID, found := service.GetPreferredChannelByAffinity(probe, taskRoutingModel, "default")
			assert.Equal(t, tc.wantPin, found, "only an upstream channel fault may release the existing pin")
			if tc.wantPin {
				assert.Equal(t, 81, channelID)
			}
			var tasks []model.Task
			require.NoError(t, db.Find(&tasks).Error)
			if tc.wantSuccess > 0 {
				require.Len(t, tasks, 1)
				assert.Equal(t, 81, tasks[0].ChannelId)
				assert.Equal(t, "upstream-task", tasks[0].PrivateData.UpstreamTaskID)
			} else {
				assert.Empty(t, tasks)
			}
		})
	}
}

func TestRelayTaskRetryAttributesEachOutcome(t *testing.T) {
	var calls atomic.Int32
	router, db := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "temporarily unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"success","message":"","data":"fallback-task"}`)
	}), 0, 1)
	seedTaskRoutingAffinity(t)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, taskRoutingRequest(`{"prompt":"hello"}`))

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, int32(2), calls.Load())
	require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 2*time.Second, time.Millisecond)
	faults := channel_score.Snapshot(channel_score.ScoreFilter{ChannelID: 81, Group: "default", Model: taskRoutingModel})
	if assert.Len(t, faults.Rows, 1) {
		assert.Equal(t, int64(1), faults.Rows[0].Total)
		assert.Zero(t, faults.Rows[0].Success)
		assert.Equal(t, -1, faults.Rows[0].TierOffset)
	}
	successes := channel_score.Snapshot(channel_score.ScoreFilter{ChannelID: 82, Group: "default", Model: taskRoutingModel})
	if assert.Len(t, successes.Rows, 1) {
		assert.Equal(t, int64(1), successes.Rows[0].Total)
		assert.Equal(t, int64(1), successes.Rows[0].Success)
	}
	probe := taskRoutingAffinityContext(t)
	channelID, found := service.GetPreferredChannelByAffinity(probe, taskRoutingModel, "default")
	assert.True(t, found)
	assert.Equal(t, 82, channelID, "affinity must follow the channel that actually succeeded")
	var tasks []model.Task
	require.NoError(t, db.Find(&tasks).Error)
	require.Len(t, tasks, 1)
	assert.Equal(t, 82, tasks[0].ChannelId)
}

func TestRelayTaskClientCancellationPreservesChannelHealth(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	router, db := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		<-release
	}), 0, 3)
	originalDisableCodes := operation_setting.AutomaticDisableStatusCodesToString()
	t.Cleanup(func() {
		require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString(originalDisableCodes))
	})
	common.AutomaticDisableChannelEnabled = true
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("500"))
	require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{81, 82}).Update("auto_ban", 1).Error)
	model.InitChannelCache()
	seedTaskRoutingAffinity(t)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, taskRoutingRequest(`{"prompt":"hello"}`).WithContext(ctx))

	assert.Equal(t, int32(1), calls.Load())
	assert.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 2*time.Second, time.Millisecond)
	var channels []model.Channel
	require.NoError(t, db.Find(&channels).Error)
	require.Len(t, channels, 2)
	for _, channel := range channels {
		assert.Equal(t, common.ChannelStatusEnabled, channel.Status, "a cancelled request must not auto-disable channel %d", channel.Id)
		cached, err := model.CacheGetChannel(channel.Id)
		require.NoError(t, err)
		assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
	}
	var logs []model.Log
	require.NoError(t, db.Find(&logs).Error)
	assert.Len(t, logs, 1, "cancellation must stop before trying or blaming a second channel")
	assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{Group: "default", Model: taskRoutingModel}).Rows,
		"a client cancellation must not penalize any attempted channel")
	probe := taskRoutingAffinityContext(t)
	channelID, found := service.GetPreferredChannelByAffinity(probe, taskRoutingModel, "default")
	assert.True(t, found)
	assert.Equal(t, 81, channelID)
}

func TestShouldRetryTaskRelayHonoursLocalErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusTemporaryRedirect} {
		c := newRetryTestContext(t)
		taskErr := &dto.TaskError{StatusCode: status, LocalError: true}
		assert.False(t, shouldRetryTaskRelay(c, 81, taskErr, 3),
			"local status %d cannot become retryable just because an upstream status would be", status)
	}
}

func TestRelayTaskPreparationFailuresAreLocal(t *testing.T) {
	for _, stage := range []string{"pricing", "request body", "request URL", "request creation", "request headers"} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			_, db := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}), 0, 0)
			originalPrices := ratio_setting.ModelPrice2JSONString()
			originalRatios := ratio_setting.ModelRatio2JSONString()
			t.Cleanup(func() {
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(originalPrices))
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalRatios))
			})
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
			var channel model.Channel
			require.NoError(t, db.First(&channel, 81).Error)
			baseURL := channel.GetBaseURL()
			channelType := constant.ChannelTypeVidu
			body := `{"model":"task-local-preparation-test","prompt":"hello"}`
			wantCode := "do_request_failed"
			if stage != "pricing" {
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"task-local-preparation-test":0}`))
			}
			switch stage {
			case "pricing":
				wantCode = "model_price_error"
			case "request body":
				body = `{"model":"task-local-preparation-test","prompt":"hello","metadata":{"seed":"not-an-integer"}}`
				wantCode = "build_request_failed"
			case "request URL":
				channelType = constant.ChannelTypeVertexAi
			case "request creation":
				baseURL = "://invalid-url"
			case "request headers":
				channelType = constant.ChannelTypeKling
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			common.SetContextKey(c, constant.ContextKeyUserId, 991)
			common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
			common.SetContextKey(c, constant.ContextKeyChannelId, 81)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, baseURL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "local-invalid-task-key")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "task-local-preparation-test")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			info, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
			require.NoError(t, err)

			result, taskErr := relay.RelayTaskSubmit(c, info)

			assert.Nil(t, result)
			require.NotNil(t, taskErr)
			require.Equal(t, wantCode, taskErr.Code)
			assert.Zero(t, calls.Load(), "preparation failures must not send an upstream request")
			assert.True(t, taskErr.LocalError, "preparing a request is not an upstream channel fault")
			assert.False(t, shouldRetryTaskRelay(c, 81, taskErr, 3))
		})
	}
}

func TestRelayTaskRequestConstructionFailurePreservesAffinity(t *testing.T) {
	var calls atomic.Int32
	router, db := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"success","message":"","data":"unexpected-fallback-task"}`)
	}), 0, 3)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 81).Update("base_url", "://invalid-url").Error)
	model.InitChannelCache()
	seedTaskRoutingAffinity(t)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, taskRoutingRequest(`{"prompt":"hello"}`))

	assert.Equal(t, http.StatusInternalServerError, recorder.Code, recorder.Body.String())
	assert.Zero(t, calls.Load(), "a local build error must not retry on the healthy fallback channel")
	require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 2*time.Second, time.Millisecond)
	assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{Group: "default", Model: taskRoutingModel}).Rows)
	probe := taskRoutingAffinityContext(t)
	channelID, found := service.GetPreferredChannelByAffinity(probe, taskRoutingModel, "default")
	assert.True(t, found)
	assert.Equal(t, 81, channelID, "a local build error must preserve the existing affinity pin")
	var tasks []model.Task
	require.NoError(t, db.Find(&tasks).Error)
	assert.Empty(t, tasks)
}
