package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectChannelCandidate is the single selector both paths reduce to: the DB path
// (GetChannel) and the memory-cache path (GetRandomSatisfiedChannel). Testing it
// directly is what keeps the two from drifting apart on tier ordering, exclusion
// or weighting — which is exactly how they had diverged, the cache path giving a
// weight-0 channel probability zero inside a tier that also held weighted
// channels, while the DB path kept it reachable via a per-candidate baseline.

// pickWeightedIndex owns the draw-to-index mapping, so the distribution can be
// asserted by enumerating every draw in range instead of sampling and hoping.
func TestPickWeightedIndexCoversEveryDrawExactlyOnce(t *testing.T) {
	weights := []int{10, 30, 1}
	total := 41

	hits := make([]int, len(weights))
	for draw := 0; draw < total; draw++ {
		index := pickWeightedIndex(weights, draw)
		require.GreaterOrEqual(t, index, 0, "draw %d fell through the cumulative walk", draw)
		hits[index]++
	}

	assert.Equal(t, weights, hits, "each candidate must own exactly its own weight in draws")
	assert.Equal(t, -1, pickWeightedIndex(weights, total),
		"a draw at or past the total is out of contract and must not silently pick the last candidate")
}

// A weight-0 channel sharing a tier with weighted ones must stay reachable. The
// baseline is per candidate, so it also caps how far a large weight can starve a
// small one — the property that made the two selection paths disagree.
func TestEffectiveChannelWeightKeepsZeroWeightReachable(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	cases := []struct {
		name   string
		weight int
		want   int
	}{
		{name: "zero weight still draws", weight: 0, want: channelWeightBaseline},
		{name: "configured weight adds to the baseline", weight: 5, want: 5 + channelWeightBaseline},
		{name: "corrupt negative weight clamps to the baseline", weight: -7, want: channelWeightBaseline},
		{name: "maximum weight saturates before adding the baseline", weight: maxInt, want: maxInt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveChannelWeight(tc.weight)
			assert.Equal(t, tc.want, got)
			assert.Positive(t, got, "a non-positive share would make the candidate unselectable")
		})
	}
}

func TestChannelGetWeightSaturatesUnsignedValuesBeforeIntConversion(t *testing.T) {
	weight := ^uint(0)
	channel := Channel{Weight: &weight}

	assert.Equal(t, int(^uint(0)>>1), channel.GetWeight())
}

func TestDatabaseSelectionMatchesCacheModelNormalizationAndStatus(t *testing.T) {
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM channels")
		common.MemoryCacheEnabled = previousMemoryCache
	})

	priority := int64(10)
	channel := &Channel{
		Name:     "db-normalized-model",
		Key:      "sk-db-normalized-model",
		Status:   common.ChannelStatusEnabled,
		Group:    "default",
		Models:   "gemini-2.5-flash-thinking-*",
		Priority: &priority,
	}
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	picked, err := GetRandomSatisfiedChannel("default", "gemini-2.5-flash-thinking-1024", 0, "", nil)
	require.NoError(t, err)
	require.NotNil(t, picked, "the no-cache path must try the normalized model name")
	assert.Equal(t, channel.Id, picked.Id)
	ReleaseChannelSlot(picked.Id)

	// Simulate a stale ability row: the database selector must still honor the
	// authoritative channel status just like the memory-cache path does.
	require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", true).Error)
	require.NoError(t, DB.Model(channel).Update("status", common.ChannelStatusAutoDisabled).Error)
	picked, err = GetRandomSatisfiedChannel("default", "gemini-2.5-flash-thinking-1024", 0, "", nil)
	require.NoError(t, err)
	assert.Nil(t, picked, "a disabled channel must not be selected from a stale ability row")
}

// The retry loop feeds every attempted channel back into the selector. A tier
// must be exhausted horizontally before selection drops to the next one,
// otherwise a retry re-draws from the full candidate set and can land on the
// channel that just failed — which is what the old retry-indexed tier lookup did
// whenever the model had a single distinct priority.
func TestSelectChannelCandidateExhaustsTierBeforeDemoting(t *testing.T) {
	candidates := []channelCandidate{
		{channelId: 1, priority: 10, weight: 0},
		{channelId: 2, priority: 10, weight: 100},
		{channelId: 3, priority: 5, weight: 50},
	}

	excluded := make(map[int]bool)
	picked := make(map[int]bool)

	// Three attempts must yield the two top-tier channels first, in some order,
	// and only then the lower tier.
	for attempt := 0; attempt < 2; attempt++ {
		id, ok := selectChannelCandidate(candidates, attempt, excluded)
		require.True(t, ok, "attempt %d must still find an untried top-tier channel", attempt)
		assert.Contains(t, []int{1, 2}, id, "selection must not demote while a top-tier sibling is untried")
		require.False(t, picked[id], "attempt %d re-picked channel %d", attempt, id)
		picked[id] = true
		excluded[id] = true
	}

	id, ok := selectChannelCandidate(candidates, 2, excluded)
	require.True(t, ok)
	assert.Equal(t, 3, id, "with the top tier spent, selection must demote to the next tier")

	excluded[3] = true
	_, ok = selectChannelCandidate(candidates, 3, excluded)
	assert.False(t, ok, "every tier exhausted must be reported, not wrapped around")
}

// A weight-0 channel alone with a weighted sibling in the same tier must be
// selectable. Enumerating both attempts proves reachability without sampling.
func TestSelectChannelCandidateReachesZeroWeightSibling(t *testing.T) {
	candidates := []channelCandidate{
		{channelId: 11, priority: 2, weight: 0},
		{channelId: 12, priority: 2, weight: 1000},
	}

	excluded := map[int]bool{12: true}
	id, ok := selectChannelCandidate(candidates, 1, excluded)
	require.True(t, ok, "the weight-0 channel must remain drawable once its sibling is spent")
	assert.Equal(t, 11, id)
}

// retry keeps its legacy meaning — starting tier index — only for callers that do
// not track attempts, such as the distributor's initial pick. Once attempts are
// tracked the excluded set alone decides when a tier is spent.
func TestSelectChannelCandidateRetryStartTierOnlyWithoutExclusions(t *testing.T) {
	candidates := []channelCandidate{
		{channelId: 21, priority: 10, weight: 1},
		{channelId: 22, priority: 5, weight: 1},
		{channelId: 23, priority: 1, weight: 1},
	}

	cases := []struct {
		name     string
		retry    int
		excluded map[int]bool
		want     int
	}{
		{name: "retry 0 starts at the top tier", retry: 0, want: 21},
		{name: "retry 1 starts one tier down", retry: 1, want: 22},
		{name: "retry past the last tier clamps to it", retry: 9, want: 23},
		{
			name:     "tracked attempts ignore retry and use the excluded set",
			retry:    2,
			excluded: map[int]bool{21: true},
			want:     22,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			excluded := tc.excluded
			if excluded == nil {
				excluded = make(map[int]bool)
			}
			id, ok := selectChannelCandidate(candidates, tc.retry, excluded)
			require.True(t, ok)
			assert.Equal(t, tc.want, id)
		})
	}
}

func TestSelectChannelCandidateReportsEmptyCandidateSet(t *testing.T) {
	_, ok := selectChannelCandidate(nil, 0, nil)
	assert.False(t, ok)
}
