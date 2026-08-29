package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestNormalizeModelNames(t *testing.T) {
	result := normalizeModelNames([]string{
		" gpt-4o ",
		"",
		"gpt-4o",
		"gpt-4.1",
		"   ",
	})

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
}

func TestMergeModelNames(t *testing.T) {
	result := mergeModelNames(
		[]string{"gpt-4o", "gpt-4.1"},
		[]string{"gpt-4.1", " gpt-4.1-mini ", "gpt-4o"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"}, result)
}

func TestSubtractModelNames(t *testing.T) {
	result := subtractModelNames(
		[]string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"},
		[]string{"gpt-4.1", "not-exists"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1-mini"}, result)
}

func TestIntersectModelNames(t *testing.T) {
	result := intersectModelNames(
		[]string{"gpt-4o", "gpt-4.1", "gpt-4.1", "not-exists"},
		[]string{"gpt-4.1", "gpt-4o-mini", "gpt-4o"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
}

func TestApplySelectedModelChanges(t *testing.T) {
	t.Run("add and remove together", func(t *testing.T) {
		result := applySelectedModelChanges(
			[]string{"gpt-4o", "gpt-4.1", "claude-3"},
			[]string{"gpt-4.1-mini"},
			[]string{"claude-3"},
		)

		require.Equal(t, []string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"}, result)
	})

	t.Run("add wins when conflict with remove", func(t *testing.T) {
		result := applySelectedModelChanges(
			[]string{"gpt-4o"},
			[]string{"gpt-4.1"},
			[]string{"gpt-4.1"},
		)

		require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
	})
}

func TestCollectPendingApplyUpstreamModelChanges(t *testing.T) {
	settings := dto.ChannelOtherSettings{
		UpstreamModelUpdateLastDetectedModels: []string{" gpt-4o ", "gpt-4o", "gpt-4.1"},
		UpstreamModelUpdateLastRemovedModels:  []string{" old-model ", "", "old-model"},
	}

	pendingAddModels, pendingRemoveModels := collectPendingApplyUpstreamModelChanges(settings)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, pendingAddModels)
	require.Equal(t, []string{"old-model"}, pendingRemoveModels)
}

func TestNormalizeChannelModelMapping(t *testing.T) {
	modelMapping := `{
		" alias-model ": " upstream-model ",
		"": "invalid",
		"invalid-target": ""
	}`
	channel := &model.Channel{
		ModelMapping: &modelMapping,
	}

	result := normalizeChannelModelMapping(channel)
	require.Equal(t, map[string]string{
		"alias-model": "upstream-model",
	}, result)
}

func TestCollectPendingUpstreamModelChangesFromModels_WithModelMapping(t *testing.T) {
	pendingAddModels, pendingRemoveModels := collectPendingUpstreamModelChangesFromModels(
		[]string{"alias-model", "gpt-4o", "stale-model"},
		[]string{"gpt-4o", "gpt-4.1", "mapped-target"},
		[]string{"gpt-4.1"},
		map[string]string{
			"alias-model": "mapped-target",
		},
	)

	require.Equal(t, []string{}, pendingAddModels)
	require.Equal(t, []string{"stale-model"}, pendingRemoveModels)
}

func TestCollectPendingUpstreamModelChangesFromModels_WithIgnoredRegexPatterns(t *testing.T) {
	pendingAddModels, pendingRemoveModels := collectPendingUpstreamModelChangesFromModels(
		[]string{"gpt-4o"},
		[]string{"gpt-4o", "claude-3-5-sonnet", "sora-video", "gpt-4.1"},
		[]string{"regex:^sora-.*$", "gpt-4.1"},
		nil,
	)

	require.Equal(t, []string{"claude-3-5-sonnet"}, pendingAddModels)
	require.Equal(t, []string{}, pendingRemoveModels)
}

func TestBuildUpstreamModelUpdateTaskNotificationContent_OmitOverflowDetails(t *testing.T) {
	channelSummaries := make([]upstreamModelUpdateChannelSummary, 0, 12)
	for i := 0; i < 12; i++ {
		channelSummaries = append(channelSummaries, upstreamModelUpdateChannelSummary{
			ChannelName: "channel-" + string(rune('A'+i)),
			AddCount:    i + 1,
			RemoveCount: i,
		})
	}

	content := buildUpstreamModelUpdateTaskNotificationContent(
		24,
		12,
		56,
		21,
		9,
		[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		channelSummaries,
		[]string{
			"gpt-4.1", "gpt-4.1-mini", "o3", "o4-mini", "gemini-2.5-pro", "claude-3.7-sonnet",
			"qwen-max", "deepseek-r1", "llama-3.3-70b", "mistral-large", "command-r-plus", "doubao-pro-32k",
			"hunyuan-large",
		},
		[]string{
			"gpt-3.5-turbo", "claude-2.1", "gemini-1.5-pro", "mixtral-8x7b", "qwen-plus", "glm-4",
			"yi-large", "moonshot-v1", "doubao-lite",
		},
	)

	require.Contains(t, content, "其余 4 个渠道已省略")
	require.Contains(t, content, "其余 1 个已省略")
	require.Contains(t, content, "失败渠道 ID（展示 10/12）")
	require.Contains(t, content, "其余 2 个已省略")
}

func TestShouldSendUpstreamModelUpdateNotification(t *testing.T) {
	channelUpstreamModelUpdateNotifyState.Lock()
	channelUpstreamModelUpdateNotifyState.lastNotifiedAt = 0
	channelUpstreamModelUpdateNotifyState.lastChangedChannels = 0
	channelUpstreamModelUpdateNotifyState.lastFailedChannels = 0
	channelUpstreamModelUpdateNotifyState.Unlock()

	baseTime := int64(2000000)

	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime, 6, 0))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+3600, 6, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+3600, 7, 0))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+7200, 7, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+8000, 0, 3))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+9000, 0, 3))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+10000, 0, 4))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+90000, 7, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+90001, 0, 0))
}

