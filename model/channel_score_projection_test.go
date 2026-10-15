package model

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// openProjectionTestDB gives the test its own channels table and points the
// package DB at it, the same way openChannelSettingsTestDB does.
func openProjectionTestDB(t *testing.T, channels ...*Channel) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&Channel{}))

	previous := DB
	DB = db
	t.Cleanup(func() { DB = previous })

	for _, channel := range channels {
		require.NoError(t, db.Create(channel).Error)
	}
	return db
}

func enableScoringForTest(t *testing.T) {
	t.Helper()
	operation_setting.SetChannelDynamicScoreSettingForTest(operation_setting.ChannelDynamicScoreSetting{
		Enabled:                   true,
		SuccessesToPromote:        5,
		FaultsToDemote:            1,
		MaxPromoteTiers:           1,
		MaxDemoteTiers:            3,
		MinSampleForWeight:        1,
		SuccessWindowSeconds:      300,
		IdleResetSeconds:          1800,
		ProjectionIntervalMinutes: 10,
	})
	channel_score.ResetAll()
	t.Cleanup(func() {
		operation_setting.ResetChannelDynamicScoreSettingForTest()
		channel_score.ResetAll()
	})
}

func testChannel(id int, priority int64, weight uint) *Channel {
	return &Channel{
		Id:       id,
		Name:     fmt.Sprintf("ch-%d", id),
		Key:      "sk-test",
		Models:   "gpt-5.6",
		Group:    "default",
		Status:   common.ChannelStatusEnabled,
		Priority: &priority,
		Weight:   &weight,
	}
}

func reloadChannel(t *testing.T, id int) *Channel {
	t.Helper()
	channel := &Channel{}
	require.NoError(t, DB.First(channel, "id = ?", id).Error)
	return channel
}

// TestProjectionWritesTheDerivedColumnsAndNotTheBaseline is the core invariant: the
// number an operator sees moves, and the number they typed does not.
func TestProjectionWritesTheDerivedColumnsAndNotTheBaseline(t *testing.T) {
	openProjectionTestDB(t, testChannel(1, 0, 10), testChannel(2, 0, 10))
	enableScoringForTest(t)

	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)

	result, err := RunChannelScoreProjection()
	require.NoError(t, err)
	assert.Equal(t, 2, result.Scanned)
	assert.Equal(t, 1, result.Projected)
	assert.Equal(t, 1, result.Updated)

	demoted := reloadChannel(t, 1)
	require.NotNil(t, demoted.EffectivePriority, "the demoted channel must carry a projection")
	assert.Equal(t, int64(-1), *demoted.EffectivePriority,
		"demoting past the only tier lands below it")
	assert.Equal(t, int64(0), demoted.GetPriority(), "the baseline the admin typed is untouched")

	untouched := reloadChannel(t, 2)
	assert.Nil(t, untouched.EffectivePriority,
		"a channel with no score must stay NULL, not be written with its baseline")
	assert.Equal(t, int64(0), untouched.GetEffectivePriority(),
		"and must read back as its baseline")
}

// TestProjectionIsIdempotent is the property that makes an hourly, ten-minutely or
// missed run all equivalent, and is what stops the compounding walk that
// TestWritingShiftedPriorityBackCompounds demonstrates.
func TestProjectionIsIdempotent(t *testing.T) {
	openProjectionTestDB(t, testChannel(1, 0, 10), testChannel(2, 0, 10))
	enableScoringForTest(t)

	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)

	first, err := RunChannelScoreProjection()
	require.NoError(t, err)
	require.Equal(t, 1, first.Updated)
	after := *reloadChannel(t, 1).EffectivePriority

	for round := 0; round < 5; round++ {
		result, err := RunChannelScoreProjection()
		require.NoError(t, err)
		assert.Zero(t, result.Updated,
			"round %d rewrote an unchanged projection", round)
		assert.Equal(t, after, *reloadChannel(t, 1).EffectivePriority,
			"round %d moved a projection that had no new score", round)
	}
}

