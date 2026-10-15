package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

// Scheduled auto-update and manual full sweep share the same availability sync:
// test every existing and advertised model on enabled channels, adopt successes
// and delete confirmed failures from the channel's model list, which is the only
// source of abilities, routing and the model plaza. Rate limits and indeterminate
// failures are retried serially and the model is kept, never hidden.

const (
	channelModelSweepDefaultGroupSize      = 1
	channelModelSweepDefaultGroupDelayMs   = 500
	channelModelSweepDefaultMaxTestsPerRun = 0 // unlimited; an explicit cap reports an incomplete run
)

// getChannelModelSweepGroupSize is how many models of one channel are tested
// concurrently before the sweep waits and moves on to the next group. Concurrency
// within a channel is what makes the sweep finish in reasonable time; the group
// boundary plus the inter-group delay is what keeps it from bursting one upstream
// into rate limiting.
func getChannelModelSweepGroupSize() int {
	size := common.GetEnvOrDefault("CHANNEL_MODEL_SWEEP_GROUP_SIZE", channelModelSweepDefaultGroupSize)
	if size < 1 {
		return 1
	}
	if size > 32 {
		return 32
	}
	return size
}

func getChannelModelSweepGroupDelay() time.Duration {
	ms := common.GetEnvOrDefault("CHANNEL_MODEL_SWEEP_GROUP_DELAY_MS", channelModelSweepDefaultGroupDelayMs)
	if ms < 0 {
		ms = channelModelSweepDefaultGroupDelayMs
	}
	return time.Duration(ms) * time.Millisecond
}

func getChannelModelSweepMaxTestsPerRun() int {
	limit := common.GetEnvOrDefault("CHANNEL_MODEL_SWEEP_MAX_TESTS_PER_RUN", channelModelSweepDefaultMaxTestsPerRun)
	if limit < 1 {
		return int(^uint(0) >> 1)
	}
	return limit
}

type channelModelSweepVerdict int

const (
	sweepVerdictPass channelModelSweepVerdict = iota
	sweepVerdictFail
	sweepVerdictInconclusive
	sweepVerdictTemporary
	sweepVerdictAccount
)

// Only upstream evidence may remove models. Local request/pricing failures,
// rate limits, and temporary transport/server errors remain pending verification.
func classifyChannelModelSweepResult(result testResult) channelModelSweepVerdict {
	if result.newAPIError == nil && result.localErr == nil {
		return sweepVerdictPass
	}
	upstreamErr := result.newAPIError
	if result.localErr != nil || upstreamErr == nil || types.IsChannelError(upstreamErr) {
		return sweepVerdictInconclusive
	}
	if upstreamErr.StatusCode == http.StatusTooManyRequests || upstreamErr.StatusCode >= 500 {
		return sweepVerdictTemporary
	}
	if upstreamErr.StatusCode == http.StatusUnauthorized || upstreamErr.StatusCode == http.StatusPaymentRequired {
		return sweepVerdictAccount
	}
	message := strings.ToLower(upstreamErr.Error())
	if strings.Contains(message, "endpoint") {
		return sweepVerdictInconclusive
	}
	// Some Google-compatible upstreams report invalid credentials as 400, not
	// 401. Treat this like the other explicit authentication failures, without
	// mistaking an arbitrary bad probe parameter for channel unavailability.
	if upstreamErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(message, "api key not valid. please pass a valid api key.") {
		return sweepVerdictAccount
	}
	// Free-tier retirement is definitive for this exact slug. A paid replacement
	// mentioned in the response is not permission to change the model mapping.
	if upstreamErr.StatusCode == http.StatusNotFound &&
		strings.Contains(message, "this model is unavailable for free.") {
		return sweepVerdictFail
	}
	// A generic 410 may describe a removed endpoint, not a removed model.
	// Only the explicit model retirement response observed upstream qualifies.
	if upstreamErr.StatusCode == http.StatusGone {
		if strings.Contains(message, "model") && strings.Contains(message, "end of life") && strings.Contains(message, "no longer available") {
			return sweepVerdictFail
		}
	}
	if isModelUnavailableError(upstreamErr, nil) ||
		(upstreamErr.StatusCode == http.StatusBadRequest && upstreamErr.GetErrorCode() == types.ErrorCodeModelNotFound) {
		return sweepVerdictFail
	}
	return sweepVerdictInconclusive
}

