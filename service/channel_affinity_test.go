package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claudeCliTraceRuleForTest mirrors the production "claude cli trace" rule: the
// affinity key comes from metadata.user_id on /v1/messages, and a failure on the
// pinned channel is not retried.
func claudeCliTraceRuleForTest() operation_setting.ChannelAffinityRule {
	return operation_setting.ChannelAffinityRule{
		Name:       "claude cli trace",
		ModelRegex: []string{"^claude-.*$"},
		PathRegex:  []string{"/v1/messages"},
		KeySources: []operation_setting.ChannelAffinityKeySource{
			{Type: "gjson", Path: "metadata.user_id"},
		},
		TTLSeconds:         120,
		SkipRetryOnFailure: true,
		IncludeUsingGroup:  true,
		IncludeRuleName:    true,
	}
}

// useChannelAffinityRulesForTest installs rules on the global setting and starts
// from an empty affinity cache, restoring both afterwards.
func useChannelAffinityRulesForTest(t *testing.T, switchOnSuccess bool, rules ...operation_setting.ChannelAffinityRule) {
	t.Helper()
	current := operation_setting.GetChannelAffinitySetting()
	require.NotNil(t, current)
	original := *current
	*current = operation_setting.ChannelAffinitySetting{
		Enabled:           true,
		SwitchOnSuccess:   switchOnSuccess,
		MaxEntries:        1000,
		DefaultTTLSeconds: 3600,
		Rules:             rules,
	}
	ClearChannelAffinityCacheAll()
	t.Cleanup(func() {
		*current = original
		ClearChannelAffinityCacheAll()
	})
}

// newClaudeMessagesRequestForTest builds a /v1/messages request context carrying
// the given metadata.user_id, the affinity key the production rule reads.
func newClaudeMessagesRequestForTest(t *testing.T, model string, userID string) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := `{"model":"` + model + `","metadata":{"user_id":"` + userID + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	return ctx
}

// pinChannelByAffinityForTest walks one successful request through the affinity
// path so the channel is left pinned in the cache.
func pinChannelByAffinityForTest(t *testing.T, model, userID string, channelID int) {
	t.Helper()
	ctx := newClaudeMessagesRequestForTest(t, model, userID)
	_, found := GetPreferredChannelByAffinity(ctx, model, "default")
	require.False(t, found, "cache must be cold before seeding")
	SetChannelAffinityRelayOutcome(ctx, true)
	RecordChannelAffinity(ctx, channelID)
}

// ShouldSkipRetryAfterChannelAffinityFailure reads one explicit flag and nothing
// else. It used to fall back to the matched rule's SkipRetryOnFailure whenever no
// flag was set, which leaked the rule's no-retry policy onto requests the
// affinity cache never served: on a cache miss the channel comes from ordinary
// priority/weight selection, and suppressing its retries strands the request on
// one channel. The flag is now written only where affinity actually pinned the
// channel (MarkChannelAffinityUsed) or cleared it (ClearCurrentChannelAffinityCache).
func TestShouldSkipRetryAfterChannelAffinityFailure(t *testing.T) {
	const (
		model  = "claude-opus-5"
		userID = "user-affinity-skip-retry"
	)

	assert.False(t, ShouldSkipRetryAfterChannelAffinityFailure(nil),
		"a nil context carries no affinity decision and must not suppress retries")

	testCases := []struct {
		name             string
		seedPinnedID     int
		affinityUsed     bool
		clearAfterUse    bool
		wantFound        bool
		wantPreferredID  int
		wantSkipRetryNow bool
	}{
		{
			// Cache miss: the channel actually serving this request came from
			// ordinary priority/weight selection, so skip_retry_on_failure must
			// not disable its retries even though the rule matched.
			name:             "cache miss does not inherit rule skip retry",
			wantFound:        false,
			wantSkipRetryNow: false,
		},
		{
			// Cache hit that the distributor could not use (disabled channel kept
			// by keep_on_channel_disabled): affinity picked nothing, no skip.
			name:             "cache hit that affinity never used",
			seedPinnedID:     71,
			wantFound:        true,
			wantPreferredID:  71,
			wantSkipRetryNow: false,
		},
		{
			name:             "cache hit served by affinity",
			seedPinnedID:     72,
			affinityUsed:     true,
			wantFound:        true,
			wantPreferredID:  72,
			wantSkipRetryNow: true,
		},
		{
			name:             "affinity cache cleared after unusable channel",
			seedPinnedID:     73,
			affinityUsed:     true,
			clearAfterUse:    true,
			wantFound:        true,
			wantPreferredID:  73,
			wantSkipRetryNow: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			useChannelAffinityRulesForTest(t, false, claudeCliTraceRuleForTest())
			if tc.seedPinnedID > 0 {
				pinChannelByAffinityForTest(t, model, userID, tc.seedPinnedID)
			}

			ctx := newClaudeMessagesRequestForTest(t, model, userID)
			preferredID, found := GetPreferredChannelByAffinity(ctx, model, "default")
			require.Equal(t, tc.wantFound, found)
			assert.Equal(t, tc.wantPreferredID, preferredID)

			if tc.affinityUsed {
				MarkChannelAffinityUsed(ctx, "default", preferredID)
			}
			if tc.clearAfterUse {
				ClearCurrentChannelAffinityCache(ctx)
			}

			assert.Equal(t, tc.wantSkipRetryNow, ShouldSkipRetryAfterChannelAffinityFailure(ctx))
		})
	}
}

