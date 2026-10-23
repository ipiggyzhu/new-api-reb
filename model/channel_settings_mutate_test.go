package model

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func TestApplyChannelModelSweepAtomicAndConcurrentChanges(t *testing.T) {
	for _, scenario := range []string{"empty", "disabled", "edited", "organization edited", "key disabled", "settings edited", "ability write fails"} {
		t.Run(scenario, func(t *testing.T) {
			channel := openChannelSettingsTestDB(t)
			require.NoError(t, DB.AutoMigrate(&Ability{}))
			channel.Status = common.ChannelStatusEnabled
			require.NoError(t, DB.Save(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			switch scenario {
			case "disabled":
				require.NoError(t, DB.Model(channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "edited":
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("models", "operator-model").Error)
			case "organization edited":
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("open_ai_organization", "new-organization").Error)
			case "key disabled":
				info := channel.ChannelInfo
				info.MultiKeyStatusList = map[int]int{0: common.ChannelStatusManuallyDisabled}
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("channel_info", info).Error)
			case "settings edited":
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("settings", `{"azure_responses_version":"2026-10-01"}`).Error)
			case "ability write fails":
				require.NoError(t, DB.Callback().Delete().Before("gorm:delete").Register("fail-sweep-abilities", func(tx *gorm.DB) {
					if tx.Statement.Table == "abilities" {
						tx.AddError(errors.New("injected ability delete failure"))
					}
				}))
			}
			applied, err := ApplyChannelModelSweep(channel, "", nil, true)
			stored, loadErr := GetChannelById(channel.Id, true)
			require.NoError(t, loadErr)
			var abilities []Ability
			require.NoError(t, DB.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
			switch scenario {
			case "empty":
				require.NoError(t, err)
				assert.True(t, applied)
				assert.Empty(t, stored.Models)
				assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
				assert.NotEmpty(t, stored.GetOtherInfo()["status_reason"])
				assert.Empty(t, abilities)
				assert.Contains(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth, "gpt-4o")
				require.NoError(t, stored.AddAbilities(nil))
				var count int64
				require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).Count(&count).Error)
				assert.Zero(t, count, "rebuilding empty-channel abilities must not create an empty-model route")
			case "disabled":
				require.NoError(t, err)
				assert.False(t, applied)
				assert.Equal(t, "gpt-4o", stored.Models)
				assert.Len(t, abilities, 1)
			case "edited":
				require.ErrorContains(t, err, "configuration changed")
				assert.False(t, applied)
				assert.Equal(t, "operator-model", stored.Models)
			case "organization edited", "key disabled", "settings edited":
				require.ErrorContains(t, err, "configuration changed")
				assert.False(t, applied)
				assert.Equal(t, "gpt-4o", stored.Models)
			case "ability write fails":
				require.ErrorContains(t, err, "injected ability delete failure")
				assert.False(t, applied)
				assert.Equal(t, "gpt-4o", stored.Models, "model write must roll back with failed ability write")
				assert.Len(t, abilities, 1)
				assert.Empty(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth)
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status, "disable must roll back with the model and ability changes")
				assert.Empty(t, stored.GetOtherInfo()["status_reason"])
			}
		})
	}
}

// openChannelSettingsTestDB gives each test its own channels table with one row,
// and points the package-level DB at it for the duration of the test.
func openChannelSettingsTestDB(t *testing.T) *Channel {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	// A shared-cache in-memory database lives as long as one connection to it is
	// open, so the pool has to close or the schema leaks into the next test in
	// this process.
	t.Cleanup(func() {
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&Channel{}))

	previous := DB
	DB = db
	t.Cleanup(func() { DB = previous })

	channel := &Channel{
		Id:     7,
		Name:   "settings-race",
		Key:    "sk-test",
		Models: "gpt-4o",
		Group:  "default",
	}
	require.NoError(t, db.Create(channel).Error)
	return channel
}

