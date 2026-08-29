package service

import (
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// ginKeyHeldChannelSlot names the channel whose concurrency slot this request
// currently owns.
const ginKeyHeldChannelSlot = "held_channel_slot"

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
	}
	c.Set(ginKeyHeldChannelSlot, channelId)
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
}
