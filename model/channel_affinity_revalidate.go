package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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
// Priority is compared using the ADMIN-CONFIGURED value only; dynamic scores are
// deliberately not consulted. The pin exists to keep a conversation on one
// upstream so its prompt cache stays warm, and dynamic offsets move on ordinary
// traffic — letting them break the pin would churn it constantly and cost far
// more in lost cache hits than the reordering could win. An admin editing
// priority is a different matter: that is an explicit instruction, and it takes
// effect on the next request instead of waiting out the affinity TTL.
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

	pinnedPriority, found := int64(0), false
	highestPriority, haveHighest := int64(0), false
	for _, candidate := range candidates {
		if candidate.channelId == channelId {
			pinnedPriority, found = candidate.priority, true
		}
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
		if !haveHighest || candidate.priority > highestPriority {
			highestPriority, haveHighest = candidate.priority, true
		}
	}
	if !found {
		return ChannelAffinityPinUnusable
	}
	if haveHighest && pinnedPriority < highestPriority {
		return ChannelAffinityPinOutranked
	}
	return ChannelAffinityPinValid
}

// affinityCandidateSnapshot builds the eligible candidate set for group/model/path
// without acquiring a concurrency slot or selecting anything. It mirrors the two
// selection paths' filtering (including the normalized-model fallback) so the
// revalidation verdict cannot disagree with what selection would actually do.
func affinityCandidateSnapshot(group string, modelName string, requestPath string) ([]channelCandidate, error) {
	if common.MemoryCacheEnabled {
		return cachedAffinityCandidates(group, modelName, requestPath), nil
	}

	abilities, err := getEnabledAbilities(group, modelName)
	if err != nil {
		return nil, err
	}
	abilities = filterAbilitiesByRequestPathAndModel(abilities, requestPath, modelName)
	if len(abilities) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(modelName)
		if normalizedModel != modelName {
			abilities, err = getEnabledAbilities(group, normalizedModel)
			if err != nil {
				return nil, err
			}
			abilities = filterAbilitiesByRequestPathAndModel(abilities, requestPath, modelName)
		}
	}
	candidates := make([]channelCandidate, 0, len(abilities))
	for _, ability := range abilities {
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
	return candidates, nil
}

// cachedAffinityCandidates is the memory-cache half of affinityCandidateSnapshot.
// It holds the same read lock the selection path takes and applies the same
// filters.
func cachedAffinityCandidates(group string, modelName string, requestPath string) []channelCandidate {
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	channels := filterChannelsByRequestPathAndModel(group2model2channels[group][modelName], requestPath, modelName)
	if len(channels) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(modelName)
		channels = filterChannelsByRequestPathAndModel(group2model2channels[group][normalizedModel], requestPath, modelName)
	}
	candidates := make([]channelCandidate, 0, len(channels))
	for _, channelId := range channels {
		channel, ok := channelsIDM[channelId]
		if !ok {
			continue
		}
		candidates = append(candidates, channelCandidate{
			channelId:      channelId,
			priority:       channel.GetPriority(),
			weight:         channel.GetWeight(),
			maxConcurrency: channel.GetMaxConcurrency(),
		})
	}
	return candidates
}
