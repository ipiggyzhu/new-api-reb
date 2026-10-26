package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupKeyEnvironmentTable(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&ChannelKeyEnvironment{}))
	clear := func() {
		require.NoError(t, DB.Exec("DELETE FROM channel_key_environments").Error)
		require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
		require.NoError(t, DB.Exec("DELETE FROM channels").Error)
	}
	clear()
	t.Cleanup(clear)
}

func keyEnvironmentRowFor(channelId int, key string, profileId string) ChannelKeyEnvironment {
	row := ChannelKeyEnvironment{ChannelId: channelId, KeyTag: ChannelKeyTag(key)}
	if profileId != "" {
		row.ProfileId = &profileId
	}
	return row
}

func keyEnvironmentChannelIds(t *testing.T) []int {
	t.Helper()
	var ids []int
	require.NoError(t, DB.Model(&ChannelKeyEnvironment{}).Distinct().Order("channel_id").Pluck("channel_id", &ids).Error)
	return ids
}

func TestChannelKeyEnvironmentConstraints(t *testing.T) {
	setupKeyEnvironmentTable(t)
	require.NoError(t, DB.Create([]ChannelKeyEnvironment{
		keyEnvironmentRowFor(1, "sk-1", "codex-0.163.0-linux-x64"),
		keyEnvironmentRowFor(1, "sk-2", ""),
		keyEnvironmentRowFor(1, "sk-3", ""),
		keyEnvironmentRowFor(2, "sk-4", "codex-0.163.0-linux-x64"),
	}).Error, "unassigned rows repeat and other channels reuse a version")

	heldKey := keyEnvironmentRowFor(2, "sk-1", "")
	assert.Error(t, DB.Create(&heldKey).Error, "a key belongs to one channel")
	takenVersion := keyEnvironmentRowFor(1, "sk-5", "codex-0.163.0-linux-x64")
	assert.Error(t, DB.Create(&takenVersion).Error, "a version serves one key per channel")
	assert.Equal(t, ChannelKeyTag("sk-1"), ChannelKeyTag(" sk-1\r\n"), "the tag ignores surrounding whitespace")
}

func TestChannelDeletePathsDropKeyEnvironments(t *testing.T) {
	cases := []struct {
		name   string
		delete func(ids []int) error
	}{
		{name: "single delete", delete: func(ids []int) error { return (&Channel{Id: ids[0]}).Delete() }},
		{name: "batch delete", delete: func(ids []int) error { return BatchDeleteChannels(ids[:1]) }},
		{name: "delete by status", delete: func(ids []int) error {
			_, err := DeleteChannelByStatus(common.ChannelStatusAutoDisabled)
			return err
		}},
		{name: "delete disabled", delete: func(ids []int) error {
			_, err := DeleteDisabledChannel()
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupKeyEnvironmentTable(t)
			doomed := &Channel{Name: "doomed", Key: "sk-doomed", Status: common.ChannelStatusAutoDisabled}
			kept := &Channel{Name: "kept", Key: "sk-kept", Status: common.ChannelStatusEnabled}
			require.NoError(t, DB.Create(doomed).Error)
			require.NoError(t, DB.Create(kept).Error)
			require.NoError(t, DB.Create([]ChannelKeyEnvironment{
				keyEnvironmentRowFor(doomed.Id, "sk-doomed", "codex-0.163.0-linux-x64"),
				keyEnvironmentRowFor(kept.Id, "sk-kept", "codex-0.163.0-linux-x64"),
			}).Error)

			require.NoError(t, tc.delete([]int{doomed.Id}))
			assert.Equal(t, []int{kept.Id}, keyEnvironmentChannelIds(t))
		})
	}
}

func TestKeyEnvironmentProfileFollowsTheSnapshot(t *testing.T) {
	setupChannelCacheTest(t)
	setupKeyEnvironmentTable(t)
	t.Cleanup(func() { installChannelKeyEnvironments(nil) })

	channel := mustCreateCachedChannel(t, "key-env", "default", "gpt-5", 0)
	channel.ChannelInfo.IsMultiKey = true
	channel.Key = "sk-a\nsk-b"
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex", KeyEnvironments: true})
	require.NoError(t, DB.Save(channel).Error)
	require.NoError(t, DB.Create([]ChannelKeyEnvironment{
		keyEnvironmentRowFor(channel.Id, "sk-a", "codex-0.163.0-linux-x64"),
		keyEnvironmentRowFor(channel.Id, "sk-b", ""),
	}).Error)
	InitChannelCache()

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	profile, ok := cached.KeyEnvironmentProfile("sk-a")
	assert.True(t, ok)
	assert.Equal(t, "codex-0.163.0-linux-x64", profile)
	_, ok = cached.KeyEnvironmentProfile("sk-b")
	assert.False(t, ok, "an unassigned key has no profile")
	_, ok = cached.KeyEnvironmentProfile("sk-unknown")
	assert.False(t, ok, "a key outside the table has no profile")

	off := *cached
	off.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex"})
	_, ok = off.KeyEnvironmentProfile("sk-a")
	assert.False(t, ok, "rows are ignored once the switch is off")
}