func TestRecordChannelAffinityUsesRelayOutcomeNotResponseStatus(t *testing.T) {
	const (
		model  = "claude-opus-5"
		userID = "user-affinity-stream-failure"
	)

	const (
		outcomeUnreported = "unreported"
		outcomeSuccess    = "success"
		outcomeFailure    = "failure"
	)

	testCases := []struct {
		name string
		// streamStarted models an upstream that answered 200 and began streaming
		// (or a keepalive ping frame) before anything went wrong.
		streamStarted bool
		// finalStatus is the status the handler tries to write when it gives up;
		// gin drops it once the stream committed 200.
		finalStatus  int
		relayOutcome string
		wantPinnedID int
		wantStatus   int
	}{
		{
			name:          "mid stream failure keeps its committed 200",
			streamStarted: true,
			finalStatus:   http.StatusInternalServerError,
			relayOutcome:  outcomeFailure,
			wantPinnedID:  0,
			wantStatus:    http.StatusOK,
		},
		{
			name:          "failure before any byte reaches the wire",
			streamStarted: false,
			finalStatus:   http.StatusInternalServerError,
			relayOutcome:  outcomeFailure,
			wantPinnedID:  0,
			wantStatus:    http.StatusInternalServerError,
		},
		{
			name:          "streamed success pins the channel",
			streamStarted: true,
			relayOutcome:  outcomeSuccess,
			wantPinnedID:  81,
			wantStatus:    http.StatusOK,
		},
		{
			name:         "non streaming success pins the channel",
			relayOutcome: outcomeSuccess,
			wantPinnedID: 81,
			wantStatus:   http.StatusOK,
		},
		{
			// Task submit handlers publish no verdict and cannot half-succeed, so
			// they keep being judged on the status they actually wrote.
			name:         "handler without a verdict falls back to the status code",
			relayOutcome: outcomeUnreported,
			wantPinnedID: 81,
			wantStatus:   http.StatusOK,
		},
		{
			name:         "handler without a verdict rejected by its error status",
			finalStatus:  http.StatusBadGateway,
			relayOutcome: outcomeUnreported,
			wantPinnedID: 0,
			wantStatus:   http.StatusBadGateway,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			useChannelAffinityRulesForTest(t, false, claudeCliTraceRuleForTest())

			ctx := newClaudeMessagesRequestForTest(t, model, userID)
			_, found := GetPreferredChannelByAffinity(ctx, model, "default")
			require.False(t, found, "affinity cache must start cold")

			if tc.streamStarted {
				_, writeErr := ctx.Writer.Write([]byte("event: message_start\n\n"))
				require.NoError(t, writeErr)
			}
			switch tc.relayOutcome {
			case outcomeSuccess:
				SetChannelAffinityRelayOutcome(ctx, true)
			case outcomeFailure:
				SetChannelAffinityRelayOutcome(ctx, false)
			}
			if tc.finalStatus > 0 {
				ctx.JSON(tc.finalStatus, gin.H{"type": "error"})
			}
			require.Equal(t, tc.wantStatus, ctx.Writer.Status())

			RecordChannelAffinity(ctx, 81)

			next := newClaudeMessagesRequestForTest(t, model, userID)
			pinnedID, pinned := GetPreferredChannelByAffinity(next, model, "default")
			assert.Equal(t, tc.wantPinnedID > 0, pinned)
			assert.Equal(t, tc.wantPinnedID, pinnedID)
		})
	}
}

