package limiter

import (
	"context"
	_ "embed"
	"time"

	"github.com/go-redis/redis/v8"
)

//go:embed lua/sliding_window.lua
var slidingWindowScript string

// slidingWindow is safe for concurrent use: redis.Script caches the SHA and
// falls back to a plain EVAL when Redis reports NOSCRIPT, so a Redis restart
// that clears the script cache repairs itself instead of failing every request.
var slidingWindow = redis.NewScript(slidingWindowScript)

// AllowSlidingWindow reports whether one more request fits inside the window for
// key. The check and the record happen in a single atomic script; splitting them
// across round trips lets a concurrent burst through the limit entirely.
func AllowSlidingWindow(ctx context.Context, rdb *redis.Client, key string, maxRequestNum int, window time.Duration, ttl time.Duration) (bool, error) {
	result, err := slidingWindow.Run(ctx, rdb, []string{key},
		maxRequestNum,
		int64(window.Seconds()),
		time.Now().Unix(),
		int64(ttl.Seconds()),
	).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}
