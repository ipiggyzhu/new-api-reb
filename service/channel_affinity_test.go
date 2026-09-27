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

// A cold key is established by whichever channel actually answered, no matter how
// SwitchOnSuccess is set. The distributor passes the channel it picked before the
// relay ran, so on a request that failed over to another channel that argument
// names a channel which served nothing; pinning it made the next request with this
// key walk into the failing channel, fault, release the pin, and start over — an
// affinity cache that is enabled and never accumulates anything.
//
// SwitchOnSuccess is off here because that is the configuration where the old code
// took the pre-relay pick verbatim, and it is the one running in production.
func TestRecordChannelAffinityPinsTheSucceedingChannelOnAColdKey(t *testing.T) {
	const (
		model            = "claude-opus-5"
		userID           = "user-affinity-cold-key-failover"
		firstPick        = 91
		succeededChannel = 92
	)

	useChannelAffinityRulesForTest(t, false, claudeCliTraceRuleForTest())

	// Cache miss: affinity selected nothing, so MarkChannelAffinityUsed never runs
	// and no pinned channel is recorded on the request.
	cold := newClaudeMessagesRequestForTest(t, model, userID)
	_, found := GetPreferredChannelByAffinity(cold, model, "default")
	require.False(t, found, "precondition: the key is cold")

	// The relay failed over from firstPick to succeededChannel, which re-stamps
	// "channel_id"; the distributor still calls RecordChannelAffinity with firstPick.
	cold.Set("channel_id", succeededChannel)
	SetChannelAffinityRelayOutcome(cold, true)
	RecordChannelAffinity(cold, firstPick)

	assert.Equal(t, succeededChannel, affinityPinnedChannelForTest(t, model, userID),
		"a cold key must be pinned to the channel that answered, not to the failed first pick")
}

// The counterpart: once a pin exists and affinity actually used it, walking the key
// to another channel is exactly what SwitchOnSuccess governs. With it off the pin
// must stay where it is even though a fallback channel is what succeeded.
func TestRecordChannelAffinityKeepsAnExistingPinWhenSwitchOnSuccessIsOff(t *testing.T) {
	const (
		model            = "claude-opus-5"
		userID           = "user-affinity-no-switch"
		pinnedChannel    = 93
		succeededChannel = 94
	)

	useChannelAffinityRulesForTest(t, false, claudeCliTraceRuleForTest())
	pinChannelByAffinityForTest(t, model, userID, pinnedChannel)

	switched := newClaudeMessagesRequestForTest(t, model, userID)
	preferred, found := GetPreferredChannelByAffinity(switched, model, "default")
	require.True(t, found)
	require.Equal(t, pinnedChannel, preferred)
	MarkChannelAffinityUsed(switched, "default", preferred)

	switched.Set("channel_id", succeededChannel)
	SetChannelAffinityRelayOutcome(switched, true)
	RecordChannelAffinity(switched, pinnedChannel)

	assert.Equal(t, pinnedChannel, affinityPinnedChannelForTest(t, model, userID),
		"with SwitchOnSuccess off an established pin must not follow the fallback channel")
}

// newChatRequestForTest builds a relay request context the way the distributor
// sees it: TokenAuth has already stamped the caller's token id.
func newChatRequestForTest(t *testing.T, path string, body string, tokenID int) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	if tokenID > 0 {
		ctx.Set("token_id", tokenID)
	}
	return ctx
}

