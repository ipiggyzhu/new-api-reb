package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

// This file implements the operator-triggered "test every model on every channel
// and prune the ones that fail" sweep, reached from the global model settings
// page.
//
// It is deliberately separate from the scheduled upstream-model巡检
// (runChannelUpstreamModelUpdateTaskOnce): that task is conservative by design —
// it validates only newly detected candidates, removes a model only after
// repeated confirmed failures across runs, and refuses to remove more than half a
// channel at once so a transient upstream outage cannot empty a channel. This
// sweep is the opposite tool the operator asked for: a single-pass, full test of
// the union of a channel's current models and everything its upstream advertises,
// removing every model that returns a confirmed channel fault or an explicit
// "model unavailable" right away, with the half-channel / cumulative-failure
// guards turned off.
//
// The one guard it keeps is that it never leaves a channel with zero models: a
// channel that fails on every single model is an outage (a dead key, a drained
// account), not a catalogue that legitimately shrank to nothing, and emptying it
// would wipe its abilities and take it out of service for every user.
//
// Rate limiting is handled the way the operator described: models are tested in
// fixed-size groups run concurrently, and the sweep waits between groups. A
// throttled model surfaces as an inconclusive result (not a channel fault), so it
// is left exactly as it was rather than removed.

const (
	channelModelSweepDefaultGroupSize      = 5
	channelModelSweepDefaultGroupDelayMs   = 500
	channelModelSweepDefaultMaxTestsPerRun = 5000
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
		return channelModelSweepDefaultMaxTestsPerRun
	}
	return limit
}

type channelModelSweepVerdict int

const (
	sweepVerdictPass channelModelSweepVerdict = iota
	sweepVerdictFail
	sweepVerdictInconclusive
)

// classifyChannelModelSweepResult turns one model test into a keep / remove /
// leave-alone decision, reusing the exact predicates the scheduled巡检 uses so the
// two paths agree on what a real failure is:
//
//   - a clean test passes;
//   - a confirmed channel fault (service.IsChannelFaultError) or an explicit
//     "this model does not exist" (isModelUnavailableError) fails, so the model is
//     removed;
//   - everything else — a rate limit, a transient upstream hiccup, or a local
//     error while building the synthetic request — is inconclusive and changes
//     nothing, so a throttled channel is never stripped of models it really has.
func classifyChannelModelSweepResult(result testResult) channelModelSweepVerdict {
	if result.newAPIError == nil && result.localErr == nil {
		return sweepVerdictPass
	}
	if service.IsChannelFaultError(result.newAPIError) || isModelUnavailableError(result.newAPIError, result.localErr) {
		return sweepVerdictFail
	}
	return sweepVerdictInconclusive
}

// channelModelSweepSummary is the machine-readable result of one full sweep,
// stored on the system task so the settings page can report what happened.
type channelModelSweepSummary struct {
	CheckedChannels    int    `json:"checked_channels"`
	ChangedChannels    int    `json:"changed_channels"`
	TestedModels       int    `json:"tested_models"`
	AddedModels        int    `json:"added_models"`
	RemovedModels      int    `json:"removed_models"`
	InconclusiveModels int    `json:"inconclusive_models"`
	FailedChannels     int    `json:"failed_channels"`
	OutageSkips        int    `json:"outage_skips"`
	ScanError          string `json:"scan_error,omitempty"`
}

// channelSweepResult is one channel's contribution to the summary.
type channelSweepResult struct {
	tested       int
	added        []string
	removed      []string
	inconclusive int
	outage       bool
	changed      bool
}

