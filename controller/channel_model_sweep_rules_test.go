package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsRedundantPrefixedModel(t *testing.T) {
	names := map[string]struct{}{"deepseek-v4-flash": {}, "deepseek-ai/deepseek-v4-flash": {}, "z-ai/glm-5.3": {}}
	require.True(t, isRedundantPrefixedModel("deepseek-ai/deepseek-v4-flash", names))
	require.False(t, isRedundantPrefixedModel("z-ai/glm-5.3", names), "no bare twin")
	require.False(t, isRedundantPrefixedModel("deepseek-v4-flash", names), "bare names are never redundant")
}

func TestMarkUnlistedModelsFailed(t *testing.T) {
	verdicts := map[string]channelModelSweepVerdict{
		"gone-and-erroring": sweepVerdictTemporary,
		"gone-but-answers":  sweepVerdictPass,
		"listed-erroring":   sweepVerdictTemporary,
		"alias":             sweepVerdictInconclusive,
		"alias-gone":        sweepVerdictAccount,
	}
	existing := []string{"gone-and-erroring", "gone-but-answers", "listed-erroring", "alias", "alias-gone", "untested"}
	mapping := map[string]string{"alias": "vendor/alias", "alias-gone": "vendor/alias-gone"}
	unlisted := markUnlistedModelsFailed(existing, []string{"listed-erroring", "vendor/alias"}, mapping, verdicts)

	require.ElementsMatch(t, []string{"gone-and-erroring", "alias-gone"}, unlisted)
	require.Equal(t, sweepVerdictFail, verdicts["gone-and-erroring"])
	require.Equal(t, sweepVerdictFail, verdicts["alias-gone"])
	require.Equal(t, sweepVerdictPass, verdicts["gone-but-answers"], "a passing test outranks the list")
	require.Equal(t, sweepVerdictTemporary, verdicts["listed-erroring"], "still listed: the failure may be the channel")
	require.Equal(t, sweepVerdictInconclusive, verdicts["alias"], "listed under its upstream id")
	_, tested := verdicts["untested"]
	require.False(t, tested, "a model the budget never reached is left alone")
}

func TestSweepLooksLikeOutage(t *testing.T) {
	existing := []string{"a", "b"}
	require.True(t, sweepLooksLikeOutage(existing, map[string]channelModelSweepVerdict{"a": sweepVerdictTemporary, "b": sweepVerdictAccount}))
	require.True(t, sweepLooksLikeOutage(existing, map[string]channelModelSweepVerdict{"a": sweepVerdictInconclusive}))
	require.False(t, sweepLooksLikeOutage(existing, map[string]channelModelSweepVerdict{"a": sweepVerdictTemporary, "b": sweepVerdictPass}))
	require.False(t, sweepLooksLikeOutage(existing, map[string]channelModelSweepVerdict{"a": sweepVerdictFail, "b": sweepVerdictTemporary}), "a definite answer means the upstream is talking")
	require.False(t, sweepLooksLikeOutage(existing, map[string]channelModelSweepVerdict{}), "nothing tested says nothing")
}

func TestCollectPendingSkipsPrefixedTwinOfListedBareModel(t *testing.T) {
	add, _, mapping := collectPendingUpstreamModelChangesFromModels(nil,
		[]string{"deepseek-v4-flash", "deepseek-ai/deepseek-v4-flash", "z-ai/glm-5.3"}, nil, nil, true)
	require.ElementsMatch(t, []string{"deepseek-v4-flash", "glm-5.3"}, add)
	require.Equal(t, map[string]string{"glm-5.3": "z-ai/glm-5.3"}, mapping)
}
