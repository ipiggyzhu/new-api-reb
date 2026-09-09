package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func TestMigrateChannelClientProfilesPreservesSettings(t *testing.T) {
	openChannelSettingsTestDB(t)
	const automatic = `{"synthetic_client_headers_profile":"auto","synthetic_client_headers":true,"proxy":"http://proxy.local","system_prompt":"keep prompt","future":{"number":9007199254740993,"enabled":false}}`
	tests := []struct {
		channelType int
		setting     string
		want        string
		unchanged   bool
	}{
		{constant.ChannelTypeOpenAI, automatic, "openai", false},
		{constant.ChannelTypeAnthropic, automatic, "claude", false},
		{constant.ChannelTypeAws, automatic, "claude", false},
		{constant.ChannelTypeVertexAi, automatic, "claude", false},
		{constant.ChannelTypeCodex, automatic, "codex", false},
		{constant.ChannelTypeGemini, automatic, "gemini", false},
		{constant.ChannelTypeOpenRouter, automatic, "generic", false},
		{constant.ChannelTypeAzure, automatic, "openai", false},
		{constant.ChannelTypeAnthropic, `{"synthetic_client_headers":true}`, "claude", false},
		{constant.ChannelTypeAnthropic, `{"synthetic_client_headers_profile":"unknown"}`, "claude", false},
		{constant.ChannelTypeOpenAI, `{"synthetic_client_headers_profile":"codex","synthetic_client_headers":true}`, "codex", true},
		{constant.ChannelTypeOpenAI, `{"synthetic_client_headers_profile":"","synthetic_client_headers":false}`, "", true},
		{constant.ChannelTypeOpenAI, `{"synthetic_client_headers_profile":"off","synthetic_client_headers":true}`, "", false},
		{constant.ChannelTypeOpenAI, `{broken`, "", true},
		{constant.ChannelTypeOpenAI, `null`, "", true},
	}
	for i, tc := range tests {
		channel := Channel{Id: 100 + i, Type: tc.channelType, Name: "keep name", Key: "keep key", Status: 2, Setting: common.GetPointer(tc.setting)}
		require.NoError(t, DB.Create(&channel).Error)
	}
	var writes int
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("count_profile_migration", func(tx *gorm.DB) {
		writes++
	}))
	require.NoError(t, migrateChannelClientHeaderProfiles(DB))
	assert.Equal(t, 11, writes)
	for i, tc := range tests {
		stored, err := GetChannelById(100+i, true)
		require.NoError(t, err)
		assert.Equal(t, "keep key", stored.Key)
		assert.Equal(t, "keep name", stored.Name)
		assert.Equal(t, 2, stored.Status, "disabled channels also need their old setting migrated")
		assert.Equal(t, tc.channelType, stored.Type)
		if tc.unchanged {
			assert.Equal(t, tc.setting, *stored.Setting)
			continue
		}
		assert.Equal(t, tc.want, gjson.Get(*stored.Setting, "synthetic_client_headers_profile").String())
		assert.Equal(t, tc.want != "", gjson.Get(*stored.Setting, "synthetic_client_headers").Bool())
		if tc.setting == automatic {
			assert.Equal(t, "http://proxy.local", gjson.Get(*stored.Setting, "proxy").String())
			assert.Equal(t, "keep prompt", gjson.Get(*stored.Setting, "system_prompt").String())
			assert.JSONEq(t, `{"number":9007199254740993,"enabled":false}`, gjson.Get(*stored.Setting, "future").Raw)
			assert.Equal(t, "9007199254740993", gjson.Get(*stored.Setting, "future.number").Raw)
		}
	}
	writes = 0
	require.NoError(t, migrateChannelClientHeaderProfiles(DB))
	assert.Zero(t, writes, "a repeated migration must not rewrite any channel")
}

func TestClientProfileMigrationKeepsConcurrentAdminChange(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	channel.Type = constant.ChannelTypeOpenAI
	channel.Setting = common.GetPointer(`{"synthetic_client_headers_profile":"auto"}`)
	require.NoError(t, DB.Save(channel).Error)
	const adminSettings = `{"synthetic_client_headers_profile":"codex","synthetic_client_headers":true,"system_prompt":"new prompt"}`
	var edited bool
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("edit_before_profile_migration", func(tx *gorm.DB) {
		if edited {
			return
		}
		edited = true
		err := DB.Model(&Channel{}).Where("id = ?", channel.Id).UpdateColumn("setting", adminSettings).Error
		if err != nil {
			tx.AddError(err)
		}
	}))
	require.NoError(t, migrateChannelClientHeaderProfiles(DB))
	require.True(t, edited)
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, adminSettings, *stored.Setting)
}

