package service

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
	"github.com/tidwall/gjson"
)

const (
	ginKeyChannelAffinityCacheKey   = "channel_affinity_cache_key"
	ginKeyChannelAffinityTTLSeconds = "channel_affinity_ttl_seconds"
	ginKeyChannelAffinityMeta       = "channel_affinity_meta"
	ginKeyChannelAffinityLogInfo    = "channel_affinity_log_info"
	ginKeyChannelAffinitySkipRetry  = "channel_affinity_skip_retry_on_failure"
	// ginKeyChannelAffinityRelayOK carries the relay handler's own verdict on the
	// request. The distributor decides affinity after c.Next() returns, when a
	// stream that failed mid-flight has already committed 200 to the wire.
	ginKeyChannelAffinityRelayOK = "channel_affinity_relay_succeeded"
	// ginKeyChannelAffinityBypassed marks that a healthy pinned channel was skipped
	// only because it was at its concurrency limit, so the request served on a
	// different channel must not repin the key to it.
	ginKeyChannelAffinityBypassed = "channel_affinity_bypassed"
	// ginKeyChannelAffinityPinnedChannel records which channel the pin actually
	// resolved to, so a failure can be attributed to the pinned channel rather than
	// to a fallback the retry loop moved on to.
	ginKeyChannelAffinityPinnedChannel = "channel_affinity_pinned_channel"
	// ginKeyChannelAffinityFaultChannel records the channel a genuine upstream
	// fault was attributed to. Only a fault on the pinned channel retracts the pin;
	// a rate limit, a client error, or a fault on some other channel must not.
	//
	// It holds a SET of channel ids, not a single id. The retry loop calls the
	// marker once per failed attempt, so a single slot let a later fallback's fault
	// overwrite the pinned channel's own: pin A faults, the retry to B faults too,
	// and the release check then compared B against A, decided the pinned channel
	// was fine and kept the pin forever. That is the same "every channel failed but
	// it stays on the old one" symptom the pin was supposed to stop having.
	ginKeyChannelAffinityFaultChannel = "channel_affinity_fault_channels"

	channelAffinityCacheNamespace           = "new-api:channel_affinity:v1"
	channelAffinityUsageCacheStatsNamespace = "new-api:channel_affinity_usage_cache_stats:v1"
)

var (
	channelAffinityCacheOnce sync.Once
	channelAffinityCache     *cachex.HybridCache[int]

	channelAffinityUsageCacheStatsOnce  sync.Once
	channelAffinityUsageCacheStatsCache *cachex.HybridCache[ChannelAffinityUsageCacheCounters]

	channelAffinityRegexCache sync.Map // map[string]*regexp.Regexp

	// channelAffinityEmptyModelRegexLogged keeps the rejection of a rule with an
	// empty model_regex down to one line per rule name per process.
	channelAffinityEmptyModelRegexLogged sync.Map // map[string]struct{}
)

type channelAffinityMeta struct {
	CacheKey       string
	TTLSeconds     int
	RuleName       string
	SkipRetry      bool
	ParamTemplate  map[string]interface{}
	KeySourceType  string
	KeySourceKey   string
	KeySourcePath  string
	KeyHint        string
	KeyFingerprint string
	UsingGroup     string
	ModelName      string
	RequestPath    string
}

type ChannelAffinityStatsContext struct {
	RuleName       string
	UsingGroup     string
	KeyFingerprint string
	TTLSeconds     int64
}

const (
	cacheTokenRateModeCachedOverPrompt           = "cached_over_prompt"
	cacheTokenRateModeCachedOverPromptPlusCached = "cached_over_prompt_plus_cached"
	cacheTokenRateModeMixed                      = "mixed"
)

type ChannelAffinityCacheStats struct {
	Enabled       bool           `json:"enabled"`
	Total         int            `json:"total"`
	Unknown       int            `json:"unknown"`
	ByRuleName    map[string]int `json:"by_rule_name"`
	CacheCapacity int            `json:"cache_capacity"`
	CacheAlgo     string         `json:"cache_algo"`
}

