package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetPreferredChannelByAffinityRevalidatesAutoGroup(t *testing.T) {
	const modelName = "claude-affinity-auto-test"
	// InitLogDB also initializes the dialect-quoted group column used by routing.
	originalLogDB, originalLogType := model.LOG_DB, common.LogDatabaseType()
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	t.Cleanup(func() {
		model.LOG_DB = originalLogDB
		common.SetLogDatabaseType(originalLogType)
	})
	cases := []struct {
		name         string
		usingGroup   string
		pinnedGroups string
		rivalGroups  string
		usableGroups string
		wantFound    bool
	}{
		{name: "concrete group rejects an outranked pin", usingGroup: "default", pinnedGroups: "default", rivalGroups: "default", usableGroups: `{"default":"default","vip":"vip"}`},
		{name: "auto uses the concrete group containing the pin", usingGroup: "auto", pinnedGroups: "default", rivalGroups: "default", usableGroups: `{"default":"default","vip":"vip"}`},
		{name: "auto keeps priorities scoped to the selected group", usingGroup: "auto", pinnedGroups: "default", rivalGroups: "vip", usableGroups: `{"default":"default","vip":"vip"}`, wantFound: true},
		{name: "auto respects the first eligible group for the pin", usingGroup: "auto", pinnedGroups: "vip,default", rivalGroups: "default", usableGroups: `{"default":"default","vip":"vip"}`, wantFound: true},
		{name: "auto cannot retain a pin outside the users groups", usingGroup: "auto", pinnedGroups: "vip", rivalGroups: "default", usableGroups: `{"default":"default"}`},
	}
	for _, memoryCache := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("memory=%t/%s", memoryCache, tc.name), func(t *testing.T) {
				originalMemory, originalRedis := common.MemoryCacheEnabled, common.RedisEnabled
				originalAutoGroups := setting.AutoGroups2JsonString()
				originalUsableGroups := setting.UserUsableGroups2JSONString()
				common.MemoryCacheEnabled, common.RedisEnabled = memoryCache, false
				t.Cleanup(func() {
					require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
					require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
					common.MemoryCacheEnabled = true
					model.InitChannelCache()
					common.MemoryCacheEnabled, common.RedisEnabled = originalMemory, originalRedis
				})
				require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip","default"]`))
				require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(tc.usableGroups))
				require.NotNil(t, model.DB)
				require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
				channels := []*model.Channel{
					{Type: constant.ChannelTypeOpenAI, Name: "affinity-pin", Models: modelName, Group: tc.pinnedGroups, Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(10))},
					{Type: constant.ChannelTypeOpenAI, Name: "affinity-rival", Models: modelName, Group: tc.rivalGroups, Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(50))},
				}
				for _, channel := range channels {
					require.NoError(t, model.DB.Create(channel).Error)
					t.Cleanup(func() {
						require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).Delete(&model.Ability{}).Error)
						require.NoError(t, model.DB.Delete(channel).Error)
					})
					require.NoError(t, channel.AddAbilities(nil))
				}
				model.InitChannelCache()
				useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
				seed := newClaudeMessagesRequestForTest(t, modelName, "auto-group-session")
				common.SetContextKey(seed, constant.ContextKeyUserGroup, "default")
				_, found := GetPreferredChannelByAffinity(seed, modelName, tc.usingGroup)
				require.False(t, found)
				SetChannelAffinityRelayOutcome(seed, true)
				RecordChannelAffinity(seed, channels[0].Id)

				ctx := newClaudeMessagesRequestForTest(t, modelName, "auto-group-session")
				common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
				preferred, found := GetPreferredChannelByAffinity(ctx, modelName, tc.usingGroup)
				assert.Equal(t, tc.wantFound, found)
				if tc.wantFound {
					assert.Equal(t, channels[0].Id, preferred)
				} else {
					assert.Zero(t, preferred)
				}
			})
		}
	}
}

func TestClearCurrentChannelAffinityCacheKeepsConcurrentReplacement(t *testing.T) {
	const (
		pinnedChannel      = 701
		replacementChannel = 702
		cacheKey           = "concurrent-clear-session"
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	ctx := buildChannelAffinityTemplateContextForTest(channelAffinityMeta{
		CacheKey: channelAffinityCacheNamespace + ":" + cacheKey,
	})
	cache := getChannelAffinityCache()
	require.NoError(t, cache.SetWithTTL(cacheKey, pinnedChannel, time.Minute))
	preferred, found, err := cache.Get(cacheKey)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, pinnedChannel, preferred)
	// A succeeding request replaces the pin after this request read it.
	require.NoError(t, cache.SetWithTTL(cacheKey, replacementChannel, time.Minute))

	assert.False(t, ClearCurrentChannelAffinityCache(ctx, preferred), "an old request must not clear another request's replacement pin")
	current, found, err := cache.Get(cacheKey)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, replacementChannel, current)
}
