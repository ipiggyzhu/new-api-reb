package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var modelRateLimitModes = []struct {
	name    string
	setup   func(t *testing.T)
	handler func(duration int64, totalMaxCount, successMaxCount int) gin.HandlerFunc
}{
	{"redis", func(t *testing.T) { withMiniRedis(t) }, redisRateLimitHandler},
	{"memory", func(t *testing.T) { gin.SetMode(gin.TestMode) }, memoryRateLimitHandler},
}

// The in-memory limiter is package state that outlives a single test, so each
// case takes a user id that no earlier case, or earlier -count run, has used.
var lastModelRateLimitUserId = 70000

func nextModelRateLimitUserId() int {
	lastModelRateLimitUserId++
	return lastModelRateLimitUserId
}

// serveModelRateLimited sends one request from userId through limit and has the
// relay answer it with status.
func serveModelRateLimited(limit gin.HandlerFunc, userId int, status int) int {
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("id", userId) }, limit)
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(status) })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	return recorder.Code
}

// The success-count limit counts only requests that succeeded, so a user whose
// requests keep failing is held back by the total-count limit alone. Memory mode
// used to check through a key that recorded every admitted request, which made
// failures consume the success quota there.
func TestModelRateLimitCountsOnlySuccessfulRequests(t *testing.T) {
	for _, mode := range modelRateLimitModes {
		t.Run(mode.name, func(t *testing.T) {
			mode.setup(t)
			limit := mode.handler(60, 0, 2)
			userId := nextModelRateLimitUserId()

			for i := 0; i < 5; i++ {
				require.Equal(t, http.StatusInternalServerError, serveModelRateLimited(limit, userId, http.StatusInternalServerError), "failed request %d", i)
			}
			for i := 0; i < 2; i++ {
				require.Equal(t, http.StatusOK, serveModelRateLimited(limit, userId, http.StatusOK), "successful request %d", i)
			}
			assert.Equal(t, http.StatusTooManyRequests, serveModelRateLimited(limit, userId, http.StatusOK))
		})
	}
}

// The total-count limit counts every admitted request, failed or not.
func TestModelRateLimitTotalCountIncludesFailures(t *testing.T) {
	for _, mode := range modelRateLimitModes {
		t.Run(mode.name, func(t *testing.T) {
			mode.setup(t)
			limit := mode.handler(60, 2, 100)
			userId := nextModelRateLimitUserId()

			for i := 0; i < 2; i++ {
				require.Equal(t, http.StatusInternalServerError, serveModelRateLimited(limit, userId, http.StatusInternalServerError), "failed request %d", i)
			}
			assert.Equal(t, http.StatusTooManyRequests, serveModelRateLimited(limit, userId, http.StatusOK))
		})
	}
}

// A request refused by the success-count limit was never served, so it does not
// use up the total-count limit either. Limits follow the token's group while the
// buckets follow the user, so the same user can meet a higher success limit on
// the next request and must find only the served request in the total count.
func TestModelRateLimitSuccessRejectionSparesTotalCount(t *testing.T) {
	for _, mode := range modelRateLimitModes {
		t.Run(mode.name, func(t *testing.T) {
			mode.setup(t)
			userId := nextModelRateLimitUserId()

			strict := mode.handler(60, 2, 1)
			require.Equal(t, http.StatusOK, serveModelRateLimited(strict, userId, http.StatusOK))
			require.Equal(t, http.StatusTooManyRequests, serveModelRateLimited(strict, userId, http.StatusOK))

			relaxed := mode.handler(60, 2, 100)
			assert.Equal(t, http.StatusOK, serveModelRateLimited(relaxed, userId, http.StatusOK))
		})
	}
}

// Successes older than the window stop counting. The handler turns the minute
// setting into the window in seconds that the shared script compares against,
// so a unit slip there keeps old successes counting against the user.
func TestModelRateLimitForgetsSuccessesOutsideTheWindow(t *testing.T) {
	cases := []struct {
		name string
		age  int64
		want int
	}{
		{"inside the window", 10, http.StatusTooManyRequests},
		{"outside the window", 120, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := withMiniRedis(t)
			userId := nextModelRateLimitUserId()
			key := fmt.Sprintf("rateLimit:%s:%d", ModelRequestRateLimitSuccessCountMark, userId)
			success := strconv.FormatInt(time.Now().Unix()-tc.age, 10)
			for i := 0; i < 2; i++ {
				_, err := server.Lpush(key, success)
				require.NoError(t, err)
			}

			assert.Equal(t, tc.want, serveModelRateLimited(redisRateLimitHandler(60, 0, 2), userId, http.StatusOK))
		})
	}
}

// A success count of 0 means no success limit. The shared window script rejects
// a non-positive limit outright, so the middleware has to keep skipping it.
func TestModelRateLimitZeroSuccessCountIsUnlimited(t *testing.T) {
	for _, mode := range modelRateLimitModes {
		t.Run(mode.name, func(t *testing.T) {
			mode.setup(t)
			limit := mode.handler(60, 0, 0)
			userId := nextModelRateLimitUserId()

			for i := 0; i < 5; i++ {
				assert.Equal(t, http.StatusOK, serveModelRateLimited(limit, userId, http.StatusOK), "request %d", i)
			}
		})
	}
}

// A negative success count can only be written through the option API, past the
// settings UI. Both modes refuse the request before it is relayed: memory mode
// used to let it through, bill it, and then panic recording the success.
func TestModelRateLimitNegativeSuccessCountRejectsBeforeServing(t *testing.T) {
	for _, mode := range modelRateLimitModes {
		t.Run(mode.name, func(t *testing.T) {
			mode.setup(t)
			limit := mode.handler(60, 0, -1)
			userId := nextModelRateLimitUserId()

			assert.Equal(t, http.StatusTooManyRequests, serveModelRateLimited(limit, userId, http.StatusOK))
		})
	}
}

// Success buckets written before the move to the shared window script hold
// formatted timestamps. After deploy they read as an expired window: a user
// already at the limit gets at most one more limit's worth of requests while the
// bucket refills with numeric entries, and then the limit applies again.
func TestModelRateLimitHealsLegacySuccessEntries(t *testing.T) {
	server := withMiniRedis(t)
	userId := nextModelRateLimitUserId()
	key := fmt.Sprintf("rateLimit:%s:%d", ModelRequestRateLimitSuccessCountMark, userId)
	for _, legacy := range []string{"2026-09-27T01:00:00.000Z", "2026-09-27T01:00:30.000Z"} {
		_, err := server.Lpush(key, legacy)
		require.NoError(t, err)
	}

	limit := redisRateLimitHandler(60, 0, 2)
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, serveModelRateLimited(limit, userId, http.StatusOK), "request %d over a legacy bucket", i)
	}
	assert.Equal(t, http.StatusTooManyRequests, serveModelRateLimited(limit, userId, http.StatusOK), "once refilled, the bucket limits again")
}
