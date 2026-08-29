package cachex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/samber/hot"
)

const (
	defaultRedisOpTimeout   = 2 * time.Second
	defaultRedisScanTimeout = 30 * time.Second
	defaultRedisDelTimeout  = 10 * time.Second
)

type HybridCacheConfig[V any] struct {
	Namespace Namespace

	// Redis is used when RedisEnabled returns true (or RedisEnabled is nil) and Redis is not nil.
	Redis        *redis.Client
	RedisCodec   ValueCodec[V]
	RedisEnabled func() bool

	// Memory builds a hot cache used when Redis is disabled. Keys stored in memory are fully namespaced.
	Memory func() *hot.HotCache[string, V]
}

// HybridCache is a small helper that uses Redis when enabled, otherwise falls back to in-memory hot cache.
type HybridCache[V any] struct {
	ns Namespace

	redis        *redis.Client
	redisCodec   ValueCodec[V]
	redisEnabled func() bool

	memOnce sync.Once
	memInit func() *hot.HotCache[string, V]
	mem     *hot.HotCache[string, V]

	// memMu serialises the in-memory path's mutations so a compare-and-delete can
	// be atomic with respect to writes. hot.HotCache locks each individual call but
	// exposes no CAS primitive, so DeleteIfEquals would otherwise read, compare and
	// delete as three separately-locked steps and could discard a value written in
	// between — the very entry the comparison was meant to protect.
	//
	// Reads stay outside it: Get is on the request hot path, and letting it observe
	// a value that is about to be deleted is no worse than the ordinary race
	// between a cache read and a concurrent invalidation.
	memMu sync.Mutex
}

func NewHybridCache[V any](cfg HybridCacheConfig[V]) *HybridCache[V] {
	return &HybridCache[V]{
		ns:           cfg.Namespace,
		redis:        cfg.Redis,
		redisCodec:   cfg.RedisCodec,
		redisEnabled: cfg.RedisEnabled,
		memInit:      cfg.Memory,
	}
}

func (c *HybridCache[V]) FullKey(key string) string {
	return c.ns.FullKey(key)
}

func (c *HybridCache[V]) redisOn() bool {
	if c.redis == nil || c.redisCodec == nil {
		return false
	}
	if c.redisEnabled == nil {
		return true
	}
	return c.redisEnabled()
}

func (c *HybridCache[V]) memCache() *hot.HotCache[string, V] {
	c.memOnce.Do(func() {
		if c.memInit == nil {
			c.mem = hot.NewHotCache[string, V](hot.LRU, 1).Build()
			return
		}
		c.mem = c.memInit()
	})
	return c.mem
}

func (c *HybridCache[V]) Get(key string) (value V, found bool, err error) {
	full := c.ns.FullKey(key)
	if full == "" {
		var zero V
		return zero, false, nil
	}

	if c.redisOn() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRedisOpTimeout)
		defer cancel()

		raw, e := c.redis.Get(ctx, full).Result()
		if e == nil {
			v, decErr := c.redisCodec.Decode(raw)
			if decErr != nil {
				var zero V
				return zero, false, decErr
			}
			return v, true, nil
		}
		if errors.Is(e, redis.Nil) {
			var zero V
			return zero, false, nil
		}
		var zero V
		return zero, false, e
	}

	return c.memCache().Get(full)
}

func (c *HybridCache[V]) SetWithTTL(key string, v V, ttl time.Duration) error {
	full := c.ns.FullKey(key)
	if full == "" {
		return nil
	}

	if c.redisOn() {
		raw, err := c.redisCodec.Encode(v)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultRedisOpTimeout)
		defer cancel()
		return c.redis.Set(ctx, full, raw, ttl).Err()
	}

	mem := c.memCache()
	c.memMu.Lock()
	defer c.memMu.Unlock()
	mem.SetWithTTL(full, v, ttl)
	return nil
}

// Keys returns keys with valid values. In Redis, it returns all matching keys.
func (c *HybridCache[V]) Keys() ([]string, error) {
	if c.redisOn() {
		return c.scanKeys(c.ns.MatchPattern())
	}
	return c.memCache().Keys(), nil
}

