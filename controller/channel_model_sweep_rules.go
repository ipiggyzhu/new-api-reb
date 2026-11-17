package controller

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

// channelSweepOutageDisableRuns is how many sweeps in a row a channel may
// answer nothing usable before it is taken out of rotation. A sweep that
// cannot tell whether any model works (dead host, expired key, 5xx) removes no
// model, because the models are not what is broken; without this the channel
// kept advertising every one of them indefinitely.
const channelSweepOutageDisableRuns = 3

// bareModelName is the part of an upstream id after its vendor prefix
// ("deepseek-ai/deepseek-v4-flash" -> "deepseek-v4-flash"), or "" when the id
// has no prefix.
func bareModelName(name string) string {
	slash := strings.LastIndex(name, "/")
	if slash < 0 {
		return ""
	}
	return strings.TrimSpace(name[slash+1:])
}

// isRedundantPrefixedModel reports a vendor-prefixed id whose bare name is in
// names as well: the same model listed twice.
func isRedundantPrefixedModel(name string, names map[string]struct{}) bool {
	bare := bareModelName(name)
	if bare == "" {
		return false
	}
	_, ok := names[bare]
	return ok
}

// markUnlistedModelsFailed turns the verdict of every model the upstream no
// longer lists into a failure, unless its own test passed. The list alone is
// not trusted (some upstreams serve more than they list), and neither is a
// failed test alone (it may be the channel); the two together are a retired
// model. A model reached through a mapping is looked up under its upstream id.
func markUnlistedModelsFailed(existing []string, upstreamModels []string, mapping map[string]string, verdicts map[string]channelModelSweepVerdict) []string {
	listed := make(map[string]struct{}, len(upstreamModels))
	for _, name := range upstreamModels {
		listed[name] = struct{}{}
	}
	var unlisted []string
	for _, name := range existing {
		verdict, tested := verdicts[name]
		if !tested || verdict == sweepVerdictPass || verdict == sweepVerdictFail {
			continue
		}
		if _, ok := listed[name]; ok {
			continue
		}
		if target := mapping[name]; target != "" {
			if _, ok := listed[target]; ok {
				continue
			}
		}
		verdicts[name] = sweepVerdictFail
		unlisted = append(unlisted, name)
	}
	return unlisted
}

// sweepLooksLikeOutage reports a sweep in which the channel's models were
// tested and not one of them got a definite answer either way.
func sweepLooksLikeOutage(existing []string, verdicts map[string]channelModelSweepVerdict) bool {
	tested := 0
	for _, name := range existing {
		verdict, ok := verdicts[name]
		if !ok {
			continue
		}
		tested++
		if verdict == sweepVerdictPass || verdict == sweepVerdictFail {
			return false
		}
	}
	return tested > 0
}

// recordChannelSweepOutage keeps the count of consecutive outage sweeps on the
// channel and disables it once the count reaches the limit. It returns whether
// the channel was disabled.
func recordChannelSweepOutage(channelID int, outage bool) bool {
	runs := 0
	err := model.MutateChannelSettings(channelID, func(settings *dto.ChannelOtherSettings) bool {
		if !outage {
			if settings.UpstreamModelUpdateOutageRuns == 0 {
				return false
			}
			settings.UpstreamModelUpdateOutageRuns = 0
			return true
		}
		settings.UpstreamModelUpdateOutageRuns++
		runs = settings.UpstreamModelUpdateOutageRuns
		return true
	})
	if err != nil {
		common.SysLog(fmt.Sprintf("channel model sweep: could not record outage state for channel %d: %v", channelID, err))
		return false
	}
	if runs < channelSweepOutageDisableRuns {
		return false
	}
	reason := fmt.Sprintf("Model availability sync got no usable answer from this upstream %d times in a row", runs)
	if !model.UpdateChannelStatus(channelID, "", common.ChannelStatusAutoDisabled, reason) {
		return false
	}
	_ = model.MutateChannelSettings(channelID, func(settings *dto.ChannelOtherSettings) bool {
		settings.UpstreamModelUpdateOutageRuns = 0
		return true
	})
	common.SysLog(fmt.Sprintf("channel model sweep: channel %d disabled: %s", channelID, reason))
	return true
}
