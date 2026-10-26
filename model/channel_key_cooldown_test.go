package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearKeyCooldowns(t *testing.T, channelId int, modelNames ...string) {
	t.Helper()
	t.Cleanup(func() {
		channelKeyCooldownLock.Lock()
		defer channelKeyCooldownLock.Unlock()
		for _, modelName := range modelNames {
			delete(channelKeyCooldowns, channelCooldownKey(channelId, modelName))
			channelCooldowns.Delete(channelCooldownKey(channelId, modelName))
		}
	})
}

func newPollingMultiKeyChannel(t *testing.T, key string) *Channel {
	t.Helper()
	truncateTables(t)
	require.False(t, common.MemoryCacheEnabled)
	channel := &Channel{
		Type:   1,
		Key:    key,
		Name:   "multikey-cooldown",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 3,
			MultiKeyMode: constant.MultiKeyModePolling,
		},
	}
	require.NoError(t, DB.Create(channel).Error)
	return channel
}

func pollKeyIndexes(t *testing.T, channel *Channel, modelName string, n int) []int {
	t.Helper()
	indexes := make([]int, 0, n)
	for i := 0; i < n; i++ {
		_, index, apiErr := channel.GetNextEnabledKey(modelName)
		require.Nil(t, apiErr)
		indexes = append(indexes, index)
	}
	return indexes
}

// Without an upstream retry-after the window backs off 10s, 20s, 40s, 80s and
// is capped at 2 minutes; an upstream hint is used as given; a key quiet for
// longer than the strike reset starts over at 10s.
func TestMarkChannelKeyCooldownBacksOffAndHonorsHint(t *testing.T) {
	const channelId, modelName = 900201, "claude-sonnet-4-5"
	clearKeyCooldowns(t, channelId, modelName)
	now := time.Unix(1_800_000_000, 0)

	for i, want := range []time.Duration{10, 20, 40, 80, 120, 120} {
		at := now.Add(time.Duration(i) * time.Millisecond)
		until := MarkChannelKeyCooldown(channelId, modelName, 0, "sk-a", 0, at)
		assert.Equal(t, at.Add(want*time.Second), until, "strike %d", i+1)
	}

	hinted := MarkChannelKeyCooldown(channelId, modelName, 1, "sk-b", 45*time.Minute, now)
	assert.Equal(t, now.Add(45*time.Minute), hinted, "the upstream's own retry-after wins")
	shorter := MarkChannelKeyCooldown(channelId, modelName, 1, "sk-b", time.Second, now.Add(time.Second))
	assert.Equal(t, hinted, shorter, "a later deadline already recorded is kept")

	quietLater := now.Add(keyCooldownStrikeReset + 3*time.Minute)
	assert.Equal(t, quietLater.Add(10*time.Second), MarkChannelKeyCooldown(channelId, modelName, 0, "sk-a", 0, quietLater),
		"a key quiet past the strike reset starts again from the base window")
}

