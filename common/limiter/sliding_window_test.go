package limiter

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A limit that only counts successful requests checks before serving and records
// afterwards. The check must leave the bucket untouched, or a request that goes
// on to fail would still be counted.
func TestCheckSlidingWindowDoesNotRecord(t *testing.T) {
	server, client := newLimiterRedis(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		allowed, err := CheckSlidingWindow(ctx, client, "sw-check", 1, time.Minute, time.Minute)
		require.NoError(t, err)
		assert.True(t, allowed, "check %d", i)
	}
	assert.False(t, server.Exists("sw-check"), "checking must not create the bucket")
}

// Recording keeps only the newest maxRequestNum entries and gives the bucket a
// TTL, so a full window rejects the next check and an idle bucket goes away.
func TestRecordSlidingWindowFillsTheWindow(t *testing.T) {
	server, client := newLimiterRedis(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, RecordSlidingWindow(ctx, client, "sw-record", 2, time.Minute))
	}
	entries, err := server.List("sw-record")
	require.NoError(t, err)
	assert.Len(t, entries, 2)
	assert.Equal(t, time.Minute, server.TTL("sw-record"))

	allowed, err := CheckSlidingWindow(ctx, client, "sw-record", 2, time.Minute, time.Minute)
	require.NoError(t, err)
	assert.False(t, allowed, "a window filled by records rejects the next check")
}

// A maximum near the int64 range is how an operator leaves a limit effectively
// off. Handed back to Redis as a trim bound it would be formatted as a float
// that LTRIM rejects, failing the request after the push and before the TTL.
func TestSlidingWindowHugeMaximumAdmitsAndKeepsTTL(t *testing.T) {
	server, client := newLimiterRedis(t)
	ctx := context.Background()

	allowed, err := AllowSlidingWindow(ctx, client, "sw-huge-allow", math.MaxInt64, time.Minute, time.Minute)
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Equal(t, time.Minute, server.TTL("sw-huge-allow"))

	require.NoError(t, RecordSlidingWindow(ctx, client, "sw-huge-record", math.MaxInt64, time.Minute))
	assert.Equal(t, time.Minute, server.TTL("sw-huge-record"))
}