// TestDefaultChannelAffinityRulesKeyEveryChatRequest runs the shipped default
// rules. Production stopped matching any rule once its client quit sending
// metadata.user_id, and /v1/chat/completions had no rule at all, so every request
// went back through cold selection and affinity never held.
func TestDefaultChannelAffinityRulesKeyEveryChatRequest(t *testing.T) {
	defaults := operation_setting.GetChannelAffinitySetting().Rules

	cases := []struct {
		name          string
		path          string
		body          string
		tokenID       int
		model         string
		wantRule      string
		wantKeySource string
	}{
		{
			name:          "claude code session keeps its session key",
			path:          "/v1/messages",
			body:          `{"model":"claude-opus-5","metadata":{"user_id":"session-a"}}`,
			tokenID:       7,
			model:         "claude-opus-5",
			wantRule:      "claude cli trace",
			wantKeySource: "gjson",
		},
		{
			name:          "messages without metadata falls back to the token",
			path:          "/v1/messages",
			body:          `{"model":"claude-opus-5"}`,
			tokenID:       7,
			model:         "claude-opus-5",
			wantRule:      "token fallback",
			wantKeySource: "context_int",
		},
		{
			name:          "chat completions falls back to the token",
			path:          "/v1/chat/completions",
			body:          `{"model":"claude-opus-5"}`,
			tokenID:       7,
			model:         "claude-opus-5",
			wantRule:      "token fallback",
			wantKeySource: "context_int",
		},
		{
			name:          "model outside the claude rule falls back to the token",
			path:          "/v1/messages",
			body:          `{"model":"gemini-3.7-flash","metadata":{"user_id":"session-a"}}`,
			tokenID:       7,
			model:         "gemini-3.7-flash",
			wantRule:      "token fallback",
			wantKeySource: "context_int",
		},
		{
			name:    "no session key and no token matches nothing",
			path:    "/v1/chat/completions",
			body:    `{"model":"claude-opus-5"}`,
			tokenID: 0,
			model:   "claude-opus-5",
		},
		{
			name:    "endpoints outside chat are left alone",
			path:    "/v1/embeddings",
			body:    `{"model":"text-embedding-3-small"}`,
			tokenID: 7,
			model:   "text-embedding-3-small",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useChannelAffinityRulesForTest(t, true, defaults...)

			ctx := newChatRequestForTest(t, tc.path, tc.body, tc.tokenID)
			_, found := GetPreferredChannelByAffinity(ctx, tc.model, "default")
			require.False(t, found, "cache is cold, no rule can report a hit")

			meta, matched := getChannelAffinityMeta(ctx)
			require.Equal(t, tc.wantRule != "", matched)
			assert.Equal(t, tc.wantRule, meta.RuleName)
			assert.Equal(t, tc.wantKeySource, meta.KeySourceType)
		})
	}
}

// TestTokenFallbackAffinityPinsPerModelAndRetries checks the two properties the
// token key needs to be safe. One token calls many models, so each model must
// keep its own pin; and a failure on the pinned channel must still be retried on
// another one instead of going straight back to the client.
func TestTokenFallbackAffinityPinsPerModelAndRetries(t *testing.T) {
	const (
		tokenID       = 11
		pinnedChannel = 61
	)
	useChannelAffinityRulesForTest(t, true, operation_setting.GetChannelAffinitySetting().Rules...)

	seed := newChatRequestForTest(t, "/v1/chat/completions", `{"model":"claude-opus-5"}`, tokenID)
	_, found := GetPreferredChannelByAffinity(seed, "claude-opus-5", "default")
	require.False(t, found, "precondition: the key is cold")
	SetChannelAffinityRelayOutcome(seed, true)
	RecordChannelAffinity(seed, pinnedChannel)

	same := newChatRequestForTest(t, "/v1/chat/completions", `{"model":"claude-opus-5"}`, tokenID)
	preferred, found := GetPreferredChannelByAffinity(same, "claude-opus-5", "default")
	require.True(t, found, "the same token and model must hit the pin")
	assert.Equal(t, pinnedChannel, preferred)
	MarkChannelAffinityUsed(same, "default", preferred)
	assert.False(t, ShouldSkipRetryAfterChannelAffinityFailure(same),
		"a failure on the pinned channel must still fail over")

	otherModel := newChatRequestForTest(t, "/v1/chat/completions", `{"model":"claude-haiku-4-5"}`, tokenID)
	_, found = GetPreferredChannelByAffinity(otherModel, "claude-haiku-4-5", "default")
	assert.False(t, found, "another model of the same token must not inherit the pin")

	otherToken := newChatRequestForTest(t, "/v1/chat/completions", `{"model":"claude-opus-5"}`, tokenID+1)
	_, found = GetPreferredChannelByAffinity(otherToken, "claude-opus-5", "default")
	assert.False(t, found, "another token must not inherit the pin")
}