// A model hidden by the retired availability quarantine must become routable and
// visible in the model plaza again, while unrelated settings survive the repair.
func TestRepairChannelModelAvailabilityRestoresListedModels(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	channel.Status = common.ChannelStatusEnabled
	channel.Models = "gpt-4o,gpt-4o-mini"
	channel.OtherSettings = `{"model_availability":{"gpt-4o":{"state":"temporary","checked_at":1}},"upstream_model_update_last_removed_models":["gpt-4o"],"upstream_model_update_last_check_time":42,"future":{"kept":true}}`
	require.NoError(t, DB.Save(channel).Error)
	// The quarantine built abilities for gpt-4o-mini only.
	require.NoError(t, DB.Create(&Ability{Group: "default", Model: "gpt-4o-mini", ChannelId: channel.Id, Enabled: true}).Error)
	require.NotContains(t, GetEnabledModels(), "gpt-4o")

	require.NoError(t, repairChannelModelAvailability(DB))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o,gpt-4o-mini", stored.Models)
	assert.False(t, gjson.Get(stored.OtherSettings, "model_availability").Exists())
	assert.False(t, gjson.Get(stored.OtherSettings, "upstream_model_update_last_removed_models").Exists())
	assert.Equal(t, int64(42), gjson.Get(stored.OtherSettings, "upstream_model_update_last_check_time").Int())
	assert.True(t, gjson.Get(stored.OtherSettings, "future.kept").Bool())
	assert.ElementsMatch(t, []string{"gpt-4o", "gpt-4o-mini"}, GetEnabledModels())
	snapshot, ok := buildChannelCacheSnapshot()
	require.True(t, ok)
	assert.Contains(t, snapshot.group2model2channels["default"]["gpt-4o"], channel.Id)

	var writes int
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("count_availability_repair", func(tx *gorm.DB) {
		writes++
	}))
	require.NoError(t, repairChannelModelAvailability(DB))
	assert.Zero(t, writes, "a repeated repair must not rewrite any channel")
}

func TestRepairChannelModelAvailabilityRemovesLegacyDeletionQueueAlone(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	channel.OtherSettings = `{"upstream_model_update_last_removed_models":["gpt-4o"],"future":{"kept":true}}`
	require.NoError(t, DB.Save(channel).Error)
	require.NoError(t, repairChannelModelAvailability(DB))
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.False(t, gjson.Get(stored.OtherSettings, "upstream_model_update_last_removed_models").Exists())
	assert.True(t, gjson.Get(stored.OtherSettings, "future.kept").Bool())
}

// The keepalive task stamps its send time into `settings` on its own schedule.
// A sweep that spans one of those stamps must still apply, and must keep the
// stamp, while a real settings edit during the sweep is still refused.
func TestApplyChannelModelSweepToleratesKeepaliveStamp(t *testing.T) {
	for _, scenario := range []string{"keepalive stamped", "keepalive stamped and settings edited"} {
		t.Run(scenario, func(t *testing.T) {
			channel := openChannelSettingsTestDB(t)
			require.NoError(t, DB.AutoMigrate(&Ability{}))
			channel.Status = common.ChannelStatusEnabled
			channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamKeepaliveEnabled: true, UpstreamKeepaliveLastTime: 100})
			require.NoError(t, DB.Save(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))

			require.NoError(t, MutateChannelSettings(channel.Id, func(settings *dto.ChannelOtherSettings) bool {
				settings.UpstreamKeepaliveLastTime = 200
				if scenario == "keepalive stamped and settings edited" {
					settings.UpstreamKeepaliveIntervalMinutes = 60
				}
				return true
			}))

			applied, err := ApplyChannelModelSweep(channel, "gpt-4o-mini", nil, false)
			stored, loadErr := GetChannelById(channel.Id, true)
			require.NoError(t, loadErr)
			if scenario == "keepalive stamped" {
				require.NoError(t, err)
				assert.True(t, applied)
				assert.Equal(t, "gpt-4o-mini", stored.Models)
				assert.Equal(t, int64(200), stored.GetOtherSettings().UpstreamKeepaliveLastTime)
				return
			}
			require.ErrorContains(t, err, "configuration changed")
			assert.False(t, applied)
			assert.Equal(t, "gpt-4o", stored.Models)
		})
	}
}

