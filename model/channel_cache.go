package model

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

var group2model2channels map[string]map[string][]int // enabled channel
var channelsIDM map[int]*Channel                     // all channels include disabled
// channel2advancedCustomConfig caches parsed Advanced Custom (type 58) configs so
// path-aware selection avoids re-parsing JSON per request. Refreshed on full sync.
var channel2advancedCustomConfig map[int]*dto.AdvancedCustomConfig
var channelSyncLock sync.RWMutex

// channelCacheGeneration counts published mutations of the channel cache. A full
// sync captures it before scanning the database and refuses to install a snapshot
// if it moved, which is what keeps a slow scan from resurrecting a channel that
// UpdateChannelStatus disabled while the scan was running. Guarded by
// channelSyncLock.
var channelCacheGeneration uint64

// channelCacheSyncAttempts bounds how many times a full sync re-scans after
// losing the generation race before falling back to scanning under the lock.
const channelCacheSyncAttempts = 3

// channelCacheSnapshot is a fully built, not yet published view of the channel
// cache.
type channelCacheSnapshot struct {
	group2model2channels         map[string]map[string][]int
	channelsIDM                  map[int]*Channel
	channel2advancedCustomConfig map[int]*dto.AdvancedCustomConfig
}

// Entries handed to readers must never be mutated in place, so every cache
// mutation clones, edits the clone, and republishes it. mutateCachedChannel
// applies that discipline for a single channel. Caller must hold the write lock.
func mutateCachedChannel(id int, mutate func(channel *Channel)) *Channel {
	existing, ok := channelsIDM[id]
	if !ok {
		return nil
	}
	updated := existing.CloneForCache()
	mutate(updated)
	channelsIDM[id] = updated
	channelCacheGeneration++
	return updated
}

// cachePublishChannel installs an already-modified channel into the cache. The
// caller must own the value (typically a copy obtained from CacheGetChannel) and
// must not retain it for further mutation afterwards.
func cachePublishChannel(channel *Channel) {
	if !common.MemoryCacheEnabled || channel == nil {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	if channelsIDM == nil {
		channelsIDM = make(map[int]*Channel)
	}
	channelsIDM[channel.Id] = channel
	channelCacheGeneration++
}

// buildChannelCacheSnapshot reads the channel and ability tables and assembles a
// new cache view. It performs no locking, so it must not touch the live maps.
func buildChannelCacheSnapshot() (*channelCacheSnapshot, bool) {
	var channels []*Channel
	if err := DB.Find(&channels).Error; err != nil {
		common.SysError(fmt.Sprintf("failed to load channels for cache sync: %v", err))
		return nil, false
	}
	var abilities []*Ability
	if err := DB.Find(&abilities).Error; err != nil {
		common.SysError(fmt.Sprintf("failed to load abilities for cache sync: %v", err))
		return nil, false
	}

	newChannelId2channel := make(map[int]*Channel, len(channels))
	newChannel2advancedCustomConfig := make(map[int]*dto.AdvancedCustomConfig)
	for _, channel := range channels {
		if channel.ChannelInfo.IsMultiKey {
			channel.Keys = channel.GetKeys()
		}
		newChannelId2channel[channel.Id] = channel
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
				newChannel2advancedCustomConfig[channel.Id] = config
			}
		}
	}

	newGroup2model2channels := make(map[string]map[string][]int)
	for _, ability := range abilities {
		if _, ok := newGroup2model2channels[ability.Group]; !ok {
			newGroup2model2channels[ability.Group] = make(map[string][]int)
		}
	}
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled {
			continue // skip disabled channels
		}
		// A channel may repeat a group or model in its comma-separated columns.
		// Left unchecked the id lands in the same candidate slice twice, which
		// doubles its weighted-random share and leaves a stale copy behind when
		// the channel is later removed from that slice.
		seenGroupModel := make(map[string]struct{})
		for _, group := range strings.Split(channel.Group, ",") {
			// A channel can briefly exist without abilities while an admin is
			// repairing the table or after a partial import. Keep cache sync
			// resilient to that inconsistent state instead of assigning through a
			// nil nested map and panicking the process.
			if _, ok := newGroup2model2channels[group]; !ok {
				newGroup2model2channels[group] = make(map[string][]int)
			}
			for _, model := range strings.Split(channel.Models, ",") {
				if _, duplicate := seenGroupModel[group+"|"+model]; duplicate {
					continue
				}
				seenGroupModel[group+"|"+model] = struct{}{}
				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel.Id)
			}
		}
	}

	// sort by priority
	for _, model2channels := range newGroup2model2channels {
		for model, channelIds := range model2channels {
			sort.Slice(channelIds, func(i, j int) bool {
				return newChannelId2channel[channelIds[i]].GetPriority() > newChannelId2channel[channelIds[j]].GetPriority()
			})
			model2channels[model] = channelIds
		}
	}

	return &channelCacheSnapshot{
		group2model2channels:         newGroup2model2channels,
		channelsIDM:                  newChannelId2channel,
		channel2advancedCustomConfig: newChannel2advancedCustomConfig,
	}, true
}

