package service

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// affinityPinnedChannelForTest reads back whichever channel the affinity cache
// currently holds for this model/user, or 0 when the key is unpinned.
func affinityPinnedChannelForTest(t *testing.T, model, userID string) int {
	t.Helper()
	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	channelID, found := GetPreferredChannelByAffinity(ctx, model, "default")
	if !found {
		return 0
	}
	return channelID
}

// TestChannelAffinityReleasedWhenThePinnedChannelFaults covers the reported bug:
// a channel that keeps failing stayed pinned because the failure path returned
// without touching the pin, and an occasional success in between refreshed its
// TTL. A genuine fault on the pinned channel must now retract it.
func TestChannelAffinityReleasedWhenThePinnedChannelFaults(t *testing.T) {
	const (
		model         = "claude-3-5-sonnet"
		userID        = "user-fault-release"
		pinnedChannel = 41
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)
	require.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"precondition: the channel is pinned")

	// A request that lands on the pinned channel and then faults on it.
	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	require.Equal(t, pinnedChannel, preferred)
	MarkChannelAffinityUsed(ctx, "default", preferred)
	MarkChannelAffinityChannelFault(ctx, preferred)
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, preferred)

	assert.Zero(t, affinityPinnedChannelForTest(t, model, userID),
		"a fault on the pinned channel must release the pin")
}

// TestChannelAffinityKeptWhenTheFailureIsNotAChannelFault protects the pin from
// everything that is not the channel's fault. Rate limits, client errors and our
// own misconfiguration all reach the failure path, and none of them is evidence
// that the upstream is bad — the caller only marks a fault when
// IsChannelFaultError accepted the error.
func TestChannelAffinityKeptWhenTheFailureIsNotAChannelFault(t *testing.T) {
	const (
		model         = "claude-3-5-sonnet"
		userID        = "user-non-fault"
		pinnedChannel = 42
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	MarkChannelAffinityUsed(ctx, "default", preferred)
	// No MarkChannelAffinityChannelFault: this failure was classified as not the
	// channel's fault.
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, preferred)

	assert.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"a non-fault failure must not cost the channel its pin")
}

// TestChannelAffinityKeptWhenAnotherChannelFaults makes sure a fault is
// attributed to the channel that actually produced it. The retry loop moves on to
// other channels, and their failures say nothing about the pinned one.
func TestChannelAffinityKeptWhenAnotherChannelFaults(t *testing.T) {
	const (
		model           = "claude-3-5-sonnet"
		userID          = "user-other-fault"
		pinnedChannel   = 43
		fallbackChannel = 44
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	MarkChannelAffinityUsed(ctx, "default", preferred)
	// The fault happened after the retry loop moved to a different channel.
	MarkChannelAffinityChannelFault(ctx, fallbackChannel)
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, fallbackChannel)

	assert.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"a fallback channel's fault must not release the pinned channel")
}

// TestChannelAffinityReleasedWhenThePinnedChannelFaultsBeforeAFallbackDoes covers
// the multi-retry shape of the reported bug. The retry loop calls the fault marker
// once per failed attempt, so with a single-slot marker the fallback's fault
// overwrote the pinned channel's own and the release check then compared the wrong
// channel, decided the pin was healthy, and kept it forever — "every channel
// failed but it stays on the old one".
func TestChannelAffinityReleasedWhenThePinnedChannelFaultsBeforeAFallbackDoes(t *testing.T) {
	const (
		model           = "claude-3-5-sonnet"
		userID          = "user-fault-then-fallback-fault"
		pinnedChannel   = 61
		fallbackChannel = 62
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)
	require.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"precondition: the channel is pinned")

	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	require.Equal(t, pinnedChannel, preferred)
	MarkChannelAffinityUsed(ctx, "default", preferred)

	// Attempt 1 faults on the pinned channel, attempt 2 faults on the fallback the
	// retry loop moved to. The request ends attributed to the fallback.
	MarkChannelAffinityChannelFault(ctx, preferred)
	MarkChannelAffinityChannelFault(ctx, fallbackChannel)
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, fallbackChannel)

	assert.Zero(t, affinityPinnedChannelForTest(t, model, userID),
		"a later fallback fault must not hide the pinned channel's own fault")
}

