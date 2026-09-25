package controller

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/ollama"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

const (
	channelUpstreamModelUpdateTaskDefaultIntervalMinutes  = 30
	channelUpstreamModelUpdateTaskBatchSize               = 100
	channelUpstreamModelUpdateTaskDefaultConcurrency      = 8
	channelUpstreamModelUpdateFetchTimeoutSeconds         = 15
	channelUpstreamModelUpdateMinCheckIntervalSeconds     = 300
	channelUpstreamModelUpdateNotifySuppressWindowSeconds = 86400
	channelUpstreamModelUpdateNotifyMaxChannelDetails     = 8
	channelUpstreamModelUpdateNotifyMaxModelDetails       = 12
	channelUpstreamModelUpdateNotifyMaxFailedChannelIDs   = 10
)

var channelUpstreamModelUpdateSelectFields = []string{
	"id",
	"name",
	"type",
	"key",
	"status",
	"base_url",
	"models",
	"model_mapping",
	"settings",
	"setting",
	"other",
	"group",
	"priority",
	"weight",
	"tag",
	// UpdateAbilities rebuilds every ability row from this struct, and each row
	// carries a copy of max_concurrency for the DB selection path. Loading the
	// channel without it wrote NULL back and silently lifted the channel's
	// concurrency cap on every scan that changed its model list.
	"max_concurrency",
	"channel_info",
	"header_override",
	// The columns below are not needed to diff model lists, but model validation
	// issues a real request through testChannel and must reproduce the channel's
	// runtime behavior. Without param_override / status_code_mapping a channel
	// that only works with an override would be misjudged as broken.
	"test_model",
	"param_override",
	"status_code_mapping",
	"auto_ban",
	// GORM derives this column from the Go field name OpenAIOrganization, not from
	// the json tag, so the column is open_ai_organization. Using the json name here
	// made every scan query fail with "no such column" and silently check nothing.
	// channelUpstreamModelUpdateSelectFields is verified against the migrated
	// schema in TestChannelUpstreamModelUpdateSelectFieldsMatchSchema.
	"open_ai_organization",
}

var channelUpstreamModelUpdateNotifyState = struct {
	sync.Mutex
	lastNotifiedAt      int64
	lastChangedChannels int
	lastFailedChannels  int
}{}

type applyChannelUpstreamModelUpdatesRequest struct {
	ID           int      `json:"id"`
	AddModels    []string `json:"add_models"`
	RemoveModels []string `json:"remove_models"`
	IgnoreModels []string `json:"ignore_models"`
}

type applyAllChannelUpstreamModelUpdatesResult struct {
	ChannelID             int      `json:"channel_id"`
	ChannelName           string   `json:"channel_name"`
	AddedModels           []string `json:"added_models"`
	RejectedModels        []string `json:"rejected_models"`
	RemovedModels         []string `json:"removed_models"`
	RemainingModels       []string `json:"remaining_models"`
	RemainingRemoveModels []string `json:"remaining_remove_models"`
}

type detectChannelUpstreamModelUpdatesResult struct {
	ChannelID       int      `json:"channel_id"`
	ChannelName     string   `json:"channel_name"`
	AddModels       []string `json:"add_models"`
	RemoveModels    []string `json:"remove_models"`
	LastCheckTime   int64    `json:"last_check_time"`
	AutoAddedModels int      `json:"auto_added_models"`
}

type upstreamModelUpdateChannelSummary struct {
	ChannelName string
	AddCount    int
	RemoveCount int
}

func normalizeModelNames(models []string) []string {
	return lo.Uniq(lo.FilterMap(models, func(model string, _ int) (string, bool) {
		trimmed := strings.TrimSpace(model)
		return trimmed, trimmed != ""
	}))
}

func mergeModelNames(base []string, appended []string) []string {
	merged := normalizeModelNames(base)
	seen := make(map[string]struct{}, len(merged))
	for _, model := range merged {
		seen[model] = struct{}{}
	}
	for _, model := range normalizeModelNames(appended) {
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		merged = append(merged, model)
	}
	return merged
}

func subtractModelNames(base []string, removed []string) []string {
	removeSet := make(map[string]struct{}, len(removed))
	for _, model := range normalizeModelNames(removed) {
		removeSet[model] = struct{}{}
	}
	return lo.Filter(normalizeModelNames(base), func(model string, _ int) bool {
		_, ok := removeSet[model]
		return !ok
	})
}

func intersectModelNames(base []string, allowed []string) []string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, model := range normalizeModelNames(allowed) {
		allowedSet[model] = struct{}{}
	}
	return lo.Filter(normalizeModelNames(base), func(model string, _ int) bool {
		_, ok := allowedSet[model]
		return ok
	})
}

func applySelectedModelChanges(originModels []string, addModels []string, removeModels []string) []string {
	// Add wins when the same model appears in both selected lists.
	normalizedAdd := normalizeModelNames(addModels)
	normalizedRemove := subtractModelNames(normalizeModelNames(removeModels), normalizedAdd)
	return subtractModelNames(mergeModelNames(originModels, normalizedAdd), normalizedRemove)
}

