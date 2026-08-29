package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/samber/lo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Ability struct {
	Group     string  `json:"group" gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Model     string  `json:"model" gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
	ChannelId int     `json:"channel_id" gorm:"primaryKey;autoIncrement:false;index"`
	Enabled   bool    `json:"enabled"`
	Priority  *int64  `json:"priority" gorm:"bigint;default:0;index"`
	Weight    uint    `json:"weight" gorm:"default:0;index"`
	Tag       *string `json:"tag" gorm:"index"`
	// MaxConcurrency mirrors Channel.MaxConcurrency. It is denormalized here for
	// the same reason Priority and Weight are: the database selection path (the one
	// production takes, with the memory cache off) builds its candidates from
	// abilities alone, and resolving the cap per candidate would mean one channel
	// query per candidate on every request. No index: it is never a filter, only a
	// value the selector reads off a row it already loaded.
	MaxConcurrency *int `json:"max_concurrency" gorm:"default:0"`
}

type AbilityWithChannel struct {
	Ability
	ChannelType int `json:"channel_type"`
}

func GetAllEnableAbilityWithChannels() ([]AbilityWithChannel, error) {
	var abilities []AbilityWithChannel
	err := DB.Table("abilities").
		Select("abilities.*, channels.type as channel_type").
		Joins("left join channels on abilities.channel_id = channels.id").
		Where("abilities.enabled = ?", true).
		Scan(&abilities).Error
	return abilities, err
}

func GetGroupEnabledModels(group string) []string {
	var models []string
	// Find distinct models
	DB.Table("abilities").Where(commonGroupCol+" = ? and enabled = ?", group, true).Distinct("model").Pluck("model", &models)
	return models
}

func GetEnabledModels() []string {
	var models []string
	// Find distinct models
	DB.Table("abilities").Where("enabled = ?", true).Distinct("model").Pluck("model", &models)
	return models
}

func GetAllEnableAbilities() []Ability {
	var abilities []Ability
	DB.Find(&abilities, "enabled = ?", true)
	return abilities
}

// channelWeightBaseline is the per-candidate floor added to every configured
// weight before the weighted random draw. It keeps a weight-0 channel reachable
// inside a tier that also holds weighted channels, and it is the single source
// of truth for the DB path and the memory-cache path alike.
const channelWeightBaseline = 10

// applyDynamicScores shifts candidates by their learned dynamic score, if the
// feature is on. Both selection paths funnel through here — the database path
// and the memory-cache path build candidate lists independently, and applying
// the adjustment in only one of them would make the feature silently inert in
// whichever configuration the deployment happens to run.
//
// The adjusted priorities live only for this selection. They are handed straight
// to selectChannelCandidate and never written back to abilities or the channel
// cache: the admin-configured priority and weight remain the baseline.
func applyDynamicScores(group string, modelName string, candidates []channelCandidate) []channelCandidate {
	if len(candidates) == 0 || !channel_score.Enabled() {
		return candidates
	}
	in := make([]channel_score.Candidate, len(candidates))
	for i, candidate := range candidates {
		in[i] = channel_score.Candidate{
			ChannelId: candidate.channelId,
			Priority:  candidate.priority,
			Weight:    candidate.weight,
		}
	}
	out := channel_score.ApplyToCandidates(group, modelName, in)
	if len(out) != len(candidates) {
		// ApplyToCandidates is specified never to add or drop candidates. If that
		// ever stops holding, fall back to the configured values rather than
		// selecting from a set that no longer matches.
		return candidates
	}
	adjusted := make([]channelCandidate, len(candidates))
	copy(adjusted, candidates)
	for i := range adjusted {
		adjusted[i].priority = out[i].Priority
		adjusted[i].weight = out[i].Weight
	}
	return adjusted
}

// channelCandidate is one selectable (channel, priority tier, weight) triple.
// Both selection paths reduce their own storage to this shape, so tier ordering,
// exclusion and weighting cannot drift apart between them.
type channelCandidate struct {
	channelId int
	priority  int64
	weight    int
	// maxConcurrency is the channel's concurrency cap, 0 meaning unlimited. A
	// channel already serving that many requests is skipped for the whole tier
	// pass, which is what makes overflow cascade down to the next priority tier
	// instead of queueing on a saturated upstream.
	maxConcurrency int
}

