package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// ChannelKeyEnvironment is one key's environment in an independent key
// environment channel: the captured CLI version it was assigned. key_tag is
// unique across the table, so a key belongs to at most one such channel;
// (channel_id, profile_id) is unique, so a version serves one key per channel.
// A NULL profile_id is an unassigned key; all three databases let NULLs repeat
// under a unique index, and the row still holds the key_tag.
type ChannelKeyEnvironment struct {
	Id        int     `json:"id"`
	ChannelId int     `json:"channel_id" gorm:"not null;uniqueIndex:idx_channel_key_env_profile"`
	KeyTag    string  `json:"-" gorm:"type:varchar(16);not null;uniqueIndex"`
	ProfileId *string `json:"profile_id" gorm:"type:varchar(64);uniqueIndex:idx_channel_key_env_profile"`
}

// ChannelKeyTag names a key without revealing it: the first 8 bytes of the
// SHA-256 of the trimmed key, hex encoded. It is never logged or sent to the
// frontend.
func ChannelKeyTag(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:8])
}

// channelKeyEnvironments is the in-memory projection of the table, keyed by
// channel id and then key tag, with "" for an unassigned key. It is built in the
// same snapshot as channelsIDM so a channel's keys and their assignments are
// read from the same scan. Inner maps are never mutated after install.
//
// keyEnvironmentLock is a leaf lock: it is never held while taking another, so
// it can be read under a channel's polling lock without ordering against
// channelSyncLock.
var (
	keyEnvironmentLock     sync.RWMutex
	channelKeyEnvironments map[int]map[string]string
)