func normalizeChannelModelMapping(channel *model.Channel) map[string]string {
	if channel == nil || channel.ModelMapping == nil {
		return nil
	}
	rawMapping := strings.TrimSpace(*channel.ModelMapping)
	if rawMapping == "" || rawMapping == "{}" {
		return nil
	}
	parsed := make(map[string]string)
	if err := common.UnmarshalJsonStr(rawMapping, &parsed); err != nil {
		return nil
	}
	normalized := make(map[string]string, len(parsed))
	for source, target := range parsed {
		normalizedSource := strings.TrimSpace(source)
		normalizedTarget := strings.TrimSpace(target)
		if normalizedSource == "" || normalizedTarget == "" {
			continue
		}
		normalized[normalizedSource] = normalizedTarget
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// stripUpstreamVendorPrefixes maps each upstream id of the form vendor/name to
// the bare name a client would actually type, for the ids where that is
// unambiguous. Aggregator upstreams list openai/gpt-4o, anthropic/claude-sonnet-4
// and the like; adopting those verbatim gives the channel model names nobody
// requests, so they sit unused while gpt-4o goes unserved.
//
// The bare name is the last path segment, so accounts/vendor/models/x also
// collapses to x. An id keeps its prefix when the bare name is already a
// distinct upstream id (openai/gpt-4o next to gpt-4o) or when two prefixed ids
// would collapse to the same name (openai/gpt-4o next to azure/gpt-4o): guessing
// which one gpt-4o should mean is exactly the kind of silent misrouting this is
// meant to prevent. Suffixes such as :free or a date stay untouched.
func stripUpstreamVendorPrefixes(upstreamModels []string) map[string]string {
	upstreamSet := make(map[string]struct{}, len(upstreamModels))
	for _, modelName := range upstreamModels {
		upstreamSet[modelName] = struct{}{}
	}
	bareCount := make(map[string]int, len(upstreamModels))
	bareOf := make(map[string]string, len(upstreamModels))
	for _, modelName := range upstreamModels {
		slash := strings.LastIndex(modelName, "/")
		if slash < 0 {
			continue
		}
		bare := strings.TrimSpace(modelName[slash+1:])
		if bare == "" {
			continue
		}
		if _, ok := upstreamSet[bare]; ok {
			continue
		}
		bareOf[modelName] = bare
		bareCount[bare]++
	}
	for modelName, bare := range bareOf {
		if bareCount[bare] > 1 {
			delete(bareOf, modelName)
		}
	}
	return bareOf
}

// collectPendingUpstreamModelChangesFromModels diffs the channel's model list
// against what the upstream advertises.
//
// pendingModelMapping carries, for every pending candidate that was adopted under
// a bare name, the upstream id it must be mapped back to (see
// stripUpstreamVendorPrefixes). A candidate is only usable together with that
// entry, so the two travel together from detection through apply.
func collectPendingUpstreamModelChangesFromModels(
	localModels []string,
	upstreamModels []string,
	ignoredModels []string,
	modelMapping map[string]string,
	stripVendorPrefix bool,
) (pendingAddModels []string, pendingRemoveModels []string, pendingModelMapping map[string]string) {
	localSet := make(map[string]struct{})
	localModels = normalizeModelNames(localModels)
	upstreamModels = normalizeModelNames(upstreamModels)
	for _, modelName := range localModels {
		localSet[modelName] = struct{}{}
	}
	// Both spellings of a prefixed id count as "listed upstream": a channel that
	// already serves openai/gpt-4o verbatim must not have it flagged for removal
	// just because this run would have adopted it as gpt-4o.
	upstreamSet := make(map[string]struct{}, len(upstreamModels))
	for _, modelName := range upstreamModels {
		upstreamSet[modelName] = struct{}{}
	}
	bareOf := map[string]string{}
	if stripVendorPrefix {
		bareOf = stripUpstreamVendorPrefixes(upstreamModels)
		for _, bare := range bareOf {
			upstreamSet[bare] = struct{}{}
		}
	}

	normalizedIgnoredModels := normalizeModelNames(ignoredModels)
	isIgnored := func(modelName string) bool {
		return lo.ContainsBy(normalizedIgnoredModels, func(ignoredModel string) bool {
			if regexBody, ok := strings.CutPrefix(ignoredModel, "regex:"); ok {
				matched, err := regexp.MatchString(strings.TrimSpace(regexBody), modelName)
				return err == nil && matched
			}
			return ignoredModel == modelName
		})
	}

	redirectSourceSet := make(map[string]struct{}, len(modelMapping))
	redirectTargetSet := make(map[string]struct{}, len(modelMapping))
	for source, target := range modelMapping {
		redirectSourceSet[source] = struct{}{}
		redirectTargetSet[target] = struct{}{}
	}

	coveredUpstreamSet := make(map[string]struct{}, len(localSet)+len(redirectTargetSet))
	for modelName := range localSet {
		coveredUpstreamSet[modelName] = struct{}{}
	}
	for modelName := range redirectTargetSet {
		coveredUpstreamSet[modelName] = struct{}{}
	}

	pendingModelMapping = make(map[string]string)
	pendingAdd := make([]string, 0, len(upstreamModels))
	for _, upstreamName := range upstreamModels {
		candidate := upstreamName
		if bare, ok := bareOf[upstreamName]; ok {
			candidate = bare
		}
		// Covered under either spelling: an admin who listed or mapped the
		// prefixed id already serves this model.
		if _, ok := coveredUpstreamSet[candidate]; ok {
			continue
		}
		if _, ok := coveredUpstreamSet[upstreamName]; ok {
			continue
		}
		// Ignore rules match either spelling too, so an admin can write the
		// upstream's own id and still have it honored after normalization.
		if isIgnored(candidate) || (candidate != upstreamName && isIgnored(upstreamName)) {
			continue
		}
		pendingAdd = append(pendingAdd, candidate)
		if candidate != upstreamName {
			pendingModelMapping[candidate] = upstreamName
		}
	}
	pendingRemove := lo.Filter(localModels, func(modelName string, _ int) bool {
		// Redirect source models are virtual aliases and should not be removed
		// only because they are absent from upstream model list.
		if _, ok := redirectSourceSet[modelName]; ok {
			return false
		}
		_, ok := upstreamSet[modelName]
		return !ok
	})
	if len(pendingModelMapping) == 0 {
		pendingModelMapping = nil
	}
	return normalizeModelNames(pendingAdd), normalizeModelNames(pendingRemove), pendingModelMapping
}

func collectPendingUpstreamModelChanges(channel *model.Channel, settings dto.ChannelOtherSettings) (pendingAddModels []string, pendingRemoveModels []string, pendingModelMapping map[string]string, err error) {
	upstreamModels, err := fetchChannelUpstreamModelIDs(channel)
	if err != nil {
		return nil, nil, nil, err
	}
	pendingAddModels, pendingRemoveModels, pendingModelMapping = collectPendingUpstreamModelChangesFromModels(
		channel.GetModels(),
		upstreamModels,
		settings.UpstreamModelUpdateIgnoredModels,
		normalizeChannelModelMapping(channel),
		operation_setting.GetMonitorSetting().UpstreamModelUpdateStripVendorPrefix,
	)
	return pendingAddModels, pendingRemoveModels, pendingModelMapping, nil
}

// pendingModelMappingFor narrows a pending mapping to the models still staged,
// so an entry never outlives the candidate it belongs to.
func pendingModelMappingFor(pending map[string]string, models []string) map[string]string {
	if len(pending) == 0 || len(models) == 0 {
		return nil
	}
	narrowed := make(map[string]string, len(models))
	for _, modelName := range normalizeModelNames(models) {
		if upstreamName, ok := pending[modelName]; ok && upstreamName != "" {
			narrowed[modelName] = upstreamName
		}
	}
	if len(narrowed) == 0 {
		return nil
	}
	return narrowed
}

// mergeChannelModelMapping returns the channel's model_mapping with entries added
// for the given models, leaving every existing entry alone: the admin's own
// redirects always win over what the task inferred. It reports false when there
// is nothing to add or the stored mapping cannot be parsed, in which case the
// column must not be rewritten — replacing an admin's hand-edited JSON with a
// reconstruction is not this task's call.
func mergeChannelModelMapping(channel *model.Channel, entries map[string]string) (*string, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	merged := make(map[string]string)
	if raw := strings.TrimSpace(channel.GetModelMapping()); raw != "" && raw != "{}" {
		if err := common.UnmarshalJsonStr(raw, &merged); err != nil {
			common.SysLog(fmt.Sprintf(
				"upstream model mapping not written: channel_id=%d model_mapping is not valid JSON: %v",
				channel.Id, err,
			))
			return nil, false
		}
	}
	changed := false
	for modelName, upstreamName := range entries {
		if _, exists := merged[modelName]; exists {
			continue
		}
		merged[modelName] = upstreamName
		changed = true
	}
	if !changed {
		return nil, false
	}
	encoded, err := common.Marshal(merged)
	if err != nil {
		return nil, false
	}
	mapping := string(encoded)
	return &mapping, true
}

func getUpstreamModelUpdateMinCheckIntervalSeconds() int64 {
	interval := int64(common.GetEnvOrDefault(
		"CHANNEL_UPSTREAM_MODEL_UPDATE_MIN_CHECK_INTERVAL_SECONDS",
		channelUpstreamModelUpdateMinCheckIntervalSeconds,
	))
	if interval < 0 {
		return channelUpstreamModelUpdateMinCheckIntervalSeconds
	}
	return interval
}

// getUpstreamModelUpdateTaskConcurrency bounds how many channels one scheduled
// scan works on at the same time. Fetch and validation are network-bound and
// each channel talks to a different upstream, so a small worker pool collapses
// the serial tail of unreachable upstreams without bursting any single one.
func getUpstreamModelUpdateTaskConcurrency() int {
	concurrency := common.GetEnvOrDefault(
		"CHANNEL_UPSTREAM_MODEL_UPDATE_CONCURRENCY",
		channelUpstreamModelUpdateTaskDefaultConcurrency,
	)
	if concurrency < 1 {
		return 1
	}
	if concurrency > 32 {
		return 32
	}
	return concurrency
}

func getUpstreamModelUpdateFetchTimeout() time.Duration {
	seconds := common.GetEnvOrDefault(
		"CHANNEL_UPSTREAM_MODEL_UPDATE_FETCH_TIMEOUT_SECONDS",
		channelUpstreamModelUpdateFetchTimeoutSeconds,
	)
	if seconds < 1 {
		seconds = channelUpstreamModelUpdateFetchTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

func fetchChannelUpstreamModelIDs(channel *model.Channel) ([]string, error) {
	baseURL := constant.ChannelBaseURLs[channel.Type]
	if channel.GetBaseURL() != "" {
		baseURL = channel.GetBaseURL()
	}

	if channel.Type == constant.ChannelTypeOllama {
		key := strings.TrimSpace(strings.Split(channel.Key, "\n")[0])
		models, err := ollama.FetchOllamaModels(baseURL, key)
		if err != nil {
			return nil, err
		}
		return normalizeModelNames(lo.Map(models, func(item ollama.OllamaModel, _ int) string {
			return item.Name
		})), nil
	}

	if channel.Type == constant.ChannelTypeGemini {
		key, _, apiErr := channel.GetNextEnabledKey()
		if apiErr != nil {
			return nil, fmt.Errorf("获取渠道密钥失败: %w", apiErr)
		}
		key = strings.TrimSpace(key)
		models, err := gemini.FetchGeminiModels(baseURL, key, channel.GetSetting().Proxy)
		if err != nil {
			return nil, err
		}
		return normalizeModelNames(models), nil
	}

	var url string
	switch channel.Type {
	case constant.ChannelTypeAli:
		url = fmt.Sprintf("%s/compatible-mode/v1/models", baseURL)
	case constant.ChannelTypeZhipu_v4:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/api/paas/v4/models", baseURL)
		}
	case constant.ChannelTypeVolcEngine:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/v1/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/v1/models", baseURL)
		}
	case constant.ChannelTypeMoonshot:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/v1/models", baseURL)
		}
	default:
		url = fmt.Sprintf("%s/v1/models", baseURL)
	}

	key, _, apiErr := channel.GetNextEnabledKey()
	if apiErr != nil {
		return nil, fmt.Errorf("获取渠道密钥失败: %w", apiErr)
	}
	key = strings.TrimSpace(key)

	headers, err := buildFetchModelsHeaders(channel, key)
	if err != nil {
		return nil, err
	}

	// A dead upstream must cost seconds, not the OS TCP stack's patience: the
	// model list is a small GET, so anything slower than this is effectively down.
	fetchCtx, cancel := context.WithTimeout(context.Background(), getUpstreamModelUpdateFetchTimeout())
	defer cancel()
	body, err := GetResponseBody(fetchCtx, http.MethodGet, url, channel, headers)
	if err != nil {
		// 上游对请求形状的口味互相矛盾，一套头无法同时满足：agentrouter 只放行
		// 真实客户端 UA（裸 UA 401 "unauthorized client detected"），ioll.pp.ua
		// 的 WAF 反过来专拦 SDK UA（403），个别 Anthropic 型网关见到 x-api-key
		// 头直接 panic 500，而 new-api 系网关只认 Bearer。首选完整装扮（认证
		// 双头 + 客户端画像），被拒后退回最朴素的 Bearer 裸头重试，两种形状
		// 合起来覆盖已知的全部上游脾气。
		retryCtx, retryCancel := context.WithTimeout(context.Background(), getUpstreamModelUpdateFetchTimeout())
		defer retryCancel()
		body, err = GetResponseBody(retryCtx, http.MethodGet, url, channel, GetAuthHeader(key))
	}
	if err != nil {
		return nil, err
	}

	var result OpenAIModelsResponse
	if err := common.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	ids := lo.Map(result.Data, func(item OpenAIModel, _ int) string {
		if channel.Type == constant.ChannelTypeGemini {
			return strings.TrimPrefix(item.ID, "models/")
		}
		return item.ID
	})

	return normalizeModelNames(ids), nil
}

// channelUpstreamModelPersistMu serializes this task's ability rebuilds. The scan
// runs channels concurrently, but SQLite allows only one writer at a time, and a
// rebuild is a delete-then-insert of every row for a channel, so letting workers
// rebuild simultaneously would trade progress for busy-wait errors.
//
// It does NOT guard the settings column any more: that is serialized per channel
// inside model.MutateChannelSettings, which also covers the WebSocket probe this
// mutex could never reach.
var channelUpstreamModelPersistMu sync.Mutex

// updateChannelUpstreamModelSettings persists this task's own fields onto the
// channel's settings.
//
// It copies field by field onto a freshly loaded copy instead of writing the
// whole struct. The task computes `settings` from a snapshot taken at the start of
// a scan that makes real upstream requests, so by the time it writes, minutes may
// have passed and the WebSocket handshake probe may have set
// websocket_unsupported on the same column. Writing the whole struct discarded
// that flag; writing only what this task owns cannot.
//
// The channel argument is still updated in place so the caller's later
// UpdateAbilities sees the new model list.
//
// modelMapping, when non-nil, is written to the model_mapping column in the same
// statement as the model list: a model adopted under a bare name is only
// reachable through the mapping that turns it back into the upstream id.
func updateChannelUpstreamModelSettings(channel *model.Channel, settings dto.ChannelOtherSettings, updateModels bool, modelMapping *string) error {
	apply := func(stored *dto.ChannelOtherSettings) bool {
		// Exactly the UpstreamModelUpdate* group. Everything else on the struct
		// belongs to the admin form or to the handshake probe, and must survive.
		stored.UpstreamModelUpdateCheckEnabled = settings.UpstreamModelUpdateCheckEnabled
		stored.UpstreamModelUpdateAutoSyncEnabled = settings.UpstreamModelUpdateAutoSyncEnabled
		stored.UpstreamModelUpdateLastCheckTime = settings.UpstreamModelUpdateLastCheckTime
		stored.UpstreamModelUpdateLastDetectedModels = settings.UpstreamModelUpdateLastDetectedModels
		stored.UpstreamModelUpdateLastRemovedModels = settings.UpstreamModelUpdateLastRemovedModels
		stored.UpstreamModelUpdateIgnoredModels = settings.UpstreamModelUpdateIgnoredModels
		stored.UpstreamModelUpdatePendingModelMapping = settings.UpstreamModelUpdatePendingModelMapping
		stored.UpstreamModelUpdateModelHealth = settings.UpstreamModelUpdateModelHealth
		stored.UpstreamModelUpdateRotationCursor = settings.UpstreamModelUpdateRotationCursor
		return true
	}

	// Keep the in-memory channel consistent with what the caller expects to have
	// written, so code after this call reads the values it just set.
	channel.SetOtherSettings(settings)
	if modelMapping != nil {
		channel.ModelMapping = modelMapping
	}

	if updateModels || modelMapping != nil {
		return model.MutateChannelSettingsWithModels(channel.Id, channel.Models, modelMapping, apply)
	}
	return model.MutateChannelSettings(channel.Id, apply)
}

// upstreamModelUpdateRunContext carries the per-run policy and state of one
// pass that may add models to channels: the scheduled scan, the manual apply
// endpoints, or "run now". A nil pointer means detect-only: stage the diff for
// review, never validate and never apply.
type upstreamModelUpdateRunContext struct {
	ctx        context.Context
	monitor    *operation_setting.MonitorSetting
	testUserID int

	// autoSyncAll is the global auto-update switch: every scanned channel adopts
	// its validated candidates. With it off, only channels whose own auto-sync
	// flag is on adopt anything — the channel-level setting takes precedence,
	// the global one fills in for the rest.
	autoSyncAll bool
	// candidatesOnly restricts validation to the models about to be added. It is
	// set for every path other than the global auto-update scan, so a manual
	// apply or a channel-level auto-sync never re-tests or removes models the
	// channel already serves: those are opted into via the global switch alone.
	candidatesOnly bool

	// mu guards every counter below: the task scans channels concurrently and
	// all workers draw from the same shared validation budget.
	mu sync.Mutex
	// validationBudget is the number of real requests left for the whole run. It
	// is shared across channels so one channel with hundreds of new upstream
	// models cannot spend the entire budget of the next ones by itself.
	validationBudget int

	validatedModels     int
	rejectedModels      int
	removedFailedModels int
}

func (r *upstreamModelUpdateRunContext) remainingValidationBudget() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.validationBudget
}

