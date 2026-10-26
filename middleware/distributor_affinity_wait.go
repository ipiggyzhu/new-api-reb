package middleware

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// affinitySlotPollInterval is how often a parked request re-checks its pinned
// channel's slot gauge. The gauge is an in-process CAS, so polling a handful of
// waiters costs nothing measurable.
const affinitySlotPollInterval = 100 * time.Millisecond

// waitForAffinitySlot parks a request whose pinned channel is at its concurrency
// limit until a slot there frees up, reporting whether it took one. The caller
// owns a true result's slot exactly as if AcquireChannelSlot had returned true.
//
// It gives up — and the request falls back to ordinary selection — when waiting
// is disabled, when the channel already has SaturatedMaxWaiters parked on it,
// when the channel starts cooling down, or when SaturatedWaitSeconds runs out.
// A client that disconnects aborts the request instead: there is no one left
// to fall back for.
//
// A streaming request whose response has not started gets the relay's own
// keepalive pings while it waits (PingIntervalEnabled / PingIntervalSeconds),
// so a proxy in front does not drop the idle connection. They are counted in
// ContextKeyAffinityWaitPings so retry logic can tell them from relayed data.
func waitForAffinitySlot(c *gin.Context, channel *model.Channel, modelName string) bool {
	affinitySetting := operation_setting.GetChannelAffinitySetting()
	wait := affinitySetting.SaturatedWait()
	if wait <= 0 || !model.EnterChannelSlotWait(channel.Id, affinitySetting.SaturatedMaxWaiters) {
		return false
	}
	defer model.LeaveChannelSlotWait(channel.Id)

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(affinitySlotPollInterval)
	defer poll.Stop()

	var pings <-chan time.Time
	generalSettings := operation_setting.GetGeneralSetting()
	if generalSettings.PingIntervalEnabled && !c.Writer.Written() {
		if storage, err := common.GetBodyStorage(c); err == nil {
			if body, err := storage.Bytes(); err == nil && gjson.GetBytes(body, "stream").Bool() {
				pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
				if pingInterval <= 0 {
					pingInterval = helper.DefaultPingInterval
				}
				pingTicker := time.NewTicker(pingInterval)
				defer pingTicker.Stop()
				pings = pingTicker.C
			}
		}
	}

	for {
		select {
		case <-c.Request.Context().Done():
			c.Abort()
			return false
		case <-deadline.C:
			return false
		case <-pings:
			helper.SetEventStreamHeaders(c)
			if err := helper.PingData(c); err != nil {
				// The connection is going away; the context check above ends the wait.
				pings = nil
				continue
			}
			common.SetContextKey(c, constant.ContextKeyAffinityWaitPings,
				common.GetContextKeyInt(c, constant.ContextKeyAffinityWaitPings)+1)
		case <-poll.C:
			if model.ChannelCoolingDown(channel.Id, modelName) {
				return false
			}
			if model.AcquireChannelSlot(channel.Id, channel.GetMaxConcurrency()) {
				return true
			}
		}
	}
}
