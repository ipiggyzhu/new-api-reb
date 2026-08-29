package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The automatic recovery path (service.EnableChannel -> UpdateChannelStatus ->
// CacheUpdateChannelStatus) is the only enable path that does not run a full
// InitChannelCache afterwards. These tests pin the contract that re-enabling a
// channel makes it selectable again immediately, not only after the next sync.

func setupChannelCacheTest(t *testing.T) {
	t.Helper()
	prevMemoryCache := common.MemoryCacheEnabled
	prevGroup2model2channels := group2model2channels
	prevChannelsIDM := channelsIDM
	prevAdvancedCustomConfig := channel2advancedCustomConfig
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM channels")
		channelSyncLock.Lock()
		group2model2channels = prevGroup2model2channels
		channelsIDM = prevChannelsIDM
		channel2advancedCustomConfig = prevAdvancedCustomConfig
		channelSyncLock.Unlock()
		common.MemoryCacheEnabled = prevMemoryCache
	})
}

func mustCreateCachedChannel(t *testing.T, name string, group string, models string, priority int64) *Channel {
	t.Helper()
	channel := &Channel{
		Name:     name,
		Key:      "sk-" + name,
		Status:   common.ChannelStatusEnabled,
		Group:    group,
		Models:   models,
		Priority: &priority,
	}
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	return channel
}

func TestCacheUpdateChannelStatusReEnableRestoresSelection(t *testing.T) {
	setupChannelCacheTest(t)
	high := mustCreateCachedChannel(t, "cache-high", "default", "model-x", 20)
	low := mustCreateCachedChannel(t, "cache-low", "default", "model-x", 5)
	InitChannelCache()

	require.True(t, IsChannelEnabledForGroupModel("default", "model-x", high.Id))
	require.True(t, IsChannelEnabledForGroupModel("default", "model-x", low.Id))

	CacheUpdateChannelStatus(high.Id, common.ChannelStatusAutoDisabled)
	assert.False(t, IsChannelEnabledForGroupModel("default", "model-x", high.Id))
	picked, err := GetRandomSatisfiedChannel("default", "model-x", 0, "", nil)
	require.NoError(t, err)
	require.NotNil(t, picked)
	assert.Equal(t, low.Id, picked.Id, "only the remaining channel may serve while disabled")

	CacheUpdateChannelStatus(high.Id, common.ChannelStatusEnabled)
	assert.True(t, IsChannelEnabledForGroupModel("default", "model-x", high.Id),
		"re-enabled channel must rejoin the selection cache without a full sync")
	cached, err := CacheGetChannel(high.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
	picked, err = GetRandomSatisfiedChannel("default", "model-x", 0, "", nil)
	require.NoError(t, err)
	require.NotNil(t, picked)
	assert.Equal(t, high.Id, picked.Id, "re-enabled channel must win at its original priority")
}

func TestCacheUpdateChannelStatusReEnableIsIdempotent(t *testing.T) {
	setupChannelCacheTest(t)
	channel := mustCreateCachedChannel(t, "cache-idem", "default", "model-x", 10)
	InitChannelCache()

	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusAutoDisabled)
	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusEnabled)
	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusEnabled)
	require.True(t, IsChannelEnabledForGroupModel("default", "model-x", channel.Id))

	// A duplicated selection entry would survive a single disable, because the
	// removal loop deletes only the first occurrence per group/model.
	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusAutoDisabled)
	assert.False(t, IsChannelEnabledForGroupModel("default", "model-x", channel.Id))
}

func TestCacheUpdateChannelStatusReEnableCoversAllGroupsAndModels(t *testing.T) {
	setupChannelCacheTest(t)
	channel := mustCreateCachedChannel(t, "cache-multi", "default,vip", "model-a,model-b", 10)
	InitChannelCache()

	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusAutoDisabled)
	require.False(t, IsChannelEnabledForGroupModel("default", "model-a", channel.Id))
	require.False(t, IsChannelEnabledForGroupModel("vip", "model-b", channel.Id))

	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusEnabled)
	assert.True(t, IsChannelEnabledForGroupModel("default", "model-a", channel.Id))
	assert.True(t, IsChannelEnabledForGroupModel("default", "model-b", channel.Id))
	assert.True(t, IsChannelEnabledForGroupModel("vip", "model-a", channel.Id))
	assert.True(t, IsChannelEnabledForGroupModel("vip", "model-b", channel.Id))
}