func getChannelAffinityCache() *cachex.HybridCache[int] {
	channelAffinityCacheOnce.Do(func() {
		setting := operation_setting.GetChannelAffinitySetting()
		capacity := setting.MaxEntries
		if capacity <= 0 {
			capacity = 100_000
		}
		defaultTTLSeconds := setting.DefaultTTLSeconds
		if defaultTTLSeconds <= 0 {
			defaultTTLSeconds = 3600
		}

		channelAffinityCache = cachex.NewHybridCache[int](cachex.HybridCacheConfig[int]{
			Namespace: cachex.Namespace(channelAffinityCacheNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.IntCodec{},
			Memory: func() *hot.HotCache[string, int] {
				return hot.NewHotCache[string, int](hot.LRU, capacity).
					WithTTL(time.Duration(defaultTTLSeconds) * time.Second).
					WithJanitor().
					Build()
			},
		})
	})
	return channelAffinityCache
}

func GetChannelAffinityCacheStats() ChannelAffinityCacheStats {
	setting := operation_setting.GetChannelAffinitySetting()
	if setting == nil {
		return ChannelAffinityCacheStats{
			Enabled:    false,
			Total:      0,
			Unknown:    0,
			ByRuleName: map[string]int{},
		}
	}

	cache := getChannelAffinityCache()
	mainCap, _ := cache.Capacity()
	mainAlgo, _ := cache.Algorithm()

	rules := setting.Rules
	ruleByName := make(map[string]operation_setting.ChannelAffinityRule, len(rules))
	for _, r := range rules {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		if !r.IncludeRuleName {
			continue
		}
		ruleByName[name] = r
	}

	byRuleName := make(map[string]int, len(ruleByName))
	for name := range ruleByName {
		byRuleName[name] = 0
	}

	keys, err := cache.Keys()
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity cache list keys failed: err=%v", err))
		keys = nil
	}
	total := len(keys)
	unknown := 0
	for _, k := range keys {
		prefix := channelAffinityCacheNamespace + ":"
		if !strings.HasPrefix(k, prefix) {
			unknown++
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		parts := strings.Split(rest, ":")
		if len(parts) < 2 {
			unknown++
			continue
		}
		ruleName := parts[0]
		rule, ok := ruleByName[ruleName]
		if !ok {
			unknown++
			continue
		}
		if rule.IncludeModelName {
			if len(parts) < 3 {
				unknown++
				continue
			}
		}
		if rule.IncludeUsingGroup {
			minParts := 3
			if rule.IncludeModelName {
				minParts = 4
			}
			if len(parts) < minParts {
				unknown++
				continue
			}
		}
		byRuleName[ruleName]++
	}

	return ChannelAffinityCacheStats{
		Enabled:       setting.Enabled,
		Total:         total,
		Unknown:       unknown,
		ByRuleName:    byRuleName,
		CacheCapacity: mainCap,
		CacheAlgo:     mainAlgo,
	}
}

func ClearChannelAffinityCacheAll() int {
	cache := getChannelAffinityCache()
	keys, err := cache.Keys()
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity cache list keys failed: err=%v", err))
		keys = nil
	}
	if len(keys) > 0 {
		if _, err := cache.DeleteMany(keys); err != nil {
			common.SysError(fmt.Sprintf("channel affinity cache delete many failed: err=%v", err))
		}
	}
	return len(keys)
}

func ClearChannelAffinityCacheByRuleName(ruleName string) (int, error) {
	ruleName = strings.TrimSpace(ruleName)
	if ruleName == "" {
		return 0, fmt.Errorf("rule_name 不能为空")
	}

	setting := operation_setting.GetChannelAffinitySetting()
	if setting == nil {
		return 0, fmt.Errorf("channel_affinity_setting 未初始化")
	}

	var matchedRule *operation_setting.ChannelAffinityRule
	for i := range setting.Rules {
		r := &setting.Rules[i]
		if strings.TrimSpace(r.Name) != ruleName {
			continue
		}
		matchedRule = r
		break
	}
	if matchedRule == nil {
		return 0, fmt.Errorf("未知规则名称")
	}
	if !matchedRule.IncludeRuleName {
		return 0, fmt.Errorf("该规则未启用 include_rule_name，无法按规则清空缓存")
	}

	cache := getChannelAffinityCache()
	deleted, err := cache.DeleteByPrefix(ruleName)
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func matchAnyRegexCached(patterns []string, s string) bool {
	if len(patterns) == 0 || s == "" {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		re, ok := channelAffinityRegexCache.Load(pattern)
		if !ok {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				continue
			}
			re = compiled
			channelAffinityRegexCache.Store(pattern, re)
		}
		if re.(*regexp.Regexp).MatchString(s) {
			return true
		}
	}
	return false
}