// effectiveChannelWeight converts a configured weight into the share used by the
// weighted draw. The baseline guarantees every candidate a non-zero share; the
// clamp keeps a corrupt (overflowed) weight from producing a negative one.
func effectiveChannelWeight(weight int) int {
	if weight < 0 {
		weight = 0
	}
	maxInt := int(^uint(0) >> 1)
	if weight > maxInt-channelWeightBaseline {
		return maxInt
	}
	return weight + channelWeightBaseline
}

// pickWeightedIndex walks the cumulative weights and returns the index owning
// draw, which must come from [0, sum(weights)). It is a separate function so the
// distribution can be asserted by enumerating every draw instead of sampling.
func pickWeightedIndex(weights []int, draw int) int {
	for index, weight := range weights {
		draw -= weight
		if draw < 0 {
			return index
		}
	}
	return -1
}

// selectChannelCandidate picks one channel id from candidates, walking priority
// tiers from highest to lowest and skipping every channel in excludedChannelIds
// (the channels already attempted for this request).
//
// A tier is exhausted horizontally before selection drops to the next one: as
// long as an untried sibling remains in the current tier, the draw stays there.
// That is what keeps a retry off the channel that just failed, which the old
// retry-indexed tier lookup could not do — with a single distinct priority every
// retry re-drew from the full candidate set, failed channel included.
//
// retry keeps its legacy meaning as the starting tier index only for callers that
// do not track attempts (empty excludedChannelIds, e.g. the distributor's initial
// pick). Once attempts are tracked, the excluded set alone decides when a tier is
// spent. Reports false when every tier is exhausted.
//
// A channel already at its concurrency cap is skipped exactly like an excluded
// one, so a saturated tier demotes to the next tier down. The count is read live;
// the caller is expected to take the slot it wins (selectAndAcquireChannel) since
// nothing here reserves it.
func selectChannelCandidate(candidates []channelCandidate, retry int, excludedChannelIds map[int]bool) (int, bool) {
	if len(candidates) == 0 {
		return 0, false
	}

	tiers := make([]int64, 0, len(candidates))
	seenTier := make(map[int64]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seenTier[candidate.priority]; ok {
			continue
		}
		seenTier[candidate.priority] = struct{}{}
		tiers = append(tiers, candidate.priority)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] > tiers[j] })

	startTier := 0
	if len(excludedChannelIds) == 0 && retry > 0 {
		startTier = retry
		if startTier >= len(tiers) {
			startTier = len(tiers) - 1
		}
	}

	for _, tier := range tiers[startTier:] {
		tierChannelIds := make([]int, 0, len(candidates))
		tierWeights := make([]int, 0, len(candidates))
		totalWeight := 0
		for _, candidate := range candidates {
			if candidate.priority != tier || excludedChannelIds[candidate.channelId] || candidate.isSaturated() {
				continue
			}
			weight := effectiveChannelWeight(candidate.weight)
			tierChannelIds = append(tierChannelIds, candidate.channelId)
			tierWeights = append(tierWeights, weight)
			maxInt := int(^uint(0) >> 1)
			if totalWeight > maxInt-weight {
				// Keep rand.Intn's upper bound positive even when several
				// operator-supplied weights approach the machine int limit.
				totalWeight = maxInt
			} else {
				totalWeight += weight
			}
		}
		if len(tierChannelIds) == 0 {
			// Every channel of this tier was already tried, saturated, or filtered
			// out by the request path; fall back to the next tier down.
			continue
		}
		index := pickWeightedIndex(tierWeights, common.GetRandomInt(totalWeight))
		if index < 0 {
			continue
		}
		return tierChannelIds[index], true
	}
	return 0, false
}

// isSaturated reports whether the channel is already serving as many requests as
// its cap allows.
func (candidate channelCandidate) isSaturated() bool {
	return candidate.maxConcurrency > 0 && ChannelInFlight(candidate.channelId) >= candidate.maxConcurrency
}

