package model

import (
	"github.com/QuantumNous/new-api/common"
)

// ChannelAffinityPinVerdict is why a pinned channel was kept or dropped. The
// reason travels back to the caller so it can be written into the request's
// admin log: "affinity ignored" is otherwise indistinguishable from "affinity
// never matched" when reading the logs of a routing complaint.
type ChannelAffinityPinVerdict int

const (
	// ChannelAffinityPinValid means the pin still points at the best-ranked
	// channel the admin configured for this request.
	ChannelAffinityPinValid ChannelAffinityPinVerdict = iota
	// ChannelAffinityPinUnusable means the channel can no longer serve this
	// request at all: disabled, removed from the group or model, or filtered out
	// by request path.
	ChannelAffinityPinUnusable
	// ChannelAffinityPinOutranked means the channel is still usable but the admin
	// has since configured another channel at a strictly higher priority.
	ChannelAffinityPinOutranked
)

// ValidateChannelAffinityPin reports whether a pinned channel should still serve
// this request.
//
// It answers two independent questions, both against the SAME candidate set
// selection would build — including the Advanced Custom request-path filter, so
// a higher-priority channel that cannot serve this path never invalidates a pin
// that can.
//
// A channel outranks the pin only when the admin configured it strictly higher
// AND it is still strictly higher after dynamic scoring. Scores alone never
// break a pin: the pin exists to keep a conversation on one upstream so its
// prompt cache stays warm, and dynamic offsets move on ordinary traffic —
// letting them outrank would churn the pin constantly. An admin editing
// priority is a different matter: that is an explicit instruction, and it takes
// effect on the next request instead of waiting out the affinity TTL.
//
// Scores can, however, keep a pin. Comparing configured priority alone let a
// top-priority channel that scoring had demoted for failing retire every pin
// below it on every request, so each request went back through selection, and
// every time the demotion decayed it landed on the failing channel again.
// Affinity never held anywhere except on the channel that was broken.
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
	// applyDynamicScores keeps order and length, so index i names the same channel
	// in both slices.
	scored := applyDynamicScores(group, modelName, candidates)

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
	for i, candidate := range candidates {
		// A saturated channel cannot take this request, so it is in no position to
		// displace the pin. Counting it anyway produced pure churn: the pin was
		// dropped for a channel selection would then skip, selection fell back to the
		// same lower-tier channel, and it was repinned — every request, for as long
		// as the higher-priority channel stayed full. That drops the key's warm
		// upstream on every request while never once routing to the channel the drop
		// was made for.
		if candidate.isSaturated() {
			continue
		}
		if candidate.priority > candidates[pinnedIndex].priority && scored[i].priority > scored[pinnedIndex].priority {
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
