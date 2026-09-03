package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/channel_score"
)

// This file writes the dynamic-score projection into channels.effective_priority
// and channels.effective_weight.
//
// It exists because the adjustment was invisible. Scoring shifted a channel
// inside one request's candidate ranking and was never written down, so both admin
// columns kept showing whatever was typed however far routing had moved, and the
// only reasonable conclusion from the list was that the feature did nothing.
//
// Three rules make writing it down safe, and each one is load-bearing:
//
//  1. The baseline columns are never written here. Only priority and weight are
//     edited by an admin, and every tier calculation starts from them, so the
//     projection cannot feed itself.
//  2. Every run recomputes ABSOLUTELY from that baseline. Nothing is incremental,
//     so a missed run, a double run, and a run after a restart all produce the
//     same result, and TestWritingShiftedPriorityBackCompounds' unbounded walk
//     cannot happen.
//  3. The selection path never reads these columns. It recomputes the same
//     movement from the same baseline on every request, so the projection interval
//     controls only how fresh the DISPLAY is, never how fast routing reacts to a
//     failing channel.

// ChannelScoreProjectionResult reports what one run did, for the task row.
type ChannelScoreProjectionResult struct {
	Scanned   int `json:"scanned"`
	Projected int `json:"projected"`
	Cleared   int `json:"cleared"`
	Updated   int `json:"updated"`
}

// RunChannelScoreProjection refreshes the projected columns for every channel.
//
// Returns the counts rather than logging them so the caller can record one task
// result; a run that changes nothing is the normal case and must stay silent.
func RunChannelScoreProjection() (ChannelScoreProjectionResult, error) {
	result := ChannelScoreProjectionResult{}

	if !channel_score.ProjectionUsable() {
		// Scoring is off or its store is unreachable, so routing has already
		// reverted to the baseline. Leaving a projection in the column would report
		// an adjustment that is no longer in force, which is worse than showing
		// nothing: an operator would tune against a number that stopped moving.
		cleared, err := clearAllChannelProjections()
		result.Cleared = cleared
		result.Updated = cleared
		return result, err
	}

	projections, _ := channel_score.ProjectByChannel()

	var channels []*Channel
	// Only the columns the projection needs. Channels carry keys and settings blobs
	// that have no business being read by a periodic job.
	err := DB.Model(&Channel{}).
		Select("id", "priority", "weight", "status", "effective_priority", "effective_weight").
		Where("status = ?", common.ChannelStatusEnabled).
		Find(&channels).Error
	if err != nil {
		return result, err
	}
	result.Scanned = len(channels)

	// The tier ladder every route resolves against. Built once from the baseline
	// priorities of all enabled channels, never from the projected ones.
	//
	// This is the projection's one documented approximation: ApplyToCandidates
	// builds its tiers from the candidates eligible for a single (group, model),
	// which is a subset of these. A channel whose route has fewer distinct tiers
	// than the global ladder can therefore be shown one tier away from where that
	// route would actually place it. Using the global ladder keeps the column
	// stable — a number that changed because an unrelated channel was edited would
	// be unreadable — and the per-route detail stays available in the badge.
	baselines := make([]int64, 0, len(channels))
	for _, channel := range channels {
		baselines = append(baselines, channel.GetPriority())
	}
	tiers := channel_score.DistinctBaselineTiers(baselines)

	for _, channel := range channels {
		projection, scored := projections[channel.Id]
		if !scored {
			// No active route: this channel is at its baseline, and the mirror has to
			// say so by being absent rather than by repeating the baseline.
			if channel.EffectivePriority != nil || channel.EffectiveWeight != nil {
				if err := clearChannelProjection(channel.Id); err != nil {
					return result, err
				}
				result.Cleared++
				result.Updated++
			}
			continue
		}
		result.Projected++

		priority := channel_score.EffectivePriority(tiers, channel.GetPriority(), projection.TierOffset)
		weight := channel_score.EffectiveWeight(channel.GetWeight(), projection.WeightFactor)

		if channel.EffectivePriority != nil && *channel.EffectivePriority == priority &&
			channel.EffectiveWeight != nil && int(*channel.EffectiveWeight) == weight {
			// Unchanged. Skipping the write is what keeps a run on a quiet
			// deployment free of database traffic entirely.
			continue
		}
		if err := writeChannelProjection(channel.Id, priority, weight); err != nil {
			return result, err
		}
		result.Updated++
	}
	return result, nil
}

// writeChannelProjection updates only the two derived columns.
//
// Column-scoped on purpose. A full-struct save would carry every other field of a
// row this job read minutes ago, so a concurrent admin edit or upstream-model sync
// would be silently reverted — the same lost-update shape that made
// MutateChannelSettings necessary. Naming the columns means the two writers touch
// disjoint sets and cannot clobber each other.
func writeChannelProjection(channelId int, priority int64, weight int) error {
	if weight < 0 {
		weight = 0
	}
	return DB.Model(&Channel{}).Where("id = ?", channelId).
		Updates(map[string]any{
			"effective_priority": priority,
			"effective_weight":   uint(weight),
		}).Error
}

// clearChannelProjection returns one channel's mirror to NULL, meaning "no
// projection in force, use the baseline".
func clearChannelProjection(channelId int) error {
	return DB.Model(&Channel{}).Where("id = ?", channelId).
		Updates(map[string]any{
			"effective_priority": nil,
			"effective_weight":   nil,
		}).Error
}

// clearAllChannelProjections wipes every mirror in one statement, for when scoring
// is switched off or its store goes unreachable.
//
// Filtered to rows that actually hold a projection so the common case — scoring
// disabled, nothing ever projected — costs one indexless scan and no writes,
// rather than rewriting every channel row on every run.
func clearAllChannelProjections() (int, error) {
	tx := DB.Model(&Channel{}).
		Where("effective_priority IS NOT NULL OR effective_weight IS NOT NULL").
		Updates(map[string]any{
			"effective_priority": nil,
			"effective_weight":   nil,
		})
	if tx.Error != nil {
		return 0, tx.Error
	}
	return int(tx.RowsAffected), nil
}

// ClearChannelProjection drops one channel's mirror immediately.
//
// Called from the admin edit and score-reset paths: those already reset the score
// itself, and leaving the projected number behind would leave the list showing an
// adjustment that was just discarded until the next scheduled run.
func ClearChannelProjection(channelId int) {
	if channelId <= 0 {
		return
	}
	if err := clearChannelProjection(channelId); err != nil {
		common.SysLog(fmt.Sprintf("failed to clear channel score projection: channel_id=%d, error=%v", channelId, err))
	}
}

// ClearChannelProjections is the batch form, for the tag paths.
func ClearChannelProjections(channelIds []int) {
	if len(channelIds) == 0 {
		return
	}
	err := DB.Model(&Channel{}).Where("id IN ?", channelIds).
		Updates(map[string]any{
			"effective_priority": nil,
			"effective_weight":   nil,
		}).Error
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to clear channel score projections: count=%d, error=%v", len(channelIds), err))
	}
}