// newValidationRunContext builds a run context with the removal policy under
// test. Validation itself is not exercised here — classifyModelValidationResult
// is fed results directly, which is what keeps these assertions deterministic.
func newValidationRunContext(t *testing.T, threshold int, removeFailed bool) *upstreamModelUpdateRunContext {
	t.Helper()
	return &upstreamModelUpdateRunContext{
		monitor: &operation_setting.MonitorSetting{
			UpstreamModelUpdateRemoveFailed:     removeFailed,
			UpstreamModelUpdateFailureThreshold: threshold,
		},
		testUserID:       1,
		validationBudget: 100,
	}
}

func channelFaultResult() testResult {
	return testResult{
		newAPIError: types.NewError(
			errors.New("upstream key rejected"),
			types.ErrorCodeChannelInvalidKey,
		),
	}
}

func rateLimitResult() testResult {
	return testResult{
		newAPIError: types.NewErrorWithStatusCode(
			errors.New("Rate limit reached for this model, please retry later"),
			types.ErrorCode("rate_limit_exceeded"),
			http.StatusTooManyRequests,
		),
	}
}

// TestClassifyModelValidationResultRateLimitLeavesModelUntouched pins the rule
// that protects a throttled production channel: a 429 must not count as a
// failure and must never remove a model, no matter how many runs see it.
func TestClassifyModelValidationResultRateLimitLeavesModelUntouched(t *testing.T) {
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	t.Cleanup(func() { operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes })
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("401"))

	run := newValidationRunContext(t, 2, true)
	health := map[string]dto.ModelHealthState{}

	for i := 0; i < 5; i++ {
		approveAdd, remove := run.classifyModelValidationResult(
			7, "gpt-4o", rateLimitResult(), false, true, health, int64(1000+i), 2,
		)
		require.False(t, approveAdd)
		require.False(t, remove, "a rate limited model must never be removed (iteration %d)", i)
	}

	assert.Empty(t, health, "a rate limit must not record a failure")
}

