package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cacheShareAlertTestSetting() operation_setting.MonitorSetting {
	return operation_setting.MonitorSetting{
		CacheShareAlertEnabled:         true,
		CacheShareAlertWindowMinutes:   60,
		CacheShareAlertThreshold:       0.5,
		CacheShareAlertMinRequests:     3,
		CacheShareAlertMinPromptTokens: 1000,
		CacheShareAlertCooldownMinutes: 60,
	}
}

func TestCacheShareUsageShareIsTokenWeighted(t *testing.T) {
	tests := []struct {
		name  string
		usage CacheShareUsage
		want  float64
	}{
		{name: "no prompt tokens", usage: CacheShareUsage{}, want: 0},
		{name: "no cache at all", usage: CacheShareUsage{InputTokens: 1000}, want: 0},
		{name: "all cache read", usage: CacheShareUsage{CacheReadTokens: 1000}, want: 1},
		// Writing the cache is not a hit: the prompt was processed in full.
		{name: "all cache write", usage: CacheShareUsage{CacheWriteTokens: 1000}, want: 0},
		{name: "cache write is in the denominator", usage: CacheShareUsage{InputTokens: 100, CacheReadTokens: 600, CacheWriteTokens: 300}, want: 0.6},
		// One large cached request outweighs nine small uncached ones: a request
		// hit rate would say 10%, the token share says 99%.
		{name: "weighted by tokens not requests", usage: CacheShareUsage{Requests: 10, InputTokens: 900, CacheReadTokens: 99100}, want: 0.991},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, tt.usage.Share(), 1e-9)
		})
	}
}

func TestEvaluateCacheShareAlerts(t *testing.T) {
	const now = int64(1_800_000_000)
	low := CacheShareUsage{Requests: 3, InputTokens: 800, CacheReadTokens: 200}
	tests := []struct {
		name        string
		configure   func(s *operation_setting.MonitorSetting)
		usage       CacheShareUsage
		lastAlerted map[int]int64
		wantAlert   bool
	}{
		{name: "low share with enough sample alerts", usage: low, wantAlert: true},
		{name: "share at the threshold does not alert", usage: CacheShareUsage{Requests: 3, InputTokens: 500, CacheReadTokens: 500}},
		{name: "too few requests", usage: CacheShareUsage{Requests: 2, InputTokens: 800, CacheReadTokens: 200}},
		{name: "too few prompt tokens", usage: CacheShareUsage{Requests: 3, InputTokens: 799, CacheReadTokens: 200}},
		{name: "excluded channel", usage: low,
			configure: func(s *operation_setting.MonitorSetting) { s.CacheShareAlertExcludedChannelIDs = []int{30} }},
		{name: "channel threshold 0 disables the channel", usage: low,
			configure: func(s *operation_setting.MonitorSetting) {
				s.CacheShareAlertChannelThresholds = map[string]float64{"30": 0}
			}},
		{name: "channel threshold below its share", usage: low,
			configure: func(s *operation_setting.MonitorSetting) {
				s.CacheShareAlertChannelThresholds = map[string]float64{"30": 0.15}
			}},
		{name: "channel threshold above the global one", usage: CacheShareUsage{Requests: 3, InputTokens: 300, CacheReadTokens: 700}, wantAlert: true,
			configure: func(s *operation_setting.MonitorSetting) {
				s.CacheShareAlertChannelThresholds = map[string]float64{"30": 0.8}
			}},
		{name: "alerted within the cooldown", usage: low, lastAlerted: map[int]int64{30: now - 3599}},
		{name: "cooldown elapsed", usage: low, lastAlerted: map[int]int64{30: now - 3600}, wantAlert: true},
		{name: "another channel's cooldown does not apply", usage: low, lastAlerted: map[int]int64{31: now}, wantAlert: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setting := cacheShareAlertTestSetting()
			if tt.configure != nil {
				tt.configure(&setting)
			}

			alerts := EvaluateCacheShareAlerts(&setting, map[int]CacheShareUsage{30: tt.usage}, tt.lastAlerted, now)

			if !tt.wantAlert {
				assert.Empty(t, alerts)
				return
			}
			require.Len(t, alerts, 1)
			assert.Equal(t, 30, alerts[0].ChannelId)
			assert.InDelta(t, tt.usage.Share(), alerts[0].Share, 1e-9)
		})
	}
}

