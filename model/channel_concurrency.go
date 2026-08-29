package model

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
)

// channelInFlight counts the relay requests currently being served by each
// channel, keyed by channel id. It is a live gauge, not a rate window: a slot is
// taken when a request is admitted to a channel and given back when that
// attempt ends, so a stream holds its slot for as long as it streams. That is
// what an upstream concurrency limit means — the vendor cares how many requests
// are open at once, not how many started in the last minute, which is why
// common.InMemoryRateLimiter and common/limiter (both sliding windows) cannot
// express it.
//
// The gauge is per process. Production runs a single instance without Redis, so
// an in-process counter is the exact answer there; with several instances each
// would enforce the limit against its own share of the traffic.
var channelInFlight sync.Map // map[int]*atomic.Int64

func channelInFlightCounter(channelId int) *atomic.Int64 {
	if counter, ok := channelInFlight.Load(channelId); ok {
		return counter.(*atomic.Int64)
	}
	counter, _ := channelInFlight.LoadOrStore(channelId, new(atomic.Int64))
	return counter.(*atomic.Int64)
}

// AcquireChannelSlot takes one concurrency slot on the channel, reporting whether
// the channel had room. limit <= 0 means unlimited.
//
// The count is kept even for an unlimited channel so that ChannelInFlight can
// report it, and so every admitted request has exactly one matching
// ReleaseChannelSlot regardless of what the limit was at admission time. An
// operator lowering a limit therefore does not strand slots held by requests
// that were admitted under the old value.
//
// The compare-and-swap makes the check and the increment one step: two requests
// racing for the last slot of a limit-1 channel cannot both see room.
func AcquireChannelSlot(channelId int, limit int) bool {
	counter := channelInFlightCounter(channelId)
	for {
		current := counter.Load()
		if limit > 0 && current >= int64(limit) {
			return false
		}
		if counter.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// ReleaseChannelSlot gives back a slot taken by AcquireChannelSlot.
func ReleaseChannelSlot(channelId int) {
	counter, ok := channelInFlight.Load(channelId)
	if !ok {
		return
	}
	if remaining := counter.(*atomic.Int64).Add(-1); remaining < 0 {
		// Only reachable if a slot was released twice. Clamping keeps the channel
		// usable (a negative gauge would let it exceed its limit forever), and the
		// log is what makes the accounting bug visible instead of self-healing.
		counter.(*atomic.Int64).Store(0)
		common.SysError(fmt.Sprintf("channel concurrency slot released more times than acquired: channel=%d", channelId))
	}
}

// ChannelInFlight reports how many requests the channel is currently serving.
func ChannelInFlight(channelId int) int {
	counter, ok := channelInFlight.Load(channelId)
	if !ok {
		return 0
	}
	return int(counter.(*atomic.Int64).Load())
}

// ChannelInFlightSnapshot returns the in-flight count of every channel that has
// served a request since startup, for the admin channel list.
func ChannelInFlightSnapshot() map[int]int {
	snapshot := make(map[int]int)
	channelInFlight.Range(func(key, value any) bool {
		channelId, ok := key.(int)
		if !ok {
			return true
		}
		if inFlight := int(value.(*atomic.Int64).Load()); inFlight > 0 {
			snapshot[channelId] = inFlight
		}
		return true
	})
	return snapshot
}
