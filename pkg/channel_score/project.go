package channel_score

import (
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// This file projects the per-(group, model) scores onto one value per channel,
// so the admin list can show a priority and a weight that actually move.
//
// The scores themselves stay per route, because that is the resolution at which
// a channel actually fails: an upstream can serve claude-opus-5 perfectly and
// return nothing but errors for gpt-5.6. Collapsing them to one number per
// channel is a deliberate loss, and the rule below chooses which route survives
// the collapse.
//
// Nothing here is read by the selection path. ApplyToCandidates keeps computing
// the shift live from the same baseline and the same score, so the projection is
// a mirror of a decision already being made rather than an input to it. That is
// what keeps the two from disagreeing, and it is why the projection interval can
// be as slow as an operator likes without routing reacting any slower.

// ChannelProjection is one channel's scores reduced to a single route's worth of
// movement, plus the counts needed to say how representative that route is.
type ChannelProjection struct {
	// Group and Model identify the route the projected values were computed for.
	// The tier universe differs per route, so the caller needs to know which one
	// to resolve the baseline tiers against.
	Group string
	Model string

	// TierOffset and WeightFactor are that route's movement.
	TierOffset   int
	WeightFactor float64

	// Active is how many of this channel's routes the selection path is currently
	// applying, and Adjusted/Weighted how many of those actually moved. A
	// projection built from 1 of 40 routes is honest only if the list says so.
	Active   int
	Adjusted int
	Weighted int
}

// worstRouteRule picks the route whose score the channel-level number reports.
//
// It takes the most-demoted route, breaking ties on the lowest weight factor.
// The alternative — an average, or the most-trafficked route — hides the case the
// operator is looking at the column to find: a channel that has started failing
// on one model reads as healthy right up until the failure spreads. Reporting the
// worst means the number moves when something is wrong, and the Active/Adjusted
// counts beside it say how much of the channel that number speaks for.
//
// Promotions are reported the same way only when nothing is demoted, so a channel
// with one promoted route and one demoted route shows the demotion.
func worstRouteRule(candidate ScoreView, best ChannelProjection, haveBest bool) bool {
	if !haveBest {
		return true
	}
	if candidate.TierOffset != best.TierOffset {
		return candidate.TierOffset < best.TierOffset
	}
	return candidate.WeightFactor < best.WeightFactor
}

// ProjectByChannel reduces the store to at most one projection per channel.
//
// Idle rows are skipped entirely rather than counted with a zero offset: the
// selection path may be applying a partially-decayed tier offset, but that is not
// safe to project as one absolute priority. A tier is request-local — it depends
// on which sibling channels match the next request — and an idle route has no
// current candidate set from which to resolve it. A channel whose routes have all
// gone idle is absent from the map, which is the caller's signal to restore the
// configured baseline in the list while routing still retains the standing,
// decaying verdict.
func ProjectByChannel() (map[int]ChannelProjection, ScoreSnapshot) {
	snapshot := Snapshot(ScoreFilter{})
	if len(snapshot.Rows) == 0 {
		return nil, snapshot
	}

	projections := make(map[int]ChannelProjection)
	for _, row := range snapshot.Rows {
		if row.Idle {
			continue
		}
		current, seen := projections[row.ChannelID]
		current.Active++
		if row.TierOffset != 0 {
			current.Adjusted++
		}
		if row.WeightFactor < 1-weightFactorEpsilon || row.WeightFactor > 1+weightFactorEpsilon {
			current.Weighted++
		}
		// The counters above accumulate across every active route; only the route
		// identity and its two values are replaced, and only by a worse route.
		if worstRouteRule(row, current, seen) {
			current.Group = row.Group
			current.Model = row.Model
			current.TierOffset = row.TierOffset
			current.WeightFactor = row.WeightFactor
		}
		projections[row.ChannelID] = current
	}
	// A channel with only idle rows accumulated nothing and must not be reported
	// as projected-at-baseline; that is indistinguishable from "no score" to the
	// caller and would keep a stale mirror alive.
	for channelID, projection := range projections {
		if projection.Active == 0 {
			delete(projections, channelID)
		}
	}
	return projections, snapshot
}

// EffectivePriority is the priority a channel lands on after moving offset tiers
// within tiers, which must be the DISTINCT BASELINE priorities of the candidates
// for the route, in descending order.
//
// Exported so the projection job resolves movement exactly the way
// ApplyToCandidates does. Computing it any other way — adding the offset to the
// priority, say — would put a different number in the column than the one
// routing is using, which is the whole defect this projection exists to fix.
//
// Feeding it a tier list built from already-projected values is the documented
// way to break it: see TestWritingShiftedPriorityBackCompounds. The tiers must
// come from the baseline column every time.
func EffectivePriority(tiers []int64, baseline int64, offset int) int64 {
	return shiftByTiers(tiers, baseline, offset)
}

// DistinctBaselineTiers is distinctTiersDesc over raw priorities, for callers
// that hold baselines rather than Candidates.
func DistinctBaselineTiers(priorities []int64) []int64 {
	candidates := make([]Candidate, 0, len(priorities))
	for _, priority := range priorities {
		candidates = append(candidates, Candidate{Priority: priority})
	}
	return distinctTiersDesc(candidates)
}

// EffectiveWeight is applyWeightFactor, exported for the same reason as
// EffectivePriority: the projected weight has to be the number the selector would
// have computed, including the floor that keeps a penalized channel reachable and
// the rule that leaves a configured 0 at 0.
func EffectiveWeight(baseline int, factor float64) int {
	return applyWeightFactor(baseline, factor)
}

// ProjectionUsable reports whether a projection run should write anything.
//
// False means the mirror must be cleared rather than left as it is: a stale
// projected priority sitting in the column after an admin turned scoring off
// would misreport routing that has already reverted to the baseline.
func ProjectionUsable() bool {
	return operation_setting.GetChannelDynamicScoreSetting().Enabled && sharedStoreUsable()
}
