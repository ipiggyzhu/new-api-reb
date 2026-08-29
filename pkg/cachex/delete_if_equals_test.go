package cachex

import (
	"sync"
	"testing"
	"time"

	"github.com/samber/hot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIntCacheForTest builds a memory-only cache. Redis is deliberately absent:
// these tests pin the compare semantics, which must hold identically on both
// backends, and the memory path is the one that can be exercised without a
// server.
func newIntCacheForTest(namespace string) *HybridCache[int] {
	return NewHybridCache[int](HybridCacheConfig[int]{
		Namespace:    Namespace(namespace),
		RedisCodec:   IntCodec{},
		RedisEnabled: func() bool { return false },
		Memory: func() *hot.HotCache[string, int] {
			return hot.NewHotCache[string, int](hot.LRU, 100).Build()
		},
	})
}

// TestDeleteIfEqualsOnlyDeletesTheExpectedValue is the property the affinity
// fault-release path depends on. A failing request retracts its own pin, but a
// concurrent successful request may already have replaced it; an unconditional
// delete would discard the healthy channel's pin and send the next request to
// cold selection.
func TestDeleteIfEqualsOnlyDeletesTheExpectedValue(t *testing.T) {
	cases := []struct {
		name        string
		seed        *int
		expected    int
		wantDeleted bool
		wantRemains *int
	}{
		{
			name:        "value matches so the entry is removed",
			seed:        ptr(7),
			expected:    7,
			wantDeleted: true,
			wantRemains: nil,
		},
		{
			name:        "value was replaced so the entry survives",
			seed:        ptr(9),
			expected:    7,
			wantDeleted: false,
			wantRemains: ptr(9),
		},
		{
			name:        "absent key deletes nothing",
			seed:        nil,
			expected:    7,
			wantDeleted: false,
			wantRemains: nil,
		},
		{
			name:        "zero value is compared like any other",
			seed:        ptr(0),
			expected:    0,
			wantDeleted: true,
			wantRemains: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := newIntCacheForTest("test:delete-if-equals:" + tc.name)
			const key = "pin"
			if tc.seed != nil {
				require.NoError(t, cache.SetWithTTL(key, *tc.seed, time.Minute))
			}

			deleted, err := cache.DeleteIfEquals(key, tc.expected)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeleted, deleted)

			value, found, err := cache.Get(key)
			require.NoError(t, err)
			if tc.wantRemains == nil {
				assert.False(t, found, "entry should be gone")
			} else {
				require.True(t, found, "entry should have survived")
				assert.Equal(t, *tc.wantRemains, value)
			}
		})
	}
}

func TestDeleteIfEqualsIgnoresEmptyKey(t *testing.T) {
	cache := newIntCacheForTest("test:delete-if-equals:empty")
	deleted, err := cache.DeleteIfEquals("", 1)
	require.NoError(t, err)
	assert.False(t, deleted)
}

// TestDeleteIfEqualsNeverDiscardsAConcurrentWrite pins the atomicity of the
// compare and the delete, not just the comparison itself.
//
// The underlying hot cache locks each call separately, so a read, a comparison
// and a delete issued as three calls can be interleaved: the write lands after
// the comparison passed, and the delete then removes a value that was never
// compared. That is precisely the pin a concurrent successful request had just
// installed. The invariant is one-directional and holds regardless of which
// goroutine wins: whatever survives must be the newly written value, and it must
// never be the case that the new value is both written and gone.
func TestDeleteIfEqualsNeverDiscardsAConcurrentWrite(t *testing.T) {
	const (
		key      = "pin"
		oldValue = 11
		newValue = 22
		rounds   = 300
	)

	for i := 0; i < rounds; i++ {
		cache := newIntCacheForTest("test:delete-if-equals:race")
		require.NoError(t, cache.SetWithTTL(key, oldValue, time.Minute))

		var wg sync.WaitGroup
		wg.Add(2)

		var deleted bool
		var deleteErr error
		go func() {
			defer wg.Done()
			deleted, deleteErr = cache.DeleteIfEquals(key, oldValue)
		}()
		go func() {
			defer wg.Done()
			_ = cache.SetWithTTL(key, newValue, time.Minute)
		}()
		wg.Wait()

		require.NoError(t, deleteErr)
		value, found, err := cache.Get(key)
		require.NoError(t, err)

		if found {
			// The writer won the ordering: its value is the only one that may be here.
			assert.Equal(t, newValue, value,
				"round %d: a surviving entry must be the newly written value", i)
			continue
		}
		// The key is gone, which is only legitimate when the delete observed the old
		// value and the write had not landed yet. If the delete reported success
		// while the newer value was already stored, that write was silently lost.
		require.True(t, deleted,
			"round %d: the entry vanished without DeleteIfEquals claiming the delete", i)
	}
}

func ptr(v int) *int { return &v }