// TestProjectionClearsWhenScoringIsDisabled covers the case a stale mirror would
// misreport: routing has already reverted to the baseline, so the column must not
// keep showing an adjustment.
func TestProjectionClearsWhenScoringIsDisabled(t *testing.T) {
	openProjectionTestDB(t, testChannel(1, 0, 10))
	enableScoringForTest(t)

	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)
	_, err := RunChannelScoreProjection()
	require.NoError(t, err)
	require.NotNil(t, reloadChannel(t, 1).EffectivePriority)

	operation_setting.SetChannelDynamicScoreSettingForTest(operation_setting.ChannelDynamicScoreSetting{Enabled: false})
	result, err := RunChannelScoreProjection()
	require.NoError(t, err)
	assert.Equal(t, 1, result.Cleared)
	assert.Nil(t, reloadChannel(t, 1).EffectivePriority,
		"disabling scoring must clear the mirror, not freeze it")
}

// TestClearChannelProjectionOnAdminEditPath checks the hook the edit path relies
// on: the projected number cannot outlive the score it was derived from.
func TestClearChannelProjectionOnAdminEditPath(t *testing.T) {
	openProjectionTestDB(t, testChannel(1, 0, 10))
	enableScoringForTest(t)

	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)
	_, err := RunChannelScoreProjection()
	require.NoError(t, err)
	require.NotNil(t, reloadChannel(t, 1).EffectivePriority)

	ClearChannelProjection(1)
	assert.Nil(t, reloadChannel(t, 1).EffectivePriority)
	assert.Nil(t, reloadChannel(t, 1).EffectiveWeight)
}

// TestProjectionWriteDoesNotClobberConcurrentColumns is the lost-update guard. The
// job reads a channel row and writes seconds later; a full-struct save would carry
// every stale field back, reverting whatever else changed in between.
func TestProjectionWriteDoesNotClobberConcurrentColumns(t *testing.T) {
	openProjectionTestDB(t, testChannel(1, 0, 10))
	enableScoringForTest(t)

	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)

	// Another writer changes an unrelated column after the projection would have
	// read the row.
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 1).
		Update("name", "renamed-by-admin").Error)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 1).
		Update("models", "gpt-5.6,claude-opus-5").Error)

	_, err := RunChannelScoreProjection()
	require.NoError(t, err)

	stored := reloadChannel(t, 1)
	assert.Equal(t, "renamed-by-admin", stored.Name, "the projection reverted a concurrent edit")
	assert.Equal(t, "gpt-5.6,claude-opus-5", stored.Models, "the projection reverted a concurrent edit")
	assert.NotNil(t, stored.EffectivePriority, "and still wrote its own columns")
}

// TestProjectionSkipsDisabledChannels keeps the job from writing rows that cannot
// serve traffic, which would put a moving number on a channel an operator has
// switched off.
func TestProjectionSkipsDisabledChannels(t *testing.T) {
	disabled := testChannel(2, 0, 10)
	disabled.Status = common.ChannelStatusManuallyDisabled
	openProjectionTestDB(t, testChannel(1, 0, 10), disabled)
	enableScoringForTest(t)

	channel_score.Report(2, "default", "gpt-5.6", channel_score.OutcomeFault)

	result, err := RunChannelScoreProjection()
	require.NoError(t, err)
	assert.Equal(t, 1, result.Scanned, "only the enabled channel is scanned")
	assert.Nil(t, reloadChannel(t, 2).EffectivePriority)
}

// TestProjectionUsesBaselineTiersAcrossChannels pins the tier ladder to the
// baseline column. Built from the projected values instead, it would oscillate —
// see TestProjectionOscillatesIfTiersComeFromProjectedValues.
func TestProjectionUsesBaselineTiersAcrossChannels(t *testing.T) {
	openProjectionTestDB(t,
		testChannel(1, 100, 10),
		testChannel(2, 50, 10),
		testChannel(3, 0, 10),
	)
	enableScoringForTest(t)

	// Channel 1 faults once: one tier down from 100 in the ladder {100,50,0} is 50.
	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)
	_, err := RunChannelScoreProjection()
	require.NoError(t, err)
	require.Equal(t, int64(50), *reloadChannel(t, 1).EffectivePriority)

	// Running again must not walk it to 0: the ladder is rebuilt from the baselines,
	// where channel 1 is still at 100.
	for round := 0; round < 3; round++ {
		_, err := RunChannelScoreProjection()
		require.NoError(t, err)
		assert.Equal(t, int64(50), *reloadChannel(t, 1).EffectivePriority,
			"round %d walked the projection down a tier", round)
	}
}

