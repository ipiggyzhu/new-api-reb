package common

import (
	"sync"
	"time"
)

type InMemoryRateLimiter struct {
	store              map[string]*[]int64
	mutex              sync.Mutex
	once               sync.Once
	expirationDuration time.Duration
}

// Init is called per request by the rate limit middlewares, so it must stay a
// no-op after the first call: only the first expirationDuration is kept, and
// sync.Once replaces the unsynchronized double-checked read of store.
func (l *InMemoryRateLimiter) Init(expirationDuration time.Duration) {
	l.once.Do(func() {
		l.mutex.Lock()
		defer l.mutex.Unlock()
		l.store = make(map[string]*[]int64)
		l.expirationDuration = expirationDuration
		if expirationDuration > 0 {
			go l.clearExpiredItems()
		}
	})
}

func (l *InMemoryRateLimiter) clearExpiredItems() {
	for {
		time.Sleep(l.expirationDuration)
		l.mutex.Lock()
		now := time.Now().Unix()
		for key := range l.store {
			queue := l.store[key]
			size := len(*queue)
			if size == 0 || now-(*queue)[size-1] > int64(l.expirationDuration.Seconds()) {
				delete(l.store, key)
			}
		}
		l.mutex.Unlock()
	}
}

// Request parameter duration's unit is seconds
func (l *InMemoryRateLimiter) Request(key string, maxRequestNum int, duration int64) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	// [old <-- new]
	queue, ok := l.store[key]
	now := time.Now().Unix()
	if ok {
		if len(*queue) < maxRequestNum {
			*queue = append(*queue, now)
			return true
		} else {
			if now-(*queue)[0] >= duration {
				*queue = (*queue)[1:]
				*queue = append(*queue, now)
				return true
			} else {
				return false
			}
		}
	} else {
		s := make([]int64, 0, maxRequestNum)
		l.store[key] = &s
		*(l.store[key]) = append(*(l.store[key]), now)
	}
	return true
}
