package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/channel_score"
)

// ChannelAffinityPinVerdict is why a pinned channel was kept or dropped. The
// reason travels back to the caller so it can be written into the request's
// admin log: "affinity ignored" is otherwise indistinguishable from "affinity
// never matched" when reading the logs of a routing complaint.
type ChannelAffinityPinVerdict int

const (
	// ChannelAffinityPinValid means the pin should keep serving this request.
	ChannelAffinityPinValid ChannelAffinityPinVerdict = iota
	// ChannelAffinityPinUnusable means the channel can no longer serve this
	// request at all: disabled, removed from the group or model, or filtered out
	// by request path.
	ChannelAffinityPinUnusable
	// ChannelAffinityPinOutranked means the channel is still usable but another
	// one is clearly better placed: configured strictly higher while dynamic
	// scoring is off, or at least affinityPinOutrankMarginTiers tiers above the
	// pin's scored position while it is on.
	ChannelAffinityPinOutranked
)

// affinityPinOutrankMarginTiers is the hysteresis band, in tiers of the route's
// ladder, by which another usable channel must lead the pin's scored position
// before revalidation retires a healthy pin.
//
// Two, because one is what ordinary traffic moves: a single fault demotes a
// channel one tier (FaultsToDemote defaults to 1, and 429 counts), and the next
// five successes promote it one tier back. A one-tier margin re-ranked pins on
// exactly that noise; two means the pinned channel itself must have kept failing,
// or another channel must sit two whole configured tiers above it.
const affinityPinOutrankMarginTiers = 2

// ValidateChannelAffinityPin reports whether a pinned channel should still serve
// this request.
//
// It compares against the SAME candidate set selection would build, including
// the Advanced Custom request-path filter, so a higher-priority channel that
// cannot serve this path never invalidates a pin that can. Saturated and cooling
// channels are passed over the way selection passes over them.
//
// With dynamic scoring off, configured priority is the whole ranking and a
// strictly higher channel outranks the pin, so an admin priority edit takes
// effect on the next request instead of waiting out the affinity TTL. Nothing
// moves between requests in that mode, so it cannot flap.
//
// With scoring on, dynamic priority decides where a NEW pin goes (selection) and
// the failover order when the pinned channel errors (the fault release in
// service.RecordChannelAffinity). It must not, by moving alone, break a pin that
// is answering. The previous rule retired a pin whenever some channel was both
// configured higher and scored higher; after a failover from A to B that is true
// again the moment A's demotion is earned back — five successes or one idle
// period — so every key pinned to B was torn off its warm upstream with no
// failure anywhere, and a single 429 on A started the cycle over.
//
// The rule is now a hysteresis band on the route's tier ladder:
//
//   - the pin is measured at its scored position, so its own faults (including
//     those other requests saw) count against it and its promotions protect it;
//   - every other channel is measured at the worse of its scored and baseline
//     position, so a promotion never helps it displace a pin and a demotion
//     still disqualifies it;
//   - the pin is outranked only when one of them leads by at least
//     affinityPinOutrankMarginTiers tiers.
func ValidateChannelAffinityPin(channelId int, group string, modelName string, requestPath string) ChannelAffinityPinVerdict {
	if channelId <= 0 {
		return ChannelAffinityPinUnusable
	}
	candidates, err := affinityCandidateSnapshot(group, modelName, requestPath)
	if err != nil || len(candidates) == 0 {
		// The candidate set could not be established (a DB error on the no-cache
		// path). Keeping the pin preserves today's behaviour rather than dropping
		// warm pins because of an unrelated failure; if the channel really is gone,
		// the distributor's own usability check still catches it.
		return ChannelAffinityPinValid
	}

	pinnedIndex := -1
	for i, candidate := range candidates {
		if candidate.channelId == channelId {
			pinnedIndex = i
			break
		}
	}
	if pinnedIndex < 0 {
		return ChannelAffinityPinUnusable
	}

	scoringEnabled := channel_score.Enabled()
	var tiers []int64
	var scored []channelCandidate
	pinnedPosition := 0
	if scoringEnabled {
		// The ladder is built from the baseline priorities of the same candidates
		// ApplyToCandidates ranks, so positions here are the tiers selection walks.
		baselines := make([]int64, 0, len(candidates))
		for _, candidate := range candidates {
			baselines = append(baselines, candidate.priority)
		}
		tiers = channel_score.DistinctBaselineTiers(baselines)
		// applyDynamicScores keeps order and length, so index i names the same
		// channel in both slices.
		scored = applyDynamicScores(group, modelName, candidates)
		pinnedPosition = channel_score.TierPosition(tiers, scored[pinnedIndex].priority)
	}

	for i, candidate := range candidates {
		if i == pinnedIndex {
			continue
		}
		// A saturated channel cannot take this request, so it is in no position to
		// displace the pin. Counting it anyway produced pure churn: the pin was
		// dropped for a channel selection would then skip, selection fell back to the
		// same lower-tier channel, and it was repinned — every request, for as long
		// as the higher-priority channel stayed full.
		//
		// A cooling channel is passed over for the same reason: selection would skip
		// it too.
		if candidate.isSaturated() || candidate.coolingDown {
			continue
		}
		if !scoringEnabled {
			if candidate.priority > candidates[pinnedIndex].priority {
				return ChannelAffinityPinOutranked
			}
			continue
		}
		rivalPosition := max(
			channel_score.TierPosition(tiers, scored[i].priority),
			channel_score.TierPosition(tiers, candidate.priority),
		)
		if pinnedPosition-rivalPosition >= affinityPinOutrankMarginTiers {
			return ChannelAffinityPinOutranked
		}
	}
	return ChannelAffinityPinValid
}

// affinityCandidateSnapshot builds the eligible candidate set for group/model/path
// without acquiring a concurrency slot or selecting anything. It reuses the same
// builders the two selection paths use (buildCachedCandidates / buildDBCandidates),
// so the revalidation verdict cannot disagree with what selection would actually
// do. Missing-from-cache channels are skipped rather than treated as fatal
// (strictMissing=false): a pin verdict must never fail a request over a transient
// cache gap.
func affinityCandidateSnapshot(group string, modelName string, requestPath string) ([]channelCandidate, error) {
	if common.MemoryCacheEnabled {
		channelSyncLock.RLock()
		defer channelSyncLock.RUnlock()
		return buildCachedCandidates(group, modelName, requestPath, false)
	}
	return buildDBCandidates(group, modelName, requestPath)
}