// ErrAllChannelsSaturated is returned when every channel that could serve the
// request is at its concurrency cap. It is deliberately distinct from "no channel
// found": nothing is misconfigured and no channel misbehaved, the upstreams are
// simply all busy, so the caller must not blame, disable, or unpin any channel.
var ErrAllChannelsSaturated = errors.New("all matching channels are at their concurrency limit")

// selectAndAcquireChannel picks a channel and takes one of its concurrency slots
// in the same step, so the winner of a race for the last slot is the only request
// admitted. The caller owns the slot from here on and must release it via
// service.ReleaseHeldChannelSlot when the attempt ends.
//
// Losing the acquire means another request took the last slot between the
// saturation check and the acquire. The channel is dropped from this request's
// draw and selection runs again, which lets it demote to a lower tier exactly as
// it would have if the channel had already been full.
func selectAndAcquireChannel(candidates []channelCandidate, retry int, excludedChannelIds map[int]bool) (int, error) {
	attempted := excludedChannelIds
	cloned := false
	for {
		channelId, ok := selectChannelCandidate(candidates, retry, attempted)
		if !ok {
			// Distinguish "everything is busy" from "nothing matches" so the caller
			// can report backpressure instead of a configuration error.
			for _, candidate := range candidates {
				if !attempted[candidate.channelId] && candidate.isSaturated() {
					return 0, ErrAllChannelsSaturated
				}
			}
			return 0, nil
		}
		limit := 0
		for _, candidate := range candidates {
			if candidate.channelId == channelId {
				limit = candidate.maxConcurrency
				break
			}
		}
		if AcquireChannelSlot(channelId, limit) {
			return channelId, nil
		}
		if !cloned {
			// The caller's excluded set belongs to the retry bookkeeping and must not
			// gain entries for channels this request never actually tried.
			attempted = make(map[int]bool, len(excludedChannelIds)+1)
			for id := range excludedChannelIds {
				attempted[id] = true
			}
			cloned = true
		}
		attempted[channelId] = true
	}
}

// GetChannel selects a channel straight from the database (the path taken when
// the memory cache is disabled). excludedChannelIds holds the channels already
// attempted for this request and is removed from every tier.
//
// All enabled tiers for group/model are loaded in one query rather than filtering
// to a single priority in SQL: the request-path filter can empty a tier, and
// selection must then demote to the next tier instead of failing. If the exact
// model has no usable abilities, the normalized model name gets the same pass.
// This keeps tiering identical to the memory-cache path, which filters before it
// groups.
func GetChannel(group string, model string, retry int, requestPath string, excludedChannelIds map[int]bool) (*Channel, error) {
	abilities, err := getEnabledAbilities(group, model)
	if err != nil {
		return nil, err
	}
	abilities = filterAbilitiesByRequestPathAndModel(abilities, requestPath, model)
	if len(abilities) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(model)
		if normalizedModel != model {
			abilities, err = getEnabledAbilities(group, normalizedModel)
			if err != nil {
				return nil, err
			}
			abilities = filterAbilitiesByRequestPathAndModel(abilities, requestPath, model)
		}
	}
	if len(abilities) == 0 {
		return nil, nil
	}

	candidates := make([]channelCandidate, 0, len(abilities))
	for _, ability := range abilities {
		// A NULL priority column maps to 0, matching Channel.GetPriority so both
		// selection paths place such a channel in the same tier.
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		maxConcurrency := 0
		if ability.MaxConcurrency != nil && *ability.MaxConcurrency > 0 {
			maxConcurrency = *ability.MaxConcurrency
		}
		candidates = append(candidates, channelCandidate{
			channelId:      ability.ChannelId,
			priority:       priority,
			weight:         saturatingUintToInt(ability.Weight),
			maxConcurrency: maxConcurrency,
		})
	}

	candidates = applyDynamicScores(group, model, candidates)

	channelId, err := selectAndAcquireChannel(candidates, retry, excludedChannelIds)
	if err != nil {
		return nil, err
	}
	if channelId == 0 {
		return nil, fmt.Errorf("no channel found, group: %s, model: %s", group, model)
	}
	channel := Channel{}
	if err = DB.First(&channel, "id = ?", channelId).Error; err != nil {
		// The slot was taken before the row was read; nothing downstream will bind
		// this channel to the request, so nothing would ever give it back.
		ReleaseChannelSlot(channelId)
		return nil, err
	}
	return &channel, nil
}

