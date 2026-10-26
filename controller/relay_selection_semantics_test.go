package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ordinary selection uses the actual channel/ability tables with either storage
// path. Scoring is off here so baseline priority and weight are isolated; the
// existing score/hysteresis tests protect the enabled-scoring contract.
func ordinarySelectionChannels(t *testing.T, memoryCache bool, modelName string) []*model.Channel {
	t.Helper()
	originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
	originalRedis := common.RedisEnabled
	db := setupKeyEnvironmentControllerTest(t)
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	score := operation_setting.GetChannelDynamicScoreSetting()
	score.Enabled = false
	operation_setting.SetChannelDynamicScoreSettingForTest(score)
	t.Cleanup(operation_setting.ResetChannelDynamicScoreSettingForTest)
	affinity := operation_setting.GetChannelAffinitySetting()
	originalAffinity := *affinity
	affinity.Enabled, affinity.SwitchOnSuccess = true, true
	service.ClearChannelAffinityCacheAll()
	t.Cleanup(func() {
		service.ClearChannelAffinityCacheAll()
		*affinity = originalAffinity
		common.RedisEnabled = originalRedis
		common.SetDatabaseTypes(originalMainType, originalLogType)
	})
	common.MemoryCacheEnabled, common.RedisEnabled = memoryCache, false
	channels := []*model.Channel{
		{Type: constant.ChannelTypeOpenAI, Name: "top-a", Key: "sk-task21-a", Priority: common.GetPointer(int64(100)), Weight: common.GetPointer(uint(0))},
		{Type: constant.ChannelTypeOpenAI, Name: "top-b", Key: "sk-task21-b", Priority: common.GetPointer(int64(100)), Weight: common.GetPointer(uint(100))},
		{Type: constant.ChannelTypeOpenAI, Name: "fallback", Key: "sk-task21-c", Priority: common.GetPointer(int64(10)), Weight: common.GetPointer(uint(1000))},
	}
	for _, channel := range channels {
		channel.Status, channel.Models, channel.Group = common.ChannelStatusEnabled, modelName, "default"
		require.False(t, channel.GetSetting().KeyEnvironments)
		require.NoError(t, db.Create(channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		t.Cleanup(func() { model.ReleaseChannelSlot(channel.Id) })
	}
	model.InitChannelCache()
	return channels
}

func ordinarySelectionRequest(t *testing.T, path, body string) *gin.Context {
	t.Helper()
	c := newRetryTestContext(t)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	t.Cleanup(func() {
		common.CleanupBodyStorage(c)
		service.ReleaseHeldChannelSlot(c)
	})
	return c
}

func TestOrdinaryRelayExhaustsPriorityTiersWithoutRepeatingFailures(t *testing.T) {
	for _, memoryCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory=%t", memoryCache), func(t *testing.T) {
			const modelName = "gpt-task21-selection"
			channels := ordinarySelectionChannels(t, memoryCache, modelName)
			c := ordinarySelectionRequest(t, "/v1/responses", `{"model":"gpt-task21-selection","input":"hi"}`)
			param := &service.RetryParam{Ctx: c, ModelName: modelName, TokenGroup: "default", RequestPath: c.Request.URL.Path}
			first, _, err := service.CacheGetRandomSatisfiedChannel(param)
			require.NoError(t, err)
			require.NotNil(t, first)
			service.HoldChannelSlot(c, first.Id)
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, first, modelName))
			info := &relaycommon.RelayInfo{OriginModelName: modelName, TokenGroup: "default"}
			var attempted []int
			for attempt := 0; attempt < 3; attempt++ {
				channel, apiErr := getChannel(c, info, param)
				require.Nil(t, apiErr)
				require.NotNil(t, channel)
				assert.NotContains(t, attempted, channel.Id, "failed channels never recur within a request")
				attempted = append(attempted, channel.Id)
				if attempt < 2 {
					assert.Contains(t, []int{channels[0].Id, channels[1].Id}, channel.Id, "priority outranks even the fallback's larger weight")
				} else {
					assert.Equal(t, channels[2].Id, channel.Id, "the next tier is reached only after both top-tier channels fail")
				}
				assert.True(t, shouldRetry(c, types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, http.StatusServiceUnavailable), 3-attempt))
				info.ChannelMeta = &relaycommon.ChannelMeta{}
				param.IncreaseRetry()
			}
			channel, apiErr := getChannel(c, info, param)
			assert.Nil(t, channel)
			require.NotNil(t, apiErr, "exhaustion terminates rather than starting again")
		})
	}
}