func TestChannelWritesMaterializeLegacyClientProfiles(t *testing.T) {
	operations := []struct {
		name    string
		create  bool
		partial bool
		write   func(*Channel) error
	}{
		{"insert", true, false, (*Channel).Insert},
		{"batch insert", true, false, func(channel *Channel) error { return BatchInsertChannels([]Channel{*channel}) }},
		{"save", false, false, (*Channel).Save},
		{"save without key", false, false, (*Channel).SaveWithoutKey},
		{"partial update", false, true, (*Channel).Update},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			channel := openChannelSettingsTestDB(t)
			require.NoError(t, DB.AutoMigrate(&Ability{}))
			channel.Type = constant.ChannelTypeAnthropic
			require.NoError(t, DB.Save(channel).Error)
			if operation.create {
				channel.Id = 0
				channel.Name = "new profile channel"
			}
			if operation.partial {
				channel.Type = 0
			}
			channel.Setting = common.GetPointer(`{"synthetic_client_headers_profile":"auto","future":{"retained":true},"pass_through_body_enabled":true}`)
			require.NoError(t, operation.write(channel))
			var stored Channel
			require.NoError(t, DB.Where("name = ?", channel.Name).First(&stored).Error)
			assert.Equal(t, constant.ChannelTypeAnthropic, stored.Type)
			assert.Equal(t, "claude", gjson.Get(*stored.Setting, "synthetic_client_headers_profile").String())
			assert.True(t, gjson.Get(*stored.Setting, "synthetic_client_headers").Bool())
			assert.True(t, gjson.Get(*stored.Setting, "future.retained").Bool())
			assert.True(t, gjson.Get(*stored.Setting, "pass_through_body_enabled").Bool())
			assert.Equal(t, "sk-test", stored.Key)
		})
	}
}

func TestGetLegacyClientProfileDoesNotMutateSharedChannel(t *testing.T) {
	const raw = `{"synthetic_client_headers_profile":"auto"}`
	channel := &Channel{Type: constant.ChannelTypeAnthropic, Setting: common.GetPointer(raw)}
	assert.Equal(t, "claude", channel.GetSetting().SyntheticClientHeadersProfile)
	assert.Equal(t, raw, *channel.Setting)
}

func TestChannelStatusUpdatePreservesInvalidClientSettings(t *testing.T) {
	for _, memoryCache := range []bool{false, true} {
		name := "without cache"
		if memoryCache {
			name = "with cache"
		}
		t.Run(name, func(t *testing.T) {
			channel := openChannelSettingsTestDB(t)
			require.NoError(t, DB.AutoMigrate(&Ability{}))
			setupChannelCacheTest(t)
			common.MemoryCacheEnabled = memoryCache
			const raw = `{broken`
			channel.Type = constant.ChannelTypeOpenAI
			channel.Setting = common.GetPointer(raw)
			require.NoError(t, DB.Save(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			if memoryCache {
				InitChannelCache()
			}

			require.True(t, UpdateChannelStatus(channel.Id, channel.Key, common.ChannelStatusAutoDisabled, "401 unauthorized"))
			stored, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
			assert.Equal(t, raw, *stored.Setting)
			assert.Equal(t, channel.Key, stored.Key)
			assert.Equal(t, "401 unauthorized", stored.GetOtherInfo()["status_reason"])
			var ability Ability
			require.NoError(t, DB.Where("channel_id = ?", channel.Id).First(&ability).Error)
			assert.False(t, ability.Enabled)
			if memoryCache {
				cached, err := CacheGetChannel(channel.Id)
				require.NoError(t, err)
				assert.Equal(t, stored.Status, cached.Status)
				assert.False(t, IsChannelEnabledForGroupModel(channel.Group, channel.Models, channel.Id))
			}
		})
	}
}

func TestChannelSavePreservesInvalidClientSettings(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	const raw = `{"synthetic_client_headers":"invalid legacy value"}`
	channel.Setting = common.GetPointer(raw)
	require.NoError(t, DB.Save(channel).Error)
	channel.Name = "renamed channel"
	require.NoError(t, channel.Save())
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, channel.Name, stored.Name)
	assert.Equal(t, raw, *stored.Setting)
}

func TestChannelClientProfileUpdatePreservesLookupFailure(t *testing.T) {
	openChannelSettingsTestDB(t)
	channel := &Channel{Id: 999, Setting: common.GetPointer(`{"synthetic_client_headers_profile":"auto"}`)}
	assert.ErrorIs(t, channel.Update(), gorm.ErrRecordNotFound)
}