func TestApplyChannelModelSweepClearsLegacyStateWithoutCatalogueChange(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	channel.Status = common.ChannelStatusEnabled
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateLastRemovedModels:   []string{"gpt-4o"},
		UpstreamModelUpdateLastDetectedModels:  []string{"gpt-4o", "candidate"},
		UpstreamModelUpdatePendingModelMapping: map[string]string{"gpt-4o": "vendor/gpt-4o", "candidate": "vendor/candidate"},
	})
	require.NoError(t, DB.Save(channel).Error)
	applied, err := ApplyChannelModelSweep(channel, channel.Models, nil, false)
	require.NoError(t, err)
	require.True(t, applied)
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	settings := stored.GetOtherSettings()
	assert.Empty(t, settings.UpstreamModelUpdateLastRemovedModels)
	assert.Equal(t, []string{"candidate"}, settings.UpstreamModelUpdateLastDetectedModels)
	assert.Equal(t, map[string]string{"candidate": "vendor/candidate"}, settings.UpstreamModelUpdatePendingModelMapping)
}

// Only a confirmed failure changes routing, and it does so by deleting the model
// from the channel: database, abilities and cache always agree with Models.
func TestApplyChannelModelSweepRemovedModelLeavesEveryRoute(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	channel.Status = common.ChannelStatusEnabled
	channel.Models = "gpt-4o,gpt-4o-mini"
	require.NoError(t, DB.Save(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	applied, err := ApplyChannelModelSweep(channel, "gpt-4o-mini", nil, false)
	require.NoError(t, err)
	require.True(t, applied)

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o-mini", stored.Models)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, []string{"gpt-4o-mini"}, GetEnabledModels())
	assert.Contains(t, stored.GetOtherSettings().UpstreamModelUpdateModelHealth, "gpt-4o", "removed model stays a retry candidate")
	snapshot, ok := buildChannelCacheSnapshot()
	require.True(t, ok)
	assert.Empty(t, snapshot.group2model2channels["default"]["gpt-4o"])
	assert.Contains(t, snapshot.group2model2channels["default"]["gpt-4o-mini"], channel.Id)
}

// An enabled channel emptied by the sweep must not keep a synthetic empty-model
// route in the live cache while abilities correctly reports no routes.
func TestApplyChannelModelSweepEmptyChannelHasNoRoutes(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	setupChannelCacheTest(t)
	channel.Status = common.ChannelStatusEnabled
	require.NoError(t, DB.Save(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	InitChannelCache()

	applied, err := ApplyChannelModelSweep(channel, "", nil, false)
	require.NoError(t, err)
	require.True(t, applied)

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Empty(t, cached.Models)
	assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
	assert.Empty(t, GetEnabledModels())
	for _, phase := range []string{"sweep", "cache re-enable"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "cache re-enable" {
				CacheUpdateChannelStatus(channel.Id, common.ChannelStatusAutoDisabled)
				CacheUpdateChannelStatus(channel.Id, common.ChannelStatusEnabled)
			}
			for _, modelName := range []string{"gpt-4o", ""} {
				picked, err := GetRandomSatisfiedChannel("default", modelName, 0, "", nil)
				require.NoError(t, err)
				assert.True(t, picked == nil, "an empty channel must not serve model %q", modelName)
			}
		})
	}
}

func storedSettings(t *testing.T, channelId int) dto.ChannelOtherSettings {
	t.Helper()
	stored, err := GetChannelById(channelId, true)
	require.NoError(t, err)
	return stored.GetOtherSettings()
}

// TestMutateChannelSettingsKeepsConcurrentWritersFields is the regression test for
// the lost update. Two writers own disjoint fields of the settings column and run
// at the same time; both fields have to survive.
//
// The loop is what makes this reliable. A single pair of goroutines interleaves
// the losing way only sometimes, so one round would pass against the broken code
// often enough to be useless as a gate.
func TestMutateChannelSettingsKeepsConcurrentWritersFields(t *testing.T) {
	channel := openChannelSettingsTestDB(t)

	for round := 0; round < 200; round++ {
		require.NoError(t, DB.Model(&Channel{}).
			Where("id = ?", channel.Id).
			Update("settings", "").Error)

		var wg sync.WaitGroup
		wg.Add(2)

		// Writer A: the WebSocket handshake probe.
		go func() {
			defer wg.Done()
			assert.NoError(t, MutateChannelSettings(channel.Id, func(s *dto.ChannelOtherSettings) bool {
				s.WebsocketUnsupported = true
				return true
			}))
		}()

		// Writer B: the upstream-model update task.
		go func() {
			defer wg.Done()
			assert.NoError(t, MutateChannelSettings(channel.Id, func(s *dto.ChannelOtherSettings) bool {
				s.UpstreamModelUpdateLastCheckTime = 1717171717
				return true
			}))
		}()

		wg.Wait()

		settings := storedSettings(t, channel.Id)
		require.True(t, settings.WebsocketUnsupported,
			"round %d: probe's websocket_unsupported was lost", round)
		require.Equal(t, int64(1717171717), settings.UpstreamModelUpdateLastCheckTime,
			"round %d: task's last check time was lost", round)
	}
}

// TestUnserializedSettingsWriteLosesUpdates is the control: it reproduces the old
// code path — load the channel, edit the whole struct, write the whole column —
// and shows it drops one writer's field. Without this, the test above could pass
// for reasons unrelated to the lock.
//
// It asserts a loss happens rather than that a specific writer loses, because
// which side wins depends on scheduling.
func TestUnserializedSettingsWriteLosesUpdates(t *testing.T) {
	channel := openChannelSettingsTestDB(t)

	// The old shape: read the row, mutate the decoded struct, save the column.
	unserializedWrite := func(mutate func(*dto.ChannelOtherSettings)) {
		loaded, err := GetChannelById(channel.Id, true)
		if !assert.NoError(t, err) {
			return
		}
		settings := loaded.GetOtherSettings()
		// The gap between the read above and the write below is the whole bug: a
		// concurrent writer commits in here and this write reverts it.
		mutate(&settings)
		loaded.SetOtherSettings(settings)
		assert.NoError(t, DB.Model(&Channel{}).
			Where("id = ?", channel.Id).
			Update("settings", loaded.OtherSettings).Error)
	}

	lost := 0
	const rounds = 200
	for round := 0; round < rounds; round++ {
		require.NoError(t, DB.Model(&Channel{}).
			Where("id = ?", channel.Id).
			Update("settings", "").Error)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			unserializedWrite(func(s *dto.ChannelOtherSettings) {
				s.WebsocketUnsupported = true
			})
		}()
		go func() {
			defer wg.Done()
			unserializedWrite(func(s *dto.ChannelOtherSettings) {
				s.UpstreamModelUpdateLastCheckTime = 1717171717
			})
		}()
		wg.Wait()

		settings := storedSettings(t, channel.Id)
		if !settings.WebsocketUnsupported || settings.UpstreamModelUpdateLastCheckTime == 0 {
			lost++
		}
	}

	require.NotZero(t, lost,
		"the unserialized write never lost a field in %d rounds, so this control proves nothing about the lock",
		rounds)
	t.Logf("unserialized write lost a field in %d of %d rounds", lost, rounds)
}

// TestMutateChannelSettingsSkipsWriteWhenMutateReturnsFalse pins the suppression
// path. The WebSocket probe relies on it to avoid re-writing a flag that is
// already set, which would emit a duplicate notification on every request to a
// channel that has already been marked unsupported.
func TestMutateChannelSettingsSkipsWriteWhenMutateReturnsFalse(t *testing.T) {
	channel := openChannelSettingsTestDB(t)

	require.NoError(t, DB.Model(&Channel{}).
		Where("id = ?", channel.Id).
		Update("settings", `{"sentinel_untouched":true}`).Error)

	called := false
	require.NoError(t, MutateChannelSettings(channel.Id, func(s *dto.ChannelOtherSettings) bool {
		called = true
		s.WebsocketUnsupported = true // discarded: the write is skipped
		return false
	}))

	assert.True(t, called, "mutate should still be consulted")

	var raw string
	require.NoError(t, DB.Model(&Channel{}).
		Where("id = ?", channel.Id).
		Select("settings").Scan(&raw).Error)
	assert.Equal(t, `{"sentinel_untouched":true}`, raw,
		"returning false must leave the column byte-identical, not rewrite it")
}

// TestMutateChannelSettingsWritesOnlyTheSettingsColumn pins that a stale value in
// another column cannot ride along. The loaded Channel is a snapshot; if the
// UPDATE named more than one column, a concurrent change to any of them would be
// reverted by whichever settings write landed next.
func TestMutateChannelSettingsWritesOnlyTheSettingsColumn(t *testing.T) {
	channel := openChannelSettingsTestDB(t)

	// Stand in for another writer changing a different column while a settings
	// mutation is in flight. Done before the mutation, so a whole-row write would
	// revert it from the snapshot the mutation loads.
	loaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Channel{}).
		Where("id = ?", channel.Id).
		Update("name", "renamed-by-admin").Error)

	// Reuse the pre-rename snapshot's data by mutating through the API, which
	// reloads internally — the point is that the reload plus the single-column
	// UPDATE together keep the rename.
	require.Equal(t, "settings-race", loaded.Name)
	require.NoError(t, MutateChannelSettings(channel.Id, func(s *dto.ChannelOtherSettings) bool {
		s.WebsocketUnsupported = true
		return true
	}))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "renamed-by-admin", stored.Name, "settings write clobbered another column")
	assert.True(t, stored.GetOtherSettings().WebsocketUnsupported)
}