// consumeValidationBudget reserves one validation request from the shared
// budget, reporting false once the run is out of budget.
func (r *upstreamModelUpdateRunContext) consumeValidationBudget() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.validationBudget <= 0 {
		return false
	}
	r.validationBudget--
	r.validatedModels++
	return true
}

// validationEnabled reports whether this run should confirm models with a real
// request before adding them and before removing them.
func (r *upstreamModelUpdateRunContext) validationEnabled() bool {
	return r != nil && r.monitor.UpstreamModelUpdateValidate && r.testUserID > 0
}

// newUpstreamModelUpdateRunContext builds the run policy from a monitor snapshot.
// It returns nil when the run could add nothing and validate nothing, which keeps
// the detect-only paths on the nil fast path.
//
// Validation issues billable requests, so it needs a user to bill. c supplies the
// admin on the manual endpoints; the scheduled scan passes nil and falls back to
// the root user. If no user can be resolved the run degrades to detect-only and
// says so in the log, rather than adopting models on trust.
func newUpstreamModelUpdateRunContext(ctx context.Context, c *gin.Context, monitor *operation_setting.MonitorSetting, candidatesOnly bool) *upstreamModelUpdateRunContext {
	if !monitor.UpstreamModelUpdateEnabled && !monitor.UpstreamModelUpdateValidate {
		return nil
	}
	run := &upstreamModelUpdateRunContext{
		ctx:              ctx,
		monitor:          monitor,
		autoSyncAll:      monitor.UpstreamModelUpdateEnabled,
		candidatesOnly:   candidatesOnly || !monitor.UpstreamModelUpdateEnabled,
		validationBudget: monitor.GetUpstreamModelUpdateMaxValidationsPerRun(),
	}
	if monitor.UpstreamModelUpdateValidate {
		testUserID, err := resolveChannelTestUserID(c)
		if err != nil {
			common.SysLog(fmt.Sprintf("upstream model validation disabled for this run: %v", err))
		} else {
			run.testUserID = testUserID
		}
	}
	return run
}

const (
	// channelUpstreamModelUpdateMaxHealthEntries bounds the per-channel health map
	// so a channel whose upstream fails wholesale cannot grow the settings column
	// without limit.
	channelUpstreamModelUpdateMaxHealthEntries = 200
	// channelUpstreamModelUpdateMaxHealthErrorLength truncates the recorded error;
	// the full text is already in the system log.
	channelUpstreamModelUpdateMaxHealthErrorLength = 200
)

