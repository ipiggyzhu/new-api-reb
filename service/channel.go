package service

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return
	}

	success := model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelError.ChannelName, channelError.ChannelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

// IsChannelFaultError 判定一个上游错误是否属于渠道自身故障，而不是限流或临时抖动。
// 判定口径完全来自管理员已配置的 AutomaticDisableStatusCodes / AutomaticDisableKeywords
// 与错误类型，因此 429、限流文案、可重试的临时错误都不会被当作故障。
//
// 和 ShouldDisableChannel 拆开是因为两个消费方各有独立的总开关：禁用渠道由
// AutomaticDisableChannelEnabled 控制，而上游模型巡检删除失效模型由
// monitor_setting.upstream_model_update_remove_failed 控制，两者不应互相牵制。
// channelConfigErrorCodes are "channel:" errors that mean our own channel
// configuration is wrong, not that the upstream channel is broken: an invalid
// param or header override template, or an AWS client that could not be built
// from the stored credential shape. Disabling the channel fixes none of them.
//
// Excluding them matters most for override templates, because a channel affinity
// rule fans one template out across a whole group: a single illegal entry makes
// every channel the rule reaches raise the same error, so treating it as a fault
// would walk the retry loop down the group disabling channels one by one.
var channelConfigErrorCodes = map[types.ErrorCode]struct{}{
	types.ErrorCodeChannelParamOverrideInvalid:  {},
	types.ErrorCodeChannelHeaderOverrideInvalid: {},
	types.ErrorCodeChannelAwsClientError:        {},
}

func IsChannelFaultError(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	// Checked ahead of IsChannelError, which would otherwise short-circuit these
	// to true on the "channel:" prefix alone and bypass the operator-configured
	// AutomaticDisableStatusCodes allowlist entirely.
	if _, isConfigError := channelConfigErrorCodes[err.GetErrorCode()]; isConfigError {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

// IsChannelRoutingFaultError 判定一个错误是否应该影响路由排序（动态优先级/权重与渠道亲和）。
// 它比 IsChannelFaultError 宽一档：后者的结论会被 ShouldDisableChannel 继续用于
// 禁用渠道，而排序只是把一个渠道往后放，代价小得多，因此可以把"上游承诺 200 却什么
// 都没吐"这类只影响可用性、不足以据此禁用的故障也算进来。
//
// 现在多出来的一项是 ErrorCodeEmptyResponse。它不带 "channel:" 前缀、也不在
// AutomaticDisableStatusCodes 白名单里，所以 IsChannelFaultError 与
// ShouldDisableChannel 都不会因此禁用渠道；空回渠道会因为评分下沉而被绕开，等它自己
// 恢复后再浮回来，符合"动态调整优先级和权重、不禁用"的要求。
func IsChannelRoutingFaultError(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if err.GetErrorCode() == types.ErrorCodeEmptyResponse {
		return true
	}
	return IsChannelFaultError(err)
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	return IsChannelFaultError(err)
}

// ShouldDisableChannelForResponseTime decides whether the latency of a completed
// channel test is, by itself, grounds for disabling the channel. It is the
// latency arm of the same channel-status decision as ShouldDisableChannel and
// ShouldEnableChannel, and takes the measurement as a parameter so the rule is
// verifiable without a clock.
//
// isChannelEnabled gates the whole judgement. A channel that is already
// auto-disabled is being retested to answer one question — has it come back? —
// and a cold start, a long context, or a reasoning model working through one of
// the built-in prompts routinely outruns the threshold on first byte. Counting
// that as a fresh fault would pin a recovered channel in auto-disabled with no
// exit but a manual click.
//
// thresholdSeconds is common.ChannelDisableThreshold as the operator configured
// it. Zero or negative switches the latency check off. A positive value below one
// millisecond truncates to a 0ms threshold, which would compare against "any
// latency above zero" — a threshold the operator never wrote and that the
// millisecond-resolution measurement cannot express. It clamps to 1ms so the
// comparison keeps a meaning the configured value can actually carry.
func ShouldDisableChannelForResponseTime(milliseconds int64, thresholdSeconds float64, isChannelEnabled bool) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if !isChannelEnabled {
		return false
	}
	if thresholdSeconds <= 0 {
		return false
	}
	thresholdMilliseconds := int64(thresholdSeconds * 1000)
	if thresholdMilliseconds <= 0 {
		thresholdMilliseconds = 1
	}
	return milliseconds > thresholdMilliseconds
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
