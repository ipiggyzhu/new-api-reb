package limiter

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/go-redis/redis/v8"
)

//go:embed lua/rate_limit.lua
var rateLimitScript string

// rateLimit is safe for concurrent use. redis.Script tries EVALSHA first and
// transparently falls back to EVAL when Redis answers NOSCRIPT, so a Redis
// restart — which drops the script cache — repairs itself. Resolving the SHA
// once at startup instead meant every later call failed with NOSCRIPT, which the
// relay middleware turns into a 500: one Redis restart took the gateway down
// until new-api itself was restarted.
var rateLimit = redis.NewScript(rateLimitScript)

type RedisLimiter struct {
	client *redis.Client
}

// New returns a limiter bound to r. It used to memoise the first client it ever
// saw, which would keep using a stale connection if RDB were ever rebuilt.
func New(ctx context.Context, r *redis.Client) *RedisLimiter {
	return &RedisLimiter{client: r}
}

func (rl *RedisLimiter) Allow(ctx context.Context, key string, opts ...Option) (bool, error) {
	// 默认配置
	config := &Config{
		Capacity:  10,
		Rate:      1,
		Requested: 1,
	}

	// 应用选项模式
	for _, opt := range opts {
		opt(config)
	}

	// 执行限流
	result, err := rateLimit.Run(
		ctx,
		rl.client,
		[]string{key},
		config.Requested,
		config.Rate,
		config.Capacity,
	).Int()

	if err != nil {
		return false, fmt.Errorf("rate limit failed: %w", err)
	}
	return result == 1, nil
}

// Config 配置选项模式
type Config struct {
	Capacity  int64
	Rate      int64
	Requested int64
}

type Option func(*Config)

func WithCapacity(c int64) Option {
	return func(cfg *Config) { cfg.Capacity = c }
}

func WithRate(r int64) Option {
	return func(cfg *Config) { cfg.Rate = r }
}

func WithRequested(n int64) Option {
	return func(cfg *Config) { cfg.Requested = n }
}
