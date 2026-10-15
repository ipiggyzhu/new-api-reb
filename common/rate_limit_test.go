package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInMemoryRateLimiterInitIsIdempotent pins the contract the rate limit
// middlewares depend on: Init runs on every request, so a second call must not
// discard the counters accumulated so far. Replacing the store would reset every
// caller's window and let a client that just exhausted its budget start over.
func TestInMemoryRateLimiterInitIsIdempotent(t *testing.T) {
	limiter := &InMemoryRateLimiter{}
	limiter.Init(0)

	require.True(t, limiter.Request("client", 1, 60), "first request within the budget must pass")
	require.False(t, limiter.Request("client", 1, 60), "budget of 1 must reject the second request")

	limiter.Init(0)

	assert.False(t, limiter.Request("client", 1, 60),
		"a repeat Init must keep the existing window instead of handing out a fresh budget")
}

// TestInMemoryRateLimiterInitKeepsFirstExpiration documents that only the first
// expirationDuration wins. The sweeper goroutine is started once and reads the
// field forever, so a later value would be recorded but never honoured — the
// caller should not be able to believe it changed the sweep interval.
func TestInMemoryRateLimiterInitKeepsFirstExpiration(t *testing.T) {
	limiter := &InMemoryRateLimiter{}
	limiter.Init(0)
	limiter.Init(30 * time.Minute)

	assert.Equal(t, time.Duration(0), limiter.expirationDuration,
		"the expiration captured by the first Init must survive later calls")
}

// TestInMemoryRateLimiterCheckFollowsTheWindow pins the window Check decides by:
// a full queue refuses while its oldest request is inside the window and admits
// once that request has aged out. The queues are seeded directly because ageing
// them through Request would mean waiting out a window.
func TestInMemoryRateLimiterCheckFollowsTheWindow(t *testing.T) {
	limiter := &InMemoryRateLimiter{}
	limiter.Init(0)
	now := time.Now().Unix()
	limiter.store["fresh"] = &[]int64{now - 10, now}
	limiter.store["aged"] = &[]int64{now - 120, now}

	assert.False(t, limiter.Check("fresh", 2, 60), "the oldest request is still inside the window")
	assert.True(t, limiter.Check("aged", 2, 60), "the oldest request has left the window")
}

// TestInMemoryRateLimiterInitIsRaceFree exercises the concurrent Init the rate
// limit middlewares actually perform (one call per request). Under -race a
// double-checked read of store outside the mutex is reported here; without the
// detector this still catches a store replaced after requests were counted.
func TestInMemoryRateLimiterInitIsRaceFree(t *testing.T) {
	limiter := &InMemoryRateLimiter{}

	const goroutines = 32
	start := make(chan struct{})
	done := make(chan bool, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			limiter.Init(0)
			done <- limiter.Request("shared", goroutines, 60)
		}()
	}
	close(start)

	granted := 0
	for i := 0; i < goroutines; i++ {
		if <-done {
			granted++
		}
	}

	assert.Equal(t, goroutines, granted,
		"a budget of %d must admit exactly %d concurrent requests; a store replaced mid-flight would lose counted requests", goroutines, goroutines)
}

// Cleanup must use each key's request window, not the first middleware's
// housekeeping interval, or long-window limits silently reset early.
func TestInMemoryRateLimiterCleanupPreservesEachWindow(t *testing.T) {
	for _, extendOnCheck := range []bool{false, true} {
		t.Run(map[bool]string{false: "request window", true: "extended check window"}[extendOnCheck], func(t *testing.T) {
			limiter := &InMemoryRateLimiter{}
			limiter.Init(10 * time.Millisecond)
			window := int64(3600)
			if extendOnCheck {
				window = 60
			}
			require.True(t, limiter.Request("long", 1, window))
			require.True(t, limiter.Request("short", 1, 1))
			if extendOnCheck {
				require.False(t, limiter.Check("long", 1, 3600))
			}
			limiter.mutex.Lock()
			*limiter.store["long"] = []int64{time.Now().Unix() - 120}
			*limiter.store["short"] = []int64{time.Now().Unix() - 120}
			limiter.mutex.Unlock()
			// Wait for an actual cleanup pass, rather than guessing with a sleep.
			require.Eventually(t, func() bool {
				limiter.mutex.Lock()
				defer limiter.mutex.Unlock()
				_, exists := limiter.store["short"]
				return !exists
			}, time.Second, 5*time.Millisecond)
			assert.False(t, limiter.Check("long", 1, 3600))
		})
	}
}