func TestInitChannelCacheHandlesChannelWithoutAbilities(t *testing.T) {
	setupChannelCacheTest(t)
	priority := int64(10)
	channel := &Channel{
		Name:     "cache-no-ability",
		Key:      "sk-cache-no-ability",
		Status:   common.ChannelStatusEnabled,
		Group:    "orphan-group",
		Models:   "orphan-model",
		Priority: &priority,
	}
	require.NoError(t, DB.Create(channel).Error)

	// InitChannelCache must tolerate the transient state between channel creation
	// and ability repair. Previously this wrote to a nil nested map and panicked.
	InitChannelCache()
	assert.True(t, IsChannelEnabledForGroupModel("orphan-group", "orphan-model", channel.Id))
}

// The cache hands out copies so a caller can hold a channel after the read lock
// is gone while the sync goroutine and the auto-ban path replace entries. These
// tests pin that contract: without it, mutating a returned channel corrupted the
// cache and reading one raced with the writers.

func TestCacheGetChannelReturnsIsolatedCopy(t *testing.T) {
	setupChannelCacheTest(t)
	channel := mustCreateCachedChannel(t, "cache-copy", "default", "model-x", 10)
	InitChannelCache()

	first, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	second, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.NotSame(t, first, second, "each caller must get its own channel value")

	first.Status = common.ChannelStatusManuallyDisabled
	first.Name = "mutated-by-caller"

	fresh, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, fresh.Status, "caller mutation must not leak into the cache")
	assert.Equal(t, "cache-copy", fresh.Name)
}

func TestCacheGetChannelCopiesMultiKeyMaps(t *testing.T) {
	setupChannelCacheTest(t)
	priority := int64(10)
	channel := &Channel{
		Name:     "cache-multikey",
		Key:      "sk-one\nsk-two",
		Status:   common.ChannelStatusEnabled,
		Group:    "default",
		Models:   "model-x",
		Priority: &priority,
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       2,
			MultiKeyMode:       constant.MultiKeyModePolling,
			MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled},
		},
	}
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	InitChannelCache()

	copied, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	// A shallow copy would share this map, so writing through one handle would be
	// visible to every other reader without any lock held.
	copied.ChannelInfo.MultiKeyStatusList[0] = common.ChannelStatusAutoDisabled

	fresh, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.NotContains(t, fresh.ChannelInfo.MultiKeyStatusList, 0,
		"per-key status map must not be shared with callers")
	assert.Contains(t, fresh.ChannelInfo.MultiKeyStatusList, 1, "original entries must survive the copy")
}

func TestCacheUpdateChannelStatusRemovesDuplicateSelectionEntries(t *testing.T) {
	setupChannelCacheTest(t)
	// A duplicated group/model pair is what produced a doubled weighted-random
	// share and a selection entry that outlived a disable.
	channel := mustCreateCachedChannel(t, "cache-dup", "default,default", "model-x,model-x", 10)
	InitChannelCache()

	channelSyncLock.RLock()
	occurrences := 0
	for _, id := range group2model2channels["default"]["model-x"] {
		if id == channel.Id {
			occurrences++
		}
	}
	channelSyncLock.RUnlock()
	assert.Equal(t, 1, occurrences, "a repeated group/model pair must not duplicate the channel")

	CacheUpdateChannelStatus(channel.Id, common.ChannelStatusAutoDisabled)
	assert.False(t, IsChannelEnabledForGroupModel("default", "model-x", channel.Id),
		"disabling must clear every selection entry, not just the first")
}