func TestGetPreferredChannelByAffinityRejectsEmptyModelRegex(t *testing.T) {
	emptyModelRegexRule := operation_setting.ChannelAffinityRule{
		Name:      "no model regex",
		PathRegex: []string{"/v1/messages"},
		KeySources: []operation_setting.ChannelAffinityKeySource{
			{Type: "gjson", Path: "metadata.user_id"},
		},
		TTLSeconds:      120,
		IncludeRuleName: true,
	}

	testCases := []struct {
		name         string
		rules        []operation_setting.ChannelAffinityRule
		model        string
		wantMatched  bool
		wantRuleName string
	}{
		{
			name:        "empty model regex never matches",
			rules:       []operation_setting.ChannelAffinityRule{emptyModelRegexRule},
			model:       "claude-opus-5",
			wantMatched: false,
		},
		{
			name:         "empty model regex does not shadow a later valid rule",
			rules:        []operation_setting.ChannelAffinityRule{emptyModelRegexRule, claudeCliTraceRuleForTest()},
			model:        "claude-opus-5",
			wantMatched:  true,
			wantRuleName: "claude cli trace",
		},
		{
			name:        "empty model regex does not match an unrelated model either",
			rules:       []operation_setting.ChannelAffinityRule{emptyModelRegexRule, claudeCliTraceRuleForTest()},
			model:       "gpt-5",
			wantMatched: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			useChannelAffinityRulesForTest(t, false, tc.rules...)
			// The one-line-per-rule warning guard is process-wide; drop it so
			// every case exercises the same code path.
			channelAffinityEmptyModelRegexLogged.Delete(emptyModelRegexRule.Name)

			ctx := newClaudeMessagesRequestForTest(t, tc.model, "user-empty-model-regex")
			_, found := GetPreferredChannelByAffinity(ctx, tc.model, "default")
			require.False(t, found, "cache is cold, no rule can report a hit")

			meta, matched := getChannelAffinityMeta(ctx)
			require.Equal(t, tc.wantMatched, matched)
			assert.Equal(t, tc.wantRuleName, meta.RuleName)
		})
	}
}

func TestGetChannelAffinityCacheStatsTrimsRuleName(t *testing.T) {
	const (
		model  = "claude-opus-5"
		userID = "user-affinity-rule-name-trim"
	)

	testCases := []struct {
		name         string
		ruleName     string
		wantBucket   string
		wantUnknown  int
		wantBucketed int
	}{
		{
			name:         "exact rule name",
			ruleName:     "claude cli trace",
			wantBucket:   "claude cli trace",
			wantUnknown:  0,
			wantBucketed: 1,
		},
		{
			name:         "rule name with surrounding whitespace",
			ruleName:     "  claude cli trace  ",
			wantBucket:   "claude cli trace",
			wantUnknown:  0,
			wantBucketed: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rule := claudeCliTraceRuleForTest()
			rule.Name = tc.ruleName
			useChannelAffinityRulesForTest(t, false, rule)

			pinChannelByAffinityForTest(t, model, userID, 91)

			stats := GetChannelAffinityCacheStats()
			require.Equal(t, 1, stats.Total)
			assert.Equal(t, tc.wantUnknown, stats.Unknown)
			assert.Equal(t, tc.wantBucketed, stats.ByRuleName[tc.wantBucket])
		})
	}
}

// A pinned channel that is merely at its concurrency limit has not failed. The
// request overflows to another channel, and that channel must not inherit the
// pin: repinning would walk the key away from its warm upstream for as long as
// the load lasts, which is the opposite of what affinity is for. The pin is also
// not cleared, so requests return to it once it drains.
func TestRecordChannelAffinityKeepsThePinWhenTheChannelWasOnlySaturated(t *testing.T) {
	const (
		model           = "claude-opus-5"
		userID          = "user-affinity-saturated"
		pinnedChannel   = 71
		overflowChannel = 72
	)

	// SwitchOnSuccess is the mode that repins to whichever channel actually served
	// the request, so it is the one that would steal the pin if bypass were ignored.
	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	overflow := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(overflow, model, "default")
	require.True(t, found)
	require.Equal(t, pinnedChannel, preferred)

	SetChannelAffinityBypassed(overflow)
	overflow.Set("channel_id", overflowChannel)
	SetChannelAffinityRelayOutcome(overflow, true)
	RecordChannelAffinity(overflow, overflowChannel)

	next := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found = GetPreferredChannelByAffinity(next, model, "default")
	require.True(t, found, "a saturated pin must be kept, not cleared")
	assert.Equal(t, pinnedChannel, preferred, "the overflow channel must not steal the pin")
}

// The counterpart of the test above: without the bypass marker a successful
// request on another channel does repin, so the kept pin there is caused by the
// saturation marker and not by the affinity cache ignoring the request.
func TestRecordChannelAffinityRepinsWhenTheChannelWasNotSaturated(t *testing.T) {
	const (
		model            = "claude-opus-5"
		userID           = "user-affinity-repin"
		pinnedChannel    = 81
		succeededChannel = 82
	)

	useChannelAffinityRulesForTest(t, true, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	switched := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(switched, model, "default")
	require.True(t, found)
	require.Equal(t, pinnedChannel, preferred)

	switched.Set("channel_id", succeededChannel)
	SetChannelAffinityRelayOutcome(switched, true)
	RecordChannelAffinity(switched, succeededChannel)

	next := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found = GetPreferredChannelByAffinity(next, model, "default")
	require.True(t, found)
	assert.Equal(t, succeededChannel, preferred, "SwitchOnSuccess must still follow a real channel switch")
}