// TestClassifyModelValidationResultRemovesOnlyAfterThreshold covers the
// "fault -> wait one retry delay -> still failing -> remove" policy: the first
// confirmed fault only records health, the second reaches the threshold.
func TestClassifyModelValidationResultRemovesOnlyAfterThreshold(t *testing.T) {
	run := newValidationRunContext(t, 2, true)
	health := map[string]dto.ModelHealthState{}

	approveAdd, remove := run.classifyModelValidationResult(
		7, "gpt-4o", channelFaultResult(), false, true, health, 1000, 2,
	)
	require.False(t, approveAdd)
	require.False(t, remove, "first confirmed fault must only record health")
	require.Equal(t, 1, health["gpt-4o"].Failures)
	require.Equal(t, int64(1000), health["gpt-4o"].LastFailureTime)
	require.NotEmpty(t, health["gpt-4o"].LastError)

	approveAdd, remove = run.classifyModelValidationResult(
		7, "gpt-4o", channelFaultResult(), false, true, health, 5000, 2,
	)
	require.False(t, approveAdd)
	assert.True(t, remove, "second confirmed fault reaches the threshold")
	assert.Equal(t, 2, health["gpt-4o"].Failures)
}

func TestClassifyModelValidationResultRemoveDisabledNeverRemoves(t *testing.T) {
	run := newValidationRunContext(t, 1, false)
	health := map[string]dto.ModelHealthState{}

	_, remove := run.classifyModelValidationResult(
		7, "gpt-4o", channelFaultResult(), false, true, health, 1000, 1,
	)

	assert.False(t, remove, "remove_failed off must suppress removal even at threshold")
	assert.Equal(t, 1, health["gpt-4o"].Failures, "health is still recorded for the report")
}

func TestClassifyModelValidationResultSuccessClearsHealthAndApprovesCandidate(t *testing.T) {
	run := newValidationRunContext(t, 2, true)
	health := map[string]dto.ModelHealthState{
		"gpt-4o": {Failures: 1, LastFailureTime: 900, LastError: "boom"},
	}

	approveAdd, remove := run.classifyModelValidationResult(
		7, "gpt-4o", testResult{}, false, true, health, 1000, 2,
	)
	require.False(t, approveAdd, "an existing model is not an add candidate")
	require.False(t, remove)
	assert.NotContains(t, health, "gpt-4o", "a recovered model must not keep its failure count")

	approveAdd, remove = run.classifyModelValidationResult(
		7, "gpt-4.1", testResult{}, true, false, health, 1000, 2,
	)
	assert.True(t, approveAdd, "a validated candidate is approved for adding")
	assert.False(t, remove)
}

// TestClassifyModelValidationResultFailedCandidateIsNotTracked keeps the health
// map from growing for models the channel does not even serve.
func TestClassifyModelValidationResultFailedCandidateIsNotTracked(t *testing.T) {
	run := newValidationRunContext(t, 2, true)
	health := map[string]dto.ModelHealthState{}

	approveAdd, remove := run.classifyModelValidationResult(
		7, "phantom-model", channelFaultResult(), true, false, health, 1000, 2,
	)

	assert.False(t, approveAdd, "a candidate that fails validation is not added")
	assert.False(t, remove)
	assert.Empty(t, health)
	assert.Equal(t, 1, run.rejectedModels)
}

