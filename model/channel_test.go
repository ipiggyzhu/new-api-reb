package model

import (
	"fmt"
	"testing"

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

	_, _, apiErr := stale.GetNextEnabledKey()
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
		key, index, apiErr := channel.GetStickyEnabledKey(seed)
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
	_, _, apiErr := allDisabled.GetStickyEnabledKey("conversation-0")
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeChannelNoAvailableKey, apiErr.GetErrorCode())
}

// A single-key channel, and the key a channel had before it became multi-key,
// wear identity slot "0", the identity the channel always had. Every other key
// of a multi-key channel wears a slot of its own, derived from the key alone.
func TestKeyIdentitySlot(t *testing.T) {
	single := &Channel{Key: "key-a"}
	assert.Equal(t, "0", single.KeyIdentitySlot("key-a"))

	converted := &Channel{Key: "key-a\nkey-b\nkey-c", ChannelInfo: ChannelInfo{
		IsMultiKey: true, MultiKeyOriginFingerprint: KeyFingerprint("key-a"),
	}}
	assert.Equal(t, "0", converted.KeyIdentitySlot("key-a"))
	assert.Equal(t, "ka30534a53b235473", converted.KeyIdentitySlot("key-b"))
	assert.NotEqual(t, converted.KeyIdentitySlot("key-b"), converted.KeyIdentitySlot("key-c"))

	created := &Channel{Key: "key-a\nkey-b", ChannelInfo: ChannelInfo{IsMultiKey: true}}
	assert.Equal(t, "kf10f781241e22466", created.KeyIdentitySlot("key-a"), "a channel created multi-key has no origin key")
}

// Removing a key shifts the indexes after it, but a slot keeps finding its own
// key; a disabled or removed key is not found.
func TestEnabledKeyForSlot(t *testing.T) {
	single := &Channel{Key: "key-a"}
	key, index, ok := single.EnabledKeyForSlot("0")
	assert.True(t, ok)
	assert.Equal(t, "key-a", key)
	assert.Zero(t, index)
	_, _, ok = single.EnabledKeyForSlot("kf10f781241e22466")
	assert.False(t, ok)

	// Converted from key-a, which has since been removed; key-b is disabled.
	channel := &Channel{Key: "key-b\nkey-c", ChannelInfo: ChannelInfo{
		IsMultiKey: true, MultiKeyOriginFingerprint: KeyFingerprint("key-a"),
		MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled},
	}}
	key, index, ok = channel.EnabledKeyForSlot(channel.KeyIdentitySlot("key-c"))
	assert.True(t, ok)
	assert.Equal(t, "key-c", key)
	assert.Equal(t, 1, index)
	_, _, ok = channel.EnabledKeyForSlot("ka30534a53b235473")
	assert.False(t, ok, "a disabled key")
	_, _, ok = channel.EnabledKeyForSlot("0")
	assert.False(t, ok, "a removed key")
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
