package controller

import (
	"context"
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
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelayClientDisconnectIsNotAChannelFault covers a caller that hangs up while
// the upstream is still working. The outbound request is bound to the caller's
// context, so the attempt ends as a do_request_failed 500 on whichever channel
// was serving it. That is not evidence against the channel: it must not be
// scored as a fault, and there is nobody left to retry for.
func TestRelayClientDisconnectIsNotAChannelFault(t *testing.T) {
	const modelName = "claude-fable-5-1"
	const initialQuota = 100_000

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
		channel_score.ResetAll()
		operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalRatios))
	})
	common.MemoryCacheEnabled = true
	common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = false, false
	common.RetryTimes, constant.CountToken = 3, false
	// Scoring on, so a wrongly attributed fault becomes observable in the store.
	scores := originalScores
	scores.Enabled = true
	operation_setting.SetChannelDynamicScoreSettingForTest(scores)
	channel_score.ResetAll()
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
	user := model.User{Id: 11, Username: "relay-cancel-user", Group: "default", Quota: initialQuota, Status: common.UserStatusEnabled}
	token := model.Token{Id: 13, UserId: user.Id, Key: "local-cancel-test-token", Name: "relay-cancel-token", RemainQuota: initialQuota}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&token).Error)

	var upstreamCalls atomic.Int32
	upstreamReached := make(chan struct{}, common.RetryTimes+1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		upstreamReached <- struct{}{}
		// Keep working until the test lets go, as a slow but healthy upstream
		// would while the caller walks away.
		<-releaseUpstream
	}))
	t.Cleanup(upstream.Close)
	for id := 1; id <= 2; id++ {
		channel := model.Channel{
			Id: id, Type: constant.ChannelTypeAnthropic, Name: "cancel-upstream", Key: "local-upstream-test-key",
			Status: common.ChannelStatusEnabled, Models: modelName, Group: "default",
			BaseURL: &upstream.URL, Priority: common.GetPointer(int64(5)), Weight: common.GetPointer(uint(10)),
		}
		require.NoError(t, db.Create(&channel).Error)
	}
	model.InitChannelCache()

	router := gin.New()
	router.POST("/v1/messages", func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
		common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
		c.Set("token_name", token.Name)
		c.Set("token_quota", initialQuota)
		c.Set(common.RequestIdKey, "relay-cancel-test")
		c.Next()
	}, middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatClaude) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-fable-5-1","max_tokens":1,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	t.Cleanup(func() {
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 },
			2*time.Second, time.Millisecond, "relay background tasks must finish before fixture cleanup")
	})

	served := make(chan struct{})
	go func() {
		router.ServeHTTP(recorder, request)
		close(served)
	}()
	select {
	case <-upstreamReached:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never received the request")
	}
	// The caller leaves while the upstream is still generating.
	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not return after the caller disconnected")
	}
	close(releaseUpstream)

	assert.Equal(t, int32(1), upstreamCalls.Load(), "a request nobody is waiting for must not be retried on another channel")
	snapshot := channel_score.Snapshot(channel_score.ScoreFilter{Group: "default", Model: modelName})
	for _, row := range snapshot.Rows {
		assert.Zero(t, row.FaultCount, "channel %d was blamed for the caller hanging up", row.ChannelID)
		assert.Zero(t, row.Total, "channel %d recorded a sample for a request the caller abandoned", row.ChannelID)
	}

	// The refund is asynchronous; the balances are the observable outcome.
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var currentUser model.User
		var currentToken model.Token
		if !assert.NoError(collect, db.First(&currentUser, user.Id).Error) ||
			!assert.NoError(collect, db.First(&currentToken, token.Id).Error) {
			return
		}
		assert.Equal(collect, initialQuota, currentUser.Quota)
		assert.Equal(collect, initialQuota, currentToken.RemainQuota)
	}, 2*time.Second, time.Millisecond, "an abandoned request must be refunded in full")
}
