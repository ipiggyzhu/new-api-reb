package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the owner's channel-selection contract end to end through
// the affinity layer, the memory channel cache and dynamic scoring:
//
//   - a new key goes to the highest dynamic-priority channel;
//   - a successful key keeps its channel while scores move;
//   - an error on the pinned channel moves the key to the next channel in dynamic
//     order, and it stays there on success.

// affinityLadderForTest creates three enabled channels for modelName at
// priorities 10, 5 and 0 (returned in that order), loads them into the memory
// channel cache, turns dynamic scoring on with fixed thresholds and installs the
// claude cli trace rule with SwitchOnSuccess, the production default.
func affinityLadderForTest(t *testing.T, modelName string) []*model.Channel {
	t.Helper()
	originalLogDB, originalLogType := model.LOG_DB, common.LogDatabaseType()
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	originalMemory, originalRedis := common.MemoryCacheEnabled, common.RedisEnabled
	common.MemoryCacheEnabled, common.RedisEnabled = true, false
	t.Cleanup(func() {
		model.InitChannelCache()
		common.MemoryCacheEnabled, common.RedisEnabled = originalMemory, originalRedis
		model.LOG_DB = originalLogDB
		common.SetLogDatabaseType(originalLogType)
	})

	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	channels := make([]*model.Channel, 0, 3)
	for _, priority := range []int64{10, 5, 0} {
		channel := &model.Channel{
			Type:     constant.ChannelTypeAnthropic,
			Name:     "affinity-ladder",
			Key:      "sk-test",
			Models:   modelName,
			Group:    "default",
			Status:   common.ChannelStatusEnabled,
			Priority: common.GetPointer(priority),
			Weight:   common.GetPointer(uint(10)),
		}
		require.NoError(t, model.DB.Create(channel).Error)
		t.Cleanup(func() {
			require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).Delete(&model.Ability{}).Error)
			require.NoError(t, model.DB.Delete(channel).Error)
		})
		require.NoError(t, channel.AddAbilities(nil))
		channels = append(channels, channel)
	}
	model.InitChannelCache()

	operation_setting.SetChannelDynamicScoreSettingForTest(operation_setting.ChannelDynamicScoreSetting{
		Enabled:                   true,
		SuccessesToPromote:        5,
		FaultsToDemote:            1,
		MaxPromoteTiers:           1,
		MaxDemoteTiers:            3,
		MinSampleForWeight:        20,
		SuccessWindowSeconds:      300,
		IdleResetSeconds:          1800,
		ProjectionIntervalMinutes: 10,
	})
	channel_score.ResetAll()
	t.Cleanup(func() {
		operation_setting.ResetChannelDynamicScoreSettingForTest()
		channel_score.ResetAll()
	})

	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	return channels
}

// reportOutcomesForTest feeds n identical outcomes for channelId into the score
// store, the way the relay loop reports each attempt.
func reportOutcomesForTest(channelId int, modelName string, outcome channel_score.Outcome, n int) {
	for i := 0; i < n; i++ {
		channel_score.Report(channelId, "default", modelName, outcome)
	}
}