func matchAnyIncludeFold(patterns []string, s string) bool {
	if len(patterns) == 0 || s == "" {
		return false
	}
	sLower := strings.ToLower(s)
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(sLower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func extractChannelAffinityValue(c *gin.Context, src operation_setting.ChannelAffinityKeySource) string {
	switch src.Type {
	case "context_int":
		if src.Key == "" {
			return ""
		}
		v := c.GetInt(src.Key)
		if v <= 0 {
			return ""
		}
		return strconv.Itoa(v)
	case "context_string":
		if src.Key == "" {
			return ""
		}
		return strings.TrimSpace(c.GetString(src.Key))
	case "request_header":
		if c == nil || c.Request == nil || src.Key == "" {
			return ""
		}
		return strings.TrimSpace(c.Request.Header.Get(src.Key))
	case "gjson":
		if src.Path == "" {
			return ""
		}
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return ""
		}
		body, err := storage.Bytes()
		if err != nil || len(body) == 0 {
			return ""
		}
		res := gjson.GetBytes(body, src.Path)
		if !res.Exists() {
			return ""
		}
		switch res.Type {
		case gjson.String, gjson.Number, gjson.True, gjson.False:
			return strings.TrimSpace(res.String())
		default:
			return strings.TrimSpace(res.Raw)
		}
	default:
		return ""
	}
}

func buildChannelAffinityCacheKeySuffix(rule operation_setting.ChannelAffinityRule, modelName string, usingGroup string, affinityValue string) string {
	parts := make([]string, 0, 4)
	// The rule name must be trimmed here exactly as GetChannelAffinityCacheStats
	// and ClearChannelAffinityCacheByRuleName trim it, otherwise a name with
	// surrounding whitespace produces keys that neither can attribute.
	if ruleName := strings.TrimSpace(rule.Name); rule.IncludeRuleName && ruleName != "" {
		parts = append(parts, ruleName)
	}
	if rule.IncludeModelName && modelName != "" {
		parts = append(parts, modelName)
	}
	if rule.IncludeUsingGroup && usingGroup != "" {
		parts = append(parts, usingGroup)
	}
	parts = append(parts, affinityValue)
	return strings.Join(parts, ":")
}

func setChannelAffinityContext(c *gin.Context, meta channelAffinityMeta) {
	c.Set(ginKeyChannelAffinityCacheKey, meta.CacheKey)
	c.Set(ginKeyChannelAffinityTTLSeconds, meta.TTLSeconds)
	c.Set(ginKeyChannelAffinityMeta, meta)
}

func getChannelAffinityContext(c *gin.Context) (string, int, bool) {
	keyAny, ok := c.Get(ginKeyChannelAffinityCacheKey)
	if !ok {
		return "", 0, false
	}
	key, ok := keyAny.(string)
	if !ok || key == "" {
		return "", 0, false
	}
	ttlAny, ok := c.Get(ginKeyChannelAffinityTTLSeconds)
	if !ok {
		return key, 0, true
	}
	ttlSeconds, _ := ttlAny.(int)
	return key, ttlSeconds, true
}

func getChannelAffinityMeta(c *gin.Context) (channelAffinityMeta, bool) {
	anyMeta, ok := c.Get(ginKeyChannelAffinityMeta)
	if !ok {
		return channelAffinityMeta{}, false
	}
	meta, ok := anyMeta.(channelAffinityMeta)
	if !ok {
		return channelAffinityMeta{}, false
	}
	return meta, true
}

func GetChannelAffinityStatsContext(c *gin.Context) (ChannelAffinityStatsContext, bool) {
	if c == nil {
		return ChannelAffinityStatsContext{}, false
	}
	meta, ok := getChannelAffinityMeta(c)
	if !ok {
		return ChannelAffinityStatsContext{}, false
	}
	ruleName := strings.TrimSpace(meta.RuleName)
	keyFp := strings.TrimSpace(meta.KeyFingerprint)
	usingGroup := strings.TrimSpace(meta.UsingGroup)
	if ruleName == "" || keyFp == "" {
		return ChannelAffinityStatsContext{}, false
	}
	ttlSeconds := int64(meta.TTLSeconds)
	if ttlSeconds <= 0 {
		return ChannelAffinityStatsContext{}, false
	}
	return ChannelAffinityStatsContext{
		RuleName:       ruleName,
		UsingGroup:     usingGroup,
		KeyFingerprint: keyFp,
		TTLSeconds:     ttlSeconds,
	}, true
}

func affinityFingerprint(s string) string {
	if s == "" {
		return ""
	}
	hex := common.Sha1([]byte(s))
	if len(hex) >= 8 {
		return hex[:8]
	}
	return hex
}

func buildChannelAffinityKeyHint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= 12 {
		return s
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func cloneStringAnyMap(src map[string]interface{}) map[string]interface{} {
	if len(src) == 0 {
		return map[string]interface{}{}
	}
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mergeChannelOverride(base map[string]interface{}, tpl map[string]interface{}) map[string]interface{} {
	if len(base) == 0 && len(tpl) == 0 {
		return map[string]interface{}{}
	}
	if len(tpl) == 0 {
		return base
	}
	out := cloneStringAnyMap(base)
	for k, v := range tpl {
		if strings.EqualFold(strings.TrimSpace(k), "operations") {
			baseOps, hasBaseOps := extractParamOperations(out[k])
			tplOps, hasTplOps := extractParamOperations(v)
			if hasTplOps {
				if hasBaseOps {
					out[k] = append(tplOps, baseOps...)
				} else {
					out[k] = tplOps
				}
				continue
			}
		}
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
	return out
}

func extractParamOperations(value interface{}) ([]interface{}, bool) {
	switch ops := value.(type) {
	case []interface{}:
		if len(ops) == 0 {
			return []interface{}{}, true
		}
		cloned := make([]interface{}, 0, len(ops))
		cloned = append(cloned, ops...)
		return cloned, true
	case []map[string]interface{}:
		cloned := make([]interface{}, 0, len(ops))
		for _, op := range ops {
			cloned = append(cloned, op)
		}
		return cloned, true
	default:
		return nil, false
	}
}

func appendChannelAffinityTemplateAdminInfo(c *gin.Context, meta channelAffinityMeta) {
	if c == nil {
		return
	}
	if len(meta.ParamTemplate) == 0 {
		return
	}

	info := channelAffinityLogInfo(c, meta)
	info["override_template"] = map[string]interface{}{
		"applied":             true,
		"rule_name":           meta.RuleName,
		"param_override_keys": len(meta.ParamTemplate),
	}
}

// ApplyChannelAffinityOverrideTemplate merges per-rule channel override templates onto the selected channel override config.
func ApplyChannelAffinityOverrideTemplate(c *gin.Context, paramOverride map[string]interface{}) (map[string]interface{}, bool) {
	if c == nil {
		return paramOverride, false
	}
	meta, ok := getChannelAffinityMeta(c)
	if !ok {
		return paramOverride, false
	}
	if len(meta.ParamTemplate) == 0 {
		return paramOverride, false
	}

	mergedParam := mergeChannelOverride(paramOverride, meta.ParamTemplate)
	appendChannelAffinityTemplateAdminInfo(c, meta)
	return mergedParam, true
}

func GetPreferredChannelByAffinity(c *gin.Context, modelName string, usingGroup string) (int, bool) {
	setting := operation_setting.GetChannelAffinitySetting()
	if setting == nil || !setting.Enabled {
		return 0, false
	}
	if c == nil {
		return 0, false
	}
	path := ""
	if c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	userAgent := ""
	if c.Request != nil {
		userAgent = c.Request.UserAgent()
	}

	for _, rule := range setting.Rules {
		// model_regex is the rule's primary selector and the rule editor requires
		// it, so an empty one can only arrive from a direct option API write.
		// Reading it as "match everything" (the way path_regex reads an empty
		// list) would turn a half-written rule into a gateway-wide affinity that
		// pins every model of every request onto one channel, so the rule is
		// rejected instead — but audibly, unlike the silent skip it used to get.
		if len(rule.ModelRegex) == 0 {
			if _, seen := channelAffinityEmptyModelRegexLogged.LoadOrStore(rule.Name, struct{}{}); !seen {
				logger.LogWarn(c, fmt.Sprintf("channel affinity rule %q ignored: model_regex is empty", rule.Name))
			}
			continue
		}
		if !matchAnyRegexCached(rule.ModelRegex, modelName) {
			continue
		}
		if len(rule.PathRegex) > 0 && !matchAnyRegexCached(rule.PathRegex, path) {
			continue
		}
		if len(rule.UserAgentInclude) > 0 && !matchAnyIncludeFold(rule.UserAgentInclude, userAgent) {
			continue
		}
		var affinityValue string
		var usedSource operation_setting.ChannelAffinityKeySource
		for _, src := range rule.KeySources {
			affinityValue = extractChannelAffinityValue(c, src)
			if affinityValue != "" {
				usedSource = src
				break
			}
		}
		if affinityValue == "" {
			continue
		}
		if rule.ValueRegex != "" && !matchAnyRegexCached([]string{rule.ValueRegex}, affinityValue) {
			continue
		}

		ttlSeconds := rule.TTLSeconds
		if ttlSeconds <= 0 {
			ttlSeconds = setting.DefaultTTLSeconds
		}
		cacheKeySuffix := buildChannelAffinityCacheKeySuffix(rule, modelName, usingGroup, affinityValue)
		cacheKeyFull := channelAffinityCacheNamespace + ":" + cacheKeySuffix
		meta := channelAffinityMeta{
			CacheKey:       cacheKeyFull,
			TTLSeconds:     ttlSeconds,
			RuleName:       strings.TrimSpace(rule.Name),
			SkipRetry:      rule.SkipRetryOnFailure,
			ParamTemplate:  cloneStringAnyMap(rule.ParamOverrideTemplate),
			KeySourceType:  strings.TrimSpace(usedSource.Type),
			KeySourceKey:   strings.TrimSpace(usedSource.Key),
			KeySourcePath:  strings.TrimSpace(usedSource.Path),
			KeyHint:        buildChannelAffinityKeyHint(affinityValue),
			KeyFingerprint: affinityFingerprint(affinityValue),
			UsingGroup:     usingGroup,
			ModelName:      modelName,
			RequestPath:    path,
		}
		setChannelAffinityContext(c, meta)

		// A matched rule is recorded before the lookup result is known, so a cold
		// cache ("matched, cache miss") stays distinguishable from a rule that
		// never matched at all (no channel_affinity entry in admin_info).
		info := channelAffinityLogInfo(c, meta)
		info["matched"] = true

		cache := getChannelAffinityCache()
		channelID, found, err := cache.Get(cacheKeySuffix)
		if err != nil {
			info["cache"] = "error"
			common.SysError(fmt.Sprintf("channel affinity cache get failed: key=%s, err=%v", cacheKeyFull, err))
			return 0, false
		}
		if found {
			info["cache"] = "hit"
			info["cached_channel_id"] = channelID
			// A hit is not by itself an answer. The cache key is built from the rule,
			// model, group and affinity value, so it does not change when an admin
			// edits priority — without this check the pin kept serving the old channel
			// until the TTL expired (an hour by default), which read as "raising a
			// channel's priority does nothing".
			//
			// Only the admin-configured priority is compared. Dynamic scores are
			// excluded on purpose: they move with ordinary traffic, and letting them
			// break the pin would defeat what the pin is for.
			switch model.ValidateChannelAffinityPin(channelID, usingGroup, modelName, path) {
			case model.ChannelAffinityPinUnusable:
				info["cache"] = "stale_unusable"
				logger.LogDebug(c, "channel affinity dropped, channel no longer eligible: rule=%s, key=%s, channel=%d", meta.RuleName, cacheKeyFull, channelID)
				dropChannelAffinityEntry(cacheKeySuffix, channelID)
				return 0, false
			case model.ChannelAffinityPinOutranked:
				info["cache"] = "stale_outranked"
				logger.LogDebug(c, "channel affinity dropped, higher priority channel configured: rule=%s, key=%s, channel=%d", meta.RuleName, cacheKeyFull, channelID)
				dropChannelAffinityEntry(cacheKeySuffix, channelID)
				return 0, false
			}
			logger.LogDebug(c, "channel affinity hit: rule=%s, key=%s, channel=%d", meta.RuleName, cacheKeyFull, channelID)
			return channelID, true
		}
		info["cache"] = "miss"
		logger.LogDebug(c, "channel affinity miss: rule=%s, key=%s", meta.RuleName, cacheKeyFull)
		return 0, false
	}
	return 0, false
}

// channelAffinityLogInfo returns the request's affinity audit payload, creating
// it from meta on first use. Callers mutate the returned map in place; it lands
// under the consume/error log's other.admin_info.channel_affinity, which is
// stripped from non-admin log views.
func channelAffinityLogInfo(c *gin.Context, meta channelAffinityMeta) map[string]interface{} {
	if anyInfo, ok := c.Get(ginKeyChannelAffinityLogInfo); ok {
		if info, ok := anyInfo.(map[string]interface{}); ok && info != nil {
			return info
		}
	}
	info := map[string]interface{}{
		"reason":       meta.RuleName,
		"rule_name":    meta.RuleName,
		"using_group":  meta.UsingGroup,
		"model":        meta.ModelName,
		"request_path": meta.RequestPath,
		"key_source":   meta.KeySourceType,
		"key_key":      meta.KeySourceKey,
		"key_path":     meta.KeySourcePath,
		"key_hint":     meta.KeyHint,
		"key_fp":       meta.KeyFingerprint,
	}
	c.Set(ginKeyChannelAffinityLogInfo, info)
	return info
}

// ShouldSkipRetryAfterChannelAffinityFailure reports whether the failed attempt
// ran on a channel that affinity itself picked, which is the only case where
// "skip_retry_on_failure" applies.
//
// It deliberately trusts nothing but the explicit key that MarkChannelAffinityUsed
// writes when affinity really selected the channel (and that
// ClearCurrentChannelAffinityCache resets when the pinned channel was unusable).
// Falling back to the rule's SkipRetryOnFailure off the request meta used to make
// every request that merely *matched* a rule non-retryable — cache miss, cache
// error, or a pinned-but-disabled channel kept by keep_on_channel_disabled — even
// though the channel actually serving it came from ordinary priority/weight
// selection and had nothing to do with affinity.
func ShouldSkipRetryAfterChannelAffinityFailure(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(ginKeyChannelAffinitySkipRetry)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func ClearCurrentChannelAffinityCache(c *gin.Context) bool {
	if c == nil {
		return false
	}
	cacheKey, _, ok := getChannelAffinityContext(c)
	if !ok || cacheKey == "" {
		return false
	}

	cache := getChannelAffinityCache()
	deleted, err := cache.DeleteMany([]string{cacheKey})
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity cache delete current failed: err=%v", err))
		return false
	}
	c.Set(ginKeyChannelAffinitySkipRetry, false)
	for _, ok := range deleted {
		if ok {
			return true
		}
	}
	return false
}

func ShouldKeepChannelAffinityOnChannelDisabled() bool {
	setting := operation_setting.GetChannelAffinitySetting()
	if setting == nil {
		return false
	}
	return setting.KeepOnChannelDisabled
}

func MarkChannelAffinityUsed(c *gin.Context, selectedGroup string, channelID int) {
	if c == nil || channelID <= 0 {
		return
	}
	meta, ok := getChannelAffinityMeta(c)
	if !ok {
		return
	}
	c.Set(ginKeyChannelAffinitySkipRetry, meta.SkipRetry)
	// Remembered so a later failure can tell "the pinned channel broke" from "a
	// fallback channel broke". Only the former should retract the pin.
	c.Set(ginKeyChannelAffinityPinnedChannel, channelID)
	info := channelAffinityLogInfo(c, meta)
	info["selected_group"] = selectedGroup
	info["channel_id"] = channelID
}

// MarkChannelAffinityChannelFault records that channelID failed with an error
// that IsChannelFaultError accepted as the channel's own fault.
//
// The caller classifies, not this function: rate limits, client errors and our
// own misconfiguration reach the same call site and must not cost a channel its
// pin. Recording the channel id (rather than a bare bool) is what lets the pin be
// retracted only when the failure was on the pinned channel itself.
//
// Faults accumulate across the request's retries instead of replacing each other.
// One request can fault on several channels, and the pinned channel is not
// necessarily the last of them, so overwriting would hide the fault that actually
// matters behind whichever fallback happened to fail last.
func MarkChannelAffinityChannelFault(c *gin.Context, channelID int) {
	if c == nil || channelID <= 0 {
		return
	}
	stored, _ := c.Get(ginKeyChannelAffinityFaultChannel)
	faulted, _ := stored.(map[int]bool)
	if faulted == nil {
		faulted = make(map[int]bool, 2)
		c.Set(ginKeyChannelAffinityFaultChannel, faulted)
	}
	// Mutating the stored map in place keeps every attempt writing to one set; gin
	// contexts are per-request and this only runs on the request's own goroutine.
	faulted[channelID] = true
}

// channelAffinityChannelFaulted reports whether channelID was recorded as having
// faulted during this request.
func channelAffinityChannelFaulted(c *gin.Context, channelID int) bool {
	if c == nil || channelID <= 0 {
		return false
	}
	stored, ok := c.Get(ginKeyChannelAffinityFaultChannel)
	if !ok {
		return false
	}
	faulted, _ := stored.(map[int]bool)
	return faulted[channelID]
}

// releaseChannelAffinityOnFault retracts the pin when this request's failure was
// a genuine fault on the pinned channel.
//
// The delete is conditional on the entry still naming that channel. A failing
// request and a concurrent succeeding one race here: the successful one may have
// already repinned the key to a healthy channel, and an unconditional delete
// would throw that away and send the next request back to cold selection.
func releaseChannelAffinityOnFault(c *gin.Context) {
	if c == nil {
		return
	}
	pinnedChannel := c.GetInt(ginKeyChannelAffinityPinnedChannel)
	if pinnedChannel <= 0 {
		// Affinity did not choose this request's channel, so there is no pin of ours
		// to retract.
		return
	}
	if !channelAffinityChannelFaulted(c, pinnedChannel) {
		// Either no failure was classified as a channel fault, or the ones that were
		// all happened on fallback channels. Neither says anything about the pinned
		// channel, which may simply never have been tried.
		return
	}
	if c.GetBool(ginKeyChannelAffinityBypassed) {
		// The pinned channel was healthy and merely full; saturation is not a fault.
		return
	}
	cacheKey, _, ok := getChannelAffinityContext(c)
	if !ok || cacheKey == "" {
		return
	}
	cache := getChannelAffinityCache()
	deleted, err := cache.DeleteIfEquals(cacheKey, pinnedChannel)
	if err != nil {
		common.SysError(fmt.Sprintf("channel affinity fault release failed: key=%s, err=%v", cacheKey, err))
		return
	}
	if deleted {
		logger.LogDebug(c, "channel affinity released after channel fault: key=%s, channel=%d", cacheKey, pinnedChannel)
	}
}

// dropChannelAffinityEntry removes the entry only while it still names
// channelID. Used by the revalidation path, where the entry has been judged
// unusable on its own terms (the channel is gone, or an admin outranked it)
// rather than as a retraction of a specific channel's pin.
//
// The verdict was reached about the channel this request read out of the cache,
// and it says nothing about any other. A concurrent request may already have
// repinned the key to a healthy, in-rank channel between that read and this
// delete; deleting unconditionally would discard that and send the next request
// back to cold selection for no reason.
func dropChannelAffinityEntry(cacheKey string, channelID int) {
	if cacheKey == "" || channelID <= 0 {
		return
	}
	cache := getChannelAffinityCache()
	if _, err := cache.DeleteIfEquals(cacheKey, channelID); err != nil {
		common.SysError(fmt.Sprintf("channel affinity stale entry delete failed: key=%s, err=%v", cacheKey, err))
	}
}

// SetChannelAffinityRelayOutcome publishes the relay handler's own verdict on the
// request so the distributor middleware can decide affinity on it.
//
// The middleware only regains control after c.Next() returns, by which time a
// streamed response has long committed its 200: gin's ResponseWriter ignores
// every WriteHeader after the first, so an upstream that dies mid-stream (or a
// keepalive ping that was the only thing ever written) leaves c.Writer.Status()
// reporting 200 for a request that failed. Pinning the channel on that status
// parks the whole affinity key on the channel that just broke.
func SetChannelAffinityRelayOutcome(c *gin.Context, succeeded bool) {
	if c == nil {
		return
	}
	c.Set(ginKeyChannelAffinityRelayOK, succeeded)
}

// SetChannelAffinityBypassed marks that the pinned channel was skipped only
// because it was at its concurrency limit. Saturation is not a fault, so the pin
// is kept and this request's fallback channel does not inherit it.
func SetChannelAffinityBypassed(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(ginKeyChannelAffinityBypassed, true)
}

func AppendChannelAffinityAdminInfo(c *gin.Context, adminInfo map[string]interface{}) {
	if c == nil || adminInfo == nil {
		return
	}
	anyInfo, ok := c.Get(ginKeyChannelAffinityLogInfo)
	if !ok || anyInfo == nil {
		return
	}
	adminInfo["channel_affinity"] = anyInfo
}

// RecordChannelAffinity pins the channel that served a successful request, so
// the next request carrying the same affinity key lands on it again.
//
// Success is taken from the relay handler's own verdict (SetChannelAffinityRelayOutcome)
// rather than from the response status, because a stream that failed mid-flight
// still reports the 200 it committed before the failure. Handlers that publish
// no verdict — midjourney/suno/video task submit — never stream a partial
// success and are still judged on the committed status code.
func RecordChannelAffinity(c *gin.Context, channelID int) {
	if channelID <= 0 || c == nil {
		return
	}
	if relayOK, reported := c.Get(ginKeyChannelAffinityRelayOK); reported {
		succeeded, _ := relayOK.(bool)
		if !succeeded {
			// A failed request used to return here and leave the pin exactly as it
			// was. Nothing else retracts it: the distributor only clears a pin whose
			// channel was already unusable when selection ran, so a channel that was
			// enabled, got picked, and then broke during the relay kept the key
			// forever — and because a stream that has written bytes is not retried,
			// nothing moved it either. An occasional success in between refreshed the
			// TTL. That is the "channels are failing but it stays on the old one"
			// case; the pin needs failure accounting, not just success accounting.
			releaseChannelAffinityOnFault(c)
			return
		}
	} else if c.Writer == nil || c.Writer.Status() >= http.StatusBadRequest {
		releaseChannelAffinityOnFault(c)
		return
	}
	setting := operation_setting.GetChannelAffinitySetting()
	if setting == nil || !setting.Enabled {
		return
	}
	if c.GetBool(ginKeyChannelAffinityBypassed) {
		// The pinned channel was healthy and simply full. Repinning to whichever
		// channel absorbed the overflow would walk the key away from its warm
		// upstream for as long as the load lasts, which is the opposite of what
		// affinity is for. Leaving the pin untouched also leaves its TTL alone, so a
		// channel that stays saturated does eventually lose the key.
		return
	}
	if setting.SwitchOnSuccess {
		if successChannelID := c.GetInt("channel_id"); successChannelID > 0 {
			channelID = successChannelID
		}
	}
	cacheKey, ttlSeconds, ok := getChannelAffinityContext(c)
	if !ok {
		return
	}
	if ttlSeconds <= 0 {
		ttlSeconds = setting.DefaultTTLSeconds
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 3600
	}
	cache := getChannelAffinityCache()
	if err := cache.SetWithTTL(cacheKey, channelID, time.Duration(ttlSeconds)*time.Second); err != nil {
		common.SysError(fmt.Sprintf("channel affinity cache set failed: key=%s, err=%v", cacheKey, err))
	}
}

type ChannelAffinityUsageCacheStats struct {
	RuleName            string `json:"rule_name"`
	UsingGroup          string `json:"using_group"`
	KeyFingerprint      string `json:"key_fp"`
	CachedTokenRateMode string `json:"cached_token_rate_mode"`

	Hit           int64 `json:"hit"`
	Total         int64 `json:"total"`
	WindowSeconds int64 `json:"window_seconds"`

	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	CachedTokens         int64 `json:"cached_tokens"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	LastSeenAt           int64 `json:"last_seen_at"`
}

type ChannelAffinityUsageCacheCounters struct {
	CachedTokenRateMode string `json:"cached_token_rate_mode"`

	Hit           int64 `json:"hit"`
	Total         int64 `json:"total"`
	WindowSeconds int64 `json:"window_seconds"`

	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	CachedTokens         int64 `json:"cached_tokens"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	LastSeenAt           int64 `json:"last_seen_at"`
}

var channelAffinityUsageCacheStatsLocks [64]sync.Mutex

// ObserveChannelAffinityUsageCacheByRelayFormat records usage cache stats with a stable rate mode derived from relay format.
func ObserveChannelAffinityUsageCacheByRelayFormat(c *gin.Context, usage *dto.Usage, relayFormat types.RelayFormat) {
	ObserveChannelAffinityUsageCacheFromContext(c, usage, cachedTokenRateModeByRelayFormat(relayFormat))
}

func ObserveChannelAffinityUsageCacheFromContext(c *gin.Context, usage *dto.Usage, cachedTokenRateMode string) {
	statsCtx, ok := GetChannelAffinityStatsContext(c)
	if !ok {
		return
	}
	observeChannelAffinityUsageCache(statsCtx, usage, cachedTokenRateMode)
}

func GetChannelAffinityUsageCacheStats(ruleName, usingGroup, keyFp string) ChannelAffinityUsageCacheStats {
	ruleName = strings.TrimSpace(ruleName)
	usingGroup = strings.TrimSpace(usingGroup)
	keyFp = strings.TrimSpace(keyFp)

	entryKey := channelAffinityUsageCacheEntryKey(ruleName, usingGroup, keyFp)
	if entryKey == "" {
		return ChannelAffinityUsageCacheStats{
			RuleName:       ruleName,
			UsingGroup:     usingGroup,
			KeyFingerprint: keyFp,
		}
	}

	cache := getChannelAffinityUsageCacheStatsCache()
	v, found, err := cache.Get(entryKey)
	if err != nil || !found {
		return ChannelAffinityUsageCacheStats{
			RuleName:       ruleName,
			UsingGroup:     usingGroup,
			KeyFingerprint: keyFp,
		}
	}
	return ChannelAffinityUsageCacheStats{
		CachedTokenRateMode:  v.CachedTokenRateMode,
		RuleName:             ruleName,
		UsingGroup:           usingGroup,
		KeyFingerprint:       keyFp,
		Hit:                  v.Hit,
		Total:                v.Total,
		WindowSeconds:        v.WindowSeconds,
		PromptTokens:         v.PromptTokens,
		CompletionTokens:     v.CompletionTokens,
		TotalTokens:          v.TotalTokens,
		CachedTokens:         v.CachedTokens,
		PromptCacheHitTokens: v.PromptCacheHitTokens,
		LastSeenAt:           v.LastSeenAt,
	}
}

func observeChannelAffinityUsageCache(statsCtx ChannelAffinityStatsContext, usage *dto.Usage, cachedTokenRateMode string) {
	entryKey := channelAffinityUsageCacheEntryKey(statsCtx.RuleName, statsCtx.UsingGroup, statsCtx.KeyFingerprint)
	if entryKey == "" {
		return
	}

	windowSeconds := statsCtx.TTLSeconds
	if windowSeconds <= 0 {
		return
	}

	cache := getChannelAffinityUsageCacheStatsCache()
	ttl := time.Duration(windowSeconds) * time.Second

	lock := channelAffinityUsageCacheStatsLock(entryKey)
	lock.Lock()
	defer lock.Unlock()

	prev, found, err := cache.Get(entryKey)
	if err != nil {
		return
	}
	next := prev
	if !found {
		next = ChannelAffinityUsageCacheCounters{}
	}
	currentMode := normalizeCachedTokenRateMode(cachedTokenRateMode)
	if currentMode != "" {
		if next.CachedTokenRateMode == "" {
			next.CachedTokenRateMode = currentMode
		} else if next.CachedTokenRateMode != currentMode && next.CachedTokenRateMode != cacheTokenRateModeMixed {
			next.CachedTokenRateMode = cacheTokenRateModeMixed
		}
	}
	next.Total++
	hit, cachedTokens, promptCacheHitTokens := usageCacheSignals(usage)
	if hit {
		next.Hit++
	}
	next.WindowSeconds = windowSeconds
	next.LastSeenAt = time.Now().Unix()
	next.CachedTokens += cachedTokens
	next.PromptCacheHitTokens += promptCacheHitTokens
	next.PromptTokens += int64(usagePromptTokens(usage))
	next.CompletionTokens += int64(usageCompletionTokens(usage))
	next.TotalTokens += int64(usageTotalTokens(usage))
	_ = cache.SetWithTTL(entryKey, next, ttl)
}

func normalizeCachedTokenRateMode(mode string) string {
	switch mode {
	case cacheTokenRateModeCachedOverPrompt:
		return cacheTokenRateModeCachedOverPrompt
	case cacheTokenRateModeCachedOverPromptPlusCached:
		return cacheTokenRateModeCachedOverPromptPlusCached
	case cacheTokenRateModeMixed:
		return cacheTokenRateModeMixed
	default:
		return ""
	}
}

func cachedTokenRateModeByRelayFormat(relayFormat types.RelayFormat) string {
	switch relayFormat {
	case types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatOpenAIResponsesCompaction:
		return cacheTokenRateModeCachedOverPrompt
	case types.RelayFormatClaude:
		return cacheTokenRateModeCachedOverPromptPlusCached
	default:
		return ""
	}
}

func channelAffinityUsageCacheEntryKey(ruleName, usingGroup, keyFp string) string {
	ruleName = strings.TrimSpace(ruleName)
	usingGroup = strings.TrimSpace(usingGroup)
	keyFp = strings.TrimSpace(keyFp)
	if ruleName == "" || keyFp == "" {
		return ""
	}
	return ruleName + "\n" + usingGroup + "\n" + keyFp
}

func usageCacheSignals(usage *dto.Usage) (hit bool, cachedTokens int64, promptCacheHitTokens int64) {
	if usage == nil {
		return false, 0, 0
	}

	cached := int64(0)
	if usage.PromptTokensDetails.CachedTokens > 0 {
		cached = int64(usage.PromptTokensDetails.CachedTokens)
	} else if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
		cached = int64(usage.InputTokensDetails.CachedTokens)
	}
	pcht := int64(0)
	if usage.PromptCacheHitTokens > 0 {
		pcht = int64(usage.PromptCacheHitTokens)
	}
	return cached > 0 || pcht > 0, cached, pcht
}

func usagePromptTokens(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.PromptTokens > 0 {
		return usage.PromptTokens
	}
	return usage.InputTokens
}

func usageCompletionTokens(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.CompletionTokens > 0 {
		return usage.CompletionTokens
	}
	return usage.OutputTokens
}

func usageTotalTokens(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	pt := usagePromptTokens(usage)
	ct := usageCompletionTokens(usage)
	if pt > 0 || ct > 0 {
		return pt + ct
	}
	return 0
}

func getChannelAffinityUsageCacheStatsCache() *cachex.HybridCache[ChannelAffinityUsageCacheCounters] {
	channelAffinityUsageCacheStatsOnce.Do(func() {
		setting := operation_setting.GetChannelAffinitySetting()
		capacity := 100_000
		defaultTTLSeconds := 3600
		if setting != nil {
			if setting.MaxEntries > 0 {
				capacity = setting.MaxEntries
			}
			if setting.DefaultTTLSeconds > 0 {
				defaultTTLSeconds = setting.DefaultTTLSeconds
			}
		}

		channelAffinityUsageCacheStatsCache = cachex.NewHybridCache[ChannelAffinityUsageCacheCounters](cachex.HybridCacheConfig[ChannelAffinityUsageCacheCounters]{
			Namespace: cachex.Namespace(channelAffinityUsageCacheStatsNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[ChannelAffinityUsageCacheCounters]{},
			Memory: func() *hot.HotCache[string, ChannelAffinityUsageCacheCounters] {
				return hot.NewHotCache[string, ChannelAffinityUsageCacheCounters](hot.LRU, capacity).
					WithTTL(time.Duration(defaultTTLSeconds) * time.Second).
					WithJanitor().
					Build()
			},
		})
	})
	return channelAffinityUsageCacheStatsCache
}

func channelAffinityUsageCacheStatsLock(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	idx := h.Sum32() % uint32(len(channelAffinityUsageCacheStatsLocks))
	return &channelAffinityUsageCacheStatsLocks[idx]
}
