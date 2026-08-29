package model

import (
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

// These tests exercise the channel cache the way production does: many readers
// holding channels well past the read lock, while the auto-ban path and the
// periodic full sync replace entries underneath them. Run with -race to see the
// failure they protect against; before the cache switched to copy-on-write, the
// cached *Channel escaped channelSyncLock and these loops raced on Status,
// ChannelInfo, and the per-key status map.

func TestChannelCacheConcurrentReadWriteIsRaceFree(t *testing.T) {
	setupChannelCacheTest(t)
	first := mustCreateCachedChannel(t, "race-first", "default", "model-x", 20)
	second := mustCreateCachedChannel(t, "race-second", "default", "model-x", 10)
	InitChannelCache()

	const iterations = 200
	var waitGroup sync.WaitGroup

	// Readers: the pattern from middleware.Distribute — fetch a channel, then keep
	// reading its fields after the lock is released.
	for readerIndex := 0; readerIndex < 4; readerIndex++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for i := 0; i < iterations; i++ {
				if channel, err := CacheGetChannel(first.Id); err == nil && channel != nil {
					_ = channel.Status
					_ = channel.Name
					_ = channel.GetPriority()
					_ = channel.GetSetting()
					_ = channel.GetOtherSettings()
					_ = channel.ChannelInfo.MultiKeyPollingIndex
					for keyIndex := range channel.ChannelInfo.MultiKeyStatusList {
						_ = channel.ChannelInfo.MultiKeyStatusList[keyIndex]
					}
				}
				if info, err := CacheGetChannelInfo(second.Id); err == nil && info != nil {
					_ = info.MultiKeyPollingIndex
					_ = info.IsMultiKey
				}
			}
		}()
	}

	// Selectors: the request routing path.
	for selectorIndex := 0; selectorIndex < 2; selectorIndex++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for i := 0; i < iterations; i++ {
				if picked, err := GetRandomSatisfiedChannel("default", "model-x", 0, "", nil); err == nil && picked != nil {
					_ = picked.Status
					_ = picked.Key
					ReleaseChannelSlot(picked.Id)
				}
			}
		}()
	}

	// Writer: the auto-ban / auto-enable path.
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				CacheUpdateChannelStatus(first.Id, common.ChannelStatusAutoDisabled)
			} else {
				CacheUpdateChannelStatus(first.Id, common.ChannelStatusEnabled)
			}
		}
	}()

	// Full sync: the background goroutine that swaps the whole cache.
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		for i := 0; i < 20; i++ {
			InitChannelCache()
		}
	}()

	waitGroup.Wait()

	// The cache must still be coherent, and the sync goroutine restores the
	// database state, so the channel ends up enabled and selectable.
	CacheUpdateChannelStatus(first.Id, common.ChannelStatusEnabled)
	cached, err := CacheGetChannel(first.Id)
	require.NoError(t, err)
	require.Equal(t, common.ChannelStatusEnabled, cached.Status)
}

func TestGetNextEnabledKeyPollingAdvancesUnderConcurrency(t *testing.T) {
	setupChannelCacheTest(t)
	priority := int64(10)
	channel := &Channel{
		Name:     "race-polling",
		Key:      "sk-a\nsk-b\nsk-c",
		Status:   common.ChannelStatusEnabled,
		Group:    "default",
		Models:   "model-x",
		Priority: &priority,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 3,
			MultiKeyMode: constant.MultiKeyModePolling,
		},
	}
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	InitChannelCache()
	t.Cleanup(func() { forgetChannelPollingIndex(channel.Id) })

	// The cursor lives in the shared polling store, so it must keep rotating even
	// though every caller works from its own copy of the channel. Holding it on
	// the copy instead made polling hand out one key forever.
	seenKeys := make(map[string]int)
	var mutex sync.Mutex
	var waitGroup sync.WaitGroup
	for workerIndex := 0; workerIndex < 4; workerIndex++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for i := 0; i < 30; i++ {
				cached, err := CacheGetChannel(channel.Id)
				if err != nil || cached == nil {
					continue
				}
				key, _, apiErr := cached.GetNextEnabledKey()
				if apiErr != nil {
					continue
				}
				mutex.Lock()
				seenKeys[key]++
				mutex.Unlock()
			}
		}()
	}
	waitGroup.Wait()

	require.Len(t, seenKeys, 3, "polling must rotate across every enabled key")
	for _, key := range []string{"sk-a", "sk-b", "sk-c"} {
		require.Positive(t, seenKeys[key], "key %s was never selected", key)
	}
}
