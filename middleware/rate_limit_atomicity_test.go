package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withMiniRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	gin.SetMode(gin.TestMode)

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})

	originalRDB, originalEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		_ = client.Close()
		common.RDB, common.RedisEnabled = originalRDB, originalEnabled
	})
	return server
}

// countAllowed runs the limiter from `concurrency` goroutines at once against a
// single client IP and reports how many were let through.
func countAllowed(t *testing.T, concurrency, maxRequestNum int, duration int64, mark string) int {
	t.Helper()

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/api/user/login", nil)
			c.Request.RemoteAddr = "198.51.100.9:44321"

			<-start
			redisRateLimiter(c, maxRequestNum, duration, mark)
			if !c.IsAborted() {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	return allowed
}

// CriticalRateLimit guards login, register, 2FA and password reset. Checking the
// counter and appending to it were separate round trips, so a burst of
// concurrent attempts all read the same under-limit length and all got through
// — the brute-force guard only ever constrained sequential traffic.
func TestRedisRateLimiterIsAtomicUnderConcurrency(t *testing.T) {
	withMiniRedis(t)

	const (
		concurrency   = 64
		maxRequestNum = 3
		duration      = int64(60)
	)

	allowed := countAllowed(t, concurrency, maxRequestNum, duration, "CT")

	assert.Equal(t, maxRequestNum, allowed,
		"a concurrent burst must not exceed the limit any more than a sequential one")
}

// The ordinary sequential path must keep behaving exactly as before: the first
// maxRequestNum attempts pass, the rest are rejected inside the window.
func TestRedisRateLimiterStillLimitsSequentialTraffic(t *testing.T) {
	withMiniRedis(t)

	const maxRequestNum = 3
	allowedInARow := 0
	for i := 0; i < 10; i++ {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/user/login", nil)
		c.Request.RemoteAddr = "198.51.100.10:44321"
		redisRateLimiter(c, maxRequestNum, 60, "CT")
		if !c.IsAborted() {
			allowedInARow++
		}
	}
	assert.Equal(t, maxRequestNum, allowedInARow)
}

// Different clients must not share a bucket.
func TestRedisRateLimiterKeysPerClient(t *testing.T) {
	withMiniRedis(t)

	for _, ip := range []string{"203.0.113.1:1", "203.0.113.2:1"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/user/login", nil)
		c.Request.RemoteAddr = ip
		redisRateLimiter(c, 1, 60, "CT")
		assert.False(t, c.IsAborted(), "first request from %s must pass", ip)
	}
}

// The window has to actually roll: once the oldest entry falls outside it, a
// new attempt is allowed again.
func TestRedisRateLimiterAllowsAfterWindowExpires(t *testing.T) {
	withMiniRedis(t)

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/user/login", nil)
		c.Request.RemoteAddr = "203.0.113.55:1"
		return c
	}

	first := newCtx()
	redisRateLimiter(first, 1, 1, "CT")
	require.False(t, first.IsAborted())

	blocked := newCtx()
	redisRateLimiter(blocked, 1, 1, "CT")
	require.True(t, blocked.IsAborted(), "second attempt inside the window is rejected")

	// The stored timestamp is what decides the window, so age it directly rather
	// than sleeping.
	key := "rateLimit:CT" + "203.0.113.55"
	require.NoError(t, common.RDB.Del(context.Background(), key).Err())

	after := newCtx()
	redisRateLimiter(after, 1, 1, "CT")
	assert.False(t, after.IsAborted(), "a fresh window admits requests again")
}

// Buckets written by the previous implementation hold formatted timestamps, not
// numbers. On deploy those keys are still live, so the script must not error on
// them — it treats an unparseable entry as an expired window and the key heals
// itself within one window.
func TestRedisRateLimiterToleratesLegacyTimestampEntries(t *testing.T) {
	server := withMiniRedis(t)

	key := "rateLimit:CT" + "203.0.113.77"
	_, err := server.Lpush(key, "2026-07-26T10:00:00.000Z")
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/login", nil)
	c.Request.RemoteAddr = "203.0.113.77:1"

	redisRateLimiter(c, 1, 60, "CT")

	assert.False(t, c.IsAborted(), "a legacy entry must not turn into a 500")
}

// The per-user limiter shares the same script, so it has to be atomic too.
func TestUserRedisRateLimiterIsAtomicUnderConcurrency(t *testing.T) {
	withMiniRedis(t)

	const (
		concurrency   = 64
		maxRequestNum = 2
	)

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/api/search", nil)

			<-start
			userRedisRateLimiter(c, maxRequestNum, 60, "rateLimit:SR:user:1234")
			if !c.IsAborted() {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, maxRequestNum, allowed)
}
