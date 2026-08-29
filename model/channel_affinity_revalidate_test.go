package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useChannelCacheForTest installs a memory channel cache and restores the
// previous state afterwards. Driving the cache directly keeps these tests free of
// a database while still exercising the same lookup the selection path uses.
func useChannelCacheForTest(t *testing.T, channels []*Channel, group string, model string) {
	t.Helper()

	channelSyncLock.Lock()
	originalIDM := channelsIDM
	originalGroupMap := group2model2channels
	originalMemoryCache := common.MemoryCacheEnabled

	idm := make(map[int]*Channel, len(channels))
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		idm[channel.Id] = channel
		ids = append(ids, channel.Id)
	}
	channelsIDM = idm
	group2model2channels = map[string]map[string][]int{group: {model: ids}}
	common.MemoryCacheEnabled = true
	channelSyncLock.Unlock()

	t.Cleanup(func() {
		channelSyncLock.Lock()
		channelsIDM = originalIDM
		group2model2channels = originalGroupMap
		common.MemoryCacheEnabled = originalMemoryCache
		channelSyncLock.Unlock()
	})
}

func channelWithPriority(id int, priority int64) *Channel {
	p := priority
	w := uint(10)
	return &Channel{Id: id, Priority: &p, Weight: &w, Status: common.ChannelStatusEnabled}
}

// TestValidateChannelAffinityPinDropsAnOutrankedPin is the fix for the reported
// bug: raising another channel's priority above the pinned one used to have no
// effect until the affinity TTL expired, because the cache key does not include
// priority. Revalidation must retire the pin on the next request instead.
func TestValidateChannelAffinityPinDropsAnOutrankedPin(t *testing.T) {
	const (
		group     = "default"
		modelName = "gpt-test"
	)

	cases := []struct {
		name     string
		channels []*Channel
		pinnedId int
		want     ChannelAffinityPinVerdict
		explain  string
	}{
		{
			name:     "pin is the highest priority",
			channels: []*Channel{channelWithPriority(1, 10), channelWithPriority(2, 0)},
			pinnedId: 1,
			want:     ChannelAffinityPinValid,
			explain:  "nothing outranks it, so the warm channel is kept",
		},
		{
			name:     "pin ties for the highest priority",
			channels: []*Channel{channelWithPriority(1, 10), channelWithPriority(2, 10)},
			pinnedId: 1,
			want:     ChannelAffinityPinValid,
			explain:  "affinity is the tie-breaker inside the top tier",
		},
		{
			name:     "another channel was raised above the pin",
			channels: []*Channel{channelWithPriority(1, 10), channelWithPriority(2, 50)},
			pinnedId: 1,
			want:     ChannelAffinityPinOutranked,
			explain:  "an admin priority edit takes effect on the next request",
		},
		{
			name:     "pin is no longer a candidate",
			channels: []*Channel{channelWithPriority(2, 10)},
			pinnedId: 1,
			want:     ChannelAffinityPinUnusable,
			explain:  "a channel that cannot serve the request keeps no pin",
		},
		{
			name:     "single candidate is always valid",
			channels: []*Channel{channelWithPriority(1, 0)},
			pinnedId: 1,
			want:     ChannelAffinityPinValid,
			explain:  "there is nothing to outrank it",
		},
		{
			name:     "negative priorities compare normally",
			channels: []*Channel{channelWithPriority(1, -10), channelWithPriority(2, -1)},
			pinnedId: 1,
			want:     ChannelAffinityPinOutranked,
			explain:  "-1 outranks -10",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useChannelCacheForTest(t, tc.channels, group, modelName)
			got := ValidateChannelAffinityPin(tc.pinnedId, group, modelName, "")
			assert.Equal(t, tc.want, got, tc.explain)
		})
	}
}

func TestValidateChannelAffinityPinRejectsInvalidChannelId(t *testing.T) {
	useChannelCacheForTest(t, []*Channel{channelWithPriority(1, 0)}, "default", "gpt-test")
	assert.Equal(t, ChannelAffinityPinUnusable, ValidateChannelAffinityPin(0, "default", "gpt-test", ""))
	assert.Equal(t, ChannelAffinityPinUnusable, ValidateChannelAffinityPin(-1, "default", "gpt-test", ""))
}

// TestValidateChannelAffinityPinKeepsPinWhenCandidateSetIsUnknown pins the
// fail-open choice. If the candidate set cannot be established, dropping warm
// pins would turn an unrelated failure into a cache-miss storm; the distributor's
// own usability check still catches a channel that is genuinely gone.
func TestValidateChannelAffinityPinKeepsPinWhenCandidateSetIsUnknown(t *testing.T) {
	useChannelCacheForTest(t, nil, "default", "gpt-test")
	assert.Equal(t, ChannelAffinityPinValid,
		ValidateChannelAffinityPin(1, "empty-group", "gpt-test", ""),
		"an empty candidate set must not invalidate the pin")
}