// selectModelsForUpstreamValidation decides which models this run should confirm
// with a real request. Validating every model on every channel would be far too
// expensive, so coverage is split three ways:
//
//   - every newly detected upstream candidate, because an unvalidated add is how
//     zombie models enter the channel in the first place;
//   - models that already failed and whose retry delay has elapsed, which is what
//     turns a single failure into a confirmed removal;
//   - a rotating sample of the models already serving traffic, so a model that
//     silently dies is eventually noticed even if upstream still lists it.
//
// The returned cursor is persisted so the rotation advances across runs instead
// of re-checking the same head of the list forever.
func selectModelsForUpstreamValidation(
	existingModels []string,
	candidateModels []string,
	health map[string]dto.ModelHealthState,
	retryDelaySeconds int64,
	rotationSampleSize int,
	cursor int,
	now int64,
	budget int,
) (selected []string, nextCursor int) {
	nextCursor = cursor
	if budget <= 0 {
		return nil, nextCursor
	}

	seen := make(map[string]struct{}, len(candidateModels)+rotationSampleSize)
	appendModel := func(modelName string) bool {
		if len(selected) >= budget {
			return false
		}
		if _, ok := seen[modelName]; ok {
			return true
		}
		seen[modelName] = struct{}{}
		selected = append(selected, modelName)
		return true
	}

	for _, modelName := range normalizeModelNames(candidateModels) {
		if !appendModel(modelName) {
			return selected, nextCursor
		}
	}

	existingModels = normalizeModelNames(existingModels)
	for _, modelName := range existingModels {
		state, ok := health[modelName]
		if !ok {
			continue
		}
		if state.LastFailureTime > 0 && now-state.LastFailureTime < retryDelaySeconds {
			continue // still cooling down; re-testing now would just burn quota
		}
		if !appendModel(modelName) {
			return selected, nextCursor
		}
	}

	if rotationSampleSize <= 0 || len(existingModels) == 0 {
		return selected, nextCursor
	}
	if cursor < 0 || cursor >= len(existingModels) {
		cursor = 0
	}
	sampleSize := min(rotationSampleSize, len(existingModels))
	for i := 0; i < sampleSize; i++ {
		if !appendModel(existingModels[(cursor+i)%len(existingModels)]) {
			break
		}
	}
	nextCursor = (cursor + sampleSize) % len(existingModels)

	return selected, nextCursor
}

// limitUpstreamModelRemovals bounds how much damage one automated run can do.
// Upstream-wide outages, an expired key, or a proxy failure make every model on
// a channel look broken at the same time; without a bound the run would empty
// the channel and wipe its abilities, taking the channel out of service for
// every user until an admin noticed. Losing a few stale model names for one more
// day is strictly cheaper than that.
func limitUpstreamModelRemovals(existingModels []string, removeModels []string) []string {
	existingModels = normalizeModelNames(existingModels)
	removeModels = intersectModelNames(removeModels, existingModels)
	if len(removeModels) == 0 {
		return nil
	}
	if len(removeModels) >= len(existingModels) {
		return nil // never leave a channel with no models at all
	}
	if len(removeModels) > len(existingModels)/2 {
		return nil // more than half failing looks like an outage, not model churn
	}
	return removeModels
}

// upstreamModelUpdateMaxCumulativeFailureRatio bounds how much of a channel may be
// under recorded suspicion before removals stop entirely.
//
// limitUpstreamModelRemovals only sees ONE run. Rotation samples a handful of
// models per run, so a channel-wide outage produces a small, individually
// "reasonable" removal set every run and passes that guard each time — eroding most
// of a working configuration over a day while never once tripping the half-channel
// rule. This looks at the accumulated health map instead: if a large share of the
// channel is failing, the explanation is the channel, not the models.
const upstreamModelUpdateMaxCumulativeFailureRatio = 0.5

// shouldSuspendUpstreamModelRemovals reports whether the breadth of recorded
// failures makes removal unsafe for this channel right now. It is deliberately
// evaluated against models the channel still serves, so stale health entries for
// already-removed models cannot hold a healthy channel hostage.
func shouldSuspendUpstreamModelRemovals(existingModels []string, health map[string]dto.ModelHealthState) bool {
	if len(health) == 0 || len(existingModels) == 0 {
		return false
	}
	failing := 0
	for _, modelName := range normalizeModelNames(existingModels) {
		if state, ok := health[modelName]; ok && state.Failures > 0 {
			failing++
		}
	}
	if failing == 0 {
		return false
	}
	return float64(failing) > float64(len(existingModels))*upstreamModelUpdateMaxCumulativeFailureRatio
}

// validateChannelModels issues one real request per selected model and turns the
// results into add/remove decisions. Only errors that service.IsChannelFaultError
// classifies as genuine channel faults count against a model: a rate limit or a
// transient hiccup leaves the model exactly as it was, which is what keeps a
// throttled channel from being stripped of its models.
func (r *upstreamModelUpdateRunContext) validateChannelModels(
	channel *model.Channel,
	settings *dto.ChannelOtherSettings,
	candidateModels []string,
) (approvedAddModels []string, removeModels []string) {
	existingModels := normalizeModelNames(channel.GetModels())
	health := settings.UpstreamModelUpdateModelHealth
	if health == nil {
		health = make(map[string]dto.ModelHealthState)
	}

	now := common.GetTimestamp()
	retryDelaySeconds := int64(r.monitor.GetUpstreamModelUpdateRetryDelayMinutes()) * 60
	selectionHealth := health
	rotationSampleSize := r.monitor.GetUpstreamModelUpdateRotationSampleSize()
	if r.candidatesOnly {
		// Neither the retry of recorded failures nor the rotation sample: both
		// exist to remove models, and this run is only allowed to add.
		selectionHealth = nil
		rotationSampleSize = 0
	}
	selected, nextCursor := selectModelsForUpstreamValidation(
		existingModels,
		candidateModels,
		selectionHealth,
		retryDelaySeconds,
		rotationSampleSize,
		settings.UpstreamModelUpdateRotationCursor,
		now,
		r.remainingValidationBudget(),
	)
	settings.UpstreamModelUpdateRotationCursor = nextCursor

	candidateSet := make(map[string]struct{}, len(candidateModels))
	for _, modelName := range normalizeModelNames(candidateModels) {
		candidateSet[modelName] = struct{}{}
	}
	existingSet := make(map[string]struct{}, len(existingModels))
	for _, modelName := range existingModels {
		existingSet[modelName] = struct{}{}
	}

	failureThreshold := r.monitor.GetUpstreamModelUpdateFailureThreshold()
	isStream := shouldUseStreamForAutomaticChannelTest(channel)

	for index, modelName := range selected {
		if r.ctx != nil && r.ctx.Err() != nil {
			break
		}
		if r.remainingValidationBudget() <= 0 {
			break
		}
		// Pace every request, not just the ones that failed: validation hits the
		// same upstream endpoints as live traffic and a burst is what triggers
		// rate limiting in the first place.
		if index > 0 && common.RequestInterval > 0 {
			if r.ctx == nil {
				time.Sleep(common.RequestInterval)
			} else {
				select {
				case <-r.ctx.Done():
					return approvedAddModels, nil
				case <-time.After(common.RequestInterval):
				}
			}
		}
		if !r.consumeValidationBudget() {
			break
		}

		result := testChannel(r.ctx, channel, r.testUserID, modelName, "", isStream)
		_, isCandidate := candidateSet[modelName]
		_, isExisting := existingSet[modelName]

		approveAdd, remove := r.classifyModelValidationResult(
			channel.Id, modelName, result, isCandidate, isExisting, health, now, failureThreshold,
		)
		if approveAdd {
			approvedAddModels = append(approvedAddModels, modelName)
		}
		if remove {
			removeModels = append(removeModels, modelName)
		}
	}

	// Checked before the per-run bound: the health map remembers what earlier runs
	// saw, which is the only place a slow channel-wide outage is visible. The
	// failures stay recorded either way, so once the channel recovers and successes
	// clear them, removal resumes on its own.
	if shouldSuspendUpstreamModelRemovals(existingModels, health) {
		if len(removeModels) > 0 {
			common.SysLog(fmt.Sprintf(
				"upstream model removal suspended: channel_id=%d models=%s reason=too many models failing at once",
				channel.Id, strings.Join(removeModels, ","),
			))
		}
		removeModels = nil
	}

	removeModels = limitUpstreamModelRemovals(existingModels, removeModels)
	if len(removeModels) > 0 {
		r.mu.Lock()
		r.removedFailedModels += len(removeModels)
		r.mu.Unlock()
	}

	pruneChannelModelHealth(health, mergeModelNames(existingModels, approvedAddModels))
	if len(health) == 0 {
		settings.UpstreamModelUpdateModelHealth = nil
	} else {
		settings.UpstreamModelUpdateModelHealth = health
	}

	return approvedAddModels, removeModels
}

