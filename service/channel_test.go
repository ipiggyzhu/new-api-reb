package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsChannelFaultError(t *testing.T) {
	// Save and restore global state
	origKeywords := operation_setting.AutomaticDisableKeywords
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	t.Cleanup(func() {
		operation_setting.AutomaticDisableKeywords = origKeywords
		operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes
	})

	operation_setting.AutomaticDisableKeywords = []string{
		"your credit balance is too low",
		"you exceeded your current quota",
	}
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("401"))

	t.Run("nil error returns false", func(t *testing.T) {
		assert.False(t, IsChannelFaultError(nil))
	})

	t.Run("channel error code returns true", func(t *testing.T) {
		err := types.NewError(
			assert.AnError,
			types.ErrorCodeChannelInvalidKey,
		)
		assert.True(t, IsChannelFaultError(err))
	})

	t.Run("skip-retry error returns false", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(
			assert.AnError,
			types.ErrorCodeBadResponseBody,
			500,
			types.ErrOptionWithSkipRetry(),
		)
		assert.True(t, types.IsSkipRetryError(err))
		assert.False(t, IsChannelFaultError(err))
	})

	t.Run("401 status code returns true", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(
			assert.AnError,
			types.ErrorCode("authentication_error"),
			401,
		)
		assert.True(t, IsChannelFaultError(err))
	})

	t.Run("429 status code returns false", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(
			assert.AnError,
			types.ErrorCode("rate_limit_exceeded"),
			429,
		)
		assert.False(t, IsChannelFaultError(err))
	})

	t.Run("keyword match returns true even on a non-fault status code", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(
			errString("Your credit balance is too low to continue"),
			types.ErrorCode("insufficient_quota"),
			400,
		)
		assert.True(t, IsChannelFaultError(err))
	})

	t.Run("rate limit message on 429 stays inconclusive", func(t *testing.T) {
		// This is the case that keeps a throttled channel from being stripped of
		// its models: the message is not in the keyword list and 429 is not in
		// AutomaticDisableStatusCodeRanges, so the model is left untouched.
		for _, message := range []string{
			"Rate limit reached for gpt-4o in organization org-xxx on requests per min",
			"429 Too Many Requests",
			"resource_exhausted: quota exceeded for concurrent requests",
			"Request timed out, please try again later",
		} {
			err := types.NewErrorWithStatusCode(
				errString(message),
				types.ErrorCode("rate_limit_exceeded"),
				429,
			)
			assert.False(t, IsChannelFaultError(err), "message should stay inconclusive: %s", message)
		}
	})

	t.Run("500 without keyword returns false", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(
			errString("upstream returned an internal server error"),
			types.ErrorCode("internal_error"),
			500,
		)
		assert.False(t, IsChannelFaultError(err))
	})
}

// TestShouldDisableChannelPreservesLegacyBehavior pins the behavior that the
// IsChannelFaultError extraction must not change: channel disabling stays gated
// on AutomaticDisableChannelEnabled, independently of the model-update path.
func TestShouldDisableChannelPreservesLegacyBehavior(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes
	})
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("401"))

	faultErr := types.NewErrorWithStatusCode(
		errString("invalid api key"),
		types.ErrorCode("authentication_error"),
		401,
	)
	require.True(t, IsChannelFaultError(faultErr))

	common.AutomaticDisableChannelEnabled = false
	assert.False(t, ShouldDisableChannel(faultErr),
		"master switch off must suppress disabling even for a confirmed fault")

	common.AutomaticDisableChannelEnabled = true
	assert.True(t, ShouldDisableChannel(faultErr))

	assert.False(t, ShouldDisableChannel(nil))
}

