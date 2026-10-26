package cachex

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/samber/hot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conditionalWriteBackends runs a test against the memory path and, through
// miniredis, the Redis scripts, which must agree. Only miniredis can advance
// time, so TTL renewal is observed on Redis alone.
func conditionalWriteBackends(t *testing.T) map[string]func() (*HybridCache[string], func(time.Duration)) {
	return map[string]func() (*HybridCache[string], func(time.Duration)){
		"memory": func() (*HybridCache[string], func(time.Duration)) {
			cache := NewHybridCache[string](HybridCacheConfig[string]{
				Namespace: Namespace("cond-test"), RedisCodec: StringCodec{}, RedisEnabled: func() bool { return false },
				Memory: func() *hot.HotCache[string, string] {
					return hot.NewHotCache[string, string](hot.LRU, 100).Build()
				},
			})
			return cache, nil
		},
		"redis": func() (*HybridCache[string], func(time.Duration)) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			cache := NewHybridCache[string](HybridCacheConfig[string]{
				Namespace: Namespace("cond-test"), Redis: client, RedisCodec: StringCodec{}, RedisEnabled: func() bool { return true },
			})
			return cache, server.FastForward
		},
	}
}

func cachedValue(t *testing.T, cache *HybridCache[string], key string) string {
	t.Helper()
	value, found, err := cache.Get(key)
	require.NoError(t, err)
	if !found {
		return "<absent>"
	}
	return value
}

func TestConditionalWrites(t *testing.T) {
	for name, build := range conditionalWriteBackends(t) {
		t.Run(name, func(t *testing.T) {
			cache, advance := build()

			set, err := cache.SetIfAbsent("k", "1:a", time.Hour)
			require.NoError(t, err)
			assert.True(t, set, "the first writer places the value")
			set, err = cache.SetIfAbsent("k", "1:b", time.Hour)
			require.NoError(t, err)
			assert.False(t, set, "a second writer does not replace it")
			assert.Equal(t, "1:a", cachedValue(t, cache, "k"))

			swapped, err := cache.CompareAndSwap("k", "1:b", "2:c", time.Hour)
			require.NoError(t, err)
			assert.False(t, swapped, "a swap from a stale value is refused")
			swapped, err = cache.CompareAndSwap("k", "1:a", "2:c", time.Hour)
			require.NoError(t, err)
			assert.True(t, swapped)
			assert.Equal(t, "2:c", cachedValue(t, cache, "k"))
			swapped, err = cache.CompareAndSwap("missing", "", "2:c", time.Hour)
			require.NoError(t, err)
			assert.False(t, swapped, "a swap does not create a key")

			renewed, err := cache.ExpireIfEquals("k", "1:a", time.Hour)
			require.NoError(t, err)
			assert.False(t, renewed, "a stale value is not renewed")
			require.NoError(t, cache.SetWithTTL("short", "3:d", 200*time.Millisecond))
			renewed, err = cache.ExpireIfEquals("short", "3:d", time.Hour)
			require.NoError(t, err)
			assert.True(t, renewed)
			if advance != nil {
				advance(400 * time.Millisecond)
				assert.Equal(t, "3:d", cachedValue(t, cache, "short"), "the renewed TTL replaced the short one")
			}
		})
	}
}

func TestSetIfAbsentHasOneWinner(t *testing.T) {
	for name, build := range conditionalWriteBackends(t) {
		t.Run(name, func(t *testing.T) {
			cache, _ := build()
			var wins atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(value string) {
					defer wg.Done()
					if set, err := cache.SetIfAbsent("race", value, time.Hour); err == nil && set {
						wins.Add(1)
					}
				}(string(rune('a' + i)))
			}
			wg.Wait()
			assert.Equal(t, int32(1), wins.Load())
		})
	}
}