// classifyModelValidationResult turns one validation result into add/remove
// decisions and records the model's health. It is the single place where the
// removal policy lives:
//
//   - success clears any recorded failure, and approves a staged candidate;
//   - an error that service.IsChannelFaultError does not consider a channel fault
//     (a rate limit, a transient upstream hiccup) changes nothing at all, so a
//     throttled channel keeps serving every model it has;
//   - a confirmed fault on a model the channel already serves counts one failure
//     and only asks for removal once the configured threshold is reached, which
//     is what forces a second run — one retry delay later — before a model goes.
func (r *upstreamModelUpdateRunContext) classifyModelValidationResult(
	channelID int,
	modelName string,
	result testResult,
	isCandidate bool,
	isExisting bool,
	health map[string]dto.ModelHealthState,
	now int64,
	failureThreshold int,
) (approveAdd bool, remove bool) {
	if result.newAPIError == nil && result.localErr == nil {
		delete(health, modelName)
		return isCandidate, false
	}

	// Two independent kinds of evidence count against a model:
	//
	//   - the channel itself faulted (IsChannelFaultError), the original rule;
	//   - the upstream said it does not have this model (isModelUnavailableError).
	//
	// The second exists because IsChannelFaultError answers a different question —
	// "should this channel be auto-disabled" — and its default status-code allowlist
	// is 401 alone. A 404 "model not supported" therefore never counted, so
	// state.Failures never advanced, removal never reached its threshold, and dead
	// model names accumulated on every channel indefinitely.
	//
	// Gated separately and off by default: correcting this changes what an existing
	// deployment does without the operator changing any setting.
	modelUnavailable := r.monitor.UpstreamModelUpdateRemoveUnavailableModels &&
		isModelUnavailableError(result.newAPIError, result.localErr)

	if !modelUnavailable && !service.IsChannelFaultError(result.newAPIError) {
		// Rate limited or otherwise transient: leave the model untouched. A new
		// candidate simply stays staged and is retried next run.
		if isCandidate {
			r.mu.Lock()
			r.rejectedModels++
			r.mu.Unlock()
		}
		common.SysLog(fmt.Sprintf(
			"upstream model validation inconclusive (not a channel fault): channel_id=%d model=%s err=%v",
			channelID, modelName, result.newAPIError,
		))
		return false, false
	}

	if isCandidate {
		r.mu.Lock()
		r.rejectedModels++
		r.mu.Unlock()
	}
	if !isExisting {
		// A brand new model that failed is just not added; tracking health for a
		// model the channel does not serve would grow the map for nothing.
		common.SysLog(fmt.Sprintf(
			"upstream model candidate rejected: channel_id=%d model=%s err=%v",
			channelID, modelName, result.newAPIError,
		))
		return false, false
	}

	state := health[modelName]
	state.Failures++
	state.LastFailureTime = now
	if result.newAPIError != nil {
		state.LastError = common.LocalLogPreview(result.newAPIError.Error())
		if len(state.LastError) > channelUpstreamModelUpdateMaxHealthErrorLength {
			state.LastError = state.LastError[:channelUpstreamModelUpdateMaxHealthErrorLength]
		}
	}
	health[modelName] = state

	// The reason is logged because the two paths carry different confidence: a
	// channel fault may clear on its own, while an upstream 404 naming the model is
	// a statement about its catalogue. An operator reviewing a deletion needs to
	// know which one drove it.
	reason := "channel_fault"
	if modelUnavailable {
		reason = "model_unavailable"
	}
	common.SysLog(fmt.Sprintf(
		"upstream model validation failed: channel_id=%d model=%s reason=%s failures=%d threshold=%d err=%v",
		channelID, modelName, reason, state.Failures, failureThreshold, result.newAPIError,
	))

	return false, r.monitor.UpstreamModelUpdateRemoveFailed && state.Failures >= failureThreshold
}

// pruneChannelModelHealth drops health entries for models the channel no longer
// serves and caps the map size, keeping the most recent failures.
func pruneChannelModelHealth(health map[string]dto.ModelHealthState, knownModels []string) {
	if len(health) == 0 {
		return
	}
	known := make(map[string]struct{}, len(knownModels))
	for _, modelName := range normalizeModelNames(knownModels) {
		known[modelName] = struct{}{}
	}
	for modelName := range health {
		if _, ok := known[modelName]; !ok {
			delete(health, modelName)
		}
	}
	if len(health) <= channelUpstreamModelUpdateMaxHealthEntries {
		return
	}
	names := lo.Keys(health)
	slices.SortFunc(names, func(a, b string) int {
		// Newest failure first, so the oldest entries are the ones dropped.
		return int(health[b].LastFailureTime - health[a].LastFailureTime)
	})
	for _, modelName := range names[channelUpstreamModelUpdateMaxHealthEntries:] {
		delete(health, modelName)
	}
}

func checkAndPersistChannelUpstreamModelUpdates(
	channel *model.Channel,
	settings *dto.ChannelOtherSettings,
	force bool,
	allowAutoApply bool,
	run *upstreamModelUpdateRunContext,
) (modelsChanged bool, autoAdded int, autoRemoved int, err error) {
	now := common.GetTimestamp()
	if !force {
		minInterval := getUpstreamModelUpdateMinCheckIntervalSeconds()
		if settings.UpstreamModelUpdateLastCheckTime > 0 &&
			now-settings.UpstreamModelUpdateLastCheckTime < minInterval {
			return false, 0, 0, nil
		}
	}

	pendingAddModels, pendingRemoveModels, pendingModelMapping, fetchErr := collectPendingUpstreamModelChanges(channel, *settings)
	settings.UpstreamModelUpdateLastCheckTime = now
	if fetchErr != nil {
		if err = updateChannelUpstreamModelSettings(channel, *settings, false, nil); err != nil {
			return false, 0, 0, err
		}
		return false, 0, 0, fetchErr
	}

	// The channel's own auto-sync flag comes first; the global switch fills in
	// for channels that never set one.
	autoSyncEnabled := settings.UpstreamModelUpdateAutoSyncEnabled || (run != nil && run.autoSyncAll)
	var failedModels []string
	var modelMapping *string
	if allowAutoApply && autoSyncEnabled && run.validationEnabled() {
		// Probe through the mapping the candidates will be adopted with, so a
		// bare gpt-4o is tested as the upstream's openai/gpt-4o and not as a
		// name the upstream has never heard of.
		storedMapping := channel.ModelMapping
		if probeMapping, ok := mergeChannelModelMapping(channel, pendingModelMapping); ok {
			channel.ModelMapping = probeMapping
		}
		pendingAddModels, failedModels = run.validateChannelModels(channel, settings, pendingAddModels)
		channel.ModelMapping = storedMapping
	}

	if allowAutoApply && autoSyncEnabled && (len(pendingAddModels) > 0 || len(failedModels) > 0) {
		originModels := normalizeModelNames(channel.GetModels())
		targetModels := originModels
		if len(pendingAddModels) > 0 {
			targetModels = mergeModelNames(targetModels, pendingAddModels)
			modelMapping, _ = mergeChannelModelMapping(channel, pendingModelMappingFor(pendingModelMapping, pendingAddModels))
		}
		autoAdded = len(targetModels) - len(originModels)
		if len(failedModels) > 0 {
			beforeRemoval := len(targetModels)
			targetModels = subtractModelNames(targetModels, failedModels)
			autoRemoved = beforeRemoval - len(targetModels)
		}
		if !slices.Equal(targetModels, originModels) {
			channel.Models = strings.Join(targetModels, ",")
			modelsChanged = true
		}
		settings.UpstreamModelUpdateLastDetectedModels = []string{}
		settings.UpstreamModelUpdatePendingModelMapping = nil
	} else {
		settings.UpstreamModelUpdateLastDetectedModels = pendingAddModels
		settings.UpstreamModelUpdatePendingModelMapping = pendingModelMappingFor(pendingModelMapping, pendingAddModels)
	}
	settings.UpstreamModelUpdateLastRemovedModels = pendingRemoveModels

	if err = updateChannelUpstreamModelSettings(channel, *settings, modelsChanged, modelMapping); err != nil {
		return modelsChanged, autoAdded, autoRemoved, err
	}
	if modelsChanged {
		channelUpstreamModelPersistMu.Lock()
		err = channel.UpdateAbilities(nil)
		channelUpstreamModelPersistMu.Unlock()
		if err != nil {
			return true, autoAdded, autoRemoved, err
		}
	}
	return modelsChanged, autoAdded, autoRemoved, nil
}