// TestIsChannelFaultErrorMatchesBuiltinKeywords pins the case-insensitivity
// contract between the compile-time AutomaticDisableKeywords defaults and the
// matcher. The defaults are stored in their original mixed case and are used
// verbatim whenever the options table has no AutomaticDisableKeywords row (i.e.
// an admin never saved the setting, so AutomaticDisableKeywordsFromString never
// lowercased them). Matching survives only because IsChannelFaultError lowers
// the message and readRunes lowers each dictionary word; if either side stops
// normalizing, an out-of-credit upstream stops being recognized as a fault and
// the channel is never auto-disabled.
func TestIsChannelFaultErrorMatchesBuiltinKeywords(t *testing.T) {
	// Deliberately does not override AutomaticDisableKeywords: the point is to
	// exercise the pristine compile-time defaults.
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	t.Cleanup(func() {
		operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes
	})
	// Empty the status-code list so a match can only come from the keywords.
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString(""))

	require.Contains(t, operation_setting.AutomaticDisableKeywords,
		"Your credit balance is too low",
		"built-in defaults changed; update this test alongside them")

	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "upstream wording as sent by Anthropic",
			message: "Your credit balance is too low to access the Anthropic API",
			want:    true,
		},
		{
			name:    "all caps upstream wording",
			message: "YOUR CREDIT BALANCE IS TOO LOW",
			want:    true,
		},
		{
			name:    "already lowercase wording",
			message: "your credit balance is too low",
			want:    true,
		},
		{
			name:    "organization disabled",
			message: "Error code: 403 - This organization has been disabled.",
			want:    true,
		},
		{
			name:    "quota exceeded",
			message: "You exceeded your current quota, please check your plan and billing details",
			want:    true,
		},
		{
			name:    "unrelated transient failure stays inconclusive",
			message: "upstream connection reset by peer",
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewErrorWithStatusCode(
				errString(tc.message),
				types.ErrorCode("insufficient_quota"),
				400,
			)
			assert.Equal(t, tc.want, IsChannelFaultError(err))
		})
	}
}

// Three "channel:" error codes report that our own channel configuration is
// wrong — an illegal param or header override template, or an AWS client that
// could not be built from the stored credential shape — not that the upstream is
// broken. Disabling the channel fixes none of them. It matters most for override
// templates: a channel affinity rule fans one template across a whole group, so a
// single illegal entry makes every channel the rule reaches raise the same error
// and the retry loop would walk down the group disabling channels one by one.
func TestIsChannelFaultErrorExcludesChannelConfigErrors(t *testing.T) {
	// Empty the status-code list so the verdict can only come from the error code,
	// not from whatever status the construction site stamped on it.
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	t.Cleanup(func() {
		operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes
	})
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString(""))

	cases := []struct {
		name      string
		errorCode types.ErrorCode
		message   string
		want      bool
	}{
		{
			name:      "invalid param override template",
			errorCode: types.ErrorCodeChannelParamOverrideInvalid,
			message:   "param override is not a valid JSON object",
			want:      false,
		},
		{
			name:      "invalid header override template",
			errorCode: types.ErrorCodeChannelHeaderOverrideInvalid,
			message:   "header override template is invalid",
			want:      false,
		},
		{
			name:      "aws client could not be constructed",
			errorCode: types.ErrorCodeChannelAwsClientError,
			message:   "failed to create aws client",
			want:      false,
		},
		{
			name:      "dead upstream key is still a fault",
			errorCode: types.ErrorCodeChannelInvalidKey,
			message:   "invalid api key",
			want:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewErrorWithStatusCode(errString(tc.message), tc.errorCode, 500)
			require.True(t, types.IsChannelError(err),
				"fixture must carry the channel: prefix, otherwise the exclusion is untested")

			assert.Equal(t, tc.want, IsChannelFaultError(err))
		})
	}
}