// TestLimitUpstreamModelRemovalsGuards is the production blast-radius guard: an
// expired key or an upstream outage makes every model fail at once, and emptying
// the channel would take it out of service for every user.
func TestLimitUpstreamModelRemovalsGuards(t *testing.T) {
	existing := []string{"m1", "m2", "m3", "m4", "m5", "m6"}

	t.Run("all models failing removes nothing", func(t *testing.T) {
		assert.Nil(t, limitUpstreamModelRemovals(existing, existing))
	})

	t.Run("single model channel is never emptied", func(t *testing.T) {
		assert.Nil(t, limitUpstreamModelRemovals([]string{"only"}, []string{"only"}))
	})

	t.Run("more than half failing removes nothing", func(t *testing.T) {
		assert.Nil(t, limitUpstreamModelRemovals(existing, []string{"m1", "m2", "m3", "m4"}))
	})

	t.Run("up to half is applied", func(t *testing.T) {
		assert.Equal(t, []string{"m1", "m2", "m3"},
			limitUpstreamModelRemovals(existing, []string{"m1", "m2", "m3"}))
	})

	t.Run("unknown models are ignored", func(t *testing.T) {
		assert.Equal(t, []string{"m1"},
			limitUpstreamModelRemovals(existing, []string{"m1", "not-served"}))
	})

	t.Run("empty removal list stays empty", func(t *testing.T) {
		assert.Nil(t, limitUpstreamModelRemovals(existing, nil))
	})
}

func TestSelectModelsForUpstreamValidation(t *testing.T) {
	const retryDelaySeconds = int64(3600)
	existing := []string{"m1", "m2", "m3", "m4", "m5"}

	t.Run("candidates always come first", func(t *testing.T) {
		selected, _ := selectModelsForUpstreamValidation(
			existing, []string{"new1", "new2"}, nil, retryDelaySeconds, 0, 0, 10000, 100,
		)
		assert.Equal(t, []string{"new1", "new2"}, selected)
	})

	t.Run("cooling down failures are skipped", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"m2": {Failures: 1, LastFailureTime: 9000},
		}
		selected, _ := selectModelsForUpstreamValidation(
			existing, nil, health, retryDelaySeconds, 0, 0, 10000, 100,
		)
		assert.Empty(t, selected, "m2 failed 1000s ago and the retry delay is 3600s")
	})

	t.Run("expired failures are retested", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"m2": {Failures: 1, LastFailureTime: 5000},
		}
		selected, _ := selectModelsForUpstreamValidation(
			existing, nil, health, retryDelaySeconds, 0, 0, 10000, 100,
		)
		assert.Equal(t, []string{"m2"}, selected)
	})

	t.Run("rotation cursor covers every model across runs", func(t *testing.T) {
		seen := map[string]int{}
		cursor := 0
		for run := 0; run < 3; run++ {
			var selected []string
			selected, cursor = selectModelsForUpstreamValidation(
				existing, nil, nil, retryDelaySeconds, 2, cursor, 10000, 100,
			)
			require.Len(t, selected, 2)
			for _, modelName := range selected {
				seen[modelName]++
			}
		}
		assert.Len(t, seen, 5, "3 runs x 2 samples must reach all 5 models: %v", seen)
	})

	t.Run("budget caps the selection", func(t *testing.T) {
		selected, _ := selectModelsForUpstreamValidation(
			existing, []string{"new1", "new2", "new3"}, nil, retryDelaySeconds, 5, 0, 10000, 2,
		)
		assert.Equal(t, []string{"new1", "new2"}, selected)
	})

	t.Run("zero budget selects nothing and keeps the cursor", func(t *testing.T) {
		selected, cursor := selectModelsForUpstreamValidation(
			existing, []string{"new1"}, nil, retryDelaySeconds, 5, 3, 10000, 0,
		)
		assert.Empty(t, selected)
		assert.Equal(t, 3, cursor, "an unused run must not advance the rotation")
	})

	t.Run("out of range cursor restarts from the head", func(t *testing.T) {
		selected, cursor := selectModelsForUpstreamValidation(
			existing, nil, nil, retryDelaySeconds, 2, 99, 10000, 100,
		)
		assert.Equal(t, []string{"m1", "m2"}, selected)
		assert.Equal(t, 2, cursor)
	})

	t.Run("rotation disabled only validates candidates and retries", func(t *testing.T) {
		selected, cursor := selectModelsForUpstreamValidation(
			existing, []string{"new1"}, nil, retryDelaySeconds, 0, 0, 10000, 100,
		)
		assert.Equal(t, []string{"new1"}, selected)
		assert.Equal(t, 0, cursor)
	})
}

