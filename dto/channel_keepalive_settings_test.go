package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The settings column is shared with the admin form, which writes these keys by
// name. A rename here would silently turn every saved switch off.
func TestUpstreamKeepaliveSettingsJSON(t *testing.T) {
	var settings ChannelOtherSettings
	require.NoError(t, common.UnmarshalJsonStr(
		`{"upstream_keepalive_enabled":true,"upstream_keepalive_interval_minutes":45,"upstream_keepalive_active_window_minutes":240,"upstream_keepalive_last_time":1700000000}`,
		&settings))
	assert.True(t, settings.UpstreamKeepaliveEnabled)
	assert.Equal(t, 45, settings.UpstreamKeepaliveIntervalMinutes)
	assert.Equal(t, 240, settings.UpstreamKeepaliveActiveWindowMinutes)
	assert.Equal(t, int64(1700000000), settings.UpstreamKeepaliveLastTime)

	// A channel that never touched keepalive keeps its stored JSON unchanged.
	encoded, err := common.Marshal(ChannelOtherSettings{})
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(encoded))
}

func TestValidateUpstreamKeepalive(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		valid   bool
	}{
		{0, true},
		{5, true},
		{30, true},
		{120, true},
		{10080, true},
		{4, false},
		{1, false},
		{-30, false},
		{10081, false},
	} {
		interval := ChannelOtherSettings{UpstreamKeepaliveIntervalMinutes: tc.minutes}
		window := ChannelOtherSettings{UpstreamKeepaliveActiveWindowMinutes: tc.minutes}
		if tc.valid {
			assert.NoError(t, interval.ValidateUpstreamKeepalive(), "interval minutes=%d", tc.minutes)
			assert.NoError(t, window.ValidateUpstreamKeepalive(), "window minutes=%d", tc.minutes)
		} else {
			assert.ErrorContains(t, interval.ValidateUpstreamKeepalive(), "upstream_keepalive_interval_minutes", "minutes=%d", tc.minutes)
			assert.ErrorContains(t, window.ValidateUpstreamKeepalive(), "upstream_keepalive_active_window_minutes", "minutes=%d", tc.minutes)
		}
	}
}

// Values that reached the database without validation must not shrink the
// interval below the floor.
func TestUpstreamKeepaliveInterval(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		seconds int64
	}{
		{0, 30 * 60},
		{-5, 30 * 60},
		{1, 5 * 60},
		{5, 5 * 60},
		{90, 90 * 60},
		{UpstreamKeepaliveMaxIntervalMinutes + 100, UpstreamKeepaliveMaxIntervalMinutes * 60},
	} {
		settings := ChannelOtherSettings{UpstreamKeepaliveIntervalMinutes: tc.minutes}
		assert.Equal(t, tc.seconds, settings.UpstreamKeepaliveInterval(), "minutes=%d", tc.minutes)
	}
}

// The active window is clamped like the interval, with its own default.
func TestUpstreamKeepaliveActiveWindow(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		seconds int64
	}{
		{0, 120 * 60},
		{-5, 120 * 60},
		{1, 5 * 60},
		{5, 5 * 60},
		{240, 240 * 60},
		{UpstreamKeepaliveMaxActiveWindowMinutes + 100, UpstreamKeepaliveMaxActiveWindowMinutes * 60},
	} {
		settings := ChannelOtherSettings{UpstreamKeepaliveActiveWindowMinutes: tc.minutes}
		assert.Equal(t, tc.seconds, settings.UpstreamKeepaliveActiveWindow(), "minutes=%d", tc.minutes)
	}
}

