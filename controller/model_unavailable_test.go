package controller

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
)

// upstreamJSONError builds the error shape service.RelayErrorHandler produces for a
// JSON error body the upstream returned, so these tests exercise the same values
// the validator sees in production rather than a hand-made approximation.
func upstreamJSONError(statusCode int, code string, message string) *types.NewAPIError {
	return types.WithOpenAIError(types.OpenAIError{
		Message: message,
		Type:    "upstream_error",
		Code:    code,
	}, statusCode)
}

// upstreamTextError builds the error shape produced when the body is JSON but not a
// recognised OpenAI error object — the path a body like
// {"error":"当前 API 不支持所选模型 X","type":"error"} takes, where only the message
// survives and our error code stays bad_response_status_code.
func upstreamTextError(statusCode int, message string) *types.NewAPIError {
	return types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponseStatusCode, statusCode)
}

// TestModelUnavailableAcceptsUpstreamModelRejection covers the evidence that must
// count, or the removal path stays as dead as it was before this predicate existed.
func TestModelUnavailableAcceptsUpstreamModelRejection(t *testing.T) {
	cases := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			// Observed verbatim on a production channel, 18 times in one week.
			name: "chinese unsupported model prose on 404",
			err:  upstreamTextError(404, "当前 API 不支持所选模型 claude-3-5-haiku-20241022"),
		},
		{
			name: "machine readable model_not_found on 404",
			err:  upstreamJSONError(404, "model_not_found", "The model `gpt-9` does not exist"),
		},
		{
			name: "english prose on 404",
			err:  upstreamTextError(404, "no such model: gpt-9"),
		},
		{
			name: "unknown model on 404",
			err:  upstreamTextError(404, "Unknown model requested"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, isModelUnavailableError(tc.err, nil))
		})
	}
}

// TestModelUnavailableRejectsAccountConditions is the test that matters most.
//
// On the deployment this was written for, 110 of 200 validation failures in one week
// were 403 insufficient_user_quota. A predicate that counted them would have deleted
// every model on a channel advertising 39 of them, purely because the upstream
// account was out of credit — and left it empty after the operator topped it up.
func TestModelUnavailableRejectsAccountConditions(t *testing.T) {
	cases := []*types.NewAPIError{
		upstreamJSONError(403, "insufficient_user_quota", "user quota is not enough"),
		upstreamTextError(403, "INSUFFICIENT_BALANCE: Insufficient account balance"),
		upstreamJSONError(402, "insufficient_quota", "Your credit balance is too low"),
		upstreamJSONError(401, "invalid_api_key", "Incorrect API key provided"),
		upstreamJSONError(429, "rate_limit_exceeded", "Rate limit reached for requests"),
		upstreamTextError(403, "organization has been disabled"),
		upstreamTextError(403, "You do not have permission to access this model"),
		upstreamTextError(403, "账户余额不足，请充值"),
		upstreamTextError(429, "请求过于频繁，请稍后重试"),
		// A 404 that is really an account condition: the exclusion list is checked
		// before any positive rule precisely so this cannot slip through.
		upstreamJSONError(404, "insufficient_quota", "model not found for this quota tier"),
	}
	for _, err := range cases {
		assert.False(t, isModelUnavailableError(err, nil), "must stay inconclusive: %v", err)
	}
}

// TestModelUnavailableRejectsTransientAndDownstreamOutages pins the status-code
// narrowing. Every 503 model_not_found observed in production came from a DOWNSTREAM
// relay whose own channel pool was momentarily empty; 500-503 are retryable by
// default in this codebase, so treating them as catalogue statements would delete
// healthy models during someone else's outage.
func TestModelUnavailableRejectsTransientAndDownstreamOutages(t *testing.T) {
	cases := []*types.NewAPIError{
		// Observed 17 times in one week, always on 503.
		upstreamJSONError(503, "model_not_found",
			"No available channel for model gpt-5.5 under group codex (distributor)"),
		upstreamJSONError(503, "system_disk_overloaded",
			"system disk overloaded (current: 93.5%, threshold: 90%)"),
		upstreamJSONError(500, "do_request_failed", "upstream error: do request failed"),
		upstreamTextError(520, "bad response status code 520"),
		upstreamTextError(504, "gateway timeout"),
	}
	for _, err := range cases {
		assert.False(t, isModelUnavailableError(err, nil), "must stay inconclusive: %v", err)
	}
}

