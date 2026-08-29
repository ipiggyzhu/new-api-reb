package channel_score

import (
	"math"
	"sort"
)

// weightFactorSteps quantizes the success-rate weight factor to 0.25 increments.
// A continuous factor recomputed on every request makes a channel's weight
// twitch with each sample near the sample threshold; snapping to a step means
// the weight only changes when the success rate crosses a bucket boundary.
const weightFactorStep = 0.25

// distinctTiersDesc returns the distinct priorities present in candidates, in
// descending order. This mirrors how selectChannelCandidate derives its tiers
// from the same list, so a movement expressed in tiers lands on a priority the
// selector will actually walk.
func distinctTiersDesc(candidates []Candidate) []int64 {
	seen := make(map[int64]struct{}, len(candidates))
	tiers := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate.Priority]; ok {
			continue
		}
		seen[candidate.Priority] = struct{}{}
		tiers = append(tiers, candidate.Priority)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] > tiers[j] })
	return tiers
}

// shiftByTiers returns the priority reached by moving offset tiers from priority
// within tiers (which must be sorted descending). A positive offset promotes, a
// negative one demotes.
//
// Movement is expressed in ranks rather than in raw priority units because
// configured priorities span arbitrary magnitudes: subtracting a fixed number
// from a channel at priority 100 leaves it above a sibling at 0, so a numeric
// offset cannot express "this channel should stop being preferred". Walking the
// rank list works whatever scale the admin chose.
//
// Moving past the ends extends beyond the list: demoting past the last tier
// yields lowest-1 (below every existing candidate but still selectable),
// promoting past the first yields highest+1. That keeps the invariant that this
// package only ever reorders candidates and never removes one.
func shiftByTiers(tiers []int64, priority int64, offset int) int64 {
	if offset == 0 || len(tiers) == 0 {
		return priority
	}
	index := -1
	for i, tier := range tiers {
		if tier == priority {
			index = i
			break
		}
	}
	if index < 0 {
		return priority
	}
	// tiers is descending, so a lower index is a higher priority: promoting
	// means moving toward index 0.
	target := index - offset
	if target < 0 {
		return addClamped(tiers[0], int64(-target))
	}
	if target >= len(tiers) {
		return addClamped(tiers[len(tiers)-1], -int64(target-len(tiers)+1))
	}
	return tiers[target]
}

// addClamped returns base+delta, saturating at the int64 bounds instead of
// wrapping.
//
// Priorities are admin-supplied int64s, so a channel configured at (or near)
// math.MaxInt64 that gets promoted past the top tier would otherwise wrap to a
// large negative number — turning the promotion into the harshest possible
// demotion. Saturating keeps the sign of the intended movement, which is the
// only property the ranking depends on.
func addClamped(base int64, delta int64) int64 {
	if delta > 0 && base > math.MaxInt64-delta {
		return math.MaxInt64
	}
	if delta < 0 && base < math.MinInt64-delta {
		return math.MinInt64
	}
	return base + delta
}

// successRateFactor converts a success rate into a weight multiplier, or 1.0
// when the sample is too small to mean anything.
//
// Below minSample the factor is exactly 1.0: a success rate measured over a
// handful of requests carries no signal, and acting on it would swing weight
// around on noise. Above it the factor spans 0.5 (never succeeds) to 1.5
// (always succeeds), quantized so the weight is stable inside a bucket.
func successRateFactor(total int64, success int64, minSample int) float64 {
	if minSample <= 0 {
		minSample = 1
	}
	if total < int64(minSample) || total <= 0 {
		return 1.0
	}
	rate := float64(success) / float64(total)
	if rate < 0 {
		rate = 0
	} else if rate > 1 {
		rate = 1
	}
	factor := 0.5 + rate
	// Snap to the nearest step so the factor only moves on a bucket crossing.
	steps := int(factor/weightFactorStep + 0.5)
	return float64(steps) * weightFactorStep
}

// applyWeightFactor scales weight, keeping a configured weight of at least 1 at
// 1 or above. effectiveChannelWeight adds its baseline on top, so a candidate
// that scaled to 0 would still be reachable; the floor exists so a penalized
// channel keeps some relative weighting inside its tier rather than having the
// operator's ordering erased.
//
// A configured weight of 0 stays 0. Zero is a supported configuration meaning
// "draw at the baseline share", and it has no relative weighting to preserve.
// Flooring it to 1 would raise its effective weight above an untouched sibling
// at 0 — so a channel being penalized for a poor success rate would end up
// drawing more traffic than before, which inverts the whole point.
func applyWeightFactor(weight int, factor float64) int {
	if factor == 1.0 || weight <= 0 {
		return weight
	}
	scaled := int(float64(weight) * factor)
	if scaled < 1 {
		scaled = 1
	}
	return scaled
}