// TestUpdateChannelStatusResetsScoreOnlyOnARealStatusChange: concurrent failures
// all try to disable the same channel, and a redundant enable can arrive for one
// that is already enabled. Neither changes the channel, so neither may discard
// the demotion and projection learned about it.
func TestUpdateChannelStatusResetsScoreOnlyOnARealStatusChange(t *testing.T) {
	openProjectionTestDB(t, testChannel(1, 0, 10))
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	require.NoError(t, reloadChannel(t, 1).AddAbilities(nil))
	enableScoringForTest(t)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)
	_, err := RunChannelScoreProjection()
	require.NoError(t, err)
	require.NotNil(t, reloadChannel(t, 1).EffectivePriority)

	assert.False(t, UpdateChannelStatus(1, "sk-test", common.ChannelStatusEnabled, ""))
	summaries, _ := channel_score.SummarizeByChannel()
	assert.Equal(t, 1, summaries[1].Adjusted, "a no-op status update must keep the demotion")
	assert.NotNil(t, reloadChannel(t, 1).EffectivePriority, "a no-op status update must keep the projection")

	assert.True(t, UpdateChannelStatus(1, "sk-test", common.ChannelStatusAutoDisabled, "401 unauthorized"))
	summaries, _ = channel_score.SummarizeByChannel()
	assert.Zero(t, summaries[1].Adjusted)
	assert.Nil(t, reloadChannel(t, 1).EffectivePriority)
}

// Failed persistence must leave every reader on the last committed state.
func TestUpdateChannelStatusPersistenceFailurePreservesRoutingState(t *testing.T) {
	cases := []struct {
		name     string
		cache    bool
		multiKey bool
		lastKey  bool
		failure  string
	}{
		{name: "database read", failure: "query"},
		{name: "cached read", cache: true, failure: "query"},
		{name: "database save", failure: "update"},
		{name: "cached save", cache: true, failure: "update"},
		{name: "database partial key save", multiKey: true, failure: "update"},
		{name: "cached partial key save", cache: true, multiKey: true, failure: "update"},
		{name: "database last key save", multiKey: true, lastKey: true, failure: "update"},
		{name: "cached last key save", cache: true, multiKey: true, lastKey: true, failure: "update"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := testChannel(1, 0, 10)
			usingKey := "sk-test"
			if tc.multiKey {
				channel.Key = "key-a\nkey-b"
				channel.ChannelInfo = ChannelInfo{IsMultiKey: true, MultiKeySize: 2}
				usingKey = "key-a"
				if tc.lastKey {
					channel.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusAutoDisabled}
				}
			}
			db := openProjectionTestDB(t, channel)
			require.NoError(t, db.AutoMigrate(&Ability{}))
			require.NoError(t, channel.AddAbilities(nil))
			enableScoringForTest(t)
			channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)
			_, err := RunChannelScoreProjection()
			require.NoError(t, err)
			before := reloadChannel(t, 1)
			require.NotNil(t, before.EffectivePriority)
			useChannelCacheForTest(t, []*Channel{before.CloneForCache()}, "default", "gpt-5.6")
			common.MemoryCacheEnabled = tc.cache

			callback := db.Callback().Update()
			if tc.failure == "query" {
				callback = db.Callback().Query()
			}
			active, injected := true, false
			require.NoError(t, callback.Before("gorm:"+tc.failure).Register("test:channel_status_failure", func(tx *gorm.DB) {
				if _, isChannel := tx.Statement.Dest.(*Channel); active && isChannel {
					injected = true
					tx.AddError(fmt.Errorf("injected channel %s failure", tc.failure))
				}
			}))
			t.Cleanup(func() { require.NoError(t, callback.Remove("test:channel_status_failure")) })

			assert.False(t, UpdateChannelStatus(1, usingKey, common.ChannelStatusAutoDisabled, "upstream rejected key"))
			active = false
			require.True(t, injected, "the persistence boundary must have been exercised")

			after := reloadChannel(t, 1)
			assert.Equal(t, common.ChannelStatusEnabled, after.Status)
			assert.Equal(t, before.ChannelInfo, after.ChannelInfo)
			assert.Equal(t, before.OtherInfo, after.OtherInfo)
			assert.Equal(t, before.EffectivePriority, after.EffectivePriority)
			assert.Equal(t, before.EffectiveWeight, after.EffectiveWeight)
			summaries, _ := channel_score.SummarizeByChannel()
			assert.Equal(t, 1, summaries[1].Adjusted, "a failed write must retain the demotion")
			var ability Ability
			require.NoError(t, db.First(&ability, "channel_id = ?", 1).Error)
			assert.True(t, ability.Enabled)
			cached, err := CacheGetChannel(1)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
			assert.Equal(t, before.ChannelInfo, cached.ChannelInfo)
			assert.True(t, IsChannelEnabledForGroupModel("default", "gpt-5.6", 1), "failed writes must not remove a live route")
		})
	}
}