func TestDefaultAffinityKeepsOrdinarySuccessAndAllowsFailover(t *testing.T) {
	for _, memoryCache := range []bool{false, true} {
		for _, family := range []string{"codex", "claude"} {
			t.Run(fmt.Sprintf("memory=%t/%s", memoryCache, family), func(t *testing.T) {
				modelName, path, body := "gpt-task21-affinity", "/v1/responses", `{"model":"gpt-task21-affinity","prompt_cache_key":"task21-session","input":"hi"}`
				if family == "claude" {
					modelName, path, body = "claude-task21-affinity", "/v1/messages", `{"model":"claude-task21-affinity","metadata":{"user_id":"task21-session"},"messages":[]}`
				}
				channels := ordinarySelectionChannels(t, memoryCache, modelName)
				cold := ordinarySelectionRequest(t, path, body)
				_, found := service.GetPreferredChannelByAffinity(cold, modelName, "default")
				require.False(t, found)
				first, _, err := service.CacheGetRandomSatisfiedChannel(&service.RetryParam{Ctx: cold, ModelName: modelName, TokenGroup: "default", RequestPath: path})
				require.NoError(t, err)
				require.NotNil(t, first)
				service.HoldChannelSlot(cold, first.Id)
				require.Nil(t, middleware.SetupContextForSelectedChannel(cold, first, modelName))
				service.SetChannelAffinityRelayOutcome(cold, true)
				service.RecordChannelAffinity(cold, first.Id)

				warm := ordinarySelectionRequest(t, path, body)
				preferred, found := service.GetPreferredChannelByAffinity(warm, modelName, "default")
				require.True(t, found)
				assert.Equal(t, first.Id, preferred, "a success stays pinned even beside a differently weighted sibling")
				service.MarkChannelAffinityUsed(warm, "default", preferred)
				require.Nil(t, middleware.SetupContextForSelectedChannel(warm, first, modelName))
				param := &service.RetryParam{Ctx: warm, ModelName: modelName, TokenGroup: "default", RequestPath: path}
				_, apiErr := getChannel(warm, &relaycommon.RelayInfo{}, param)
				require.Nil(t, apiErr)
				upstreamErr := types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, http.StatusServiceUnavailable)
				require.True(t, shouldRetry(warm, upstreamErr, 3), "the shipped default affinity rules allow a failed pinned attempt to fail over")
				service.MarkChannelAffinityChannelFault(warm, preferred)
				param.IncreaseRetry()
				fallback, apiErr := getChannel(warm, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, OriginModelName: modelName}, param)
				require.Nil(t, apiErr)
				require.NotNil(t, fallback)
				assert.NotEqual(t, preferred, fallback.Id)
				assert.Contains(t, []int{channels[0].Id, channels[1].Id}, fallback.Id, "failover stays in the top tier while a sibling remains")
				service.SetChannelAffinityRelayOutcome(warm, true)
				service.RecordChannelAffinity(warm, first.Id)

				next := ordinarySelectionRequest(t, path, body)
				preferred, found = service.GetPreferredChannelByAffinity(next, modelName, "default")
				require.True(t, found)
				assert.Equal(t, fallback.Id, preferred, "the actual successful fallback owns the next request")
			})
		}
	}
}