// TestUpstreamKeepaliveDue pins the activity-aware schedule with the default
// 30-minute interval and 120-minute window. The minutes chosen sit outside the
// ±10% jitter, so every verdict is exact.
func TestUpstreamKeepaliveDue(t *testing.T) {
	const active int64 = 1_700_000_000
	const minute int64 = 60
	on := func(lastTime int64) *ChannelOtherSettings {
		return &ChannelOtherSettings{UpstreamKeepaliveEnabled: true, UpstreamKeepaliveLastTime: lastTime}
	}
	for _, tc := range []struct {
		name       string
		settings   *ChannelOtherSettings
		lastActive int64
		now        int64
		due        bool
	}{
		{"switched off", &ChannelOtherSettings{}, active, active + 40*minute, false},
		{"no real request seen", on(0), 0, active, false},
		{"task10 send stamp without real traffic", on(active - 600*minute), 0, active, false},
		{"request in flight", on(0), active + 40*minute, active + 40*minute, false},
		{"active, interval not reached", on(0), active, active + 20*minute, false},
		{"quiet for an interval inside the window", on(0), active, active + 40*minute, true},
		{"old task10 send stamp before the activity", on(active - 600*minute), active, active + 40*minute, true},
		{"recent keepalive, interval not reached", on(active + 40*minute), active, active + 60*minute, false},
		{"keepalive interval reached inside the window", on(active + 40*minute), active, active + 80*minute, true},
		{"window closed", on(active + 80*minute), active, active + 120*minute, false},
		{"long after the window", on(active + 80*minute), active, active + 600*minute, false},
		{"longer window keeps bridging", &ChannelOtherSettings{
			UpstreamKeepaliveEnabled: true, UpstreamKeepaliveActiveWindowMinutes: 240, UpstreamKeepaliveLastTime: active + 160*minute,
		}, active, active + 200*minute, true},
		{"dirty window value is clamped, not unbounded", &ChannelOtherSettings{
			UpstreamKeepaliveEnabled: true, UpstreamKeepaliveActiveWindowMinutes: 1_000_000,
		}, active, active + 10081*minute, false},
	} {
		assert.Equal(t, tc.due, tc.settings.UpstreamKeepaliveDue(7, tc.lastActive, tc.now), tc.name)
	}
}

// TestUpstreamKeepaliveDueStopsWithoutRealTraffic replays a scheduler that
// checks every minute and stamps each send, as the keepalive task does. The
// keepalive does not move the activity record, so after one real request it
// bridges the gap a few times, stops for good once the window closes, and only
// a new real request brings it back.
func TestUpstreamKeepaliveDueStopsWithoutRealTraffic(t *testing.T) {
	const start int64 = 1_700_000_000
	const minute int64 = 60
	settings := ChannelOtherSettings{UpstreamKeepaliveEnabled: true}
	lastActive := start
	var sends []int64
	for now := start; now <= start+600*minute; now += minute {
		if now == start+300*minute {
			lastActive = now
		}
		if settings.UpstreamKeepaliveDue(3, lastActive, now) {
			settings.UpstreamKeepaliveLastTime = now
			sends = append(sends, (now-start)/minute)
		}
	}
	var first, renewed []int64
	for _, minutes := range sends {
		switch {
		case minutes < 120:
			first = append(first, minutes)
		case minutes >= 300 && minutes < 420:
			renewed = append(renewed, minutes)
		default:
			t.Fatalf("keepalive sent %d minutes in, outside both active windows: %v", minutes, sends)
		}
	}
	// 30 minutes ±10% apart inside a 120-minute window: three or four sends.
	assert.GreaterOrEqual(t, len(first), 3, "%v", sends)
	assert.LessOrEqual(t, len(first), 4, "%v", sends)
	assert.GreaterOrEqual(t, first[0], int64(27), "nothing is sent before the channel has been quiet for an interval")
	assert.GreaterOrEqual(t, len(renewed), 3, "a new real request reopens the window: %v", sends)
	assert.GreaterOrEqual(t, renewed[0], int64(327))
}

// Channels used at the same moment drift apart: none is due before 90% of the
// interval, all are due after 110%, and the ones due in between differ.
func TestUpstreamKeepaliveDueJitter(t *testing.T) {
	const active int64 = 1_700_000_000
	const interval int64 = 30 * 60
	dueAtInterval := 0
	for channelId := 1; channelId <= 200; channelId++ {
		settings := ChannelOtherSettings{UpstreamKeepaliveEnabled: true}
		require.False(t, settings.UpstreamKeepaliveDue(channelId, active, active+interval*9/10-1), "channel %d due too early", channelId)
		require.True(t, settings.UpstreamKeepaliveDue(channelId, active, active+interval*11/10), "channel %d not due after the jitter window", channelId)
		// The verdict for one channel is stable between the scheduler's check
		// and the run that follows it.
		require.Equal(t, settings.UpstreamKeepaliveDue(channelId, active, active+interval), settings.UpstreamKeepaliveDue(channelId, active, active+interval))
		if settings.UpstreamKeepaliveDue(channelId, active, active+interval) {
			dueAtInterval++
		}
	}
	assert.Greater(t, dueAtInterval, 0)
	assert.Less(t, dueAtInterval, 200)
}
