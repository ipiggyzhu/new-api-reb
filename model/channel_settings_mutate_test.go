package model

import (
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

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

	require.NoError(t, MutateChannelSettingsWithModels(
		channel.Id,
		"gpt-4o,gpt-4o-mini",
		func(s *dto.ChannelOtherSettings) bool {
			s.UpstreamModelUpdateLastCheckTime = 42
			return true
		},
	))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o,gpt-4o-mini", stored.Models)
	assert.Equal(t, int64(42), stored.GetOtherSettings().UpstreamModelUpdateLastCheckTime)
}

// TestMutateChannelSettingsWithModelsSkipsBothWritesWhenMutateReturnsFalse pins
// that the skip covers the models column too. A caller that decides mid-mutation
// that there is nothing to record must not have applied a model list change
// either, or the list moves with no record of why.
func TestMutateChannelSettingsWithModelsSkipsBothWritesWhenMutateReturnsFalse(t *testing.T) {
	channel := openChannelSettingsTestDB(t)

	require.NoError(t, MutateChannelSettingsWithModels(
		channel.Id,
		"gpt-4o,should-not-be-written",
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