func refreshChannelRuntimeCache() {
	if common.MemoryCacheEnabled {
		func() {
			defer func() {
				if r := recover(); r != nil {
					common.SysLog(fmt.Sprintf("InitChannelCache panic: %v", r))
				}
			}()
			model.InitChannelCache()
		}()
	}
	service.ResetProxyClientCache()
}

func shouldSendUpstreamModelUpdateNotification(now int64, changedChannels int, failedChannels int) bool {
	if changedChannels <= 0 && failedChannels <= 0 {
		return true
	}

	channelUpstreamModelUpdateNotifyState.Lock()
	defer channelUpstreamModelUpdateNotifyState.Unlock()

	if channelUpstreamModelUpdateNotifyState.lastNotifiedAt > 0 &&
		now-channelUpstreamModelUpdateNotifyState.lastNotifiedAt < channelUpstreamModelUpdateNotifySuppressWindowSeconds &&
		channelUpstreamModelUpdateNotifyState.lastChangedChannels == changedChannels &&
		channelUpstreamModelUpdateNotifyState.lastFailedChannels == failedChannels {
		return false
	}

	channelUpstreamModelUpdateNotifyState.lastNotifiedAt = now
	channelUpstreamModelUpdateNotifyState.lastChangedChannels = changedChannels
	channelUpstreamModelUpdateNotifyState.lastFailedChannels = failedChannels
	return true
}

func buildUpstreamModelUpdateTaskNotificationContent(
	checkedChannels int,
	changedChannels int,
	detectedAddModels int,
	detectedRemoveModels int,
	autoAddedModels int,
	failedChannelIDs []int,
	channelSummaries []upstreamModelUpdateChannelSummary,
	addModelSamples []string,
	removeModelSamples []string,
) string {
	var builder strings.Builder
	failedChannels := len(failedChannelIDs)
	builder.WriteString(fmt.Sprintf(
		"上游模型巡检摘要：检测渠道 %d 个，发现变更 %d 个，新增 %d 个，删除 %d 个，自动同步新增 %d 个，失败 %d 个。",
		checkedChannels,
		changedChannels,
		detectedAddModels,
		detectedRemoveModels,
		autoAddedModels,
		failedChannels,
	))

	if len(channelSummaries) > 0 {
		displayCount := min(len(channelSummaries), channelUpstreamModelUpdateNotifyMaxChannelDetails)
		builder.WriteString(fmt.Sprintf("\n\n变更渠道明细（展示 %d/%d）：", displayCount, len(channelSummaries)))
		for _, summary := range channelSummaries[:displayCount] {
			builder.WriteString(fmt.Sprintf("\n- %s (+%d / -%d)", summary.ChannelName, summary.AddCount, summary.RemoveCount))
		}
		if len(channelSummaries) > displayCount {
			builder.WriteString(fmt.Sprintf("\n- 其余 %d 个渠道已省略", len(channelSummaries)-displayCount))
		}
	}

	normalizedAddModelSamples := normalizeModelNames(addModelSamples)
	if len(normalizedAddModelSamples) > 0 {
		displayCount := min(len(normalizedAddModelSamples), channelUpstreamModelUpdateNotifyMaxModelDetails)
		builder.WriteString(fmt.Sprintf("\n\n新增模型示例（展示 %d/%d）：%s",
			displayCount,
			len(normalizedAddModelSamples),
			strings.Join(normalizedAddModelSamples[:displayCount], ", "),
		))
		if len(normalizedAddModelSamples) > displayCount {
			builder.WriteString(fmt.Sprintf("（其余 %d 个已省略）", len(normalizedAddModelSamples)-displayCount))
		}
	}

	normalizedRemoveModelSamples := normalizeModelNames(removeModelSamples)
	if len(normalizedRemoveModelSamples) > 0 {
		displayCount := min(len(normalizedRemoveModelSamples), channelUpstreamModelUpdateNotifyMaxModelDetails)
		builder.WriteString(fmt.Sprintf("\n\n删除模型示例（展示 %d/%d）：%s",
			displayCount,
			len(normalizedRemoveModelSamples),
			strings.Join(normalizedRemoveModelSamples[:displayCount], ", "),
		))
		if len(normalizedRemoveModelSamples) > displayCount {
			builder.WriteString(fmt.Sprintf("（其余 %d 个已省略）", len(normalizedRemoveModelSamples)-displayCount))
		}
	}

	if failedChannels > 0 {
		displayCount := min(failedChannels, channelUpstreamModelUpdateNotifyMaxFailedChannelIDs)
		displayIDs := lo.Map(failedChannelIDs[:displayCount], func(channelID int, _ int) string {
			return fmt.Sprintf("%d", channelID)
		})
		builder.WriteString(fmt.Sprintf(
			"\n\n失败渠道 ID（展示 %d/%d）：%s",
			displayCount,
			failedChannels,
			strings.Join(displayIDs, ", "),
		))
		if failedChannels > displayCount {
			builder.WriteString(fmt.Sprintf("（其余 %d 个已省略）", failedChannels-displayCount))
		}
	}
	return builder.String()
}

type upstreamModelUpdateSummary struct {
	CheckedChannels      int `json:"checked_channels"`
	ChangedChannels      int `json:"changed_channels"`
	DetectedAddModels    int `json:"detected_add_models"`
	DetectedRemoveModels int `json:"detected_remove_models"`
	FailedChannels       int `json:"failed_channels"`
	AutoAddedModels      int `json:"auto_added_models"`
	// Validation counters are zero unless the run performed real-request
	// validation, which makes it visible in the task history whether models were
	// adopted on trust or actually confirmed.
	ValidatedModels     int `json:"validated_models"`
	RejectedModels      int `json:"rejected_models"`
	RemovedFailedModels int `json:"removed_failed_models"`
	// ScanError is set when the channel scan query itself failed. Without it a
	// broken query looks identical in task history to "nothing needed checking":
	// the run reported succeeded with every counter at zero, which is how a
	// column-name mistake stayed invisible for a day of scheduled runs.
	ScanError string `json:"scan_error,omitempty"`
}

