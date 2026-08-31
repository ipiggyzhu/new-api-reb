package channel_score

import (
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// ScoreView is one (channel, group, model) score row, copied out for diagnostics.
//
// Every field is a value: handing out *entry, *snapshot or scoreState would let a
// reader observe a state transition halfway through, and would make the store's
// internals part of the package's API.
type ScoreView struct {
	ChannelID int    `json:"channel_id"`
	Group     string `json:"group"`
	Model     string `json:"model"`

	// TierOffset is the accumulated movement in tiers: positive promotes.
	TierOffset int `json:"tier_offset"`

	// Total and Success are the sliding window's request volume and successes.
	Total   int64 `json:"total"`
	Success int64 `json:"success"`

	// ConsecutiveSuccess and FaultCount are the streaks toward the next promotion
	// and demotion. With Redis configured these are not mirrored locally and read
	// as zero — see Snapshot's Complete field.
	ConsecutiveSuccess int `json:"consecutive_success"`
	FaultCount         int `json:"fault_count"`

	UpdatedAt int64 `json:"updated_at"`

	// WeightFactor is the multiplier the success rate currently earns. Computed
	// here rather than by the caller because it needs successRateFactor and the
	// sanitized MinSampleForWeight, neither of which is exported.
	WeightFactor float64 `json:"weight_factor"`

	// Idle marks a row whose traffic-derived sample window has expired. Its weight
	// factor therefore returns to neutral, but a tier offset may still be applied in
	// decayed form: a demotion is what stopped traffic reaching the channel, so
	// silence must not erase that standing verdict all at once.
	Idle bool `json:"idle"`
}

// ScoreSnapshot is the result of reading the store, with the metadata needed to
// judge how much the rows can be trusted.
type ScoreSnapshot struct {
	// Enabled is the admin switch; Usable additionally requires the shared store
	// to be reachable. Scoring only affects routing when both are true.
	Enabled bool `json:"enabled"`
	Usable  bool `json:"usable"`

	RedisConfigured bool `json:"redis_configured"`

	// InstanceLocal and Complete describe the rows' scope. Without Redis the local
	// store is the whole truth. With Redis these rows are a mirror of the keys this
	// process happened to touch, and publishLocal only ever mirrors tierOffset,
	// total and success — so ConsecutiveSuccess and FaultCount are absent and other
	// instances' keys are missing entirely.
	InstanceLocal bool `json:"instance_local"`
	Complete      bool `json:"complete"`

	Rows []ScoreView `json:"rows"`
}

// ScoreFilter narrows which rows Snapshot returns. A zero value returns all rows.
type ScoreFilter struct {
	ChannelID int
	Group     string
	Model     string
}

// parseScoreKey reverses scoreKey. The group is length-prefixed, so the split is
// unambiguous even when the group or the model contains a colon; the model is the
// final segment and may hold any bytes.
//
// Reports false for anything it did not produce, so a malformed key is skipped
// instead of yielding a row with a mangled group or model.
func parseScoreKey(key string) (channelId int, group string, model string, ok bool) {
	rest, found := strings.CutPrefix(key, scoreKeyPrefix)
	if !found {
		return 0, "", "", false
	}
	channelPart, rest, found := strings.Cut(rest, ":")
	if !found {
		return 0, "", "", false
	}
	channelId, err := strconv.Atoi(channelPart)
	if err != nil {
		return 0, "", "", false
	}
	lengthPart, rest, found := strings.Cut(rest, ":")
	if !found {
		return 0, "", "", false
	}
	groupLen, err := strconv.Atoi(lengthPart)
	if err != nil || groupLen < 0 || groupLen > len(rest) {
		return 0, "", "", false
	}
	group = rest[:groupLen]
	rest = rest[groupLen:]
	// The byte after the group must be the separator scoreKey wrote. Without this
	// check a truncated key could still parse, silently reattributing a row.
	if !strings.HasPrefix(rest, ":") {
		return 0, "", "", false
	}
	return channelId, group, rest[1:], true
}

// Snapshot copies the current scores out of the store.
//
// Strictly non-mutating: it only Ranges the map and copies under each entry's
// existing lock. It must never call LoadOrStore or entryGeneration, both of which
// create entries — a diagnostic read that materialized rows would change what a
// later Reset has to clear.
//
// Rows with nothing published yet are skipped: they carry no observable score.
func Snapshot(filter ScoreFilter) ScoreSnapshot {
	setting := operation_setting.GetChannelDynamicScoreSetting()
	result := ScoreSnapshot{
		Enabled:         setting.Enabled,
		Usable:          Enabled(),
		RedisConfigured: redisActive(),
		Rows:            make([]ScoreView, 0),
	}
	// Without Redis the in-process store is authoritative and every field is real.
	result.InstanceLocal = result.RedisConfigured
	result.Complete = !result.RedisConfigured

	now := nowFunc().Unix()
	localStore.Range(func(key, value any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		channelId, group, modelName, ok := parseScoreKey(keyString)
		if !ok {
			return true
		}
		if filter.ChannelID > 0 && channelId != filter.ChannelID {
			return true
		}
		if filter.Group != "" && group != filter.Group {
			return true
		}
		if filter.Model != "" && modelName != filter.Model {
			return true
		}
		e, ok := value.(*entry)
		if !ok {
			return true
		}

		// Held only for the copy. applyLocal mutates state and publishes the derived
		// snapshot under this same lock, so a locked copy cannot straddle a
		// transition; serializing the response outside the lock keeps a diagnostic
		// request from delaying Report any longer than the copy itself.
		e.mu.Lock()
		published := e.published.Load()
		state := e.state
		e.mu.Unlock()

		if published == nil {
			return true
		}

		row := ScoreView{
			ChannelID:          channelId,
			Group:              group,
			Model:              modelName,
			TierOffset:         published.tierOffset,
			Total:              published.total,
			Success:            published.success,
			ConsecutiveSuccess: state.consecutiveSuccess,
			FaultCount:         state.faultCount,
			UpdatedAt:          state.updatedAt,
			WeightFactor:       successRateFactor(published.total, published.success, setting.MinSampleForWeight),
		}
		// Match lookup exactly. Once the rate sample has gone idle its factor
		// returns to neutral, while the accumulated tier verdict decays one
		// step per idle period and remains visible until neutral. The old
		// implementation marked the whole row inactive here, making the list
		// say "no change" while routing was still keeping a failed channel out
		// of the top tier.
		if setting.IdleResetSeconds > 0 && state.updatedAt > 0 {
			elapsed := now - state.updatedAt
			if elapsed >= int64(setting.IdleResetSeconds) {
				row.Idle = true
				row.TierOffset = decayedOffset(row.TierOffset, elapsed, int64(setting.IdleResetSeconds))
				row.Total = 0
				row.Success = 0
				row.WeightFactor = 1
			}
		}
		result.Rows = append(result.Rows, row)
		return true
	})
	return result
}