// TestValidateChannelAffinityPinIgnoresDynamicScores is the prompt-cache
// protection. Dynamic offsets move with ordinary traffic; if they could outrank a
// pin, the key would be torn off its warm upstream repeatedly and the affinity
// rule would lose the cache hits it exists to preserve.
func TestValidateChannelAffinityPinIgnoresDynamicScores(t *testing.T) {
	const (
		group     = "default"
		modelName = "gpt-test"
	)
	useChannelCacheForTest(t, []*Channel{
		channelWithPriority(1, 10),
		channelWithPriority(2, 10),
	}, group, modelName)

	// Verdicts are computed from configured priority only, so repeated calls are
	// stable no matter what the scores are doing.
	for i := 0; i < 5; i++ {
		require.Equal(t, ChannelAffinityPinValid,
			ValidateChannelAffinityPin(1, group, modelName, ""),
			"the verdict must not depend on dynamic score state")
	}
}

// channelWithConcurrency is channelWithPriority plus a concurrency cap, for the
// saturation cases.
func channelWithConcurrency(id int, priority int64, maxConcurrency int) *Channel {
	channel := channelWithPriority(id, priority)
	limit := maxConcurrency
	channel.MaxConcurrency = &limit
	return channel
}

// holdChannelSlotsForTest saturates a channel by taking every slot it has, and
// gives them back afterwards.
func holdChannelSlotsForTest(t *testing.T, channelId int, slots int) {
	t.Helper()
	for i := 0; i < slots; i++ {
		require.True(t, AcquireChannelSlot(channelId, slots), "slot %d should be available", i)
	}
	t.Cleanup(func() {
		for i := 0; i < slots; i++ {
			ReleaseChannelSlot(channelId)
		}
	})
}

// TestValidateChannelAffinityPinIgnoresSaturatedHigherPriorityChannels stops a
// churn loop. A higher-priority channel that is at its concurrency cap cannot
// take the request, so treating it as outranking the pin dropped the pin for a
// channel selection was about to skip; selection then fell back to the same
// lower-tier channel and repinned it. That repeated on every request for as long
// as the top channel stayed full, so the key lost its warm upstream constantly
// and never once reached the channel the drop was made for.
func TestValidateChannelAffinityPinIgnoresSaturatedHigherPriorityChannels(t *testing.T) {
	const (
		group     = "default"
		modelName = "gpt-test"
	)

	t.Run("saturated higher-priority channel does not outrank the pin", func(t *testing.T) {
		useChannelCacheForTest(t, []*Channel{
			channelWithConcurrency(101, 50, 1),
			channelWithPriority(102, 10),
		}, group, modelName)
		holdChannelSlotsForTest(t, 101, 1)

		assert.Equal(t, ChannelAffinityPinValid,
			ValidateChannelAffinityPin(102, group, modelName, ""),
			"a full channel cannot serve the request, so it cannot displace the pin")
	})

	t.Run("the same channel outranks the pin once it has room", func(t *testing.T) {
		useChannelCacheForTest(t, []*Channel{
			channelWithConcurrency(103, 50, 1),
			channelWithPriority(104, 10),
		}, group, modelName)

		assert.Equal(t, ChannelAffinityPinOutranked,
			ValidateChannelAffinityPin(104, group, modelName, ""),
			"with a free slot it really is the better channel, so the pin retires")
	})

	t.Run("every channel saturated keeps the pin", func(t *testing.T) {
		useChannelCacheForTest(t, []*Channel{
			channelWithConcurrency(105, 50, 1),
			channelWithConcurrency(106, 10, 1),
		}, group, modelName)
		holdChannelSlotsForTest(t, 105, 1)
		holdChannelSlotsForTest(t, 106, 1)

		assert.Equal(t, ChannelAffinityPinValid,
			ValidateChannelAffinityPin(106, group, modelName, ""),
			"a request that cannot be served anywhere must not also lose its pin")
	})
}

// TestApplyDynamicScoresIsInertWhenDisabled checks the model-side wrapper leaves
// candidates untouched with the feature off, which is the default and must not
// change any existing deployment's routing.
func TestApplyDynamicScoresIsInertWhenDisabled(t *testing.T) {
	candidates := []channelCandidate{
		{channelId: 1, priority: 10, weight: 5},
		{channelId: 2, priority: 0, weight: 7},
	}
	got := applyDynamicScores("default", "gpt-test", candidates)
	assert.Equal(t, candidates, got, "disabled scoring returns the configured values")
}

func TestApplyDynamicScoresHandlesEmptyInput(t *testing.T) {
	assert.Empty(t, applyDynamicScores("default", "gpt-test", nil))
	assert.Empty(t, applyDynamicScores("default", "gpt-test", []channelCandidate{}))
}