// TestModelUnavailableRejectsAmbiguous400s covers the case that proves prose cannot
// be trusted on 400: one production channel returned both of these with the same
// status. "已下线" really is gone; "1m 上下文" means the model works fine and a
// feature flag is off. No text rule separates them, so neither counts.
func TestModelUnavailableRejectsAmbiguous400s(t *testing.T) {
	cases := []*types.NewAPIError{
		upstreamTextError(400, "claude-opus-4-6 已下线，请切换到 claude-opus-4-7 模型"),
		upstreamTextError(400, "1m 上下文已经全量可用，请启用 1m 上下文后重试"),
		upstreamJSONError(400, "invalid_request_error", "unsupported parameter: max_tokens"),
	}
	for _, err := range cases {
		assert.False(t, isModelUnavailableError(err, nil), "400 must stay inconclusive: %v", err)
	}
}

// TestModelUnavailableRejectsOurOwnFailures keeps the predicate from blaming the
// upstream for a request we built wrong. buildTestRequest infers the endpoint
// heuristically from the model name, so a 404 is a plausible symptom of our own
// routing being off — and a local error never reached an upstream at all.
func TestModelUnavailableRejectsOurOwnFailures(t *testing.T) {
	// A local error alongside a qualifying upstream error still disqualifies.
	assert.False(t, isModelUnavailableError(
		upstreamTextError(404, "no such model"), errors.New("connection refused")))

	assert.False(t, isModelUnavailableError(nil, errors.New("dial tcp: timeout")))
	assert.False(t, isModelUnavailableError(nil, nil))

	// Raised by our own relay layer, not by the upstream describing its catalogue.
	assert.False(t, isModelUnavailableError(
		types.NewOpenAIError(errors.New("model mapping is invalid"),
			types.ErrorCodeChannelModelMappedError, 404), nil))

	// A bare 404 with no model-scoped marker: most likely the wrong endpoint.
	assert.False(t, isModelUnavailableError(upstreamTextError(404, "404 page not found"), nil))
	assert.False(t, isModelUnavailableError(
		upstreamTextError(404, "The requested endpoint does not exist"), nil))
	assert.False(t, isModelUnavailableError(
		upstreamTextError(404, "deployment not found"), nil))
}

// TestSuspendRemovalsWhenMostOfChannelIsFailing covers the cross-run guard.
//
// limitUpstreamModelRemovals only sees one run, and rotation samples a few models
// per run, so a channel-wide outage yields a small "reasonable" removal set every
// run and passes that bound each time — eroding a working configuration over a day
// without ever tripping the half-channel rule.
func TestSuspendRemovalsWhenMostOfChannelIsFailing(t *testing.T) {
	existing := []string{"m1", "m2", "m3", "m4", "m5", "m6"}

	t.Run("no health recorded", func(t *testing.T) {
		assert.False(t, shouldSuspendUpstreamModelRemovals(existing, nil))
		assert.False(t, shouldSuspendUpstreamModelRemovals(existing, map[string]dto.ModelHealthState{}))
	})

	t.Run("a minority failing still allows removal", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"m1": {Failures: 2},
			"m2": {Failures: 1},
		}
		assert.False(t, shouldSuspendUpstreamModelRemovals(existing, health))
	})

	t.Run("exactly half still allows removal", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"m1": {Failures: 1}, "m2": {Failures: 1}, "m3": {Failures: 1},
		}
		assert.False(t, shouldSuspendUpstreamModelRemovals(existing, health))
	})

	t.Run("more than half suspends removal", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"m1": {Failures: 1}, "m2": {Failures: 1},
			"m3": {Failures: 1}, "m4": {Failures: 1},
		}
		assert.True(t, shouldSuspendUpstreamModelRemovals(existing, health))
	})

	t.Run("zero-failure entries do not count", func(t *testing.T) {
		health := map[string]dto.ModelHealthState{
			"m1": {Failures: 0}, "m2": {Failures: 0},
			"m3": {Failures: 0}, "m4": {Failures: 0}, "m5": {Failures: 1},
		}
		assert.False(t, shouldSuspendUpstreamModelRemovals(existing, health))
	})

	t.Run("stale entries for removed models do not hold a channel hostage", func(t *testing.T) {
		// Health remembers models the channel no longer serves. Counting those would
		// let old removals permanently block new ones.
		health := map[string]dto.ModelHealthState{
			"gone-1": {Failures: 3}, "gone-2": {Failures: 3},
			"gone-3": {Failures: 3}, "gone-4": {Failures: 3},
			"m1": {Failures: 1},
		}
		assert.False(t, shouldSuspendUpstreamModelRemovals(existing, health))
	})

	t.Run("empty channel", func(t *testing.T) {
		assert.False(t, shouldSuspendUpstreamModelRemovals(nil,
			map[string]dto.ModelHealthState{"m1": {Failures: 5}}))
	})
}