// runChannelUpstreamModelUpdateTaskOnce runs one synchronous upstream model
// detection cycle and returns a summary for system task history. It honors ctx
// cancellation between batches so a runner that loses its lease stops promptly.
// force bypasses the per-channel minimum check interval and allowAutoApply lets
// channels adopt detected models automatically. The scheduled job calls
// (force=false, allowAutoApply=true); the manual "detect all" trigger calls
// (force=true, allowAutoApply=false) so it always re-checks and only stages
// changes for explicit review.
//
// When monitor_setting has the upstream model auto-update switch on and the run
// is allowed to apply, each candidate model is confirmed with a real request
// before being added and failing models are removed once they exceed the
// configured failure threshold. With the switch off, only channels whose own
// auto-sync flag is on adopt anything, and their candidates are still validated
// before being added; nothing is removed on that path.
func runChannelUpstreamModelUpdateTaskOnce(ctx context.Context, force bool, allowAutoApply bool, report func(processed, total int)) upstreamModelUpdateSummary {
	checkedChannels := 0
	failedChannels := 0
	failedChannelIDs := make([]int, 0)
	changedChannels := 0
	detectedAddModels := 0
	detectedRemoveModels := 0
	autoAddedModels := 0
	channelSummaries := make([]upstreamModelUpdateChannelSummary, 0)
	addModelSamples := make([]string, 0)
	removeModelSamples := make([]string, 0)
	refreshNeeded := false
	scanErr := ""

	// Snapshot the settings once per run so every worker and every channel in
	// this scan applies one consistent policy even if an admin saves changes
	// mid-run.
	monitor := *operation_setting.GetMonitorSetting()
	scanAllChannels := monitor.UpstreamModelUpdateEnabled && monitor.UpstreamModelUpdateScanAllChannels
	// Built even with the global switch off: a channel whose own auto-sync flag
	// is on still adds models on this path, and those adds must be validated
	// like any other. The run then carries the policy but applies to no channel
	// of its own accord.
	var run *upstreamModelUpdateRunContext
	if allowAutoApply {
		run = newUpstreamModelUpdateRunContext(ctx, nil, &monitor, false)
	}

	// Count the enabled channels up front so progress can be reported as a
	// percentage; a count error is non-fatal (progress just won't show a %).
	var totalChannels int64
	if err := model.DB.Model(&model.Channel{}).Where("status = ?", common.ChannelStatusEnabled).Count(&totalChannels).Error; err != nil {
		totalChannels = 0
	}
	processed := 0

	// Channels talk to independent upstreams, so a bounded worker pool is safe;
	// a configured REQUEST_INTERVAL promises serial pacing across every upstream
	// call the task makes, which only holds with a single worker.
	concurrency := getUpstreamModelUpdateTaskConcurrency()
	if common.RequestInterval > 0 {
		concurrency = 1
	}
	// mu guards the summary accumulators shared by the workers.
	var mu sync.Mutex

	lastID := 0
	for {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		var channels []*model.Channel
		query := model.DB.
			Select(channelUpstreamModelUpdateSelectFields).
			Where("status = ?", common.ChannelStatusEnabled).
			Order("id asc").
			Limit(channelUpstreamModelUpdateTaskBatchSize)
		if lastID > 0 {
			query = query.Where("id > ?", lastID)
		}
		err := query.Find(&channels).Error
		if err != nil {
			scanErr = err.Error()
			common.SysLog(fmt.Sprintf("upstream model update task query failed: %v", err))
			break
		}
		if len(channels) == 0 {
			break
		}
		lastID = channels[len(channels)-1].Id

		var wg sync.WaitGroup
		sem := make(chan struct{}, concurrency)
		for _, channel := range channels {
			if channel == nil {
				continue
			}
			if ctx != nil && ctx.Err() != nil {
				break
			}

			processed++
			if report != nil {
				report(processed, int(totalChannels))
			}

			wg.Add(1)
			sem <- struct{}{}
			go func(channel *model.Channel) {
				defer wg.Done()
				// The semaphore slot is released only after the trailing
				// REQUEST_INTERVAL sleep so that single-worker pacing matches the
				// old serial loop exactly.
				defer func() { <-sem }()

				if ctx != nil && ctx.Err() != nil {
					return
				}

				settings := channel.GetOtherSettings()
				if !settings.UpstreamModelUpdateCheckEnabled && !scanAllChannels {
					return
				}

				mu.Lock()
				checkedChannels++
				mu.Unlock()

				modelsChanged, autoAdded, autoRemoved, err := checkAndPersistChannelUpstreamModelUpdates(channel, &settings, force, allowAutoApply, run)

				mu.Lock()
				if err != nil {
					failedChannels++
					failedChannelIDs = append(failedChannelIDs, channel.Id)
					common.SysLog(fmt.Sprintf("upstream model update check failed: channel_id=%d channel_name=%s err=%v", channel.Id, channel.Name, err))
				} else {
					currentAddModels := normalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels)
					currentRemoveModels := normalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels)
					currentAddCount := len(currentAddModels) + autoAdded
					currentRemoveCount := len(currentRemoveModels) + autoRemoved
					detectedAddModels += currentAddCount
					detectedRemoveModels += currentRemoveCount
					if currentAddCount > 0 || currentRemoveCount > 0 {
						changedChannels++
						channelSummaries = append(channelSummaries, upstreamModelUpdateChannelSummary{
							ChannelName: channel.Name,
							AddCount:    currentAddCount,
							RemoveCount: currentRemoveCount,
						})
					}
					addModelSamples = mergeModelNames(addModelSamples, currentAddModels)
					removeModelSamples = mergeModelNames(removeModelSamples, currentRemoveModels)
					if modelsChanged {
						refreshNeeded = true
					}
					autoAddedModels += autoAdded
				}
				mu.Unlock()

				if common.RequestInterval > 0 {
					if ctx == nil {
						time.Sleep(common.RequestInterval)
					} else {
						select {
						case <-ctx.Done():
						case <-time.After(common.RequestInterval):
						}
					}
				}
			}(channel)
		}
		wg.Wait()

		if ctx != nil && ctx.Err() != nil {
			break
		}
		if len(channels) < channelUpstreamModelUpdateTaskBatchSize {
			break
		}
	}

	if report != nil && (ctx == nil || ctx.Err() == nil) {
		report(int(totalChannels), int(totalChannels)) // mark complete only when the full scan finished
	}

	if refreshNeeded {
		refreshChannelRuntimeCache()
	}

	summary := upstreamModelUpdateSummary{
		CheckedChannels:      checkedChannels,
		ChangedChannels:      changedChannels,
		DetectedAddModels:    detectedAddModels,
		DetectedRemoveModels: detectedRemoveModels,
		FailedChannels:       failedChannels,
		AutoAddedModels:      autoAddedModels,
		ScanError:            scanErr,
	}
	if run != nil {
		summary.ValidatedModels = run.validatedModels
		summary.RejectedModels = run.rejectedModels
		summary.RemovedFailedModels = run.removedFailedModels
	}

	if checkedChannels > 0 || common.DebugEnabled {
		common.SysLog(fmt.Sprintf(
			"upstream model update task done: checked_channels=%d changed_channels=%d detected_add_models=%d detected_remove_models=%d failed_channels=%d auto_added_models=%d validated_models=%d rejected_models=%d removed_failed_models=%d",
			checkedChannels,
			changedChannels,
			detectedAddModels,
			detectedRemoveModels,
			failedChannels,
			autoAddedModels,
			summary.ValidatedModels,
			summary.RejectedModels,
			summary.RemovedFailedModels,
		))
	}
	if changedChannels > 0 || failedChannels > 0 {
		now := common.GetTimestamp()
		if !shouldSendUpstreamModelUpdateNotification(now, changedChannels, failedChannels) {
			common.SysLog(fmt.Sprintf(
				"upstream model update notification skipped in 24h window: changed_channels=%d failed_channels=%d",
				changedChannels,
				failedChannels,
			))
			return summary
		}
		service.NotifyUpstreamModelUpdateWatchers(
			"上游模型巡检通知",
			buildUpstreamModelUpdateTaskNotificationContent(
				checkedChannels,
				changedChannels,
				detectedAddModels,
				detectedRemoveModels,
				autoAddedModels,
				failedChannelIDs,
				channelSummaries,
				addModelSamples,
				removeModelSamples,
			),
		)
	}
	return summary
}

func ApplyChannelUpstreamModelUpdates(c *gin.Context) {
	var req applyChannelUpstreamModelUpdatesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
		return
	}
	if req.ID <= 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "invalid channel id",
		})
		return
	}

	channel, err := model.GetChannelById(req.ID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	beforeSettings := channel.GetOtherSettings()
	ignoredModels := intersectModelNames(req.IgnoreModels, beforeSettings.UpstreamModelUpdateLastDetectedModels)

	monitor := *operation_setting.GetMonitorSetting()
	run := newUpstreamModelUpdateRunContext(c.Request.Context(), c, &monitor, true)
	applied, err := applyChannelUpstreamModelUpdates(
		channel,
		req.AddModels,
		req.IgnoreModels,
		req.RemoveModels,
		run,
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	addedModels, removedModels := applied.addedModels, applied.removedModels
	remainingModels, remainingRemoveModels, modelsChanged := applied.remainingModels, applied.remainingRemoveModels, applied.modelsChanged

	if modelsChanged {
		refreshChannelRuntimeCache()
	}

	recordManageAudit(c, "channel.upstream_apply", map[string]interface{}{
		"id": channel.Id,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"id":                      channel.Id,
			"added_models":            addedModels,
			"rejected_models":         applied.rejectedModels,
			"removed_models":          removedModels,
			"ignored_models":          ignoredModels,
			"remaining_models":        remainingModels,
			"remaining_remove_models": remainingRemoveModels,
			"models":                  channel.Models,
			"settings":                channel.OtherSettings,
		},
	})
}

func DetectChannelUpstreamModelUpdates(c *gin.Context) {
	var req applyChannelUpstreamModelUpdatesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
		return
	}
	if req.ID <= 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "invalid channel id",
		})
		return
	}

	channel, err := model.GetChannelById(req.ID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	settings := channel.GetOtherSettings()
	modelsChanged, autoAdded, _, err := checkAndPersistChannelUpstreamModelUpdates(channel, &settings, true, false, nil)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if modelsChanged {
		refreshChannelRuntimeCache()
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": detectChannelUpstreamModelUpdatesResult{
			ChannelID:       channel.Id,
			ChannelName:     channel.Name,
			AddModels:       normalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels),
			RemoveModels:    normalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels),
			LastCheckTime:   settings.UpstreamModelUpdateLastCheckTime,
			AutoAddedModels: autoAdded,
		},
	})
}

type appliedChannelUpstreamModelUpdates struct {
	addedModels []string
	// rejectedModels were selected for adding but failed the validation request;
	// they stay staged so the admin can see them and retry.
	rejectedModels        []string
	removedModels         []string
	remainingModels       []string
	remainingRemoveModels []string
	modelsChanged         bool
}

