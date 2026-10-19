package service

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/types"
)

// maxChannelCooldown bounds how long one refusal can keep a channel out of
// selection. Subscription windows report resets hours away; after this long a
// single request probes again and, if still refused, renews the cooldown.
const maxChannelCooldown = time.Hour

// Used when the upstream refused without saying for how long. A 429 is
// usually a per-minute bucket refilling within seconds; a 529 is Anthropic
// shedding load, which tends to last longer.
const (
	defaultRateLimitCooldown = 10 * time.Second
	defaultOverloadCooldown  = 30 * time.Second
)

// UpstreamRetryAfter reads how long an upstream asked to be left alone:
// retry-after-ms or retry-after (seconds or an HTTP date) first, then the reset
// of an Anthropic limit reported as exhausted — the unified subscription limit
// when its status is rejected, otherwise the latest reset among the
// requests/tokens/input-tokens/output-tokens buckets whose remaining is 0.
// Zero means no usable hint. The result is capped at maxChannelCooldown.
func UpstreamRetryAfter(header http.Header, now time.Time) time.Duration {
	wait := time.Duration(0)
	if ms, err := strconv.ParseFloat(strings.TrimSpace(header.Get("retry-after-ms")), 64); err == nil && ms > 0 {
		wait = time.Duration(min(ms, float64(maxChannelCooldown/time.Millisecond))) * time.Millisecond
	} else if value := strings.TrimSpace(header.Get("retry-after")); value != "" {
		if seconds, err := strconv.ParseFloat(value, 64); err == nil {
			if seconds > 0 {
				wait = time.Duration(min(seconds, maxChannelCooldown.Seconds()) * float64(time.Second))
			}
		} else if at, err := http.ParseTime(value); err == nil {
			wait = at.Sub(now)
		}
	}
	if wait <= 0 && strings.EqualFold(strings.TrimSpace(header.Get("anthropic-ratelimit-unified-status")), "rejected") {
		if unix, err := strconv.ParseInt(strings.TrimSpace(header.Get("anthropic-ratelimit-unified-reset")), 10, 64); err == nil {
			if unix > 1e11 {
				unix /= 1000 // milliseconds
			}
			wait = time.Unix(unix, 0).Sub(now)
		}
	}
	if wait <= 0 {
		for _, bucket := range []string{"requests", "tokens", "input-tokens", "output-tokens"} {
			if strings.TrimSpace(header.Get("anthropic-ratelimit-"+bucket+"-remaining")) != "0" {
				continue
			}
			if at, err := time.Parse(time.RFC3339, strings.TrimSpace(header.Get("anthropic-ratelimit-"+bucket+"-reset"))); err == nil {
				wait = max(wait, at.Sub(now))
			}
		}
	}
	if wait <= 0 {
		return 0
	}
	return min(wait, maxChannelCooldown)
}

// ChannelCooldownFor is how long a channel should sit out of selection for the
// model after err: the upstream's own hint when it gave one, else a default
// for a rate limit (429, rate_limit_error) or an overload (529,
// overloaded_error). Zero for every other error. The error types matter
// because a refusal can arrive inside a 200 stream, or through a channel's
// status-code mapping, without the status code that names it.
func ChannelCooldownFor(err *types.NewAPIError) time.Duration {
	if err == nil {
		return 0
	}
	errorType := ""
	switch relayError := err.RelayError.(type) {
	case types.OpenAIError:
		errorType = relayError.Type
	case types.ClaudeError:
		errorType = relayError.Type
	}
	limited := err.StatusCode == http.StatusTooManyRequests || errorType == "rate_limit_error"
	overloaded := err.StatusCode == 529 || errorType == "overloaded_error"
	switch {
	case !limited && !overloaded && err.RetryAfter <= 0:
		return 0
	case err.RetryAfter > 0:
		return err.RetryAfter
	case overloaded:
		return defaultOverloadCooldown
	default:
		return defaultRateLimitCooldown
	}
}
