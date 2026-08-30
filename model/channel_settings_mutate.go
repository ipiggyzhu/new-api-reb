package model

import (
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/dto"
)

// The `settings` column holds three groups of fields with three different owners:
// the admin (via the channel form), the upstream-model update task
// (UpstreamModelUpdate*) and the WebSocket handshake probe
// (WebsocketUnsupported). Two of those are written by background code, from
// different goroutines, on their own schedules.
//
// Every one of them used to do read-modify-write on the whole column with no
// shared lock, so a concurrent pair silently lost one side's field: the
// upstream-model task loads the settings, the probe persists
// websocket_unsupported, then the task writes back the object it loaded before
// that and the flag is gone. The reverse order discards the task's model-health
// state and rotation cursor instead. The task's own mutex did not help, because it
// only serialized that task against itself.
//
// MutateChannelSettings is the one way to change the column: it holds a
// per-channel lock across the reload, the mutation and the write, so a mutation
// always applies to what is currently stored rather than to a snapshot from
// before someone else's write.

// channelSettingsLocks holds one mutex per channel id.
//
// Entries are never deleted. A channel id is small and bounded by the channel
// table, so the map cannot grow without limit, and deleting would need reference
// counting to avoid freeing a mutex another goroutine is about to lock.
var channelSettingsLocks sync.Map // int -> *sync.Mutex

func channelSettingsLock(channelId int) *sync.Mutex {
	if existing, ok := channelSettingsLocks.Load(channelId); ok {
		return existing.(*sync.Mutex)
	}
	actual, _ := channelSettingsLocks.LoadOrStore(channelId, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// MutateChannelSettings applies mutate to a channel's freshly loaded settings and
// persists the result, serialized per channel.
//
// mutate receives the settings as currently stored and edits them in place. It
// must touch only the fields its caller owns: this serializes concurrent writers
// but cannot tell which field a writer meant to change, so a mutate that
// overwrites an unrelated field still clobbers it. Returning false skips the
// write entirely, which is how a caller says "already in the desired state" —
// used to suppress a redundant write and its notification.
//
// Only the `settings` column is written. Loading the row selects every column so
// the in-memory Channel is complete for the cache refresh, but the UPDATE names
// one column, so a stale value in another field cannot be written back over a
// concurrent change to it.
func MutateChannelSettings(channelId int, mutate func(*dto.ChannelOtherSettings) bool) error {
	if channelId <= 0 {
		return fmt.Errorf("invalid channel id: %d", channelId)
	}

	lock := channelSettingsLock(channelId)
	lock.Lock()
	defer lock.Unlock()

	// selectAll so the Channel handed to CacheUpdateChannel below carries the key
	// and every other column; omitting them would publish a half-empty channel to
	// the cache.
	channel, err := GetChannelById(channelId, true)
	if err != nil {
		return fmt.Errorf("load channel %d: %w", channelId, err)
	}

	settings := channel.GetOtherSettings()
	if !mutate(&settings) {
		return nil
	}
	channel.SetOtherSettings(settings)

	if err := DB.Model(&Channel{}).
		Where("id = ?", channelId).
		Update("settings", channel.OtherSettings).Error; err != nil {
		return fmt.Errorf("persist settings for channel %d: %w", channelId, err)
	}

	CacheUpdateChannel(channel)
	return nil
}

// MutateChannelSettingsWithModels is MutateChannelSettings for the one caller that
// has to change `models` in the same statement as `settings`.
//
// The upstream-model task applies a model-list change and records what it did in
// the same breath; splitting them would leave a window where the list has grown
// but nothing remembers why, and a crash there would re-detect and re-apply the
// same models forever. models is passed as a value rather than mutated, because
// the caller computes it from the diff it has already validated.
//
// Abilities are NOT rebuilt here. The caller does that after this returns, since
// it owns the transaction semantics that rebuild needs.
func MutateChannelSettingsWithModels(
	channelId int,
	models string,
	mutate func(*dto.ChannelOtherSettings) bool,
) error {
	if channelId <= 0 {
		return fmt.Errorf("invalid channel id: %d", channelId)
	}

	lock := channelSettingsLock(channelId)
	lock.Lock()
	defer lock.Unlock()

	channel, err := GetChannelById(channelId, true)
	if err != nil {
		return fmt.Errorf("load channel %d: %w", channelId, err)
	}

	settings := channel.GetOtherSettings()
	if !mutate(&settings) {
		return nil
	}
	channel.SetOtherSettings(settings)
	channel.Models = models

	if err := DB.Model(&Channel{}).
		Where("id = ?", channelId).
		Updates(map[string]interface{}{
			"settings": channel.OtherSettings,
			"models":   models,
		}).Error; err != nil {
		return fmt.Errorf("persist settings and models for channel %d: %w", channelId, err)
	}

	CacheUpdateChannel(channel)
	return nil
}
