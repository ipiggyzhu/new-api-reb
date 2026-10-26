package operation_setting

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A non-streaming request cannot be kept alive while it waits for its pinned
// channel, so however the option is set the wait must leave the upstream most
// of Cloudflare's ~100s window, and a non-positive value means no waiting.
func TestSaturatedWaitIsBounded(t *testing.T) {
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{seconds: -5, want: 0},
		{seconds: 0, want: 0},
		{seconds: 20, want: 20 * time.Second},
		{seconds: 60, want: 60 * time.Second},
		{seconds: 600, want: MaxChannelAffinitySaturatedWait},
	}
	for _, tc := range cases {
		setting := ChannelAffinitySetting{SaturatedWaitSeconds: tc.seconds}
		assert.Equal(t, tc.want, setting.SaturatedWait(), "saturated_wait_seconds=%d", tc.seconds)
	}
}