// getEnabledAbilities mirrors the memory-cache path's channel-status filter.
// The abilities table is denormalized and can briefly lag behind a channel
// status update; joining channels here prevents the no-cache deployment mode
// from routing a request to a channel that has already been disabled.
func getEnabledAbilities(group string, modelName string) ([]Ability, error) {
	var abilities []Ability
	err := DB.Joins("JOIN channels ON channels.id = abilities.channel_id").
		Where("abilities."+commonGroupCol+" = ? and abilities.model = ? and abilities.enabled = ? and channels.status = ?", group, modelName, true, common.ChannelStatusEnabled).
		Order("abilities.priority DESC").Order("abilities.weight DESC").Find(&abilities).Error
	return abilities, err
}

// filterAbilitiesByRequestPathAndModel restricts candidates by request path and
// model for the DB (non-memory-cache) selection path. Only Advanced Custom
// (type 58) channels are path-checked: kept only when one of their routes matches
// requestPath and model; all other channel types always pass. When requestPath is
// empty, filtering is skipped.
func filterAbilitiesByRequestPathAndModel(abilities []Ability, requestPath string, model string) []Ability {
	if requestPath == "" || len(abilities) == 0 {
		return abilities
	}

	channelIds := make([]int, 0, len(abilities))
	seen := make(map[int]struct{}, len(abilities))
	for _, ability := range abilities {
		if _, ok := seen[ability.ChannelId]; ok {
			continue
		}
		seen[ability.ChannelId] = struct{}{}
		channelIds = append(channelIds, ability.ChannelId)
	}

	var channels []*Channel
	if err := DB.Where("id IN ?", channelIds).Find(&channels).Error; err != nil {
		// On error, fall back to unfiltered candidates to avoid blocking selection
		return abilities
	}

	advancedConfigs := make(map[int]*dto.AdvancedCustomConfig)
	for _, channel := range channels {
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			advancedConfigs[channel.Id] = channel.GetOtherSettings().AdvancedCustom
		}
	}

	filtered := make([]Ability, 0, len(abilities))
	for _, ability := range abilities {
		config, isAdvancedCustom := advancedConfigs[ability.ChannelId]
		if !isAdvancedCustom {
			filtered = append(filtered, ability)
			continue
		}
		if config != nil && config.SupportsPathForModel(requestPath, model) {
			filtered = append(filtered, ability)
		}
	}
	return filtered
}

