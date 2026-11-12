package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetNextEnabledKeyPollingPreservesConcurrentKeyDisable(t *testing.T) {
	truncateTables(t)
	require.False(t, common.MemoryCacheEnabled, "test covers the no-memory-cache persistence path")

	channel := &Channel{
		Type:   1,
		Key:    "key-a\nkey-b\nkey-c",
		Name:   "multikey-polling",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 3,
			MultiKeyMode: constant.MultiKeyModePolling,
		},
	}
	require.NoError(t, DB.Create(channel).Error)

	// In-flight request's copy, loaded before the disable below commits.
	stale, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	// Concurrent request auto-disables key index 2 through the same
	// read-modify-write UpdateChannelStatus performs.
	disabler, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	handlerMultiKeyUpdate(disabler, "key-c", common.ChannelStatusAutoDisabled, "401 unauthorized")
	require.Equal(t, common.ChannelStatusAutoDisabled, disabler.ChannelInfo.MultiKeyStatusList[2])
	require.NoError(t, disabler.SaveWithoutKey())

	_, _, apiErr := stale.GetNextEnabledKey("")
	require.Nil(t, apiErr)

	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[2],
		"polling index save must not resurrect a concurrently disabled key")
	assert.Equal(t, "401 unauthorized", reloaded.ChannelInfo.MultiKeyDisabledReason[2])
	assert.Equal(t, 1, reloaded.ChannelInfo.MultiKeyPollingIndex, "polling index advance must still be persisted")
}

// The fingerprint is stored in channel_info as the key of every per-key
// environment and as the converted channel's origin key; a different value for
// the same key would orphan the stored environments and change identities.
func TestKeyFingerprint(t *testing.T) {
	assert.Equal(t, "f10f781241e22466", KeyFingerprint("key-a"))
	assert.Equal(t, "a30534a53b235473", KeyFingerprint("key-b"))
	assert.Equal(t, KeyFingerprint("key-b"), KeyFingerprint(" key-b\r"), "a key pasted with a CRLF line end is the same key")
}

// A conversation on a multi-key channel stays on one key. Disabling or removing
// a key moves only the conversations that were on it: every other conversation
// keeps its key, even when the removal shifts that key's index.
func TestGetStickyEnabledKeyMovesOnlyTheLostKeysConversations(t *testing.T) {
	pick := func(channel *Channel, seed string) string {
		key, index, apiErr := channel.GetStickyEnabledKey(seed, "sticky-model")
		require.Nil(t, apiErr)
		require.Equal(t, channel.GetKeys()[index], key, "the index names the key returned")
		return key
	}
	channel := &Channel{Key: "key-a\nkey-b\nkey-c", ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModeRandom}}
	seeds := make([]string, 30)
	pinned := make(map[string]string, len(seeds))
	used := make(map[string]bool)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("conversation-%d", i)
		pinned[seeds[i]] = pick(channel, seeds[i])
		assert.Equal(t, pinned[seeds[i]], pick(channel, seeds[i]), "the same conversation, the same key")
		used[pinned[seeds[i]]] = true
	}
	require.Len(t, used, 3, "the conversations spread over every key")

	disabled := &Channel{Key: channel.Key, ChannelInfo: ChannelInfo{
		IsMultiKey: true, MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled},
	}}
	removed := &Channel{Key: "key-b\nkey-c", ChannelInfo: ChannelInfo{IsMultiKey: true}}
	for _, seed := range seeds {
		if pinned[seed] == "key-b" {
			assert.NotEqual(t, "key-b", pick(disabled, seed), "a disabled key serves no conversation")
		} else {
			assert.Equal(t, pinned[seed], pick(disabled, seed), "%s stays on its key", seed)
		}
		if pinned[seed] != "key-a" {
			assert.Equal(t, pinned[seed], pick(removed, seed), "%s stays on its key after re-indexing", seed)
		}
	}

	allDisabled := &Channel{Key: "key-a", ChannelInfo: ChannelInfo{
		IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled},
	}}
	_, _, apiErr := allDisabled.GetStickyEnabledKey("conversation-0", "sticky-model")
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeChannelNoAvailableKey, apiErr.GetErrorCode())
}

