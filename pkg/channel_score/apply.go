package channel_score

import (
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// ApplyToCandidates returns candidates with dynamic priority and weight applied.
//
// The returned priorities are REQUEST-LOCAL and must never be persisted or
// carried to another request. Movement is expressed in tiers within the ranking
// of the candidate set it is given, so the same channel legitimately resolves to
// different absolute priorities for different requests: in {A:100, B:0} demoting
// A one tier lands on 0, while in {A:100, C:50, B:0} it lands on 50. That is
// deliberate — the question being answered is "where does this channel belong
// among the channels eligible for THIS request", and request-path and model
// filtering change who is eligible.
//
// Invariant: this function only ever reorders and reweights. It never drops a
// candidate, so a set where every channel has been demoted is still the same
// non-empty set in the same relative order, and selection can always proceed.
func ApplyToCandidates(group string, model string, candidates []Candidate) []Candidate {
	setting := operation_setting.GetChannelDynamicScoreSetting()
	if !setting.Enabled || len(candidates) == 0 {
		return candidates
	}
	if !sharedStoreUsable() {
		// Redis is configured but unreachable. Applying per-instance scores here
		// would route each instance by its own private view; leaving the
		// candidates untouched degrades to exactly the admin's configuration.
		return candidates
	}

	tiers := distinctTiersDesc(candidates)
	adjusted := make([]Candidate, len(candidates))
	copy(adjusted, candidates)

	for i := range adjusted {
		snap := lookup(scoreKey(adjusted[i].ChannelId, group, model), setting.IdleResetSeconds)
		if snap == nil {
			continue
		}
		if snap.tierOffset != 0 {
			adjusted[i].Priority = shiftByTiers(tiers, adjusted[i].Priority, snap.tierOffset)
		}
		factor := successRateFactor(snap.total, snap.success, setting.MinSampleForWeight)
		adjusted[i].Weight = applyWeightFactor(adjusted[i].Weight, factor)
	}
	return adjusted
}

// Enabled reports whether dynamic scoring is on and its store is usable. The
// reporting hooks call this to skip work on the request path when the feature is
// off.
func Enabled() bool {
	setting := operation_setting.GetChannelDynamicScoreSetting()
	if !setting.Enabled {
		return false
	}
	return sharedStoreUsable()
}
