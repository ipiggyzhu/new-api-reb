package middleware

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func createKeyEnvironmentChannel(t *testing.T, db *gorm.DB, id int, keys string, assignments map[string]string) *model.Channel {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.ChannelKeyEnvironment{}))
	channel := &model.Channel{
		Id: id, Type: constant.ChannelTypeOpenAI, Name: "key-env", Status: common.ChannelStatusEnabled, Key: keys,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling},
	}
	channel.SetSetting(dto.ChannelSettings{
		SyntheticClientHeadersProfile: "codex", KeyEnvironments: true,
		CLIVersionProfile: "codex-0.156.1-windows-x64", TLSFingerprint: "codex-cli",
	})
	require.NoError(t, db.Create(channel).Error)
	for key, profile := range assignments {
		row := model.ChannelKeyEnvironment{ChannelId: id, KeyTag: model.ChannelKeyTag(key)}
		if profile != "" {
			row.ProfileId = &profile
		}
		require.NoError(t, db.Create(&row).Error)
	}
	return channel
}

func TestSelectedKeyWearsItsEnvironment(t *testing.T) {
	db := setupDistributorTestDB(t)
	memoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = memoryCache })
	channel := createKeyEnvironmentChannel(t, db, 1401, "sk-a\nsk-b\nsk-c", map[string]string{
		"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64", "sk-c": "",
	})
	want := map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64"}

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		c, _ := newJSONTestContext(t, http.MethodPost, "/v1/responses", `{}`)
		require.Nil(t, SetupContextForSelectedChannel(c, channel, "gpt-5"))
		key := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
		require.Contains(t, want, key, "an unassigned key is never selected")
		seen[key] = true
		setting, ok := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
		require.True(t, ok)
		assert.Equal(t, want[key], setting.CLIVersionProfile)
		assert.Empty(t, setting.TLSFingerprint, "the key's version brings its own TLS")
		assert.Equal(t, model.ChannelKeyTag(key), common.GetContextKeyString(c, constant.ContextKeyChannelKeyTag))
	}
	assert.Len(t, seen, 2, "polling still rotates over the assigned keys")

	t.Run("a single-key copy wears its key's version", func(t *testing.T) {
		copied := *channel
		copied.Key = "sk-b"
		copied.ChannelInfo.IsMultiKey = false
		c, _ := newJSONTestContext(t, http.MethodPost, "/v1/responses", `{}`)
		common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 2)
		require.Nil(t, SetupContextForSelectedChannel(c, &copied, "gpt-5"))
		setting, _ := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
		assert.Equal(t, "codex-0.160.0-windows-x64", setting.CLIVersionProfile)
		assert.Equal(t, 0, common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex), "a single key leaves no index behind")
	})
	t.Run("a single-key copy of an unassigned key serves nothing", func(t *testing.T) {
		copied := *channel
		copied.Key = "sk-c"
		copied.ChannelInfo.IsMultiKey = false
		c, _ := newJSONTestContext(t, http.MethodPost, "/v1/responses", `{}`)
		err := SetupContextForSelectedChannel(c, &copied, "gpt-5")
		require.NotNil(t, err)
		assert.Equal(t, types.ErrorCodeChannelNoAvailableKey, err.GetErrorCode())
	})
	t.Run("a retry onto an ordinary channel drops the key tag and index", func(t *testing.T) {
		c, _ := newJSONTestContext(t, http.MethodPost, "/v1/responses", `{}`)
		require.Nil(t, SetupContextForSelectedChannel(c, channel, "gpt-5"))
		require.NotEmpty(t, common.GetContextKeyString(c, constant.ContextKeyChannelKeyTag))
		common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 1)
		ordinary := &model.Channel{Id: 1402, Type: constant.ChannelTypeOpenAI, Name: "plain", Status: common.ChannelStatusEnabled, Key: "sk-plain"}
		require.Nil(t, SetupContextForSelectedChannel(c, ordinary, "gpt-5"))
		assert.Empty(t, common.GetContextKeyString(c, constant.ContextKeyChannelKeyTag))
		assert.Equal(t, 0, common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex))
	})
}