// A key that failed cools down for the model it failed on, and while it cools
// its conversations go to their next key and no other conversation moves. With
// every key cooling each conversation keeps its own, and once the window ends
// it is back on its key.
func TestGetStickyEnabledKeyPassesOverCoolingKeys(t *testing.T) {
	const modelName = "sticky-model"
	channel := &Channel{Id: 900131, Key: "key-a\nkey-b\nkey-c", ChannelInfo: ChannelInfo{IsMultiKey: true}}
	forget := func() {
		channelKeyCooldownLock.Lock()
		delete(channelKeyCooldowns, modelName)
		channelKeyCooldownLock.Unlock()
	}
	forget()
	t.Cleanup(forget)
	pick := func(seed string, modelName string) string {
		key, _, apiErr := channel.GetStickyEnabledKey(seed, modelName)
		require.Nil(t, apiErr)
		return key
	}
	seeds := make([]string, 30)
	pinned := make(map[string]string, len(seeds))
	onKeyB := 0
	for i := range seeds {
		seeds[i] = fmt.Sprintf("conversation-%d", i)
		pinned[seeds[i]] = pick(seeds[i], modelName)
		if pinned[seeds[i]] == "key-b" {
			onKeyB++
		}
	}
	require.NotZero(t, onKeyB)

	MarkChannelKeyCooldown(channel.Id, modelName, 1, "key-b", time.Hour, time.Now())
	for _, seed := range seeds {
		if pinned[seed] == "key-b" {
			assert.NotEqual(t, "key-b", pick(seed, modelName), "a cooling key hands %s to its next key", seed)
			assert.Equal(t, "key-b", pick(seed, "other-model"), "a cooldown belongs to the model that failed")
		} else {
			assert.Equal(t, pinned[seed], pick(seed, modelName), "%s stays on its key", seed)
		}
	}

	MarkChannelKeyCooldown(channel.Id, modelName, 0, "key-a", time.Hour, time.Now())
	MarkChannelKeyCooldown(channel.Id, modelName, 2, "key-c", time.Hour, time.Now())
	for _, seed := range seeds {
		assert.Equal(t, pinned[seed], pick(seed, modelName), "with every key cooling %s keeps its own", seed)
	}

	forget()
	MarkChannelKeyCooldown(channel.Id, modelName, 1, "key-b", time.Second, time.Now().Add(-time.Minute))
	for _, seed := range seeds {
		assert.Equal(t, pinned[seed], pick(seed, modelName), "once the window ends %s is back on its key", seed)
	}
}

// The canonical key tag survives conversion and movement between channels.
// The independent identity is derived from this tag only when the switch is on.
func TestKeyTagReplacesIdentitySlot(t *testing.T) {
	single := &Channel{Id: 1, Key: "key-a"}
	converted := &Channel{Id: 1, Key: "key-a\nkey-b", ChannelInfo: ChannelInfo{IsMultiKey: true}}
	moved := &Channel{Id: 2, Key: "key-b\nkey-a", ChannelInfo: ChannelInfo{IsMultiKey: true}}
	assert.Equal(t, ChannelKeyTag(single.Key), ChannelKeyTag(converted.GetKeys()[0]))
	assert.Equal(t, ChannelKeyTag(single.Key), ChannelKeyTag(moved.GetKeys()[1]))
	assert.NotEqual(t, ChannelKeyTag("key-a"), ChannelKeyTag("key-b"))
}

// A key tag keeps finding its enabled key after indexes shift. Removed and
// disabled keys cannot be used by keepalive or a bound conversation.
func TestEnabledKeyForTag(t *testing.T) {
	single := &Channel{Key: "key-a"}
	index, key, cooling := single.KeyEnvironmentKey(ChannelKeyTag("key-a"), "")
	assert.Zero(t, index)
	assert.Equal(t, "key-a", key)
	assert.True(t, cooling.IsZero())
	index, _, _ = single.KeyEnvironmentKey(ChannelKeyTag("key-b"), "")
	assert.Equal(t, -1, index)

	channel := &Channel{Key: "key-b\nkey-c", ChannelInfo: ChannelInfo{
		IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled},
	}}
	index, key, cooling = channel.KeyEnvironmentKey(ChannelKeyTag("key-c"), "")
	assert.Equal(t, 1, index)
	assert.Equal(t, "key-c", key)
	assert.True(t, cooling.IsZero())
	for _, absent := range []string{"key-a", "key-b"} {
		index, _, _ = channel.KeyEnvironmentKey(ChannelKeyTag(absent), "")
		assert.Equal(t, -1, index, absent)
	}
}

// Saving a multi-key channel drops the environments of keys no longer on it and
// leaves the others with their keys.
func TestChannelUpdateDropsEnvironmentsOfRemovedKeys(t *testing.T) {
	truncateTables(t)
	channel := &Channel{
		Type: 1, Key: "key-a\nkey-b\nkey-c", Name: "multikey-envs", Status: common.ChannelStatusEnabled,
		Models: "gpt-4o", Group: "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey: true, MultiKeySize: 3,
			MultiKeyEnvs: map[string]MultiKeyEnv{
				KeyFingerprint("key-a"): {Proxy: "http://proxy-a:8080"},
				KeyFingerprint("key-b"): {CLIVersionProfile: "codex-0.156.1-windows-x64"},
			},
		},
	}
	require.NoError(t, DB.Create(channel).Error)

	channel.Key = "key-b\nkey-c"
	require.NoError(t, channel.Update())

	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, 2, reloaded.ChannelInfo.MultiKeySize)
	assert.Equal(t, map[string]MultiKeyEnv{
		KeyFingerprint("key-b"): {CLIVersionProfile: "codex-0.156.1-windows-x64"},
	}, reloaded.ChannelInfo.MultiKeyEnvs)
}
