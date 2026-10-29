package model

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"gorm.io/gorm"
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

// channelModelValidationConfigMatches rejects results obtained before a channel edit.
// Polling progress is excluded because choosing a key updates it during a probe.
// Keepalive progress is excluded for the same reason: the keepalive task stamps
// its send time into `settings` on its own schedule, and a sweep of a channel
// with many models routinely spans one of those stamps.
func channelModelValidationConfigMatches(current, expected *Channel) bool {
	return current.Status == expected.Status && current.Models == expected.Models &&
		current.GetModelMapping() == expected.GetModelMapping() && channelSettingsMatchIgnoringKeepalive(current.OtherSettings, expected.OtherSettings) &&
		current.Key == expected.Key && current.Type == expected.Type && current.GetBaseURL() == expected.GetBaseURL() &&
		reflect.DeepEqual(current.OpenAIOrganization, expected.OpenAIOrganization) &&
		current.ChannelInfo.IsMultiKey == expected.ChannelInfo.IsMultiKey &&
		reflect.DeepEqual(current.ChannelInfo.MultiKeyStatusList, expected.ChannelInfo.MultiKeyStatusList) &&
		reflect.DeepEqual(current.ChannelInfo.MultiKeyEnvs, expected.ChannelInfo.MultiKeyEnvs) &&
		current.Other == expected.Other && reflect.DeepEqual(current.Setting, expected.Setting) &&
		reflect.DeepEqual(current.ParamOverride, expected.ParamOverride) && reflect.DeepEqual(current.HeaderOverride, expected.HeaderOverride)
}

// channelSettingsMatchIgnoringKeepalive compares two raw `settings` columns with
// the keepalive send time cleared. Unparsable JSON only matches byte for byte,
// which is the comparison this replaced.
func channelSettingsMatchIgnoringKeepalive(current, expected string) bool {
	if current == expected {
		return true
	}
	var currentSettings, expectedSettings dto.ChannelOtherSettings
	if common.UnmarshalJsonStr(current, &currentSettings) != nil || common.UnmarshalJsonStr(expected, &expectedSettings) != nil {
		return false
	}
	currentSettings.UpstreamKeepaliveLastTime = 0
	expectedSettings.UpstreamKeepaliveLastTime = 0
	return reflect.DeepEqual(currentSettings, expectedSettings)
}

// ApplyChannelModelSweep commits a validated catalogue, its routes, and a
// confirmed-empty channel's disabled status together.
// A channel disabled or edited during network probes must not receive stale results.
func ApplyChannelModelSweep(expected *Channel, models string, modelMapping *string, disableIfEmpty bool) (bool, error) {
	lock := channelSettingsLock(expected.Id)
	lock.Lock()
	defer lock.Unlock()

	var current Channel
	applied := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).First(&current, expected.Id).Error; err != nil {
			return err
		}
		if current.Status != common.ChannelStatusEnabled {
			return nil
		}
		if !channelModelValidationConfigMatches(&current, expected) {
			return fmt.Errorf("channel %d configuration changed during model validation; results not applied", expected.Id)
		}
		current.Models = models
		columns := map[string]any{"models": models}
		if disableIfEmpty && len(current.GetModels()) == 0 {
			current.Status = common.ChannelStatusAutoDisabled
			info := current.GetOtherInfo()
			info["status_reason"] = "Model availability sync found no available models"
			info["status_time"] = common.GetTimestamp()
			current.SetOtherInfo(info)
			columns["status"] = current.Status
			columns["other_info"] = current.OtherInfo
		}
		settings := current.GetOtherSettings()
		settings.UpstreamModelUpdateLastRemovedModels = nil
		if settings.UpstreamModelUpdateModelHealth == nil {
			settings.UpstreamModelUpdateModelHealth = make(map[string]dto.ModelHealthState)
		}
		remaining := make(map[string]bool)
		for _, name := range current.GetModels() {
			remaining[name] = true
			delete(settings.UpstreamModelUpdateModelHealth, name)
			delete(settings.UpstreamModelUpdatePendingModelMapping, name)
		}
		pending := make([]string, 0, len(settings.UpstreamModelUpdateLastDetectedModels))
		for _, name := range settings.UpstreamModelUpdateLastDetectedModels {
			if !remaining[name] {
				pending = append(pending, name)
			}
		}
		settings.UpstreamModelUpdateLastDetectedModels = pending
		for _, name := range expected.GetModels() {
			if !remaining[name] {
				// Keep removed names as retry candidates, not as routable abilities.
				// Some upstreams have no usable /models endpoint at all.
				settings.UpstreamModelUpdateModelHealth[name] = dto.ModelHealthState{Failures: 1, LastFailureTime: common.GetTimestamp()}
			}
		}
		settings.UpstreamModelUpdateLastCheckTime = common.GetTimestamp()
		current.SetOtherSettings(settings)
		columns["settings"] = current.OtherSettings
		if modelMapping != nil {
			current.ModelMapping = modelMapping
			columns["model_mapping"] = *modelMapping
		}
		if err := tx.Model(&Channel{}).Where("id = ?", current.Id).Updates(columns).Error; err != nil {
			return err
		}
		if err := current.UpdateAbilities(tx); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if applied {
		CacheUpdateChannel(&current)
		// The model plaza caches pricing even when channel routing uses the DB.
		InitChannelCache()
	}
	return applied, nil
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
// modelMapping is the same story for the `model_mapping` column: a model adopted
// under a name that differs from the upstream id is only usable together with
// the mapping that turns it back, so the two land together. nil leaves the
// column untouched.
//
// The expected configuration, models, settings and abilities are checked and
// written together, so a probe cannot overwrite a later operator edit.
func MutateChannelSettingsWithModels(
	expected *Channel,
	models string,
	modelMapping *string,
	mutate func(*dto.ChannelOtherSettings) bool,
) error {
	if expected.Id <= 0 {
		return fmt.Errorf("invalid channel id: %d", expected.Id)
	}
	lock := channelSettingsLock(expected.Id)
	lock.Lock()
	defer lock.Unlock()
	var channel Channel
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).First(&channel, expected.Id).Error; err != nil {
			return err
		}
		if !channelModelValidationConfigMatches(&channel, expected) {
			return fmt.Errorf("channel %d configuration changed during model validation; results not applied", expected.Id)
		}
		settings := channel.GetOtherSettings()
		if !mutate(&settings) {
			return nil
		}
		channel.SetOtherSettings(settings)
		channel.Models = models
		columns := map[string]interface{}{"settings": channel.OtherSettings, "models": models}
		if modelMapping != nil {
			channel.ModelMapping = modelMapping
			columns["model_mapping"] = *modelMapping
		}
		if err := tx.Model(&Channel{}).Where("id = ?", expected.Id).Updates(columns).Error; err != nil {
			return err
		}
		if err := channel.UpdateAbilities(tx); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		CacheUpdateChannel(&channel)
		InitChannelCache()
	}
	return nil
}