func TestRunCacheShareAlertAggregatesConsumeLogsAndHonoursTheCooldown(t *testing.T) {
	restore := operation_setting.SetMonitorSettingForTest(cacheShareAlertTestSetting())
	t.Cleanup(restore)
	// Far in the future so no other test's logs fall in the window.
	const now = int64(4_000_000_000)
	const channelId = 9301
	logs := []model.Log{
		// Channel 9301: CR 300 against input 2400 + CW 300 (one row only has the
		// unsplit cache_creation_tokens) → share 300/3000 = 10%.
		{ChannelId: channelId, PromptTokens: 800, Other: `{"usage_semantic":"anthropic","cache_tokens":100,"cache_write_tokens":100,"cache_creation_tokens":100}`},
		{ChannelId: channelId, PromptTokens: 800, Other: `{"usage_semantic":"anthropic","cache_tokens":100,"cache_creation_tokens":200}`},
		{ChannelId: channelId, PromptTokens: 800, Other: `{"usage_semantic":"anthropic","cache_tokens":100}`},
		// Not counted: OpenAI semantics, a log before the window, a non-consume log.
		{ChannelId: channelId, PromptTokens: 50000, Other: `{"cache_tokens":0}`},
		{ChannelId: channelId, PromptTokens: 0, CreatedAt: now - 3601, Other: `{"usage_semantic":"anthropic","cache_tokens":900000}`},
		{ChannelId: channelId, PromptTokens: 0, Type: model.LogTypeSystem, Other: `{"usage_semantic":"anthropic","cache_tokens":900000}`},
	}
	for i := range logs {
		if logs[i].Type == 0 {
			logs[i].Type = model.LogTypeConsume
		}
		if logs[i].CreatedAt == 0 {
			logs[i].CreatedAt = now - 60
		}
		require.NoError(t, model.LOG_DB.Create(&logs[i]).Error)
	}
	t.Cleanup(func() { model.LOG_DB.Where("channel_id = ?", channelId).Delete(&model.Log{}) })

	type notification struct{ kind, subject, content string }
	var sent []notification
	notify := func(kind, subject, content string) { sent = append(sent, notification{kind, subject, content}) }

	result, err := RunCacheShareAlert(now, map[int]int64{}, notify)
	require.NoError(t, err)
	require.Len(t, result.Alerts, 1)
	alert := result.Alerts[0]
	assert.Equal(t, channelId, alert.ChannelId)
	assert.Equal(t, CacheShareUsage{Requests: 3, InputTokens: 2400, CacheReadTokens: 300, CacheWriteTokens: 300}, alert.CacheShareUsage)
	assert.InDelta(t, 0.1, alert.Share, 1e-9)
	require.Len(t, sent, 1, "one notification per run")
	assert.Equal(t, dto.NotifyTypeCacheShareAlert, sent[0].kind)
	assert.Contains(t, sent[0].content, "#9301")
	assert.Equal(t, now, result.LastAlerted[channelId])

	// The handler feeds the previous run's LastAlerted back in: within the
	// cooldown the same channel stays quiet, and the cooldown is carried on.
	var persisted CacheShareAlertResult
	data, err := common.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(data, &persisted))
	again, err := RunCacheShareAlert(now+600, persisted.LastAlerted, notify)
	require.NoError(t, err)
	assert.Empty(t, again.Alerts)
	assert.Len(t, sent, 1)
	assert.Equal(t, now, again.LastAlerted[channelId])
}