// TestMutateChannelSettingsWithModelsWritesBothColumns pins the two-column
// variant: the upstream-model task's model list change and the record of why it
// changed have to land together, or a crash between them re-detects the same
// models forever.
func TestMutateChannelSettingsWithModelsWritesBothColumns(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))

	require.NoError(t, MutateChannelSettingsWithModels(
		channel,
		"gpt-4o,gpt-4o-mini",
		nil,
		func(s *dto.ChannelOtherSettings) bool {
			s.UpstreamModelUpdateLastCheckTime = 42
			return true
		},
	))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o,gpt-4o-mini", stored.Models)
	assert.Equal(t, int64(42), stored.GetOtherSettings().UpstreamModelUpdateLastCheckTime)
	assert.Nil(t, stored.ModelMapping, "nil modelMapping must leave the column untouched")
}

func TestMutateChannelSettingsWithModelsRejectsConcurrentCatalogueEdits(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("models", "operator-model").Error)
	err := MutateChannelSettingsWithModels(channel, "gpt-4o,gpt-4o-mini", nil, func(settings *dto.ChannelOtherSettings) bool {
		settings.UpstreamModelUpdateLastCheckTime = 42
		return true
	})
	require.ErrorContains(t, err, "configuration changed")
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "operator-model", stored.Models)
	assert.Zero(t, stored.GetOtherSettings().UpstreamModelUpdateLastCheckTime)
}

