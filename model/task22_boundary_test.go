package model

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strconv"
	"testing"
)

func TestChannelWeightAndExhaustionBoundary(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, weight := range []int{maxInt, maxInt - 9, 0, -1} {
		t.Run(strconv.Itoa(weight), func(t *testing.T) {
			candidates := []channelCandidate{{channelId: 99101, weight: weight}, {channelId: 99102, weight: weight}}
			var id int
			var ok bool
			require.NotPanics(t, func() { id, ok = selectChannelCandidate(candidates, 0, nil) })
			require.True(t, ok)
			assert.Contains(t, []int{99101, 99102}, id)
			id, ok = selectChannelCandidate(candidates, 0, map[int]bool{99101: true})
			require.True(t, ok)
			assert.Equal(t, 99102, id)
			id, ok = selectChannelCandidate(candidates, 0, map[int]bool{99101: true, 99102: true})
			assert.False(t, ok)
			assert.Zero(t, id)
		})
	}
	assert.Equal(t, 1, pickWeightedIndex([]int{10, 10}, 10))
	assert.Equal(t, 0, pickWeightedIndex([]int{10, 10}, 9))
}

func TestSaturatedWaitQueueBoundary(t *testing.T) {
	const channel = 99202
	defer LeaveChannelSlotWait(channel)
	assert.False(t, EnterChannelSlotWait(channel, 0))
	assert.False(t, EnterChannelSlotWait(channel, -1))
	require.True(t, EnterChannelSlotWait(channel, 1))
	assert.False(t, EnterChannelSlotWait(channel, 1))
	assert.Equal(t, 1, ChannelSlotWaiters(channel))
	LeaveChannelSlotWait(channel)
	assert.Zero(t, ChannelSlotWaiters(channel))
	require.True(t, EnterChannelSlotWait(channel, 1))
}
