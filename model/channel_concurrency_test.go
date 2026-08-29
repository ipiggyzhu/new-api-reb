package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The in-flight gauge is process-global, so every test owns a disjoint block of
// channel ids and gives back what it takes.

func TestAcquireChannelSlotAdmitsExactlyUpToTheLimit(t *testing.T) {
	const channelId = 900001

	require.True(t, AcquireChannelSlot(channelId, 2), "an idle channel must admit the first request")
	require.True(t, AcquireChannelSlot(channelId, 2), "a channel below its limit must admit the second request")
	assert.False(t, AcquireChannelSlot(channelId, 2), "the request that would exceed the limit must be refused")
	assert.Equal(t, 2, ChannelInFlight(channelId), "a refused request must not be counted against the channel")

	ReleaseChannelSlot(channelId)
	assert.Equal(t, 1, ChannelInFlight(channelId))
	assert.True(t, AcquireChannelSlot(channelId, 2), "the freed slot must be reusable")

	ReleaseChannelSlot(channelId)
	ReleaseChannelSlot(channelId)
	assert.Equal(t, 0, ChannelInFlight(channelId))
}

// An unlimited channel still counts, so the admin list can report its load and so
// that lowering the limit later does not strand slots taken under the old value.
func TestAcquireChannelSlotTreatsNonPositiveLimitAsUnlimited(t *testing.T) {
	cases := []struct {
		name  string
		id    int
		limit int
	}{
		{name: "zero means unlimited", id: 900011, limit: 0},
		{name: "negative means unlimited", id: 900012, limit: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for admitted := 1; admitted <= 3; admitted++ {
				require.True(t, AcquireChannelSlot(tc.id, tc.limit), "admission %d must not be capped", admitted)
			}
			assert.Equal(t, 3, ChannelInFlight(tc.id), "an unlimited channel must still report its load")

			for range 3 {
				ReleaseChannelSlot(tc.id)
			}
			assert.Equal(t, 0, ChannelInFlight(tc.id))
		})
	}
}

// A slot released twice must not drive the gauge negative: a negative count would
// let the channel exceed its limit forever, which is worse than the accounting
// bug that caused it.
func TestReleaseChannelSlotClampsAtZero(t *testing.T) {
	const channelId = 900021

	require.True(t, AcquireChannelSlot(channelId, 1))
	ReleaseChannelSlot(channelId)
	ReleaseChannelSlot(channelId)

	assert.Equal(t, 0, ChannelInFlight(channelId), "the gauge must not go below zero")

	require.True(t, AcquireChannelSlot(channelId, 1), "the channel must still admit one request")
	assert.False(t, AcquireChannelSlot(channelId, 1), "a clamped gauge must still enforce the limit")

	ReleaseChannelSlot(channelId)
}

func TestReleaseChannelSlotOnUntouchedChannelIsANoop(t *testing.T) {
	const channelId = 900031

	ReleaseChannelSlot(channelId)
	assert.Equal(t, 0, ChannelInFlight(channelId))
	assert.NotContains(t, ChannelInFlightSnapshot(), channelId,
		"a channel that never served a request must not appear in the snapshot")
}

// The snapshot feeds the admin channel list, where an entry means "busy right
// now". Channels that have drained must drop out instead of lingering at zero.
func TestChannelInFlightSnapshotReportsOnlyBusyChannels(t *testing.T) {
	const busyId = 900041
	const drainedId = 900042

	require.True(t, AcquireChannelSlot(busyId, 0))
	require.True(t, AcquireChannelSlot(busyId, 0))
	require.True(t, AcquireChannelSlot(drainedId, 0))
	ReleaseChannelSlot(drainedId)

	snapshot := ChannelInFlightSnapshot()
	assert.Equal(t, 2, snapshot[busyId], "the busy channel must report its exact load")
	assert.NotContains(t, snapshot, drainedId, "a drained channel must not stay in the snapshot")

	ReleaseChannelSlot(busyId)
	ReleaseChannelSlot(busyId)
}

func TestChannelCandidateIsSaturatedOnlyAtOrAboveAConfiguredCap(t *testing.T) {
	const cappedId = 900051
	const uncappedId = 900052

	require.True(t, AcquireChannelSlot(cappedId, 0))
	require.True(t, AcquireChannelSlot(uncappedId, 0))
	defer ReleaseChannelSlot(cappedId)
	defer ReleaseChannelSlot(uncappedId)

	assert.False(t, channelCandidate{channelId: cappedId, maxConcurrency: 2}.isSaturated(),
		"a channel below its cap must stay selectable")
	assert.True(t, channelCandidate{channelId: cappedId, maxConcurrency: 1}.isSaturated(),
		"a channel at its cap must be skipped")
	assert.False(t, channelCandidate{channelId: uncappedId, maxConcurrency: 0}.isSaturated(),
		"an uncapped channel is never saturated no matter how busy it is")
}