// TestChannelAffinityPinSurvivesScoreJitter is regression (i): score movement on
// a healthy pin's route must not move its traffic. The pin sits on the failover
// channel B after A faulted. A then recovers, faults once more on another key's
// request, recovers again, and the third channel jitters too. Under the previous
// rule the pin on B was retired as soon as A was back at or above its baseline,
// although B never failed.
func TestChannelAffinityPinSurvivesScoreJitter(t *testing.T) {
	const (
		modelName = "claude-affinity-jitter-test"
		userID    = "jitter-session"
	)
	channels := affinityLadderForTest(t, modelName)
	a, b, c := channels[0].Id, channels[1].Id, channels[2].Id

	reportOutcomesForTest(a, modelName, channel_score.OutcomeFault, 1)
	pinChannelByAffinityForTest(t, modelName, userID, b)

	steps := []struct {
		name      string
		channelId int
		outcome   channel_score.Outcome
		count     int
	}{
		{name: "A earns its demotion back", channelId: a, outcome: channel_score.OutcomeSuccess, count: 5},
		{name: "A is promoted above its baseline", channelId: a, outcome: channel_score.OutcomeSuccess, count: 5},
		{name: "the pinned channel keeps succeeding", channelId: b, outcome: channel_score.OutcomeSuccess, count: 5},
		{name: "A faults once on another key", channelId: a, outcome: channel_score.OutcomeFault, count: 1},
		{name: "A recovers again", channelId: a, outcome: channel_score.OutcomeSuccess, count: 5},
		{name: "the lowest channel jitters", channelId: c, outcome: channel_score.OutcomeFault, count: 1},
		{name: "the lowest channel recovers", channelId: c, outcome: channel_score.OutcomeSuccess, count: 5},
	}
	for _, step := range steps {
		reportOutcomesForTest(step.channelId, modelName, step.outcome, step.count)
		ctx := newClaudeMessagesRequestForTest(t, modelName, userID)
		preferred, found := GetPreferredChannelByAffinity(ctx, modelName, "default")
		require.True(t, found, "%s: the pin must survive", step.name)
		assert.Equal(t, b, preferred, "%s: traffic must stay on the pinned channel", step.name)
	}
}

// TestChannelAffinityFailsOverInDynamicOrderAndSticks is regression (ii): an
// error on the pinned channel moves the key to the next channel in dynamic order,
// and the key stays there once that channel succeeds, even after the failed
// channel recovers.
func TestChannelAffinityFailsOverInDynamicOrderAndSticks(t *testing.T) {
	const (
		modelName = "claude-affinity-failover-test"
		userID    = "failover-session"
	)
	channels := affinityLadderForTest(t, modelName)
	a, b := channels[0].Id, channels[1].Id

	// A new key goes to the highest dynamic-priority channel.
	first, err := model.GetRandomSatisfiedChannel("default", modelName, 0, "/v1/messages", nil)
	require.NoError(t, err)
	require.NotNil(t, first)
	model.ReleaseChannelSlot(first.Id)
	require.Equal(t, a, first.Id, "a cold key starts on the top channel")
	pinChannelByAffinityForTest(t, modelName, userID, a)

	// The pinned channel answers with a routing fault; the relay loop records it,
	// excludes the channel and retries.
	ctx := newClaudeMessagesRequestForTest(t, modelName, userID)
	preferred, found := GetPreferredChannelByAffinity(ctx, modelName, "default")
	require.True(t, found)
	require.Equal(t, a, preferred)
	MarkChannelAffinityUsed(ctx, "default", a)
	MarkChannelAffinityChannelFault(ctx, a)
	reportOutcomesForTest(a, modelName, channel_score.OutcomeFault, 1)

	retry, err := model.GetRandomSatisfiedChannel("default", modelName, 0, "/v1/messages", map[int]bool{a: true})
	require.NoError(t, err)
	require.NotNil(t, retry)
	model.ReleaseChannelSlot(retry.Id)
	require.Equal(t, b, retry.Id, "the retry takes the next channel in dynamic order")

	// The retry succeeds on B.
	ctx.Set("channel_id", b)
	SetChannelAffinityRelayOutcome(ctx, true)
	RecordChannelAffinity(ctx, a)
	reportOutcomesForTest(b, modelName, channel_score.OutcomeSuccess, 1)

	// B keeps the key while A recovers and climbs back above its baseline.
	for round := 0; round < 3; round++ {
		next := newClaudeMessagesRequestForTest(t, modelName, userID)
		preferred, found := GetPreferredChannelByAffinity(next, modelName, "default")
		require.True(t, found, "round %d: the failover pin must hold", round)
		assert.Equal(t, b, preferred, "round %d: traffic stays on the channel that succeeded", round)
		reportOutcomesForTest(a, modelName, channel_score.OutcomeSuccess, 5)
	}
}