// channelModelSweepSummary is the machine-readable result of one full sweep,
// stored on the system task so the settings page can report what happened.
type channelModelSweepSummary struct {
	CheckedChannels    int                       `json:"checked_channels"`
	ChangedChannels    int                       `json:"changed_channels"`
	TestedModels       int                       `json:"tested_models"`
	AddedModels        int                       `json:"added_models"`
	RemovedModels      int                       `json:"removed_models"`
	InconclusiveModels int                       `json:"inconclusive_models"`
	FailedChannels     int                       `json:"failed_channels"`
	EmptiedChannels    int                       `json:"emptied_channels"`
	DisabledChannels   int                       `json:"disabled_channels"`
	Complete           bool                      `json:"complete"`
	Channels           []channelModelSweepDetail `json:"channels"`
	ScanError          string                    `json:"scan_error,omitempty"`
}

type channelModelSweepDetail struct {
	ChannelID    int      `json:"channel_id"`
	Tested       int      `json:"tested_models"`
	Added        []string `json:"added_models"`
	Removed      []string `json:"removed_models"`
	Inconclusive int      `json:"inconclusive_models"`
	FetchFailed  bool     `json:"fetch_failed"`
	Failed       bool     `json:"failed"`
	Skipped      bool     `json:"skipped"`
	Disabled     bool     `json:"disabled"`
	Unverified   []string `json:"unverified_models,omitempty"`
}

// channelSweepResult is one channel's contribution to the summary.
type channelSweepResult struct {
	tested       int
	added        []string
	removed      []string
	inconclusive int
	emptied      bool
	disabled     bool
	changed      bool
	fetchFailed  bool
	skipped      bool
	incomplete   bool
	unverified   []string
}

// computeSweptModels decides a channel's next model list from the per-model
// verdicts. verdicts is keyed by the name a model is kept or adopted under; a
// model absent from the map was never tested (the per-run budget ran out) and is
// left exactly as it was — an existing model stays, a candidate is not adopted —
// so a partial run never deletes or adds on missing evidence.
//
// Confirmed failures can empty a channel; unknown results never invent evidence.
func computeSweptModels(
	existingModels []string,
	candidateModels []string,
	verdicts map[string]channelModelSweepVerdict,
) (nextModels []string, addedModels []string, removedModels []string, outage bool) {
	existing := normalizeModelNames(existingModels)
	existingSet := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		existingSet[m] = struct{}{}
	}

	next := make([]string, 0, len(existing)+len(candidateModels))
	for _, m := range existing {
		if v, ok := verdicts[m]; ok && v == sweepVerdictFail {
			removedModels = append(removedModels, m)
			continue
		}
		next = append(next, m)
	}
	for _, m := range normalizeModelNames(candidateModels) {
		if _, ok := existingSet[m]; ok {
			continue
		}
		if v, ok := verdicts[m]; ok && v == sweepVerdictPass {
			next = append(next, m)
			addedModels = append(addedModels, m)
		}
	}

	return next, addedModels, removedModels, false
}

// sweepTestItem pairs the model name a test is sent under (testName, the upstream
// id) with the name the model is kept/adopted under on the channel (adoptName).
// The two differ only for a newly detected model whose upstream id carries a
// vendor prefix that the channel serves under a bare name.
type sweepTestItem struct {
	testName  string
	adoptName string
}