func installChannelCacheSnapshot(snapshot *channelCacheSnapshot) {
	group2model2channels = snapshot.group2model2channels
	channelsIDM = snapshot.channelsIDM
	channel2advancedCustomConfig = snapshot.channel2advancedCustomConfig
	channelCacheGeneration++
}

func InitChannelCache() {
	if !common.MemoryCacheEnabled {
		InvalidatePricingCache()
		return
	}

	// Building a snapshot means two full table scans. Holding channelSyncLock
	// across them stalls every reader for the duration (a waiting writer also
	// parks new RLock callers), which shows up as a periodic routing pause. So
	// the scans run unlocked and only the swap is serialized. channelCacheGeneration
	// closes the gap the unlocked scan opens: if a status update was published
	// while the scan was in flight, the snapshot is stale and gets rebuilt. The
	// last attempt scans under the lock so a channel under constant status churn
	// still converges.
	synced := false
	for attempt := 1; attempt <= channelCacheSyncAttempts && !synced; attempt++ {
		if attempt == channelCacheSyncAttempts {
			func() {
				channelSyncLock.Lock()
				defer channelSyncLock.Unlock()
				snapshot, ok := buildChannelCacheSnapshot()
				if !ok {
					return
				}
				installChannelCacheSnapshot(snapshot)
				synced = true
			}()
			break
		}

		channelSyncLock.RLock()
		generationBeforeScan := channelCacheGeneration
		channelSyncLock.RUnlock()

		snapshot, ok := buildChannelCacheSnapshot()
		if !ok {
			return
		}

		func() {
			channelSyncLock.Lock()
			defer channelSyncLock.Unlock()
			if channelCacheGeneration != generationBeforeScan {
				return
			}
			installChannelCacheSnapshot(snapshot)
			synced = true
		}()
	}
	if !synced {
		return
	}
	// Lock ordering: InvalidatePricingCache acquires updatePricingLock, and
	// GetPricing (holding updatePricingLock) nests channelSyncLock.RLock via
	// loadPricingAdvancedCustomConfigs. channelSyncLock MUST be released before
	// invalidating the pricing cache, otherwise the reversed order deadlocks.
	InvalidatePricingCache()
	common.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

// GetRandomSatisfiedChannel selects a channel for group/model.
// excludedChannelIds holds the channels already attempted for this request; they
// are removed from every priority tier so a retry cannot land on the channel that
// just failed. Tier demotion, exclusion and weighting are delegated to
// selectChannelCandidate, which the DB path uses as well, so the two paths cannot
// diverge.
func GetRandomSatisfiedChannel(group string, model string, retry int, requestPath string, excludedChannelIds map[int]bool) (*Channel, error) {
	// if memory cache is disabled, get channel directly from database
	if !common.MemoryCacheEnabled {
		return GetChannel(group, model, retry, requestPath, excludedChannelIds)
	}

	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	// First, try to find channels with the exact model name.
	channels := filterChannelsByRequestPathAndModel(group2model2channels[group][model], requestPath, model)

	// If no channels found, try to find channels with the normalized model name.
	if len(channels) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(model)
		channels = filterChannelsByRequestPathAndModel(group2model2channels[group][normalizedModel], requestPath, model)
	}

	if len(channels) == 0 {
		return nil, nil
	}

	candidates := make([]channelCandidate, 0, len(channels))
	for _, channelId := range channels {
		channel, ok := channelsIDM[channelId]
		if !ok {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelId)
		}
		candidates = append(candidates, channelCandidate{
			channelId:      channelId,
			priority:       channel.GetPriority(),
			weight:         channel.GetWeight(),
			maxConcurrency: channel.GetMaxConcurrency(),
		})
	}

	candidates = applyDynamicScores(group, model, candidates)

	channelId, err := selectAndAcquireChannel(candidates, retry, excludedChannelIds)
	if err != nil {
		return nil, err
	}
	if channelId == 0 {
		return nil, fmt.Errorf("no channel found, group: %s, model: %s", group, model)
	}
	// Copy for the same reason as CacheGetChannel: the selected channel travels
	// with the request long after this read lock is gone.
	return channelsIDM[channelId].CloneForCache(), nil
}

