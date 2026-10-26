package service

import (
	"fmt"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

const (
	cacheShareAlertPageSize = 1000
	// cacheShareAlertMaxRows bounds one run's scan. The newest rows are kept, so
	// a window busier than this is judged on its most recent part.
	cacheShareAlertMaxRows = 100000
)

// CacheShareUsage is one channel's token-weighted prompt cache usage over the
// alert window. Input excludes the cache, as Anthropic reports input_tokens.
type CacheShareUsage struct {
	Requests         int `json:"requests"`
	InputTokens      int `json:"input_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// PromptTokens is the prompt side the share is weighted by: input + CR + CW.
func (u CacheShareUsage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Share is CR/(input+CR+CW): the fraction of prompt tokens served from cache.
// It is weighted by tokens, not by requests, so one huge cached request and
// many tiny uncached ones are not reported as a 50% hit rate.
func (u CacheShareUsage) Share() float64 {
	promptTokens := u.PromptTokens()
	if promptTokens <= 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(promptTokens)
}

type CacheShareAlert struct {
	ChannelId int     `json:"channel_id"`
	Share     float64 `json:"share"`
	Threshold float64 `json:"threshold"`
	CacheShareUsage
}

// CacheShareAlertResult is the system task result. LastAlerted (channel id to
// unix seconds) is the per-channel cooldown: the next run reads it from this
// run's row, which keeps the cooldown across instances and restarts.
type CacheShareAlertResult struct {
	WindowStart int64             `json:"window_start"`
	ScannedRows int               `json:"scanned_rows"`
	Truncated   bool              `json:"truncated"`
	Channels    int               `json:"channels"`
	Alerts      []CacheShareAlert `json:"alerts"`
	LastAlerted map[int]int64     `json:"last_alerted"`
}

// EvaluateCacheShareAlerts returns the channels to alert on, by channel id. A
// channel alerts only when all hold: its threshold is not 0 (excluded or
// disabled), the window has enough requests and prompt tokens to judge, its
// share is below the threshold, and it was not alerted within the cooldown.
func EvaluateCacheShareAlerts(setting *operation_setting.MonitorSetting, usage map[int]CacheShareUsage, lastAlerted map[int]int64, now int64) []CacheShareAlert {
	cooldown := int64(setting.GetCacheShareAlertCooldownMinutes()) * 60
	alerts := make([]CacheShareAlert, 0)
	for channelId, channelUsage := range usage {
		threshold := setting.CacheShareAlertThresholdFor(channelId)
		if threshold <= 0 {
			continue
		}
		if channelUsage.Requests < setting.GetCacheShareAlertMinRequests() || channelUsage.PromptTokens() < setting.GetCacheShareAlertMinPromptTokens() {
			continue
		}
		share := channelUsage.Share()
		if share >= threshold {
			continue
		}
		if alertedAt, ok := lastAlerted[channelId]; ok && now-alertedAt < cooldown {
			continue
		}
		alerts = append(alerts, CacheShareAlert{ChannelId: channelId, Share: share, Threshold: threshold, CacheShareUsage: channelUsage})
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].ChannelId < alerts[j].ChannelId })
	return alerts
}

// RunCacheShareAlert aggregates the Claude-semantic consume logs of the window
// per channel, evaluates them, and sends one notification for all channels
// that alert. Only Anthropic usage is counted: those requests opt into the
// cache explicitly, while OpenAI-semantic traffic routinely has no cache and
// would alert forever. notify is NotifyRootUser outside tests.
func RunCacheShareAlert(now int64, lastAlerted map[int]int64, notify func(t string, subject string, content string)) (*CacheShareAlertResult, error) {
	setting := operation_setting.GetMonitorSetting()
	result := &CacheShareAlertResult{
		WindowStart: now - int64(setting.GetCacheShareAlertWindowMinutes())*60,
		LastAlerted: map[int]int64{},
	}
	cooldown := int64(setting.GetCacheShareAlertCooldownMinutes()) * 60
	for channelId, alertedAt := range lastAlerted {
		if now-alertedAt < cooldown {
			result.LastAlerted[channelId] = alertedAt
		}
	}

	usage := map[int]CacheShareUsage{}
	truncated, err := model.ScanRecentConsumeLogUsage(result.WindowStart, cacheShareAlertPageSize, cacheShareAlertMaxRows, func(row model.ConsumeLogUsageRow) {
		result.ScannedRows++
		if row.ChannelId <= 0 || row.Other == "" {
			return
		}
		var other struct {
			UsageSemantic       string `json:"usage_semantic"`
			CacheTokens         int    `json:"cache_tokens"`
			CacheCreationTokens int    `json:"cache_creation_tokens"`
			CacheWriteTokens    int    `json:"cache_write_tokens"`
		}
		if common.UnmarshalJsonStr(row.Other, &other) != nil || other.UsageSemantic != "anthropic" {
			return
		}
		// cache_write_tokens is the normalized 5m+1h total; older rows and
		// unsplit upstreams only carry cache_creation_tokens.
		cacheWrite := other.CacheWriteTokens
		if cacheWrite <= 0 {
			cacheWrite = other.CacheCreationTokens
		}
		channelUsage := usage[row.ChannelId]
		channelUsage.Requests++
		channelUsage.InputTokens += max(row.PromptTokens, 0)
		channelUsage.CacheReadTokens += max(other.CacheTokens, 0)
		channelUsage.CacheWriteTokens += max(cacheWrite, 0)
		usage[row.ChannelId] = channelUsage
	})
	if err != nil {
		return result, err
	}
	result.Truncated = truncated
	result.Channels = len(usage)
	result.Alerts = EvaluateCacheShareAlerts(setting, usage, result.LastAlerted, now)
	if len(result.Alerts) == 0 {
		return result, nil
	}

	lines := make([]string, 0, len(result.Alerts))
	for _, alert := range result.Alerts {
		result.LastAlerted[alert.ChannelId] = now
		name := ""
		if channel, err := model.CacheGetChannel(alert.ChannelId); err == nil && channel != nil {
			name = channel.Name
		}
		lines = append(lines, fmt.Sprintf("渠道 #%d %s：缓存占比 %.1f%%（阈值 %.1f%%），%d 次请求，input %d / 缓存读 %d / 缓存写 %d tokens",
			alert.ChannelId, name, alert.Share*100, alert.Threshold*100, alert.Requests, alert.InputTokens, alert.CacheReadTokens, alert.CacheWriteTokens))
	}
	content := fmt.Sprintf("最近 %d 分钟内以下渠道的 Claude 缓存占比（缓存读 / (input+缓存读+缓存写)，按 token 加权）低于阈值：\n%s\n同一渠道 %d 分钟内不再重复提醒；可在 monitor_setting 的 cache_share_alert_excluded_channel_ids / cache_share_alert_channel_thresholds 中排除或调整。",
		setting.GetCacheShareAlertWindowMinutes(), strings.Join(lines, "\n"), setting.GetCacheShareAlertCooldownMinutes())
	if result.Truncated {
		content += fmt.Sprintf("\n本轮只统计了窗口内最新的 %d 条消费日志。", cacheShareAlertMaxRows)
	}
	notify(dto.NotifyTypeCacheShareAlert, fmt.Sprintf("%d 个渠道缓存占比偏低", len(result.Alerts)), content)
	return result, nil
}
