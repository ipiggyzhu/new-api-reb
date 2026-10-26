package middleware

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectedKeyIndex runs channel setup for one request of the given affinity
// conversation ("" = no rule matched) and returns the chosen key index.
func selectedKeyIndex(t *testing.T, channel *model.Channel, conversation string) int {
	t.Helper()
	return selectedKeyIndexForModel(t, channel, conversation, "claude-sonnet-4-5")
}

func selectedKeyIndexForModel(t *testing.T, channel *model.Channel, conversation string, modelName string) int {
	t.Helper()
	c, _ := newJSONTestContext(t, http.MethodPost, "/v1/messages", `{}`)
	if conversation != "" {
		c.Set("channel_affinity_cache_key", "new-api:channel_affinity:v1:claude cli trace:"+conversation)
	}
	require.Nil(t, SetupContextForSelectedChannel(c, channel, modelName))
	return common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
}

// The upstream prompt cache is per account: with sticky_multi_key an
// affinity-matched conversation keeps one key of a multi-key channel, seeds
// spread over the keys, disabling a key moves only its conversations, and the
// polling cursor that unmatched traffic rotates on is not advanced.
func TestAffinityConversationSticksToOneKey(t *testing.T) {
	db := setupDistributorTestDB(t)
	setting := operation_setting.GetChannelAffinitySetting()
	enabled, sticky := setting.Enabled, setting.StickyMultiKey
	t.Cleanup(func() { setting.Enabled, setting.StickyMultiKey = enabled, sticky })
	setting.Enabled, setting.StickyMultiKey = true, true

	channel := &model.Channel{
		Id: 1301, Type: 14, Name: "multi-key", Status: common.ChannelStatusEnabled,
		Key:         "sk-a\nsk-b\nsk-c",
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 3, MultiKeyMode: constant.MultiKeyModePolling},
	}
	require.NoError(t, db.Create(channel).Error)

	first := selectedKeyIndex(t, channel, "conv-a")
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, selectedKeyIndex(t, channel, "conv-a"))
	}
	assert.Equal(t, 0, channel.ChannelInfo.MultiKeyPollingIndex, "sticky picks leave the polling cursor alone")

	perKey := map[int][]string{}
	for i := 0; i < 300; i++ {
		conversation := fmt.Sprintf("conv-%d", i)
		index := selectedKeyIndex(t, channel, conversation)
		perKey[index] = append(perKey[index], conversation)
	}
	require.Len(t, perKey, 3)
	for index, conversations := range perKey {
		assert.Greater(t, len(conversations), 60, "key %d gets a fair share of conversations", index)
	}

	channel.ChannelInfo.MultiKeyStatusList = map[int]int{first: common.ChannelStatusAutoDisabled}
	for index, conversations := range perKey {
		for _, conversation := range conversations[:20] {
			if index == first {
				assert.NotEqual(t, first, selectedKeyIndex(t, channel, conversation), "a disabled key's conversation moves")
			} else {
				assert.Equal(t, index, selectedKeyIndex(t, channel, conversation), "other conversations keep their key")
			}
		}
	}
	channel.ChannelInfo.MultiKeyStatusList = nil

	setting.StickyMultiKey = false
	assert.ElementsMatch(t, []int{0, 1, 2}, []int{
		selectedKeyIndex(t, channel, "conv-a"), selectedKeyIndex(t, channel, "conv-a"), selectedKeyIndex(t, channel, "conv-a"),
	}, "with sticky_multi_key off the channel's polling mode decides")
	setting.StickyMultiKey = true
	assert.ElementsMatch(t, []int{0, 1, 2}, []int{
		selectedKeyIndex(t, channel, ""), selectedKeyIndex(t, channel, ""), selectedKeyIndex(t, channel, ""),
	}, "requests no affinity rule matched keep rotating")
}

// A key cooling after a 429 is passed over by the sticky (HRW) pick of an
// affinity conversation as well, and the conversation returns to its key once
// the window ends; with every key cooling the pick is unchanged.
func TestAffinityConversationSkipsCoolingKey(t *testing.T) {
	db := setupDistributorTestDB(t)
	setting := operation_setting.GetChannelAffinitySetting()
	enabled, sticky := setting.Enabled, setting.StickyMultiKey
	t.Cleanup(func() { setting.Enabled, setting.StickyMultiKey = enabled, sticky })
	setting.Enabled, setting.StickyMultiKey = true, true

	channel := &model.Channel{
		Id: 1302, Type: 14, Name: "multi-key-cooling", Status: common.ChannelStatusEnabled,
		Key:         "sk-a\nsk-b\nsk-c",
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 3, MultiKeyMode: constant.MultiKeyModePolling},
	}
	require.NoError(t, db.Create(channel).Error)
	keys := channel.GetKeys()
	// Cooldowns are process-wide; a model name of its own keeps reruns apart.
	modelName := fmt.Sprintf("claude-sonnet-4-5-cooling-%d", time.Now().UnixNano())

	pinned := selectedKeyIndexForModel(t, channel, "conv-cool", modelName)
	past := time.Now().Add(-time.Hour)
	// A window that already ended does not move the conversation.
	model.MarkChannelKeyCooldown(channel.Id, modelName, pinned, keys[pinned], time.Second, past)
	assert.Equal(t, pinned, selectedKeyIndexForModel(t, channel, "conv-cool", modelName))

	model.MarkChannelKeyCooldown(channel.Id, modelName, pinned, keys[pinned], time.Minute, time.Now())
	moved := selectedKeyIndexForModel(t, channel, "conv-cool", modelName)
	assert.NotEqual(t, pinned, moved, "the pinned key is cooling for this model")
	assert.Equal(t, moved, selectedKeyIndexForModel(t, channel, "conv-cool", modelName))

	for index, key := range keys {
		model.MarkChannelKeyCooldown(channel.Id, modelName, index, key, time.Minute, time.Now())
	}
	assert.Equal(t, pinned, selectedKeyIndexForModel(t, channel, "conv-cool", modelName), "every key cooling: the ordinary pick")
}