func (channel *Channel) AddAbilities(tx *gorm.DB) error {
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilitySet := make(map[string]struct{})
	abilities := make([]Ability, 0, len(models_))
	for _, model := range models_ {
		for _, group := range groups_ {
			key := group + "|" + model
			if _, exists := abilitySet[key]; exists {
				continue
			}
			abilitySet[key] = struct{}{}
			ability := Ability{
				Group:          group,
				Model:          model,
				ChannelId:      channel.Id,
				Enabled:        channel.Status == common.ChannelStatusEnabled,
				Priority:       channel.Priority,
				Weight:         uint(channel.GetWeight()),
				Tag:            channel.Tag,
				MaxConcurrency: channel.MaxConcurrency,
			}
			abilities = append(abilities, ability)
		}
	}
	if len(abilities) == 0 {
		return nil
	}
	// choose DB or provided tx
	useDB := DB
	if tx != nil {
		useDB = tx
	}
	for _, chunk := range lo.Chunk(abilities, 50) {
		err := useDB.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error
		if err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) DeleteAbilities() error {
	return DB.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
}

// UpdateAbilities updates abilities of this channel.
// Make sure the channel is completed before calling this function.
func (channel *Channel) UpdateAbilities(tx *gorm.DB) error {
	isNewTx := false
	// 如果没有传入事务，创建新的事务
	if tx == nil {
		tx = DB.Begin()
		if tx.Error != nil {
			return tx.Error
		}
		isNewTx = true
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
			}
		}()
	}

	// First delete all abilities of this channel
	err := tx.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
	if err != nil {
		if isNewTx {
			tx.Rollback()
		}
		return err
	}

	// Then add new abilities
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilitySet := make(map[string]struct{})
	abilities := make([]Ability, 0, len(models_))
	for _, model := range models_ {
		for _, group := range groups_ {
			key := group + "|" + model
			if _, exists := abilitySet[key]; exists {
				continue
			}
			abilitySet[key] = struct{}{}
			ability := Ability{
				Group:          group,
				Model:          model,
				ChannelId:      channel.Id,
				Enabled:        channel.Status == common.ChannelStatusEnabled,
				Priority:       channel.Priority,
				Weight:         uint(channel.GetWeight()),
				Tag:            channel.Tag,
				MaxConcurrency: channel.MaxConcurrency,
			}
			abilities = append(abilities, ability)
		}
	}

	if len(abilities) > 0 {
		for _, chunk := range lo.Chunk(abilities, 50) {
			err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error
			if err != nil {
				if isNewTx {
					tx.Rollback()
				}
				return err
			}
		}
	}

	// 如果是新创建的事务，需要提交
	if isNewTx {
		return tx.Commit().Error
	}

	return nil
}

func UpdateAbilityStatus(channelId int, status bool) error {
	return DB.Model(&Ability{}).Where("channel_id = ?", channelId).Select("enabled").Update("enabled", status).Error
}

func UpdateAbilityStatusByTag(tag string, status bool) error {
	return DB.Model(&Ability{}).Where("tag = ?", tag).Select("enabled").Update("enabled", status).Error
}

func UpdateAbilityByTag(tag string, newTag *string, priority *int64, weight *uint, maxConcurrency *int) error {
	ability := Ability{}
	if newTag != nil {
		ability.Tag = newTag
	}
	if priority != nil {
		ability.Priority = priority
	}
	if weight != nil {
		ability.Weight = *weight
	}
	// The denormalized copy must follow the channel, or the database selection
	// path would keep skipping (or admitting) requests against the old cap.
	if maxConcurrency != nil {
		ability.MaxConcurrency = maxConcurrency
	}
	return DB.Model(&Ability{}).Where("tag = ?", tag).Updates(ability).Error
}

var fixLock = sync.Mutex{}

func FixAbility() (int, int, error) {
	lock := fixLock.TryLock()
	if !lock {
		return 0, 0, errors.New("已经有一个修复任务在运行中，请稍后再试")
	}
	defer fixLock.Unlock()

	// truncate abilities table
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		err := DB.Exec("DELETE FROM abilities").Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Delete abilities failed: %s", err.Error()))
			return 0, 0, err
		}
	} else {
		err := DB.Exec("TRUNCATE TABLE abilities").Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Truncate abilities failed: %s", err.Error()))
			return 0, 0, err
		}
	}
	var channels []*Channel
	// Find all channels
	err := DB.Model(&Channel{}).Find(&channels).Error
	if err != nil {
		return 0, 0, err
	}
	if len(channels) == 0 {
		return 0, 0, nil
	}
	successCount := 0
	failCount := 0
	for _, chunk := range lo.Chunk(channels, 50) {
		ids := lo.Map(chunk, func(c *Channel, _ int) int { return c.Id })
		// Delete all abilities of this channel
		err = DB.Where("channel_id IN ?", ids).Delete(&Ability{}).Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Delete abilities failed: %s", err.Error()))
			failCount += len(chunk)
			continue
		}
		// Then add new abilities
		for _, channel := range chunk {
			err = channel.AddAbilities(nil)
			if err != nil {
				common.SysLog(fmt.Sprintf("Add abilities for channel %d failed: %s", channel.Id, err.Error()))
				failCount++
			} else {
				successCount++
			}
		}
	}
	InitChannelCache()
	return successCount, failCount, nil
}