// filterChannelsByRequestPathAndModel restricts candidates by request path and
// model. Only Advanced Custom (type 58) channels are path-checked: they are kept
// only when one of their configured routes matches requestPath and model. All
// other channel types always pass. When requestPath is empty, filtering is skipped.
// Caller must hold channelSyncLock (read lock). The cached slice is never mutated.
func filterChannelsByRequestPathAndModel(channels []int, requestPath string, model string) []int {
	if requestPath == "" || len(channels) == 0 {
		return channels
	}
	filtered := make([]int, 0, len(channels))
	for _, channelId := range channels {
		channel, ok := channelsIDM[channelId]
		if !ok {
			// keep it so the downstream consistency error is raised as before
			filtered = append(filtered, channelId)
			continue
		}
		if channel.Type != constant.ChannelTypeAdvancedCustom {
			filtered = append(filtered, channelId)
			continue
		}
		if config := channel2advancedCustomConfig[channelId]; config != nil && config.SupportsPathForModel(requestPath, model) {
			filtered = append(filtered, channelId)
		}
	}
	return filtered
}

// CacheGetChannel returns a private copy of the cached channel. Callers keep
// using it after channelSyncLock is released, and both the sync goroutine and the
// auto-ban path replace cache entries concurrently, so handing out the cached
// pointer would let callers read a channel while it is being rewritten.
func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return c.CloneForCache(), nil
}

// CacheGetChannelInfo returns a copy of the channel's info for the same reason as
// CacheGetChannel: a pointer into the cached struct would outlive the read lock.
func CacheGetChannelInfo(id int) (*ChannelInfo, error) {
	if !common.MemoryCacheEnabled {
		channel, err := GetChannelById(id, true)
		if err != nil {
			return nil, err
		}
		return &channel.ChannelInfo, nil
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	info := c.ChannelInfo.clone()
	return &info, nil
}

func CacheUpdateChannelStatus(id int, status int) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	channel := mutateCachedChannel(id, func(channel *Channel) {
		channel.Status = status
	})
	if status == common.ChannelStatusEnabled {
		// The auto re-enable path (service.EnableChannel) is the only enable path
		// without a follow-up InitChannelCache, so the channel must rejoin the
		// selection map here or it receives no traffic until the next full sync.
		if channel == nil {
			return
		}
		if group2model2channels == nil {
			group2model2channels = make(map[string]map[string][]int)
		}
		for _, group := range strings.Split(channel.Group, ",") {
			model2channels, groupExists := group2model2channels[group]
			if !groupExists {
				model2channels = make(map[string][]int)
				group2model2channels[group] = model2channels
			}
			for _, model := range strings.Split(channel.Models, ",") {
				channels := model2channels[model]
				if isChannelIDInList(channels, id) {
					continue
				}
				channels = append(channels, id)
				sort.Slice(channels, func(i, j int) bool {
					ci, iok := channelsIDM[channels[i]]
					cj, jok := channelsIDM[channels[j]]
					if !iok || !jok {
						return iok
					}
					return ci.GetPriority() > cj.GetPriority()
				})
				model2channels[model] = channels
			}
		}
		return
	}
	// Remove the channel from group2model2channels. Every occurrence has to go:
	// stopping at the first match would leave a duplicate id behind and the
	// disabled channel would keep being selected until the next full sync.
	for _, model2channels := range group2model2channels {
		for model, channels := range model2channels {
			remaining := make([]int, 0, len(channels))
			for _, channelId := range channels {
				if channelId != id {
					remaining = append(remaining, channelId)
				}
			}
			if len(remaining) != len(channels) {
				model2channels[model] = remaining
			}
		}
	}
}

func CacheUpdateChannel(channel *Channel) {
	if !common.MemoryCacheEnabled || channel == nil {
		return
	}
	// The critical section is a closure so the unlock rides on a defer: a panic
	// between lock and unlock would otherwise leave channelSyncLock held and block
	// every reader for the life of the process. InvalidatePricingCache must stay
	// outside it — GetPricing takes updatePricingLock first and then
	// channelSyncLock.RLock (via loadPricingAdvancedCustomConfigs), so acquiring
	// updatePricingLock while holding channelSyncLock would be an AB-BA deadlock.
	func() {
		channelSyncLock.Lock()
		defer channelSyncLock.Unlock()

		if channelsIDM == nil {
			channelsIDM = make(map[int]*Channel)
		}
		channelsIDM[channel.Id] = channel
		if channel2advancedCustomConfig == nil {
			channel2advancedCustomConfig = make(map[int]*dto.AdvancedCustomConfig)
		}
		delete(channel2advancedCustomConfig, channel.Id)
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
				channel2advancedCustomConfig[channel.Id] = config
			}
		}
		channelCacheGeneration++
		logger.LogDebug(nil, "CacheUpdateChannel: id=%d, name=%s, status=%d", channel.Id, channel.Name, channel.Status)
	}()
	InvalidatePricingCache()
}