// The latency arm of the channel-status decision. isChannelEnabled gates the
// whole judgement so an auto-disabled channel being retested for recovery is not
// re-condemned by a cold start, and a sub-millisecond threshold clamps to 1ms
// instead of truncating to zero, which used to invert an operator's strictest
// setting into "never disable on latency".
func TestShouldDisableChannelForResponseTime(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = origEnabled })

	cases := []struct {
		name             string
		masterSwitch     bool
		milliseconds     int64
		thresholdSeconds float64
		isChannelEnabled bool
		want             bool
	}{
		{
			name:             "master switch off",
			masterSwitch:     false,
			milliseconds:     60_000,
			thresholdSeconds: 5,
			isChannelEnabled: true,
			want:             false,
		},
		{
			name:             "already disabled channel under retest",
			masterSwitch:     true,
			milliseconds:     600_000,
			thresholdSeconds: 5,
			isChannelEnabled: false,
			want:             false,
		},
		{
			name:             "threshold zero switches the check off",
			masterSwitch:     true,
			milliseconds:     60_000,
			thresholdSeconds: 0,
			isChannelEnabled: true,
			want:             false,
		},
		{
			name:             "negative threshold switches the check off",
			masterSwitch:     true,
			milliseconds:     60_000,
			thresholdSeconds: -1,
			isChannelEnabled: true,
			want:             false,
		},
		{
			// Discriminates the clamp: truncating 0.0005s to a 0ms threshold would
			// make any latency above zero a fault, so a 1ms response would be
			// condemned by a threshold the operator never configured.
			name:             "sub-millisecond threshold clamps to 1ms",
			masterSwitch:     true,
			milliseconds:     1,
			thresholdSeconds: 0.0005,
			isChannelEnabled: true,
			want:             false,
		},
		{
			name:             "clamped 1ms threshold still fires above it",
			masterSwitch:     true,
			milliseconds:     2,
			thresholdSeconds: 0.0005,
			isChannelEnabled: true,
			want:             true,
		},
		{
			name:             "exactly at the threshold is not over it",
			masterSwitch:     true,
			milliseconds:     5000,
			thresholdSeconds: 5,
			isChannelEnabled: true,
			want:             false,
		},
		{
			name:             "over the threshold",
			masterSwitch:     true,
			milliseconds:     5001,
			thresholdSeconds: 5,
			isChannelEnabled: true,
			want:             true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			common.AutomaticDisableChannelEnabled = tc.masterSwitch

			assert.Equal(t, tc.want,
				ShouldDisableChannelForResponseTime(tc.milliseconds, tc.thresholdSeconds, tc.isChannelEnabled))
		})
	}
}