func (c *HybridCache[V]) scanKeys(match string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisScanTimeout)
	defer cancel()

	var cursor uint64
	keys := make([]string, 0, 1024)
	for {
		k, next, err := c.redis.Scan(ctx, cursor, match, 1000).Result()
		if err != nil {
			return keys, err
		}
		keys = append(keys, k...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

func (c *HybridCache[V]) Purge() error {
	if c.redisOn() {
		keys, err := c.scanKeys(c.ns.MatchPattern())
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		_, err = c.DeleteMany(keys)
		return err
	}

	mem := c.memCache()
	c.memMu.Lock()
	defer c.memMu.Unlock()
	mem.Purge()
	return nil
}

func (c *HybridCache[V]) DeleteByPrefix(prefix string) (int, error) {
	fullPrefix := c.ns.FullKey(prefix)
	if fullPrefix == "" {
		return 0, nil
	}
	if !strings.HasSuffix(fullPrefix, ":") {
		fullPrefix += ":"
	}

	if c.redisOn() {
		match := fullPrefix + "*"
		keys, err := c.scanKeys(match)
		if err != nil {
			return 0, err
		}
		if len(keys) == 0 {
			return 0, nil
		}

		res, err := c.DeleteMany(keys)
		if err != nil {
			return 0, err
		}
		deleted := 0
		for _, ok := range res {
			if ok {
				deleted++
			}
		}
		return deleted, nil
	}

	// In memory, we filter keys and bulk delete.
	allKeys := c.memCache().Keys()
	keys := make([]string, 0, 128)
	for _, k := range allKeys {
		if strings.HasPrefix(k, fullPrefix) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	res, _ := c.DeleteMany(keys)
	deleted := 0
	for _, ok := range res {
		if ok {
			deleted++
		}
	}
	return deleted, nil
}

// DeleteMany accepts either fully namespaced keys or raw keys and deletes them.
// It returns a map keyed by fully namespaced keys.
func (c *HybridCache[V]) DeleteMany(keys []string) (map[string]bool, error) {
	res := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return res, nil
	}

	fullKeys := make([]string, 0, len(keys))
	for _, k := range keys {
		k = c.ns.FullKey(k)
		if k == "" {
			continue
		}
		fullKeys = append(fullKeys, k)
	}
	if len(fullKeys) == 0 {
		return res, nil
	}

	if c.redisOn() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRedisDelTimeout)
		defer cancel()

		pipe := c.redis.Pipeline()
		cmds := make([]*redis.IntCmd, 0, len(fullKeys))
		for _, k := range fullKeys {
			// UNLINK is non-blocking vs DEL for large key batches.
			cmds = append(cmds, pipe.Unlink(ctx, k))
		}
		_, err := pipe.Exec(ctx)
		if err != nil && !errors.Is(err, redis.Nil) {
			return res, err
		}
		for i, cmd := range cmds {
			deleted := cmd != nil && cmd.Err() == nil && cmd.Val() > 0
			res[fullKeys[i]] = deleted
		}
		return res, nil
	}

	mem := c.memCache()
	c.memMu.Lock()
	defer c.memMu.Unlock()
	return mem.DeleteMany(fullKeys), nil
}

// deleteIfEqualsScript removes a key only when it still holds the expected
// encoded value.
//
// A read followed by a delete cannot express this: between the two round trips
// another request can replace the value, and the delete then discards a value
// the caller never inspected. Callers use this to retract their own entry
// without being able to destroy someone else's.
var deleteIfEqualsScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == false then
  return 0
end
if current ~= ARGV[1] then
  return 0
end
return redis.call('DEL', KEYS[1])
`)

// DeleteIfEquals deletes key only if its current value equals expected, and
// reports whether the delete happened. A key that is absent, or that holds a
// different value, is left alone and returns false.
func (c *HybridCache[V]) DeleteIfEquals(key string, expected V) (bool, error) {
	full := c.ns.FullKey(key)
	if full == "" {
		return false, nil
	}

	if c.redisOn() {
		raw, err := c.redisCodec.Encode(expected)
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultRedisDelTimeout)
		defer cancel()
		deleted, err := deleteIfEqualsScript.Run(ctx, c.redis, []string{full}, raw).Int()
		if err != nil {
			return false, err
		}
		return deleted > 0, nil
	}

	// memMu makes the read, the comparison and the delete one step with respect to
	// every other mutation on this cache. hot.HotCache locks each call separately,
	// so without this the value could be replaced after the comparison passed and
	// the delete would discard the newer value the comparison existed to protect.
	mem := c.memCache()
	c.memMu.Lock()
	defer c.memMu.Unlock()

	current, found, err := mem.Get(full)
	if err != nil || !found {
		return false, err
	}
	currentRaw, err := c.encodeForCompare(current)
	if err != nil {
		return false, err
	}
	expectedRaw, err := c.encodeForCompare(expected)
	if err != nil {
		return false, err
	}
	if currentRaw != expectedRaw {
		return false, nil
	}
	return mem.DeleteMany([]string{full})[full], nil
}

// encodeForCompare renders a value as a comparable string. The codec is the
// authority when one is configured; without it (memory-only caches may leave it
// nil) fmt is enough for the scalar value types these caches hold.
func (c *HybridCache[V]) encodeForCompare(v V) (string, error) {
	if c.redisCodec != nil {
		return c.redisCodec.Encode(v)
	}
	return fmt.Sprintf("%v", v), nil
}

func (c *HybridCache[V]) Capacity() (mainCacheCapacity int, missingCacheCapacity int) {
	if c.redisOn() {
		return 0, 0
	}
	return c.memCache().Capacity()
}

func (c *HybridCache[V]) Algorithm() (mainCacheAlgorithm string, missingCacheAlgorithm string) {
	if c.redisOn() {
		return "redis", ""
	}
	return c.memCache().Algorithm()
}
