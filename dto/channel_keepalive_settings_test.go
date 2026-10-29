package dto

import (
	"fmt"
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

// TestUpstreamKeepaliveDue pins the activity-aware schedule of one key with the
// default 30-minute interval and 120-minute window. The minutes chosen sit
// outside the ±10% jitter, so every verdict is exact.
func TestUpstreamKeepaliveDue(t *testing.T) {
	const active int64 = 1_700_000_000
	const minute int64 = 60
	on := &ChannelOtherSettings{UpstreamKeepaliveEnabled: true}
	for _, tc := range []struct {
		name          string
		settings      *ChannelOtherSettings
		lastActive    int64
		lastKeepalive int64
		now           int64
		due           bool
	}{
		{"switched off", &ChannelOtherSettings{}, active, 0, active + 40*minute, false},
		{"no real request seen", on, 0, 0, active, false},
		{"keepalive stamp without real traffic", on, 0, active - 600*minute, active, false},
		{"request in flight", on, active + 40*minute, 0, active + 40*minute, false},
		{"active, interval not reached", on, active, 0, active + 20*minute, false},
		{"quiet for an interval inside the window", on, active, 0, active + 40*minute, true},
		{"keepalive from before the activity", on, active, active - 600*minute, active + 40*minute, true},
		{"recent keepalive, interval not reached", on, active, active + 40*minute, active + 60*minute, false},
		{"keepalive interval reached inside the window", on, active, active + 40*minute, active + 80*minute, true},
		{"window closed", on, active, active + 80*minute, active + 120*minute, false},
		{"long after the window", on, active, active + 80*minute, active + 600*minute, false},
		// The channel's stored stamp is written by whichever key went last; it
		// must not hold back another key.
		{"another key's keepalive on the channel", &ChannelOtherSettings{
			UpstreamKeepaliveEnabled: true, UpstreamKeepaliveLastTime: active + 40*minute,
		}, active, 0, active + 60*minute, true},
		{"longer window keeps bridging", &ChannelOtherSettings{
			UpstreamKeepaliveEnabled: true, UpstreamKeepaliveActiveWindowMinutes: 240,
		}, active, active + 160*minute, active + 200*minute, true},
		{"dirty window value is clamped, not unbounded", &ChannelOtherSettings{
			UpstreamKeepaliveEnabled: true, UpstreamKeepaliveActiveWindowMinutes: 1_000_000,
		}, active, 0, active + 10081*minute, false},
	} {
		assert.Equal(t, tc.due, tc.settings.UpstreamKeepaliveDue(7, "0", tc.lastActive, tc.lastKeepalive, tc.now), tc.name)
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
	var lastKeepalive int64
	var sends []int64
	for now := start; now <= start+600*minute; now += minute {
		if now == start+300*minute {
			lastActive = now
		}
		if settings.UpstreamKeepaliveDue(3, "0", lastActive, lastKeepalive, now) {
			lastKeepalive = now
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

// Keys used at the same moment drift apart, including the keys of one channel:
// none is due before 90% of the interval, all are due after 110%, and the ones
// due in between differ.
func TestUpstreamKeepaliveDueJitter(t *testing.T) {
	const active int64 = 1_700_000_000
	const interval int64 = 30 * 60
	settings := ChannelOtherSettings{UpstreamKeepaliveEnabled: true}
	dueAtInterval := 0
	for key := 1; key <= 200; key++ {
		slot := fmt.Sprintf("k%d", key)
		require.False(t, settings.UpstreamKeepaliveDue(7, slot, active, 0, active+interval*9/10-1), "key %s due too early", slot)
		require.True(t, settings.UpstreamKeepaliveDue(7, slot, active, 0, active+interval*11/10), "key %s not due after the jitter window", slot)
		// The verdict for one key is stable between the scheduler's check and
		// the run that follows it.
		require.Equal(t, settings.UpstreamKeepaliveDue(7, slot, active, 0, active+interval), settings.UpstreamKeepaliveDue(7, slot, active, 0, active+interval))
		if settings.UpstreamKeepaliveDue(7, slot, active, 0, active+interval) {
			dueAtInterval++
		}
	}
	assert.Greater(t, dueAtInterval, 0)
	assert.Less(t, dueAtInterval, 200)
}