func TestUpdateChannelStatusMultiKeyLifecycle(t *testing.T) {
	for _, cacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cacheEnabled), func(t *testing.T) {
			channel := testChannel(1, 0, 10)
			channel.Key = "key-a\nkey-b"
			channel.ChannelInfo = ChannelInfo{IsMultiKey: true, MultiKeySize: 2}
			db := openProjectionTestDB(t, channel)
			require.NoError(t, db.AutoMigrate(&Ability{}))
			require.NoError(t, channel.AddAbilities(nil))
			enableScoringForTest(t)
			channel_score.Report(1, "default", "gpt-5.6", channel_score.OutcomeFault)
			_, err := RunChannelScoreProjection()
			require.NoError(t, err)
			useChannelCacheForTest(t, []*Channel{reloadChannel(t, 1)}, "default", "gpt-5.6")
			common.MemoryCacheEnabled = cacheEnabled

			require.True(t, UpdateChannelStatus(1, "key-a", common.ChannelStatusAutoDisabled, "upstream rejected key"))
			stored := reloadChannel(t, 1)
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled}, stored.ChannelInfo.MultiKeyStatusList)
			assert.NotNil(t, stored.EffectivePriority)
			summaries, _ := channel_score.SummarizeByChannel()
			assert.Equal(t, 1, summaries[1].Adjusted, "one disabled key must not erase channel-level scores")
			cached, err := CacheGetChannel(1)
			require.NoError(t, err)
			key, _, keyErr := cached.GetNextEnabledKey()
			require.Nil(t, keyErr)
			assert.Equal(t, "key-b", key)
			assert.True(t, IsChannelEnabledForGroupModel("default", "gpt-5.6", 1))

			// An enabled channel can still contain a disabled key that needs restoring.
			require.True(t, UpdateChannelStatus(1, "key-a", common.ChannelStatusEnabled, ""))
			assert.Empty(t, reloadChannel(t, 1).ChannelInfo.MultiKeyStatusList)
			assert.False(t, UpdateChannelStatus(1, "key-a", common.ChannelStatusEnabled, ""), "restoring an already enabled key is a no-op")
			cached, err = CacheGetChannel(1)
			require.NoError(t, err)
			key, _, keyErr = cached.GetNextEnabledKey()
			require.Nil(t, keyErr)
			assert.Equal(t, "key-a", key)
			summaries, _ = channel_score.SummarizeByChannel()
			assert.Equal(t, 1, summaries[1].Adjusted)

			require.True(t, UpdateChannelStatus(1, "key-a", common.ChannelStatusAutoDisabled, "upstream rejected key"))
			require.True(t, UpdateChannelStatus(1, "key-b", common.ChannelStatusAutoDisabled, "upstream rejected key"))
			stored = reloadChannel(t, 1)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
			assert.Nil(t, stored.EffectivePriority)
			assert.Nil(t, stored.EffectiveWeight)
			summaries, _ = channel_score.SummarizeByChannel()
			assert.Zero(t, summaries[1].Adjusted)
			assert.False(t, IsChannelEnabledForGroupModel("default", "gpt-5.6", 1))

			require.True(t, UpdateChannelStatus(1, "key-b", common.ChannelStatusEnabled, ""))
			stored = reloadChannel(t, 1)
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled}, stored.ChannelInfo.MultiKeyStatusList)
			cached, err = CacheGetChannel(1)
			require.NoError(t, err)
			key, _, keyErr = cached.GetNextEnabledKey()
			require.Nil(t, keyErr)
			assert.Equal(t, "key-b", key)
			assert.True(t, IsChannelEnabledForGroupModel("default", "gpt-5.6", 1), "a restored key must immediately restore the route")
		})
	}
}
