package model

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// On a multi-key channel a 429 is one key's quota running out, not the
// channel's: the sibling keys usually still have room. channelKeyCooldowns
// therefore cools the (channel, model, key) that was refused, and key selection
// passes over a cooling key while an enabled key that is not cooling remains.
// Only when every enabled key is cooling does the channel itself cool, through
// the ordinary channel cooldown (MarkChannelKeyCooldownAndChannel).
//
// Entries are keyed by model and ChannelKeyTag, across channels. Reordering
// keys or moving a key to another channel keeps its cooldown and strike count;
// a replacement key never inherits the old index's state. Logs show the index,
// never the tag or key.
//
// Per process, like channelCooldowns.
const (
	keyCooldownBase        = 10 * time.Second
	keyCooldownMax         = 2 * time.Minute
	keyCooldownStrikeReset = 10 * time.Minute
)

type keyCooldownEntry struct {
	until    time.Time
	lastMark time.Time
	strikes  int
}

var (
	channelKeyCooldownLock sync.RWMutex
	channelKeyCooldowns    = map[string]map[string]keyCooldownEntry{}
)

// MarkChannelKeyCooldown keeps key keyIndex of the channel out of selection for
// modelName and returns the deadline in force. hint is the upstream's own
// retry-after when it gave one; otherwise the window backs off from 10s,
// doubling on each refusal up to 2 minutes, and starts over after 10 quiet
// minutes. A later deadline already recorded is kept.
func MarkChannelKeyCooldown(channelId int, modelName string, keyIndex int, key string, hint time.Duration, now time.Time) time.Time {
	bucketKey := modelName
	tag := ChannelKeyTag(key)

	channelKeyCooldownLock.Lock()
	defer channelKeyCooldownLock.Unlock()

	bucket := channelKeyCooldowns[bucketKey]
	if bucket == nil {
		bucket = map[string]keyCooldownEntry{}
		channelKeyCooldowns[bucketKey] = bucket
	}
	for index, entry := range bucket {
		if now.Sub(entry.lastMark) > keyCooldownStrikeReset && !now.Before(entry.until) {
			delete(bucket, index)
		}
	}

	entry, ok := bucket[tag]
	if !ok {
		entry = keyCooldownEntry{}
	} else if now.Sub(entry.lastMark) > keyCooldownStrikeReset {
		// Reset backoff strikes without shortening an upstream deadline that
		// still applies to this key after a long quiet interval.
		entry.strikes = 0
	}
	entry.strikes++
	entry.lastMark = now

	wait := hint
	if wait <= 0 {
		wait = keyCooldownBase
		for i := 1; i < entry.strikes && wait < keyCooldownMax; i++ {
			wait *= 2
		}
		wait = min(wait, keyCooldownMax)
	}
	if until := now.Add(wait); until.After(entry.until) {
		entry.until = until
	}
	bucket[tag] = entry
	return entry.until
}

// channelKeyCoolingUntil returns, for each listed key index still cooling for
// modelName at now, when it recovers. Nil when none is cooling.
func channelKeyCoolingUntil(channelId int, modelName string, keys []string, indexes []int, now time.Time) map[int]time.Time {
	channelKeyCooldownLock.RLock()
	defer channelKeyCooldownLock.RUnlock()

	bucket := channelKeyCooldowns[modelName]
	if len(bucket) == 0 {
		return nil
	}
	var cooling map[int]time.Time
	for _, index := range indexes {
		if index < 0 || index >= len(keys) {
			continue
		}
		entry, ok := bucket[ChannelKeyTag(keys[index])]
		if !ok || !now.Before(entry.until) {
			continue
		}
		if cooling == nil {
			cooling = map[int]time.Time{}
		}
		cooling[index] = entry.until
	}
	return cooling
}

// enabledKeyIndexes lists the keys whose status is enabled (a missing status
// means enabled). An independent key environment channel lists only its
// assigned keys, so no selector can hand an unassigned key to a request. The
// caller holds the channel's polling lock when it needs a consistent view with
// the polling cursor.
func (channel *Channel) enabledKeyIndexes(keys []string) []int {
	enabled := make([]int, 0, len(keys))
	for i := range keys {
		if status, ok := channel.ChannelInfo.MultiKeyStatusList[i]; ok && status != common.ChannelStatusEnabled {
			continue
		}
		enabled = append(enabled, i)
	}
	return channel.keyEnvironmentAssigned(keys, enabled)
}

// selectableKeyIndexes narrows the enabled keys to those not cooling for
// modelName. With every enabled key cooling (or no model to check against) it
// returns them all, so selection proceeds exactly as it did before key
// cooldowns existed: a probably-refused request beats a guaranteed failure.
func (channel *Channel) selectableKeyIndexes(keys []string, enabled []int, modelName string) []int {
	if modelName == "" || len(enabled) == 0 {
		return enabled
	}
	cooling := channelKeyCoolingUntil(channel.Id, modelName, keys, enabled, time.Now())
	if len(cooling) == 0 || len(cooling) == len(enabled) {
		return enabled
	}
	ready := make([]int, 0, len(enabled)-len(cooling))
	for _, index := range enabled {
		if _, ok := cooling[index]; !ok {
			ready = append(ready, index)
		}
	}
	return ready
}

// MarkChannelKeyCooldownAndChannel cools the refused key and, once every
// enabled key of the channel is cooling for modelName, cools the channel
// itself until the first of them recovers — the same soft cooldown a
// single-key channel gets from one refusal: other channels are preferred, but
// it is still chosen when nothing else is left. The channel is never disabled.
// Reports the key's deadline and whether the channel was cooled too.
func MarkChannelKeyCooldownAndChannel(channel *Channel, modelName string, keyIndex int, key string, hint time.Duration) (time.Time, bool) {
	now := time.Now()
	keyUntil := MarkChannelKeyCooldown(channel.Id, modelName, keyIndex, key, hint, now)

	keys := channel.GetKeys()
	lock := GetChannelPollingLock(channel.Id)
	lock.Lock()
	enabled := channel.enabledKeyIndexes(keys)
	lock.Unlock()
	if len(enabled) == 0 {
		return keyUntil, false
	}
	cooling := channelKeyCoolingUntil(channel.Id, modelName, keys, enabled, now)
	if len(cooling) < len(enabled) {
		return keyUntil, false
	}
	earliest := time.Time{}
	for _, until := range cooling {
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	MarkChannelCooldown(channel.Id, modelName, earliest)
	return keyUntil, true
}