func TestPruneChannelModelHealth(t *testing.T) {
	t.Run("drops entries for models the channel no longer serves", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"kept":    {Failures: 1, LastFailureTime: 100},
			"dropped": {Failures: 1, LastFailureTime: 100},
		}
		pruneChannelModelHealth(health, []string{"kept"})
		assert.Equal(t, map[string]dto.ModelHealthState{
			"kept": {Failures: 1, LastFailureTime: 100},
		}, health)
	})

	t.Run("caps the map keeping the newest failures", func(t *testing.T) {
		health := make(map[string]dto.ModelHealthState)
		known := make([]string, 0, channelUpstreamModelUpdateMaxHealthEntries+10)
		for i := 0; i < channelUpstreamModelUpdateMaxHealthEntries+10; i++ {
			name := fmt.Sprintf("model-%03d", i)
			health[name] = dto.ModelHealthState{Failures: 1, LastFailureTime: int64(i)}
			known = append(known, name)
		}

		pruneChannelModelHealth(health, known)

		require.Len(t, health, channelUpstreamModelUpdateMaxHealthEntries)
		assert.NotContains(t, health, "model-000", "the oldest failure is dropped first")
		assert.Contains(t, health, fmt.Sprintf("model-%03d", channelUpstreamModelUpdateMaxHealthEntries+9))
	})
}

func TestDetectAllChannelUpstreamModelUpdatesRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeModelUpdate, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/upstream-models/detect-all", nil)

	DetectAllChannelUpstreamModelUpdates(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有模型更新任务正在运行或等待中")
}

// TestChannelUpstreamModelUpdateSelectFieldsMatchSchema runs the scan's own
// Select list against a migrated channels table.
//
// This is a regression test for a production outage: the list contained
// "openai_organization" (the json tag) where the migrated column is
// "open_ai_organization" (GORM's name for the field OpenAIOrganization). Every
// scheduled run failed with "no such column" and reported success with all
// counters at zero, so nothing was checked for a full day without any signal.
// Asserting the column names by hand would restate the mistake, so the test
// executes the real query instead: any name the schema does not have fails here.
func TestChannelUpstreamModelUpdateSelectFieldsMatchSchema(t *testing.T) {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))

	originalDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = originalDB })

	var channels []*model.Channel
	err = model.DB.
		Select(channelUpstreamModelUpdateSelectFields).
		Where("status = ?", common.ChannelStatusEnabled).
		Order("id asc").
		Find(&channels).Error
	require.NoError(t, err, "select list must name real columns on the migrated channels table")
}

// TestRunChannelUpstreamModelUpdateTaskOnceReportsScanError pins the other half
// of that outage: a scan that could not read any channel must say so, rather
// than being indistinguishable from a run that found nothing to do.
func TestRunChannelUpstreamModelUpdateTaskOnceReportsScanError(t *testing.T) {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	// Deliberately do NOT migrate Channel: the query then fails the same way a
	// wrong column name did.
	originalDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = originalDB })

	summary := runChannelUpstreamModelUpdateTaskOnce(context.Background(), false, false, nil)

	require.NotEmpty(t, summary.ScanError, "a failed scan query must be reported, not swallowed")
	assert.Zero(t, summary.CheckedChannels)
}

// TestConsumeValidationBudgetSharedAcrossWorkers pins the invariant the
// concurrent channel scan relies on: workers drawing from one shared budget can
// never spend more validation requests than the budget allows, and every spent
// request is counted exactly once.
func TestConsumeValidationBudgetSharedAcrossWorkers(t *testing.T) {
	run := &upstreamModelUpdateRunContext{validationBudget: 50}

	var wg sync.WaitGroup
	var consumed atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if run.consumeValidationBudget() {
					consumed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(50), consumed.Load())
	assert.Equal(t, 0, run.remainingValidationBudget())
	assert.Equal(t, 50, run.validatedModels)
}