func loadChannelKeyEnvironments(db *gorm.DB, channelIds ...int) (map[int]map[string]string, error) {
	var rows []ChannelKeyEnvironment
	query := db.Model(&ChannelKeyEnvironment{})
	if len(channelIds) > 0 {
		query = query.Where("channel_id IN ?", channelIds)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	projection := make(map[int]map[string]string)
	for _, row := range rows {
		if projection[row.ChannelId] == nil {
			projection[row.ChannelId] = make(map[string]string)
		}
		profileId := ""
		if row.ProfileId != nil {
			profileId = *row.ProfileId
		}
		projection[row.ChannelId][row.KeyTag] = profileId
	}
	return projection, nil
}

func installChannelKeyEnvironments(projection map[int]map[string]string) {
	keyEnvironmentLock.Lock()
	defer keyEnvironmentLock.Unlock()
	channelKeyEnvironments = projection
}

// ChannelKeyEnvironmentAssignments returns the channel's key tag → profile id
// map ("" = unassigned). With the memory cache on it is the cached projection
// and must not be modified; with it off it is read from the database, as the
// channel itself was.
func ChannelKeyEnvironmentAssignments(channelId int) map[string]string {
	if !common.MemoryCacheEnabled {
		projection, err := loadChannelKeyEnvironments(DB, channelId)
		if err != nil {
			common.SysError("failed to load channel key environments: " + err.Error())
			return nil
		}
		return projection[channelId]
	}
	keyEnvironmentLock.RLock()
	defer keyEnvironmentLock.RUnlock()
	return channelKeyEnvironments[channelId]
}

// KeyEnvironmentProfile is the profile assigned to key when the channel runs
// independent key environments. ok is false for a channel without the switch
// and for an unassigned or unknown key, which must then serve no traffic.
func (channel *Channel) KeyEnvironmentProfile(key string) (string, bool) {
	if channel == nil || !channel.GetSetting().KeyEnvironments {
		return "", false
	}
	profileId := ChannelKeyEnvironmentAssignments(channel.Id)[ChannelKeyTag(key)]
	return profileId, profileId != ""
}

// keyEnvironmentAssigned narrows enabled key indexes to the assigned keys of an
// independent key environment channel. Unassigned keys are filtered here, below
// every key selector, so no caller can hand one to a request.
func (channel *Channel) keyEnvironmentAssigned(keys []string, indexes []int) []int {
	if !channel.GetSetting().KeyEnvironments {
		return indexes
	}
	assignments := ChannelKeyEnvironmentAssignments(channel.Id)
	assigned := make([]int, 0, len(indexes))
	for _, i := range indexes {
		if assignments[ChannelKeyTag(keys[i])] != "" {
			assigned = append(assigned, i)
		}
	}
	return assigned
}

// EnabledKeyIndex is the index of key among the keys a selector may pick (for
// an independent key environment channel, its enabled assigned keys), or -1.
func (channel *Channel) EnabledKeyIndex(key string) int {
	if key == "" {
		return -1
	}
	if !channel.ChannelInfo.IsMultiKey {
		if channel.Key != key {
			return -1
		}
		if _, assigned := channel.KeyEnvironmentProfile(key); channel.GetSetting().KeyEnvironments && !assigned {
			return -1
		}
		return 0
	}
	keys := channel.GetKeys()
	lock := GetChannelPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()
	for _, i := range channel.enabledKeyIndexes(keys) {
		if keys[i] == key {
			return i
		}
	}
	return -1
}

// KeyEnvironmentKey finds the key with tag among the keys the channel may
// serve (enabled and assigned), with the deadline of its cooldown for modelName
// (zero when it is not cooling). index is -1 when no such key serves.
func (channel *Channel) KeyEnvironmentKey(tag string, modelName string) (index int, key string, coolingUntil time.Time) {
	keys := channel.GetKeys()
	if !channel.ChannelInfo.IsMultiKey {
		keys = []string{channel.Key}
	}
	lock := GetChannelPollingLock(channel.Id)
	lock.Lock()
	enabled := channel.enabledKeyIndexes(keys)
	if !channel.ChannelInfo.IsMultiKey {
		enabled = channel.keyEnvironmentAssigned(keys, []int{0})
	}
	lock.Unlock()
	for _, i := range enabled {
		if ChannelKeyTag(keys[i]) != tag {
			continue
		}
		if modelName != "" {
			coolingUntil = channelKeyCoolingUntil(channel.Id, modelName, keys, []int{i}, time.Now())[i]
		}
		return i, keys[i], coolingUntil
	}
	return -1, "", time.Time{}
}

// GroupModelHasKeyEnvironmentChannel reports whether any channel serving
// modelName in one of groups runs independent key environments. Without the
// memory cache there is no cheap answer, so it reports true.
func GroupModelHasKeyEnvironmentChannel(groups []string, modelName string) bool {
	if !common.MemoryCacheEnabled {
		return true
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	for _, group := range groups {
		for _, id := range group2model2channels[group][modelName] {
			if channel, ok := channelsIDM[id]; ok && channel.GetSetting().KeyEnvironments {
				return true
			}
		}
	}
	return false
}

// LoadChannelKeyEnvironmentRows reads one channel's rows inside tx, for the
// reconcile that rewrites them.
func LoadChannelKeyEnvironmentRows(tx *gorm.DB, channelId int) ([]ChannelKeyEnvironment, error) {
	var rows []ChannelKeyEnvironment
	err := tx.Where("channel_id = ?", channelId).Find(&rows).Error
	return rows, err
}

// ChannelKeyEnvironmentOwners maps each of tags already held by a channel other
// than channelId to that channel.
func ChannelKeyEnvironmentOwners(tx *gorm.DB, channelId int, tags []string) (map[string]int, error) {
	owners := make(map[string]int)
	if len(tags) == 0 {
		return owners, nil
	}
	var rows []ChannelKeyEnvironment
	for start := 0; start < len(tags); start += 200 {
		end := min(start+200, len(tags))
		var chunk []ChannelKeyEnvironment
		if err := tx.Where("key_tag IN ? AND channel_id <> ?", tags[start:end], channelId).Find(&chunk).Error; err != nil {
			return nil, err
		}
		rows = append(rows, chunk...)
	}
	for _, row := range rows {
		owners[row.KeyTag] = row.ChannelId
	}
	return owners, nil
}

// ReplaceChannelKeyEnvironmentRows makes rows the channel's complete set. The
// old rows go first so a version can move between keys without tripping the
// (channel_id, profile_id) unique index halfway through.
func ReplaceChannelKeyEnvironmentRows(tx *gorm.DB, channelId int, rows []ChannelKeyEnvironment) error {
	if err := tx.Where("channel_id = ?", channelId).Delete(&ChannelKeyEnvironment{}).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		rows[i].Id = 0
		rows[i].ChannelId = channelId
	}
	return tx.CreateInBatches(rows, 100).Error
}

func deleteChannelKeyEnvironments(tx *gorm.DB, channelIds []int) error {
	if len(channelIds) == 0 {
		return nil
	}
	return tx.Where("channel_id IN ?", channelIds).Delete(&ChannelKeyEnvironment{}).Error
}

// ReassignChannelKeyEnvironments runs a reassignment of the channel's key
// environments in one transaction, serialized with saves of the channel.
func ReassignChannelKeyEnvironments(channelId int, reassign func(tx *gorm.DB) (any, error)) (any, error) {
	lock := channelSettingsLock(channelId)
	lock.Lock()
	defer lock.Unlock()
	var result any
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = reassign(tx)
		return err
	})
	return result, err
}
