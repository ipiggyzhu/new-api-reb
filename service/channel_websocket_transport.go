package service

import (
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/bytedance/gopkg/util/gopool"
)

// websocketDowngradeInFlight collapses concurrent downgrades of the same channel.
//
// A channel that stops speaking WebSocket refuses every in-flight request at
// once, so without this the first burst would each load the row, marshal the
// settings and save — a write storm against one row, plus a matching burst of
// admin notifications. The map holds only channels currently being written; the
// persisted flag is what suppresses later requests.
var websocketDowngradeInFlight sync.Map // channelId int -> struct{}

// MarkChannelWebsocketUnsupported persists that a channel's upstream refused the
// WebSocket upgrade, so later requests skip the handshake and go straight to
// HTTP+SSE. Returns immediately; the write happens on a worker.
//
// Callers must only invoke this for handshake-stage refusals that are
// definitive about the endpoint not speaking WebSocket. Anything ambiguous —
// timeouts, 5xx, 429, 401/403 — must downgrade for the current request only,
// without persisting: this flag survives until an admin saves the channel, so a
// wrong positive is a silent permanent regression rather than a retry.
func MarkChannelWebsocketUnsupported(channelId int, channelName string, reason string) {
	if channelId <= 0 {
		return
	}
	if _, loaded := websocketDowngradeInFlight.LoadOrStore(channelId, struct{}{}); loaded {
		return
	}

	gopool.Go(func() {
		defer websocketDowngradeInFlight.Delete(channelId)
		defer func() {
			if r := recover(); r != nil {
				common.SysLog(fmt.Sprintf("panic while marking channel websocket unsupported: channel_id=%d, error=%v", channelId, r))
			}
		}()

		// Goes through MutateChannelSettings rather than load-edit-Save: the
		// upstream-model update task rewrites the same column from its own snapshot,
		// so an unsynchronized Save here was lost whenever the two overlapped.
		// The mutation returning false means the flag was already set, which
		// suppresses both the redundant write and the duplicate notification.
		alreadySet := false
		err := model.MutateChannelSettings(channelId, func(settings *dto.ChannelOtherSettings) bool {
			if settings.WebsocketUnsupported {
				alreadySet = true
				return false
			}
			settings.WebsocketUnsupported = true
			return true
		})
		if err != nil {
			common.SysLog(fmt.Sprintf("failed to persist websocket unsupported flag: channel_id=%d, error=%v", channelId, err))
			return
		}
		if alreadySet {
			return
		}

		common.SysLog(fmt.Sprintf("通道「%s」（#%d）的 WebSocket 传输已降级为 SSE，原因：%s", channelName, channelId, common.LocalLogPreview(reason)))

		// The admin turned this on deliberately, so a silent downgrade would leave
		// them believing the channel still runs on WebSocket. The switch stays on
		// in the UI; saving the channel clears the flag and retries.
		subject := fmt.Sprintf("通道「%s」（#%d）WebSocket 传输已降级为 SSE", channelName, channelId)
		content := fmt.Sprintf(
			"通道「%s」（#%d）的上游拒绝了 WebSocket 升级，已自动降级为 SSE，后续请求不再尝试。\n\n原因：%s\n\n"+
				"如果上游地址已更换或已支持 WebSocket，重新保存该渠道即可恢复尝试。",
			channelName, channelId, common.LocalLogPreview(reason))
		NotifyRootUser(fmt.Sprintf("channel_websocket_downgrade_%d", channelId), subject, content)
	})
}