// The user-facing behaviour of the whole feature: when the top tier is full, the
// overflow falls through to the next priority tier instead of queueing.
func TestSelectChannelCandidateDemotesWhenTheTopTierIsSaturated(t *testing.T) {
	const topA = 900061
	const topB = 900062
	const lower = 900063

	candidates := []channelCandidate{
		{channelId: topA, priority: 10, weight: 100, maxConcurrency: 1},
		{channelId: topB, priority: 10, weight: 100, maxConcurrency: 1},
		{channelId: lower, priority: 5, weight: 1},
	}

	require.True(t, AcquireChannelSlot(topA, 1))
	defer ReleaseChannelSlot(topA)

	id, ok := selectChannelCandidate(candidates, 0, nil)
	require.True(t, ok)
	assert.Equal(t, topB, id, "a saturated channel must be skipped in favour of its untried tier sibling")

	require.True(t, AcquireChannelSlot(topB, 1))
	defer ReleaseChannelSlot(topB)

	id, ok = selectChannelCandidate(candidates, 0, nil)
	require.True(t, ok)
	assert.Equal(t, lower, id, "a fully saturated tier must demote to the next priority tier")
}

func TestSelectAndAcquireChannelTakesTheSlotItWins(t *testing.T) {
	const channelId = 900071

	candidates := []channelCandidate{{channelId: channelId, priority: 1, weight: 0, maxConcurrency: 1}}

	won, err := selectAndAcquireChannel(candidates, 0, nil)
	require.NoError(t, err)
	require.Equal(t, channelId, won)
	assert.Equal(t, 1, ChannelInFlight(channelId), "selection must reserve the slot, not merely check it")

	_, err = selectAndAcquireChannel(candidates, 0, nil)
	assert.ErrorIs(t, err, ErrAllChannelsSaturated,
		"the channel it just filled must be reported as busy, not as missing")

	ReleaseChannelSlot(channelId)
}

// "Everything is busy" and "nothing matches" are different answers: the first is
// backpressure a client may retry, the second is a configuration problem.
func TestSelectAndAcquireChannelSeparatesSaturationFromNoMatch(t *testing.T) {
	const saturatedId = 900081
	const excludedId = 900082

	require.True(t, AcquireChannelSlot(saturatedId, 1))
	defer ReleaseChannelSlot(saturatedId)

	saturated := []channelCandidate{{channelId: saturatedId, priority: 1, maxConcurrency: 1}}
	id, err := selectAndAcquireChannel(saturated, 0, nil)
	assert.ErrorIs(t, err, ErrAllChannelsSaturated)
	assert.Equal(t, 0, id)

	// A channel excluded because this request already tried it is not backpressure:
	// the request has simply run out of channels.
	tried := []channelCandidate{{channelId: excludedId, priority: 1, maxConcurrency: 1}}
	id, err = selectAndAcquireChannel(tried, 0, map[int]bool{excludedId: true})
	assert.NoError(t, err, "an exhausted retry must not be reported as a saturated upstream")
	assert.Equal(t, 0, id)

	id, err = selectAndAcquireChannel(nil, 0, nil)
	assert.NoError(t, err)
	assert.Equal(t, 0, id)
}

// The excluded set is the retry loop's bookkeeping. A channel dropped only
// because it lost a race for the last slot was never attempted, so recording it
// there would make the retry loop skip a channel it never used.
func TestSelectAndAcquireChannelDoesNotMutateTheCallersExcludedSet(t *testing.T) {
	const fullId = 900091
	const freeId = 900092
	const alreadyTriedId = 900093

	require.True(t, AcquireChannelSlot(fullId, 1))
	defer ReleaseChannelSlot(fullId)

	candidates := []channelCandidate{
		{channelId: fullId, priority: 10, maxConcurrency: 1},
		{channelId: freeId, priority: 5},
		{channelId: alreadyTriedId, priority: 1},
	}
	excluded := map[int]bool{alreadyTriedId: true}

	won, err := selectAndAcquireChannel(candidates, 0, excluded)
	require.NoError(t, err)
	require.Equal(t, freeId, won)
	defer ReleaseChannelSlot(freeId)

	assert.Equal(t, map[int]bool{alreadyTriedId: true}, excluded,
		"selection must leave the caller's attempted-channel set exactly as it found it")
}