// TestChannelAffinityFaultsAccumulateAcrossRetries pins down the accumulation
// itself, independent of the cache: every faulted attempt stays queryable and
// channels that never faulted stay clean, in either arrival order.
func TestChannelAffinityFaultsAccumulateAcrossRetries(t *testing.T) {
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	ctx := newClaudeMessagesRequestForTest(t, "claude-3-5-sonnet", "user-accumulate")

	MarkChannelAffinityChannelFault(ctx, 71)
	MarkChannelAffinityChannelFault(ctx, 72)
	MarkChannelAffinityChannelFault(ctx, 71) // a repeat must stay idempotent

	assert.True(t, channelAffinityChannelFaulted(ctx, 71))
	assert.True(t, channelAffinityChannelFaulted(ctx, 72))
	assert.False(t, channelAffinityChannelFaulted(ctx, 73), "a channel that never faulted")
	assert.False(t, channelAffinityChannelFaulted(ctx, 0), "an invalid id is never faulted")
	assert.False(t, channelAffinityChannelFaulted(nil, 71), "a nil context is never faulted")
}

// TestChannelAffinityKeptWhenAffinityDidNotChooseTheChannel guards the case where
// the request never used the pin at all (cache miss, or the pinned channel was
// skipped). There is no pin of ours to retract, so a fault must not delete
// somebody else's entry.
func TestChannelAffinityKeptWhenAffinityDidNotChooseTheChannel(t *testing.T) {
	const (
		model         = "claude-3-5-sonnet"
		userID        = "user-not-chosen"
		pinnedChannel = 45
		otherChannel  = 46
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	_, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	// MarkChannelAffinityUsed is never called: affinity did not select the channel
	// that served this request.
	MarkChannelAffinityChannelFault(ctx, otherChannel)
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, otherChannel)

	assert.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"a request that did not use the pin must not release it")
}

// TestChannelAffinityKeptWhenThePinnedChannelWasOnlySaturated keeps the existing
// saturation semantics intact: being full is not a fault, and the pin is meant to
// survive so the key returns to its warm upstream once the load drains.
func TestChannelAffinityKeptWhenThePinnedChannelWasOnlySaturated(t *testing.T) {
	const (
		model         = "claude-3-5-sonnet"
		userID        = "user-saturated"
		pinnedChannel = 47
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	MarkChannelAffinityUsed(ctx, "default", preferred)
	SetChannelAffinityBypassed(ctx)
	MarkChannelAffinityChannelFault(ctx, preferred)
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, preferred)

	assert.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"saturation is not a fault, so the pin survives")
}

// TestSuccessfulRequestStillRepins is a guard that adding failure accounting did
// not disturb the success path.
func TestSuccessfulRequestStillRepins(t *testing.T) {
	const (
		model            = "claude-3-5-sonnet"
		userID           = "user-still-repins"
		pinnedChannel    = 48
		succeededChannel = 49
	)
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	_, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.True(t, found)
	ctx.Set("channel_id", succeededChannel)
	SetChannelAffinityRelayOutcome(ctx, true)
	RecordChannelAffinity(ctx, succeededChannel)

	assert.Equal(t, succeededChannel, affinityPinnedChannelForTest(t, model, userID),
		"SwitchOnSuccess must still follow a real channel switch")
}

// TestFaultReleaseIsANoOpWhenAffinityIsDisabled checks the feature gate: with
// affinity off there is no pin to manage and the helpers must stay inert.
func TestFaultReleaseIsANoOpWhenAffinityIsDisabled(t *testing.T) {
	current := operation_setting.GetChannelAffinitySetting()
	require.NotNil(t, current)
	original := *current
	*current = operation_setting.ChannelAffinitySetting{Enabled: false}
	t.Cleanup(func() { *current = original })

	ctx := newClaudeMessagesRequestForTest(t, "claude-3-5-sonnet", "user-disabled")
	MarkChannelAffinityUsed(ctx, "default", 50)
	MarkChannelAffinityChannelFault(ctx, 50)
	SetChannelAffinityRelayOutcome(ctx, false)
	RecordChannelAffinity(ctx, 50)

	_, found := GetPreferredChannelByAffinity(ctx, "claude-3-5-sonnet", "default")
	assert.False(t, found, "affinity is off, so nothing is pinned either way")
}

func TestMarkChannelAffinityChannelFaultIgnoresInvalidInput(t *testing.T) {
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	ctx := newClaudeMessagesRequestForTest(t, "claude-3-5-sonnet", "user-invalid")

	// Neither of these should panic or record anything.
	MarkChannelAffinityChannelFault(nil, 5)
	MarkChannelAffinityChannelFault(ctx, 0)
	MarkChannelAffinityChannelFault(ctx, -1)

	_, exists := ctx.Get(ginKeyChannelAffinityFaultChannel)
	assert.False(t, exists, "an invalid channel id must not be recorded as a fault")
}