// testChannelModelGroup tests one group of a channel's models concurrently and
// returns each model's verdict keyed by adopt name. Every goroutine loads its own
// channel row: a single *model.Channel carries mutable multi-key rotation state,
// and sharing it across concurrent tests would race.
func testChannelModelGroup(ctx context.Context, channelID int, testUserID int, isStream bool, items []sweepTestItem) map[string]channelModelSweepVerdict {
	verdicts := make(map[string]channelModelSweepVerdict, len(items))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		go func(item sweepTestItem) {
			defer wg.Done()
			verdict := sweepVerdictInconclusive
			if channel, err := model.GetChannelById(channelID, true); err == nil && channel.Status == common.ChannelStatusEnabled {
				if item.testName != item.adoptName {
					if mapping, changed := mergeChannelModelMapping(channel, map[string]string{item.adoptName: item.testName}); changed {
						channel.ModelMapping = mapping
					}
				}
				verdict = classifyChannelModelSweepResult(testChannel(ctx, channel, testUserID, item.adoptName, "", isStream))
			}
			mu.Lock()
			verdicts[item.adoptName] = verdict
			mu.Unlock()
		}(item)
	}
	wg.Wait()
	return verdicts
}

// sweepChannelModels tests the union of one channel's current models and the
// models its upstream advertises, in concurrent groups paced apart, then removes
// every confirmed failure (including models already on the channel) and adopts
// every newly passing upstream model. budget is the run-wide remaining test count:
// it is decremented per model and the sweep stops once it reaches zero.
func sweepChannelModels(ctx context.Context, channelID int, testUserID int, budget *int) (channelSweepResult, error) {
	result := channelSweepResult{}
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return result, err
	}
	if channel.Status != common.ChannelStatusEnabled {
		result.skipped = true
		return result, nil
	}
	existing := normalizeModelNames(channel.GetModels())

	// Only active aliases cover upstream ids. A stale mapping left after removal
	// must not prevent a recovered model from ever being discovered again.
	activeMapping := normalizeChannelModelMapping(channel)
	for alias := range activeMapping {
		if !slices.Contains(existing, alias) {
			delete(activeMapping, alias)
		}
	}
	upstreamModels, fetchErr := fetchChannelUpstreamModelIDs(channel)
	candidates, _, pendingMapping := collectPendingUpstreamModelChangesFromModels(existing, upstreamModels, nil, activeMapping,
		operation_setting.GetMonitorSetting().UpstreamModelUpdateStripVendorPrefix)
	if fetchErr != nil {
		// A fetch failure is not a reason to skip the channel: its existing models
		// still deserve a health check. Test them alone and adopt nothing new.
		common.SysLog(fmt.Sprintf("channel model sweep: upstream model fetch failed for channel %d, testing existing models only: %v", channelID, fetchErr))
		candidates = nil
		pendingMapping = nil
		result.fetchFailed = true
	}
	for name := range channel.GetOtherSettings().UpstreamModelUpdateModelHealth {
		candidates = append(candidates, name)
	}
	candidates = normalizeModelNames(candidates)

	existingSet := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		existingSet[m] = struct{}{}
	}
	items := make([]sweepTestItem, 0, len(existing)+len(candidates))
	for _, m := range existing {
		items = append(items, sweepTestItem{testName: m, adoptName: m})
	}
	for _, m := range candidates {
		if _, ok := existingSet[m]; ok {
			continue
		}
		testName := m
		if upstream, ok := pendingMapping[m]; ok && upstream != "" {
			testName = upstream
		}
		items = append(items, sweepTestItem{testName: testName, adoptName: m})
	}
	isStream := shouldUseStreamForAutomaticChannelTest(channel)
	groupSize := getChannelModelSweepGroupSize()
	groupDelay := getChannelModelSweepGroupDelay()

	verdicts := make(map[string]channelModelSweepVerdict, len(items))
	for start := 0; start < len(items); start += groupSize {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		if *budget <= 0 {
			break
		}
		end := start + groupSize
		if end > len(items) {
			end = len(items)
		}
		if end-start > *budget {
			end = start + *budget
		}

		for name, verdict := range testChannelModelGroup(ctx, channelID, testUserID, isStream, items[start:end]) {
			verdicts[name] = verdict
			result.tested++
			*budget--
			if verdict == sweepVerdictInconclusive {
				result.inconclusive++
			}
		}

		if end < len(items) {
			// Pace between groups so a burst of concurrent probes does not trip an
			// upstream rate limit; a configured global RequestInterval is additive.
			if wait := groupDelay + common.RequestInterval; wait > 0 {
				if ctx == nil {
					time.Sleep(wait)
				} else {
					timer := time.NewTimer(wait)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
					}
				}
			}
		}
	}
	// A burst limit is not model unavailability. Retry unresolved probes one at
	// a time after a pause; bounded retries leave explicit unverified results.
	for attempt := 0; attempt < 2; attempt++ {
		for _, item := range items {
			if verdict, ok := verdicts[item.adoptName]; !ok || (verdict != sweepVerdictInconclusive && verdict != sweepVerdictTemporary) {
				continue
			}
			if *budget <= 0 || (ctx != nil && ctx.Err() != nil) {
				break
			}
			wait := max(groupDelay, time.Second) * time.Duration(attempt+1)
			timer := time.NewTimer(wait)
			if ctx == nil {
				<-timer.C
			} else {
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return result, ctx.Err()
				}
			}
			for name, verdict := range testChannelModelGroup(ctx, channelID, testUserID, isStream, []sweepTestItem{item}) {
				verdicts[name] = verdict
				*budget--
			}
		}
	}
	result.inconclusive = 0
	for _, item := range items {
		verdict, tested := verdicts[item.adoptName]
		if !tested || verdict == sweepVerdictInconclusive || verdict == sweepVerdictTemporary || verdict == sweepVerdictAccount {
			result.unverified = append(result.unverified, item.adoptName)
		}
		if !tested {
			result.incomplete = true
		} else if verdict == sweepVerdictInconclusive || verdict == sweepVerdictTemporary || verdict == sweepVerdictAccount {
			result.inconclusive++
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}

	nextModels, added, removed, _ := computeSweptModels(existing, candidates, verdicts)
	// Removed models stay retry candidates in UpstreamModelUpdateModelHealth, so
	// an emptied channel is kept enabled: the next sync re-tests and restores them.
	disable := false
	if len(added) == 0 && len(removed) == 0 {
		return result, nil
	}

	var modelMapping *string
	if len(added) > 0 {
		modelMapping, _ = mergeChannelModelMapping(channel, pendingModelMappingFor(pendingMapping, added))
	}

	joined := strings.Join(nextModels, ",")
	applied, err := model.ApplyChannelModelSweep(channel, joined, modelMapping, disable)
	if err != nil {
		return result, err
	}
	if !applied {
		result.skipped = true
		return result, nil
	}
	result.added, result.removed = added, removed
	result.emptied = len(existing) > 0 && len(nextModels) == 0
	result.disabled = disable
	result.changed = true
	return result, nil
}

