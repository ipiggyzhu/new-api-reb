package limiter

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLimiterRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

// Redis drops its script cache on restart and on SCRIPT FLUSH. A limiter that
// resolved its SHA once at startup then answers every later call with NOSCRIPT,
// and the relay middleware turns that into a 500 — so a Redis restart would take
// the whole gateway down until new-api itself was restarted.
func TestTokenBucketSurvivesScriptCacheFlush(t *testing.T) {
	server, client := newLimiterRedis(t)
	ctx := context.Background()

	rl := New(ctx, client)
	allowed, err := rl.Allow(ctx, "flush-test", WithCapacity(100), WithRate(10), WithRequested(1))
	require.NoError(t, err)
	require.True(t, allowed, "precondition: the limiter works before the flush")

	// Same thing Redis does across a restart.
	server.FlushAll()
	require.NoError(t, client.ScriptFlush(ctx).Err())

	allowed, err = rl.Allow(ctx, "flush-test", WithCapacity(100), WithRate(10), WithRequested(1))
	assert.NoError(t, err, "a dropped script cache must not fail every request")
	assert.True(t, allowed)
}

// The sliding-window limiter shares the same exposure and must recover too.
func TestSlidingWindowSurvivesScriptCacheFlush(t *testing.T) {
	server, client := newLimiterRedis(t)
	ctx := context.Background()

	allowed, err := AllowSlidingWindow(ctx, client, "sw-flush", 5, time.Minute, time.Minute)
	require.NoError(t, err)
	require.True(t, allowed)

	server.FlushAll()
	require.NoError(t, client.ScriptFlush(ctx).Err())

	allowed, err = AllowSlidingWindow(ctx, client, "sw-flush", 5, time.Minute, time.Minute)
	assert.NoError(t, err)
	assert.True(t, allowed)
}