// computeSweptModels decides a channel's next model list from the per-model
// verdicts. verdicts is keyed by the name a model is kept or adopted under; a
// model absent from the map was never tested (the per-run budget ran out) and is
// left exactly as it was — an existing model stays, a candidate is not adopted —
// so a partial run never deletes or adds on missing evidence.
//
// The single guard retained from the conservative巡检 path is that a sweep never
// empties a channel: when every existing model failed, the cause is the channel
// itself (a dead key, a drained account), not a catalogue that shrank to nothing,
// so the original list is kept unchanged and outage is reported true.
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

	if len(next) == 0 && len(existing) > 0 {
		return existing, nil, nil, true
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
			if channel, err := model.GetChannelById(channelID, true); err == nil {
				verdict = classifyChannelModelSweepResult(
					testChannel(ctx, channel, testUserID, item.testName, "", isStream),
				)
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
	existing := normalizeModelNames(channel.GetModels())

	settings := channel.GetOtherSettings()
	candidates, _, pendingMapping, fetchErr := collectPendingUpstreamModelChanges(channel, settings)
	if fetchErr != nil {
		// A fetch failure is not a reason to skip the channel: its existing models
		// still deserve a health check. Test them alone and adopt nothing new.
		common.SysLog(fmt.Sprintf("channel model sweep: upstream model fetch failed for channel %d, testing existing models only: %v", channelID, fetchErr))
		candidates = nil
		pendingMapping = nil
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
	if len(items) == 0 {
		return result, nil
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

	nextModels, added, removed, outage := computeSweptModels(existing, candidates, verdicts)
	result.added = added
	result.removed = removed
	result.outage = outage
	if outage {
		common.SysLog(fmt.Sprintf("channel model sweep: every model failed on channel %d; keeping its %d model(s) unchanged (treated as a channel-wide outage, not a catalogue that shrank to nothing)", channelID, len(existing)))
	}
	if len(added) == 0 && len(removed) == 0 {
		return result, nil
	}

	var modelMapping *string
	if len(added) > 0 {
		modelMapping, _ = mergeChannelModelMapping(channel, pendingModelMappingFor(pendingMapping, added))
	}

	joined := strings.Join(nextModels, ",")
	if err := model.MutateChannelSettingsWithModels(channelID, joined, modelMapping, func(*dto.ChannelOtherSettings) bool { return true }); err != nil {
		return result, err
	}

	// Abilities are rebuilt from channel.Models (not the mapping), on the same
	// in-memory channel, exactly as the scheduled巡检 does after persisting.
	channel.Models = joined
	channelUpstreamModelPersistMu.Lock()
	err = channel.UpdateAbilities(nil)
	channelUpstreamModelPersistMu.Unlock()
	if err != nil {
		return result, fmt.Errorf("rebuild abilities for channel %d: %w", channelID, err)
	}
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
	summary := channelModelSweepSummary{}

	testUserID, err := resolveChannelTestUserID(nil)
	if err != nil {
		summary.ScanError = err.Error()
		common.SysLog(fmt.Sprintf("channel model sweep: cannot resolve test user: %v", err))
		return summary
	}

	var totalChannels int64
	if err := model.DB.Model(&model.Channel{}).Where("status = ?", common.ChannelStatusEnabled).Count(&totalChannels).Error; err != nil {
		totalChannels = 0
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

		// PLACEHOLDER_RUNNER_INNER
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
			if err != nil {
				summary.FailedChannels++
				common.SysLog(fmt.Sprintf("channel model sweep failed for channel %d: %v", channel.Id, err))
				continue
			}
			summary.TestedModels += channelResult.tested
			summary.InconclusiveModels += channelResult.inconclusive
			summary.AddedModels += len(channelResult.added)
			summary.RemovedModels += len(channelResult.removed)
			if channelResult.outage {
				summary.OutageSkips++
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

	if report != nil {
		report(processed, int(totalChannels))
	}
	if changed {
		refreshChannelRuntimeCache()
	}
	common.SysLog(fmt.Sprintf(
		"channel model sweep finished: checked=%d changed=%d tested=%d added=%d removed=%d inconclusive=%d failed=%d outageSkips=%d",
		summary.CheckedChannels, summary.ChangedChannels, summary.TestedModels, summary.AddedModels,
		summary.RemovedModels, summary.InconclusiveModels, summary.FailedChannels, summary.OutageSkips,
	))
	return summary
}
