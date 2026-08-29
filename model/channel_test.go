package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
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
