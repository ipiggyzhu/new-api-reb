package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkChannelCooldownKeepsTheLaterDeadline(t *testing.T) {
	const channelId = 900101
	const modelName = "cooldown-model"
	t.Cleanup(func() { channelCooldowns.Delete(channelCooldownKey(channelId, modelName)) })

	long := time.Now().Add(time.Hour)
	MarkChannelCooldown(channelId, modelName, long)
	MarkChannelCooldown(channelId, modelName, time.Now().Add(time.Second))
	stored, ok := channelCooldowns.Load(channelCooldownKey(channelId, modelName))
	require.True(t, ok)
	assert.Equal(t, long, stored, "a short hint must not cut short a longer window")

	longer := long.Add(time.Minute)
	MarkChannelCooldown(channelId, modelName, longer)
	stored, _ = channelCooldowns.Load(channelCooldownKey(channelId, modelName))
	assert.Equal(t, longer, stored, "a longer window extends the cooldown")

	assert.True(t, ChannelCoolingDown(channelId, modelName))
	assert.False(t, ChannelCoolingDown(channelId, "other-model"), "a cooldown belongs to the model that was refused")
	assert.False(t, ChannelCoolingDown(channelId+1, modelName))
}

func TestChannelCoolingDownForgetsExpiredWindows(t *testing.T) {
	const channelId = 900102
	const modelName = "cooldown-model"
	key := channelCooldownKey(channelId, modelName)
	t.Cleanup(func() { channelCooldowns.Delete(key) })

	MarkChannelCooldown(channelId, modelName, time.Now().Add(-time.Second))
	assert.False(t, ChannelCoolingDown(channelId, modelName))
	_, kept := channelCooldowns.Load(key)
	assert.False(t, kept, "an expired window must be dropped instead of accumulating")
}

func TestSelectChannelCandidatePassesOverCoolingChannels(t *testing.T) {
	const topCooling = 900111
	const topReady = 900112
	const lower = 900113

	cases := []struct {
		name       string
		candidates []channelCandidate
		retry      int
		excluded   map[int]bool
		want       int
	}{
		{
			name: "a ready sibling in the same tier wins",
			candidates: []channelCandidate{
				{channelId: topCooling, priority: 10, weight: 1000, coolingDown: true},
				{channelId: topReady, priority: 10},
			},
			want: topReady,
		},
		{
			name: "a cooling tier demotes to the next one",
			candidates: []channelCandidate{
				{channelId: topCooling, priority: 10, coolingDown: true},
				{channelId: lower, priority: 5},
			},
			want: lower,
		},
		{
			name: "with every candidate cooling one is still chosen",
			candidates: []channelCandidate{
				{channelId: topCooling, priority: 10, coolingDown: true},
				{channelId: lower, priority: 5, coolingDown: true},
			},
			want: topCooling,
		},
		{
			name: "an already tried ready channel does not hold the cooling one back",
			candidates: []channelCandidate{
				{channelId: topCooling, priority: 10, coolingDown: true},
				{channelId: topReady, priority: 10},
			},
			excluded: map[int]bool{topReady: true},
			want:     topCooling,
		},
		{
			name: "a ready channel above the starting tier does not count",
			candidates: []channelCandidate{
				{channelId: topReady, priority: 10},
				{channelId: lower, priority: 5, coolingDown: true},
			},
			retry: 1,
			want:  lower,
		},
	}
	// Each case leaves exactly one eligible channel in the first non-empty tier,
	// so the weighted draw has a single outcome.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := selectChannelCandidate(tc.candidates, tc.retry, tc.excluded)
			require.True(t, ok)
			assert.Equal(t, tc.want, id)
		})
	}
}