// TestMutateChannelSettingsWithModelsWritesModelMapping pins that a model adopted
// under a bare name lands together with the mapping that turns it back into the
// upstream id: one without the other is a model that resolves nowhere.
func TestMutateChannelSettingsWithModelsWritesModelMapping(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))

	mapping := `{"gpt-4o-mini":"openai/gpt-4o-mini"}`
	require.NoError(t, MutateChannelSettingsWithModels(
		channel,
		"gpt-4o,gpt-4o-mini",
		&mapping,
		func(s *dto.ChannelOtherSettings) bool { return true },
	))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o,gpt-4o-mini", stored.Models)
	require.NotNil(t, stored.ModelMapping)
	assert.Equal(t, mapping, *stored.ModelMapping)
}

// TestMutateChannelSettingsWithModelsSkipsBothWritesWhenMutateReturnsFalse pins
// that the skip covers the models column too. A caller that decides mid-mutation
// that there is nothing to record must not have applied a model list change
// either, or the list moves with no record of why.
func TestMutateChannelSettingsWithModelsSkipsBothWritesWhenMutateReturnsFalse(t *testing.T) {
	channel := openChannelSettingsTestDB(t)
	require.NoError(t, DB.AutoMigrate(&Ability{}))

	require.NoError(t, MutateChannelSettingsWithModels(
		channel,
		"gpt-4o,should-not-be-written",
		nil,
		func(s *dto.ChannelOtherSettings) bool { return false },
	))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", stored.Models, "models was written despite mutate returning false")
}

// TestMutateChannelSettingsRejectsMissingChannel pins that a bad id surfaces as an
// error instead of silently inserting a row or reporting success. Callers log the
// failure and carry on with their own work, so a swallowed error here would be
// invisible.
func TestMutateChannelSettingsRejectsMissingChannel(t *testing.T) {
	openChannelSettingsTestDB(t)

	called := false
	err := MutateChannelSettings(4242, func(s *dto.ChannelOtherSettings) bool {
		called = true
		return true
	})
	require.Error(t, err)
	assert.False(t, called, "mutate must not run when the channel cannot be loaded")

	// A non-positive id is rejected before touching the database at all.
	require.Error(t, MutateChannelSettings(0, func(s *dto.ChannelOtherSettings) bool { return true }))
}