// Polling and HRW-pinned selection both pass over a key cooling for the model,
// leave other models alone, return to the key once its window ends, and fall
// back to the ordinary choice when every enabled key is cooling.
func TestKeySelectionSkipsCoolingKeys(t *testing.T) {
	const modelName, otherModel = "claude-sonnet-4-5", "claude-haiku-4-5"
	channel := newPollingMultiKeyChannel(t, "sk-a\nsk-b\nsk-c")
	clearKeyCooldowns(t, channel.Id, modelName, otherModel)

	assert.Equal(t, []int{0, 1, 2, 0, 1, 2}, pollKeyIndexes(t, channel, modelName, 6))

	MarkChannelKeyCooldown(channel.Id, modelName, 1, "sk-b", 0, time.Now())
	assert.Equal(t, []int{0, 2, 0, 2}, pollKeyIndexes(t, channel, modelName, 4), "polling skips the cooling key")
	assert.Equal(t, []int{0, 1, 2}, pollKeyIndexes(t, channel, otherModel, 3), "a cooldown belongs to the refused model")

	const seed = "new-api:channel_affinity:v1:claude cli trace:conv-a"
	_, pinned, apiErr := channel.GetEnabledKeyForSeed(seed, otherModel)
	require.Nil(t, apiErr)
	MarkChannelKeyCooldown(channel.Id, modelName, pinned, channel.GetKeys()[pinned], 0, time.Now())
	_, moved, apiErr := channel.GetEnabledKeyForSeed(seed, modelName)
	require.Nil(t, apiErr)
	assert.NotEqual(t, pinned, moved, "the conversation leaves its pinned key while it cools")
	for i := 0; i < 3; i++ {
		_, again, _ := channel.GetEnabledKeyForSeed(seed, modelName)
		assert.Equal(t, moved, again, "and stays on one fallback key meanwhile")
	}

	// Every enabled key cooling: selection proceeds as if none were.
	for index, key := range channel.GetKeys() {
		MarkChannelKeyCooldown(channel.Id, modelName, index, key, 0, time.Now())
	}
	_, allCooling, apiErr := channel.GetEnabledKeyForSeed(seed, modelName)
	require.Nil(t, apiErr)
	assert.Equal(t, pinned, allCooling)
	assert.ElementsMatch(t, []int{0, 1, 2}, pollKeyIndexes(t, channel, modelName, 3))

	// Windows end: the pinned key takes its conversation back.
	channelKeyCooldownLock.Lock()
	for index, entry := range channelKeyCooldowns[channelCooldownKey(channel.Id, modelName)] {
		entry.until = time.Now().Add(-time.Second)
		channelKeyCooldowns[channelCooldownKey(channel.Id, modelName)][index] = entry
	}
	channelKeyCooldownLock.Unlock()
	_, recovered, apiErr := channel.GetEnabledKeyForSeed(seed, modelName)
	require.Nil(t, apiErr)
	assert.Equal(t, pinned, recovered, "the pinned key is selected again once its window ends")
	assert.ElementsMatch(t, []int{0, 1, 2}, pollKeyIndexes(t, channel, modelName, 3))
}

// A cooldown follows the key, not the slot: after the key list is edited, the
// key that moved into a refused key's index is not cooling.
func TestKeyCooldownIgnoresAnEditedKeySlot(t *testing.T) {
	const modelName = "gpt-5"
	channel := newPollingMultiKeyChannel(t, "sk-a\nsk-b\nsk-c")
	clearKeyCooldowns(t, channel.Id, modelName)

	MarkChannelKeyCooldown(channel.Id, modelName, 1, "sk-b", 0, time.Now())
	assert.NotContains(t, pollKeyIndexes(t, channel, modelName, 4), 1)

	channel.Key = "sk-a\nsk-new\nsk-c"
	channel.Keys = nil
	assert.Contains(t, pollKeyIndexes(t, channel, modelName, 3), 1)
}

// The channel itself cools only when every enabled key is cooling, and only
// until the first of them recovers. Disabled keys do not count.
func TestMarkChannelKeyCooldownAndChannelCoolsChannelOnceAllEnabledKeysCool(t *testing.T) {
	const modelName = "claude-opus-4-1"
	channel := newPollingMultiKeyChannel(t, "sk-a\nsk-b\nsk-c")
	channel.ChannelInfo.MultiKeyStatusList = map[int]int{2: common.ChannelStatusAutoDisabled}
	clearKeyCooldowns(t, channel.Id, modelName)

	firstUntil, cooled := MarkChannelKeyCooldownAndChannel(channel, modelName, 0, "sk-a", 30*time.Second)
	assert.False(t, cooled)
	assert.False(t, ChannelCoolingDown(channel.Id, modelName), "a sibling key still has room")

	_, cooled = MarkChannelKeyCooldownAndChannel(channel, modelName, 1, "sk-b", 5*time.Minute)
	assert.True(t, cooled)
	assert.True(t, ChannelCoolingDown(channel.Id, modelName))
	stored, ok := channelCooldowns.Load(channelCooldownKey(channel.Id, modelName))
	require.True(t, ok)
	assert.Equal(t, firstUntil, stored.(time.Time), "the channel comes back with its first recovering key")

	_, index, apiErr := channel.GetNextEnabledKey(modelName)
	require.Nil(t, apiErr, "a fully cooling channel still hands out a key")
	assert.NotEqual(t, 2, index, "a disabled key stays disabled")
}
