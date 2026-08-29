package controller

import (
	"strings"

	"github.com/QuantumNous/new-api/types"
)

// This file answers one narrow question for the upstream model-update validator:
// is an error proof that THIS MODEL does not exist on THIS CHANNEL?
//
// That is deliberately NOT service.IsChannelFaultError, which answers "should this
// channel be auto-disabled". Conflating them is what left the removal path dead:
// IsChannelFaultError only accepts the "channel:" error prefix or a status code in
// the operator's AutomaticDisableStatusCodes allowlist, which defaults to 401
// alone, so a 404 "model not supported" never counted and dead model names
// accumulated forever.
//
// The bar here is high because the consequence is deletion. Every rule below was
// checked against a week of real validation failures on a production deployment,
// where the naive version of this predicate would have deleted a channel's entire
// 39-model list because the upstream ACCOUNT had run out of credit.

// modelUnavailableExclusionMarkers are substrings that keep an error inconclusive
// no matter what else it looks like.
//
// These are account-, key- or capacity-level conditions. They arrive on the same
// status codes as a genuine "no such model" and say nothing about any individual
// model: every model on the channel fails identically while the account is broke
// or throttled. Counting them would strip a channel of everything it serves and
// leave it empty once the operator topped the account back up.
var modelUnavailableExclusionMarkers = []string{
	"quota",
	"balance",
	"insufficient",
	"exceeded",
	"rate limit",
	"rate_limit",
	"too many requests",
	"overloaded",
	"unauthorized",
	"invalid api key",
	"invalid_api_key",
	"expired",
	"permission",
	"forbidden",
	"disabled",
	"suspended",
	"欠费",
	"余额",
	"额度",
	"限流",
	"频繁",
	"无权",
	"未授权",
}

// modelUnavailableMarkers are substrings that identify the MODEL as the thing that
// is missing or unsupported, rather than the request, the route or the account.
//
// Kept narrow on purpose. "not found" alone is not here: an endpoint-not-found or
// deployment-not-found says the request went to the wrong place, which is our
// problem to fix, not grounds to delete a model the channel may well serve.
var modelUnavailableMarkers = []string{
	"model_not_found",
	"model not found",
	"no such model",
	"unknown model",
	"unsupported model",
	"model is not supported",
	"model does not exist",
	"invalid model",
	"不支持所选模型",
	"不支持该模型",
	"模型不存在",
	"未知模型",
	"无效的模型",
}

// isModelUnavailableError reports whether err is evidence that modelName is not
// available on the channel that produced it.
//
// Only HTTP 404 qualifies. The narrowness is the point:
//
//   - 503 is excluded even when the body carries a machine-readable
//     model_not_found. In this codebase 500-503 are retryable by default
//     (operation_setting.AutomaticRetryStatusCodeRanges), and the observed 503
//     model_not_found responses all came from DOWNSTREAM relays whose own channel
//     pool was momentarily empty — their model comes back when their upstream does.
//     Deleting on that would garbage-collect a healthy channel during someone
//     else's outage.
//   - 400 is excluded because it is where genuinely ambiguous prose lives. The same
//     status carried both "claude-opus-4-6 已下线" (really gone) and "1m 上下文已经
//     全量可用，请启用 1m 上下文后重试" (model fine, a feature flag is off) on one
//     deployment. No text rule separates those safely, and guessing wrong deletes a
//     working model.
//   - 401/402/403/429 are account and key conditions, handled by the exclusion list.
//
// A local error (one we produced before reaching the upstream) never qualifies:
// a malformed synthetic test request is our bug to fix, not the model's absence.
func isModelUnavailableError(err *types.NewAPIError, localErr error) bool {
	if err == nil || localErr != nil {
		return false
	}
	if err.StatusCode != 404 {
		return false
	}
	// An error carrying the "channel:" prefix was raised by our own relay layer
	// (model mapping failure, invalid override, AWS client construction) rather
	// than by the upstream describing its catalogue.
	if types.IsChannelError(err) {
		return false
	}

	haystack := strings.ToLower(err.Error())
	if body := err.GetUpstreamBody(); body != "" {
		haystack += " " + strings.ToLower(body)
	}

	// Checked before any positive rule: a 404 that also mentions quota is an
	// account condition wearing a model-shaped status code.
	for _, marker := range modelUnavailableExclusionMarkers {
		if strings.Contains(haystack, marker) {
			return false
		}
	}

	// Structural first. WithOpenAIError copies the upstream's error.code straight
	// into our error code (types/error.go:376), so a well-behaved upstream is
	// identified without reading prose at all.
	if err.GetErrorCode() == types.ErrorCodeModelNotFound {
		return true
	}

	for _, marker := range modelUnavailableMarkers {
		if strings.Contains(haystack, marker) {
			return true
		}
	}

	// No marker matched. A bare 404 stays inconclusive even though it names the
	// model: "endpoint not found for model X" is our routing problem, and
	// buildTestRequest infers the endpoint heuristically from the model name, so a
	// 404 is a plausible symptom of OUR request being wrong rather than the model
	// being absent.
	return false
}
