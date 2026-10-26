package service

import (
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The usage-cache counters live in one process-wide cache keyed by
// rule/group/fingerprint, so each test needs its own key or it reads another
// test's totals. A counter is used instead of time.Now().UnixNano() because the
// clock is too coarse on some platforms (Windows ticks in milliseconds) to
// separate tests that finish in well under a millisecond.
var channelAffinityTestKeySeq atomic.Uint64

func uniqueChannelAffinityTestKeys() (ruleName string, keyFP string) {
	seq := channelAffinityTestKeySeq.Add(1)
	return fmt.Sprintf("rule_%d", seq), fmt.Sprintf("fp_%d", seq)
}

func buildChannelAffinityStatsContextForTest(ruleName, usingGroup, keyFP string) *gin.Context {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	setChannelAffinityContext(ctx, channelAffinityMeta{
		CacheKey:       fmt.Sprintf("test:%s:%s:%s", ruleName, usingGroup, keyFP),
		TTLSeconds:     600,
		RuleName:       ruleName,
		UsingGroup:     usingGroup,
		KeyFingerprint: keyFP,
	})
	return ctx
}

func TestObserveChannelAffinityUsageCacheByRelayFormat_ClaudeMode(t *testing.T) {
	ruleName, keyFP := uniqueChannelAffinityTestKeys()
	usingGroup := "default"
	ctx := buildChannelAffinityStatsContextForTest(ruleName, usingGroup, keyFP)

	usage := &dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 40,
		TotalTokens:      140,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 30,
		},
	}

	ObserveChannelAffinityUsageCacheByRelayFormat(ctx, usage, types.RelayFormatClaude)
	stats := GetChannelAffinityUsageCacheStats(ruleName, usingGroup, keyFP)

	require.EqualValues(t, 1, stats.Total)
	require.EqualValues(t, 1, stats.Hit)
	require.EqualValues(t, 100, stats.PromptTokens)
	require.EqualValues(t, 40, stats.CompletionTokens)
	require.EqualValues(t, 140, stats.TotalTokens)
	require.EqualValues(t, 30, stats.CachedTokens)
	require.Equal(t, cacheTokenRateModeCachedOverPromptPlusCached, stats.CachedTokenRateMode)
}

// Counts of two token shapes are never summed: a shape change restarts the
// counters in the new shape, so the admin view keeps showing a rate. A bucket
// left "mixed" by an older build recovers on its next write the same way.
func TestObserveChannelAffinityUsageCacheShapeChangeRestartsCounters(t *testing.T) {
	usingGroup := "default"
	openAIUsage := &dto.Usage{
		PromptTokens: 100,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 10,
		},
	}
	claudeUsage := &dto.Usage{
		PromptTokens: 80,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 20,
		},
	}

	ruleName, keyFP := uniqueChannelAffinityTestKeys()
	ctx := buildChannelAffinityStatsContextForTest(ruleName, usingGroup, keyFP)
	ObserveChannelAffinityUsageCacheByRelayFormat(ctx, claudeUsage, types.RelayFormatClaude)
	ObserveChannelAffinityUsageCacheByRelayFormat(ctx, openAIUsage, types.RelayFormatOpenAI)
	ObserveChannelAffinityUsageCacheByRelayFormat(ctx, openAIUsage, types.RelayFormatOpenAI)
	stats := GetChannelAffinityUsageCacheStats(ruleName, usingGroup, keyFP)
	require.Equal(t, cacheTokenRateModeCachedOverPrompt, stats.CachedTokenRateMode)
	require.EqualValues(t, 2, stats.Total)
	require.EqualValues(t, 200, stats.PromptTokens)
	require.EqualValues(t, 20, stats.CachedTokens)

	ruleName, keyFP = uniqueChannelAffinityTestKeys()
	ctx = buildChannelAffinityStatsContextForTest(ruleName, usingGroup, keyFP)
	legacy := ChannelAffinityUsageCacheCounters{CachedTokenRateMode: cacheTokenRateModeMixed, Total: 6, Hit: 6, PromptTokens: 5050, CachedTokens: 5400}
	require.NoError(t, getChannelAffinityUsageCacheStatsCache().SetWithTTL(channelAffinityUsageCacheEntryKey(ruleName, usingGroup, keyFP), legacy, time.Minute))
	ObserveChannelAffinityUsageCacheByRelayFormat(ctx, openAIUsage, types.RelayFormatOpenAI)
	stats = GetChannelAffinityUsageCacheStats(ruleName, usingGroup, keyFP)
	require.Equal(t, cacheTokenRateModeCachedOverPrompt, stats.CachedTokenRateMode)
	require.EqualValues(t, 1, stats.Total)
	require.EqualValues(t, 100, stats.PromptTokens)
	require.EqualValues(t, 10, stats.CachedTokens)
}

func TestObserveChannelAffinityUsageCacheByRelayFormat_UnsupportedModeKeepsEmpty(t *testing.T) {
	ruleName, keyFP := uniqueChannelAffinityTestKeys()
	usingGroup := "default"
	ctx := buildChannelAffinityStatsContextForTest(ruleName, usingGroup, keyFP)

	usage := &dto.Usage{
		PromptTokens: 100,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 25,
		},
	}

	ObserveChannelAffinityUsageCacheByRelayFormat(ctx, usage, types.RelayFormatGemini)
	stats := GetChannelAffinityUsageCacheStats(ruleName, usingGroup, keyFP)

	require.EqualValues(t, 1, stats.Total)
	require.EqualValues(t, 1, stats.Hit)
	require.EqualValues(t, 25, stats.CachedTokens)
	require.Equal(t, "", stats.CachedTokenRateMode)
}