// Only ReconcileKeyEnvironments writes the table. A binary that predates it
// still saves channels, rewrites the setting without the switch and disables
// keys, and none of that may touch the rows, so switching back on restores the
// old assignments.
func TestChannelWritesOutsideReconcileLeaveKeyEnvironmentsAlone(t *testing.T) {
	setupChannelCacheTest(t)
	setupKeyEnvironmentTable(t)
	t.Cleanup(func() { installChannelKeyEnvironments(nil) })

	channel := mustCreateCachedChannel(t, "key-env-legacy", "default", "gpt-5", 0)
	channel.ChannelInfo.IsMultiKey = true
	channel.Key = "sk-a\nsk-b"
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex", KeyEnvironments: true})
	require.NoError(t, DB.Save(channel).Error)
	require.NoError(t, DB.Create([]ChannelKeyEnvironment{
		keyEnvironmentRowFor(channel.Id, "sk-a", "codex-0.163.0-linux-x64"),
		keyEnvironmentRowFor(channel.Id, "sk-b", "codex-0.160.0-windows-x64"),
	}).Error)
	var before []ChannelKeyEnvironment
	require.NoError(t, DB.Order("id").Find(&before).Error)

	channel.Name = "key-env-legacy-renamed"
	channel.Setting = common.GetPointer(`{"synthetic_client_headers_profile":"codex"}`)
	require.NoError(t, channel.Update())
	UpdateChannelStatus(channel.Id, "sk-b", common.ChannelStatusAutoDisabled, "quota exhausted")

	var after []ChannelKeyEnvironment
	require.NoError(t, DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after)

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.False(t, stored.GetSetting().KeyEnvironments, "the setting was rewritten without the switch")
	require.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[1], "the key was disabled")
	stored.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex", KeyEnvironments: true})
	require.NoError(t, DB.Model(stored).Update("setting", stored.Setting).Error)
	InitChannelCache()

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	for key, want := range map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64"} {
		profile, ok := cached.KeyEnvironmentProfile(key)
		assert.True(t, ok, key)
		assert.Equal(t, want, profile, key)
	}
}

func TestEnabledKeyIndexServesOnlyAssignedEnabledKeys(t *testing.T) {
	setupKeyEnvironmentTable(t)
	memoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = memoryCache })

	channel := &Channel{Id: 8101, Name: "task-keys", Key: "sk-a\nsk-b\nsk-c", ChannelInfo: ChannelInfo{
		IsMultiKey: true, MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled},
	}}
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex", KeyEnvironments: true})
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, DB.Create([]ChannelKeyEnvironment{
		keyEnvironmentRowFor(channel.Id, "sk-a", "codex-0.163.0-linux-x64"),
		keyEnvironmentRowFor(channel.Id, "sk-b", "codex-0.160.0-windows-x64"),
		keyEnvironmentRowFor(channel.Id, "sk-c", ""),
	}).Error)

	for key, want := range map[string]int{"sk-a": 0, "sk-b": -1, "sk-c": -1, "sk-gone": -1, "": -1} {
		assert.Equal(t, want, channel.EnabledKeyIndex(key), key)
	}
	single := *channel
	single.Key, single.ChannelInfo = "sk-a", ChannelInfo{}
	assert.Equal(t, 0, single.EnabledKeyIndex("sk-a"))
	single.Key = "sk-c"
	assert.Equal(t, -1, single.EnabledKeyIndex("sk-c"), "an unassigned single key cannot serve")
}

// Keepalive, model probing, ratio sync, upstream model listing and task
// resubmission all pick keys through these two selectors, so an unassigned key
// never reaches any of them.
func TestKeySelectorsNeverPickAnUnassignedKey(t *testing.T) {
	setupKeyEnvironmentTable(t)
	memoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = memoryCache })

	for _, mode := range []constant.MultiKeyMode{constant.MultiKeyModePolling, constant.MultiKeyModeRandom} {
		t.Run(string(mode), func(t *testing.T) {
			require.NoError(t, DB.Exec("DELETE FROM channel_key_environments").Error)
			require.NoError(t, DB.Exec("DELETE FROM channels").Error)
			channel := &Channel{Id: 8201, Name: "selectors", Key: "sk-a\nsk-b\nsk-c\nsk-d", ChannelInfo: ChannelInfo{
				IsMultiKey: true, MultiKeyMode: mode,
			}}
			channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex", KeyEnvironments: true})
			require.NoError(t, DB.Create(channel).Error)
			require.NoError(t, DB.Create([]ChannelKeyEnvironment{
				keyEnvironmentRowFor(channel.Id, "sk-a", "codex-0.163.0-linux-x64"),
				keyEnvironmentRowFor(channel.Id, "sk-b", ""),
				keyEnvironmentRowFor(channel.Id, "sk-c", "codex-0.160.0-windows-x64"),
			}).Error)
			assigned := []string{"sk-a", "sk-c"}

			picked := map[string]bool{}
			for i := 0; i < 16; i++ {
				key, _, apiErr := channel.GetNextEnabledKey("")
				require.Nil(t, apiErr)
				picked[key] = true
				key, _, apiErr = channel.GetEnabledKeyForSeed(fmt.Sprintf("keepalive:%d", i), "")
				require.Nil(t, apiErr)
				picked[key] = true
			}
			assert.ElementsMatch(t, assigned, lo.Keys(picked), "only assigned keys are picked, and both of them")

			channel.ChannelInfo.MultiKeyStatusList = map[int]int{0: common.ChannelStatusManuallyDisabled, 2: common.ChannelStatusAutoDisabled}
			_, _, apiErr := channel.GetNextEnabledKey("")
			require.NotNil(t, apiErr, "with every assigned key disabled the unassigned ones do not stand in")
			assert.Equal(t, types.ErrorCodeChannelNoAvailableKey, apiErr.GetErrorCode())
			_, _, apiErr = channel.GetEnabledKeyForSeed("keepalive:0", "")
			require.NotNil(t, apiErr)
			assert.Equal(t, types.ErrorCodeChannelNoAvailableKey, apiErr.GetErrorCode())
		})
	}
}
