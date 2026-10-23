package service

import (
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// ginKeyHeldChannelSlot names the channel whose concurrency slot this request
// currently owns.
const ginKeyHeldChannelSlot = "held_channel_slot"

// channelLastActive holds, per channel id, the unix second at which a relay
// request last took or gave back a slot on it. Together with the in-flight
// gauge it is the channel's real-traffic activity record, which the upstream
// keepalive uses to tell a channel in use from an idle one.
//
// It is stamped only here: binding a slot is the single step every relay
// attempt takes (the distributor and the relay retry loop), and the channel
// test and the keepalive never take one, so neither counts as activity. The
// record lives in process memory, like the in-flight gauge, so the request
// path pays no database or Redis write. A restart forgets it, and a channel
// then waits for its next real request before keepalives resume.
var channelLastActive sync.Map // map[int]*atomic.Int64

func markChannelActive(channelId int) {
	if channelId <= 0 {
		return
	}
	now := common.GetTimestamp()
	value, ok := channelLastActive.Load(channelId)
	if !ok {
		value, _ = channelLastActive.LoadOrStore(channelId, new(atomic.Int64))
	}
	stamp := value.(*atomic.Int64)
	for {
		current := stamp.Load()
		if current >= now || stamp.CompareAndSwap(current, now) {
			return
		}
	}
}

// ChannelLastActiveAt reports when real relay traffic last used channelId: now
// while one of its requests is still in flight (a long stream is activity for
// as long as it runs), else the last time a request took or gave back a slot,
// or 0 if none has since the process started.
func ChannelLastActiveAt(channelId int, now int64) int64 {
	if model.ChannelInFlight(channelId) > 0 {
		return now
	}
	value, ok := channelLastActive.Load(channelId)
	if !ok {
		return 0
	}
	return value.(*atomic.Int64).Load()
}

// SetChannelLastActiveForTest overwrites channelId's activity record, and at
// <= 0 forgets it, so tests can place real traffic in the past.
func SetChannelLastActiveForTest(channelId int, at int64) {
	if at <= 0 {
		channelLastActive.Delete(channelId)
		return
	}
	stamp := new(atomic.Int64)
	stamp.Store(at)
	channelLastActive.Store(channelId, stamp)
}

// HoldChannelSlot records that the request now owns a concurrency slot on
// channelId, giving back the slot it held before.
//
// A request holds at most one slot at a time: the retry loop moves to another
// channel only after the previous attempt has finished, so keeping the old slot
// would make a request that retried across three channels count against all
// three for the rest of its life. Selection is what takes the slot (atomically,
// so a race for the last one has a single winner); this only tracks it so it can
// be given back.
func HoldChannelSlot(c *gin.Context, channelId int) {
	if c == nil {
		return
	}
	if previous := c.GetInt(ginKeyHeldChannelSlot); previous > 0 && previous != channelId {
		model.ReleaseChannelSlot(previous)
		markChannelActive(previous)
	}
	c.Set(ginKeyHeldChannelSlot, channelId)
	markChannelActive(channelId)
}

// ReleaseHeldChannelSlot gives back whatever slot the request holds. It is
// idempotent, so the deferred call at the end of the request is safe even when
// the slot was already released.
func ReleaseHeldChannelSlot(c *gin.Context) {
	if c == nil {
		return
	}
	channelId := c.GetInt(ginKeyHeldChannelSlot)
	if channelId <= 0 {
		return
	}
	c.Set(ginKeyHeldChannelSlot, 0)
	model.ReleaseChannelSlot(channelId)
	markChannelActive(channelId)
}