// applyChannelUpstreamModelUpdates carries out an admin's selection from the
// staged diff. Selected additions go through the same validation as the
// scheduled scan when run allows it: a manual "add all" used to be the one path
// that adopted upstream ids on trust, which is how models nobody could use ended
// up on channels.
func applyChannelUpstreamModelUpdates(
	channel *model.Channel,
	addModelsInput []string,
	ignoreModelsInput []string,
	removeModelsInput []string,
	run *upstreamModelUpdateRunContext,
) (appliedChannelUpstreamModelUpdates, error) {
	var applied appliedChannelUpstreamModelUpdates
	settings := channel.GetOtherSettings()
	pendingAddModels := normalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels)
	pendingRemoveModels := normalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels)
	addModels := intersectModelNames(addModelsInput, pendingAddModels)
	ignoreModels := intersectModelNames(ignoreModelsInput, pendingAddModels)
	removeModels := intersectModelNames(removeModelsInput, pendingRemoveModels)
	removeModels = subtractModelNames(removeModels, addModels)

	if len(addModels) > 0 && run.validationEnabled() {
		storedMapping := channel.ModelMapping
		if probeMapping, ok := mergeChannelModelMapping(channel, pendingModelMappingFor(settings.UpstreamModelUpdatePendingModelMapping, addModels)); ok {
			channel.ModelMapping = probeMapping
		}
		approved, _ := run.validateChannelModels(channel, &settings, addModels)
		channel.ModelMapping = storedMapping
		applied.rejectedModels = subtractModelNames(addModels, approved)
		addModels = approved
	}

	originModels := normalizeModelNames(channel.GetModels())
	nextModels := applySelectedModelChanges(originModels, addModels, removeModels)
	applied.modelsChanged = !slices.Equal(originModels, nextModels)
	var modelMapping *string
	if applied.modelsChanged {
		channel.Models = strings.Join(nextModels, ",")
		modelMapping, _ = mergeChannelModelMapping(channel, pendingModelMappingFor(settings.UpstreamModelUpdatePendingModelMapping, addModels))
	}

	settings.UpstreamModelUpdateIgnoredModels = mergeModelNames(settings.UpstreamModelUpdateIgnoredModels, ignoreModels)
	if len(addModels) > 0 {
		settings.UpstreamModelUpdateIgnoredModels = subtractModelNames(settings.UpstreamModelUpdateIgnoredModels, addModels)
	}
	applied.remainingModels = subtractModelNames(pendingAddModels, append(addModels, ignoreModels...))
	applied.remainingRemoveModels = subtractModelNames(pendingRemoveModels, removeModels)
	settings.UpstreamModelUpdateLastDetectedModels = applied.remainingModels
	settings.UpstreamModelUpdatePendingModelMapping = pendingModelMappingFor(settings.UpstreamModelUpdatePendingModelMapping, applied.remainingModels)
	settings.UpstreamModelUpdateLastRemovedModels = applied.remainingRemoveModels
	settings.UpstreamModelUpdateLastCheckTime = common.GetTimestamp()
	applied.addedModels = addModels
	applied.removedModels = removeModels

	if err := updateChannelUpstreamModelSettings(channel, settings, applied.modelsChanged, modelMapping); err != nil {
		return appliedChannelUpstreamModelUpdates{}, err
	}

	if applied.modelsChanged {
		// Same lock as the scheduled scan: a rebuild is delete-then-insert of
		// every ability row, and interleaving two of them for one channel leaves
		// the union of both model sets behind.
		channelUpstreamModelPersistMu.Lock()
		err := channel.UpdateAbilities(nil)
		channelUpstreamModelPersistMu.Unlock()
		if err != nil {
			return applied, err
		}
	}
	return applied, nil
}

func collectPendingApplyUpstreamModelChanges(settings dto.ChannelOtherSettings) (pendingAddModels []string, pendingRemoveModels []string) {
	return normalizeModelNames(settings.UpstreamModelUpdateLastDetectedModels), normalizeModelNames(settings.UpstreamModelUpdateLastRemovedModels)
}

func findEnabledChannelsAfterID(lastID int, batchSize int) ([]*model.Channel, error) {
	var channels []*model.Channel
	query := model.DB.
		Select(channelUpstreamModelUpdateSelectFields).
		Where("status = ?", common.ChannelStatusEnabled).
		Order("id asc").
		Limit(batchSize)
	if lastID > 0 {
		query = query.Where("id > ?", lastID)
	}
	return channels, query.Find(&channels).Error
}

func ApplyAllChannelUpstreamModelUpdates(c *gin.Context) {
	results := make([]applyAllChannelUpstreamModelUpdatesResult, 0)
	failed := make([]int, 0)
	refreshNeeded := false
	addedModelCount := 0
	rejectedModelCount := 0
	removedModelCount := 0

	// One run for the whole batch so the validation budget is shared across
	// channels exactly as it is in the scheduled scan.
	monitor := *operation_setting.GetMonitorSetting()
	run := newUpstreamModelUpdateRunContext(c.Request.Context(), c, &monitor, true)

	lastID := 0
	for {
		channels, err := findEnabledChannelsAfterID(lastID, channelUpstreamModelUpdateTaskBatchSize)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		if len(channels) == 0 {
			break
		}
		lastID = channels[len(channels)-1].Id

		for _, channel := range channels {
			if channel == nil {
				continue
			}

			settings := channel.GetOtherSettings()
			if !settings.UpstreamModelUpdateCheckEnabled {
				continue
			}

			pendingAddModels, pendingRemoveModels := collectPendingApplyUpstreamModelChanges(settings)
			if len(pendingAddModels) == 0 && len(pendingRemoveModels) == 0 {
				continue
			}

			applied, err := applyChannelUpstreamModelUpdates(
				channel,
				pendingAddModels,
				nil,
				pendingRemoveModels,
				run,
			)
			if err != nil {
				failed = append(failed, channel.Id)
				continue
			}
			if applied.modelsChanged {
				refreshNeeded = true
			}
			addedModelCount += len(applied.addedModels)
			rejectedModelCount += len(applied.rejectedModels)
			removedModelCount += len(applied.removedModels)
			results = append(results, applyAllChannelUpstreamModelUpdatesResult{
				ChannelID:             channel.Id,
				ChannelName:           channel.Name,
				AddedModels:           applied.addedModels,
				RejectedModels:        applied.rejectedModels,
				RemovedModels:         applied.removedModels,
				RemainingModels:       applied.remainingModels,
				RemainingRemoveModels: applied.remainingRemoveModels,
			})
		}

		if len(channels) < channelUpstreamModelUpdateTaskBatchSize {
			break
		}
	}

	if refreshNeeded {
		refreshChannelRuntimeCache()
	}

	recordManageAudit(c, "channel.upstream_apply_all", map[string]interface{}{
		"count": len(results),
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"processed_channels": len(results),
			"added_models":       addedModelCount,
			"rejected_models":    rejectedModelCount,
			"removed_models":     removedModelCount,
			"failed_channel_ids": failed,
			"results":            results,
		},
	})
}

// DetectAllChannelUpstreamModelUpdates enqueues a model_update system task
// (manual variant) instead of scanning inline. Routing the manual trigger
// through the framework gives it the same cross-instance lease dedup and run
// history as the scheduled scan. If any model_update task is already active, the
// manual run is rejected so the caller does not mistake a scheduled run for this
// manual one.
//
// An optional {"auto_apply": true} body turns this into "run the scheduled
// auto-update now": validate candidates with real requests and apply the result
// instead of only staging it. The body is optional so existing callers that send
// nothing keep the detect-only behavior.
func DetectAllChannelUpstreamModelUpdates(c *gin.Context) {
	var req struct {
		AutoApply bool `json:"auto_apply"`
		FullSweep bool `json:"full_sweep"`
	}
	if c.Request != nil && c.Request.Body != nil {
		// A missing or malformed body is not an error here: detect-only is the
		// documented default and the legacy channels page sends no body at all.
		_ = c.ShouldBindJSON(&req)
	}

	task, created, err := service.EnqueueSystemTask(model.SystemTaskTypeModelUpdate, modelUpdateTaskPayload{
		Manual:    true,
		AutoApply: req.AutoApply,
		FullSweep: req.FullSweep,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !created {
		c.JSON(http.StatusConflict, gin.H{
			"success": false,
			"message": "已有模型更新任务正在运行或等待中，不能启动本次手动任务",
			"data": gin.H{
				"task_id": task.TaskID,
				"status":  task.Status,
				"type":    task.Type,
			},
		})
		return
	}

	auditAction := "channel.upstream_detect_all"
	if req.FullSweep {
		auditAction = "channel.model_sweep_all"
	}
	recordManageAudit(c, auditAction, map[string]interface{}{
		"task_id": task.TaskID,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"task_id": task.TaskID,
			"status":  task.Status,
		},
	})
}