// TestIsChannelRoutingFaultErrorDemotesWithoutDisabling pins the split that lets
// a failing channel sink in the routing order without ever becoming eligible for
// auto-disabling. Both halves matter: if the routing side stops accepting these,
// a channel that cannot serve a single request keeps its share of traffic — and
// keeps leading it, when its static priority is the highest in the group; if the
// disable side starts accepting them, a channel that hits one bad stretch gets
// turned off and needs a manual click to come back.
func TestIsChannelRoutingFaultErrorDemotesWithoutDisabling(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	origKeywords := operation_setting.AutomaticDisableKeywords
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	origRetryCodes := operation_setting.AutomaticRetryStatusCodeRanges
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origEnabled
		operation_setting.AutomaticDisableKeywords = origKeywords
		operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes
		operation_setting.AutomaticRetryStatusCodeRanges = origRetryCodes
	})
	operation_setting.AutomaticDisableKeywords = nil
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("401"))
	// The shipped defaults, set explicitly: routing now reads the retry ranges, so
	// the fixture has to state them rather than inherit whatever ran before it.
	require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString(
		"100-199,300-399,401-407,409-499,500-503,505-523,525-599"))
	// Worst case for the disable half: the operator turned the master switch on.
	common.AutomaticDisableChannelEnabled = true

	emptyResponse := types.NewErrorWithStatusCode(
		errString("upstream returned no output"),
		types.ErrorCodeEmptyResponse,
		502,
	)
	channelFault := types.NewErrorWithStatusCode(
		errString("invalid api key"),
		types.ErrorCode("authentication_error"),
		401,
	)
	rateLimited := types.NewErrorWithStatusCode(
		errString("rate limit reached"),
		types.ErrorCode("rate_limit_error"),
		429,
	)
	// The shape that started this: a dead key the upstream reports as its own 500
	// with a message no keyword list has ever heard of. Nothing about it is a
	// "channel:" code, and 500 is not in the 401-only disable allowlist, so before
	// routing read the retry ranges this failed every request while accruing
	// nothing — and led the group anyway, because its static priority was highest.
	deadKey := types.NewErrorWithStatusCode(
		errString("Failed to validate API key"),
		types.ErrorCode("upstream_error"),
		500,
	)
	upstreamGatewayFault := types.NewErrorWithStatusCode(
		errString("Upstream request failed (request id: abc)"),
		types.ErrorCodeBadResponseStatusCode,
		502,
	)
	timedOut := types.NewErrorWithStatusCode(
		errString("upstream timed out"),
		types.ErrorCodeBadResponse,
		504,
	)
	// Client errors sit on the other side of the line: the request itself is
	// malformed, so every channel in the group would reject it identically.
	badRequest := types.NewErrorWithStatusCode(
		errString("max_tokens must be positive"),
		types.ErrorCodeInvalidRequest,
		400,
	)
	// Concurrency limits mean the deployment is right and no channel misbehaved.
	// Only the routing half is asserted, below: this error is raised while picking
	// a channel, so no channel is ever in hand to disable when it happens.
	saturated := types.NewErrorWithStatusCode(
		errString("all channels at their concurrency limit"),
		types.ErrorCodeChannelsSaturated,
		503,
		types.ErrOptionWithSkipRetry(),
	)
	// One illegal override template fans out across every channel an affinity rule
	// reaches, so counting it would walk the whole group down the order.
	badOverride := types.NewErrorWithStatusCode(
		errString("param override template is not valid json"),
		types.ErrorCodeChannelParamOverrideInvalid,
		500,
	)

	cases := []struct {
		name        string
		err         *types.NewAPIError
		wantRouting bool
		wantDisable bool
	}{
		{name: "nil error", err: nil, wantRouting: false, wantDisable: false},
		{name: "empty response demotes only", err: emptyResponse, wantRouting: true, wantDisable: false},
		{name: "channel fault does both", err: channelFault, wantRouting: true, wantDisable: true},
		{name: "upstream 500 dead key demotes only", err: deadKey, wantRouting: true, wantDisable: false},
		{name: "upstream 502 demotes only", err: upstreamGatewayFault, wantRouting: true, wantDisable: false},
		{name: "timeout demotes only", err: timedOut, wantRouting: true, wantDisable: false},
		{name: "rate limit demotes only", err: rateLimited, wantRouting: true, wantDisable: false},
		{name: "client error does neither", err: badRequest, wantRouting: false, wantDisable: false},
		{name: "bad override does neither", err: badOverride, wantRouting: false, wantDisable: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantRouting, IsChannelRoutingFaultError(tc.err))
			assert.Equal(t, tc.wantDisable, ShouldDisableChannel(tc.err))
		})
	}

	assert.False(t, IsChannelRoutingFaultError(saturated),
		"every channel being busy is the deployment working as configured, not a fault to reorder on")
}

// TestIsChannelRoutingFaultErrorFollowsConfiguredRetryCodes pins routing to the
// operator's retry configuration rather than to a second hardcoded list. An
// operator who declares a status code not worth retrying is saying another
// channel would not do better, and routing must not then demote on it.
func TestIsChannelRoutingFaultErrorFollowsConfiguredRetryCodes(t *testing.T) {
	origKeywords := operation_setting.AutomaticDisableKeywords
	origStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	origRetryCodes := operation_setting.AutomaticRetryStatusCodeRanges
	t.Cleanup(func() {
		operation_setting.AutomaticDisableKeywords = origKeywords
		operation_setting.AutomaticDisableStatusCodeRanges = origStatusCodes
		operation_setting.AutomaticRetryStatusCodeRanges = origRetryCodes
	})
	operation_setting.AutomaticDisableKeywords = nil
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("401"))
	// An operator who decided 429 is the client's problem, not the channel's.
	require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString("500-503"))

	rateLimited := types.NewErrorWithStatusCode(
		errString("rate limit reached"), types.ErrorCode("rate_limit_error"), 429)
	serverError := types.NewErrorWithStatusCode(
		errString("internal error"), types.ErrorCodeBadResponseStatusCode, 500)

	assert.False(t, IsChannelRoutingFaultError(rateLimited))
	assert.True(t, IsChannelRoutingFaultError(serverError))
}

// errString is a minimal error carrying an exact message; the keyword matcher
// works on the rendered message, so the text has to survive verbatim.
type errString string

func (e errString) Error() string { return string(e) }