// runChannelModelSweepTaskOnce performs one operator-triggered sweep of every
// enabled channel. Channels are processed serially — matching the operator's
// "finish channel 1, then channel 2" description and keeping only one upstream
// under concurrent load at a time — and the sweep stops early if the run-wide test
// budget is exhausted or the task is cancelled. The returned summary is stored on
// the system task for the settings page to display.
func runChannelModelSweepTaskOnce(ctx context.Context, report func(processed, total int)) channelModelSweepSummary {
	summary := channelModelSweepSummary{Complete: true, Channels: make([]channelModelSweepDetail, 0)}

	testUserID, err := resolveChannelTestUserID(nil)
	if err != nil {
		summary.Complete = false
		summary.ScanError = err.Error()
		common.SysLog(fmt.Sprintf("channel model sweep: cannot resolve test user: %v", err))
		return summary
	}

	var totalChannels int64
	if err := model.DB.Model(&model.Channel{}).Where("status = ?", common.ChannelStatusEnabled).Count(&totalChannels).Error; err != nil {
		summary.Complete = false
		summary.ScanError = err.Error()
		return summary
	}

	budget := getChannelModelSweepMaxTestsPerRun()
	changed := false
	processed := 0
	lastID := 0
	for {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		channels, err := findEnabledChannelsAfterID(lastID, channelUpstreamModelUpdateTaskBatchSize)
		if err != nil {
			summary.ScanError = err.Error()
			common.SysLog(fmt.Sprintf("channel model sweep query failed: %v", err))
			break
		}
		if len(channels) == 0 {
			break
		}
		lastID = channels[len(channels)-1].Id

		for _, channel := range channels {
			if channel == nil {
				continue
			}
			if ctx != nil && ctx.Err() != nil {
				break
			}
			if budget <= 0 {
				common.SysLog(fmt.Sprintf("channel model sweep: per-run test budget (%d) exhausted; remaining channels left untouched", getChannelModelSweepMaxTestsPerRun()))
				break
			}
			processed++
			if report != nil {
				report(processed, int(totalChannels))
			}
			summary.CheckedChannels++
			channelResult, err := sweepChannelModels(ctx, channel.Id, testUserID, &budget)
			summary.Channels = append(summary.Channels, channelModelSweepDetail{
				ChannelID: channel.Id, Tested: channelResult.tested, Added: channelResult.added, Removed: channelResult.removed,
				Inconclusive: channelResult.inconclusive, FetchFailed: channelResult.fetchFailed, Skipped: channelResult.skipped,
				Unverified: channelResult.unverified, Failed: err != nil, Disabled: channelResult.disabled,
			})
			if err != nil || channelResult.fetchFailed {
				summary.FailedChannels++
			}
			if err != nil {
				common.SysLog(fmt.Sprintf("channel model sweep failed for channel %d: %v", channel.Id, err))
			}
			if err != nil || channelResult.fetchFailed || channelResult.incomplete || channelResult.inconclusive > 0 {
				summary.Complete = false
			}
			summary.TestedModels += channelResult.tested
			summary.InconclusiveModels += channelResult.inconclusive
			summary.AddedModels += len(channelResult.added)
			summary.RemovedModels += len(channelResult.removed)
			if channelResult.emptied {
				summary.EmptiedChannels++
			}
			if channelResult.disabled {
				summary.DisabledChannels++
			}
			if channelResult.changed {
				summary.ChangedChannels++
				changed = true
			}
		}

		if budget <= 0 || len(channels) < channelUpstreamModelUpdateTaskBatchSize {
			break
		}
	}
	if ctx != nil && ctx.Err() != nil {
		summary.Complete = false
		summary.ScanError = ctx.Err().Error()
	}
	if processed < int(totalChannels) || summary.ScanError != "" {
		summary.Complete = false
	}
	if !summary.Complete && summary.ScanError == "" {
		summary.ScanError = "model sync incomplete: inspect per-channel fetch failures, unverified models, or test budget"
	}

	if report != nil {
		report(processed, int(totalChannels))
	}
	if changed {
		refreshChannelRuntimeCache()
	}
	common.SysLog(fmt.Sprintf(
		"channel model sweep finished: checked=%d changed=%d tested=%d added=%d removed=%d inconclusive=%d failed=%d emptied=%d disabled=%d complete=%t",
		summary.CheckedChannels, summary.ChangedChannels, summary.TestedModels, summary.AddedModels,
		summary.RemovedModels, summary.InconclusiveModels, summary.FailedChannels, summary.EmptiedChannels, summary.DisabledChannels, summary.Complete,
	))
	return summary
}
