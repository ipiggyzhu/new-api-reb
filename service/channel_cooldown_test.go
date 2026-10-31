package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header map[string]string
		want   time.Duration
	}{
		{"retry-after seconds", map[string]string{"retry-after": "7"}, 7 * time.Second},
		{"retry-after-ms wins", map[string]string{"retry-after-ms": "1500", "retry-after": "7"}, 1500 * time.Millisecond},
		{"retry-after http date", map[string]string{"retry-after": now.Add(90 * time.Second).Format(http.TimeFormat)}, 90 * time.Second},
		{"retry-after capped", map[string]string{"retry-after": "86400"}, time.Hour},
		{"retry-after date in the past", map[string]string{"retry-after": now.Add(-time.Minute).Format(http.TimeFormat)}, 0},
		{
			"unified subscription limit rejected",
			map[string]string{"anthropic-ratelimit-unified-status": "rejected", "anthropic-ratelimit-unified-reset": strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10)},
			5 * time.Minute,
		},
		{
			"unified reset in milliseconds",
			map[string]string{"anthropic-ratelimit-unified-status": "rejected", "anthropic-ratelimit-unified-reset": strconv.FormatInt(now.Add(5*time.Minute).UnixMilli(), 10)},
			5 * time.Minute,
		},
		{
			"unified window hours away is capped",
			map[string]string{"anthropic-ratelimit-unified-status": "rejected", "anthropic-ratelimit-unified-reset": strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10)},
			time.Hour,
		},
		{
			"unified reset ignored while allowed",
			map[string]string{"anthropic-ratelimit-unified-status": "allowed", "anthropic-ratelimit-unified-reset": strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10)},
			0,
		},
		{
			"latest reset among exhausted buckets",
			map[string]string{
				"anthropic-ratelimit-requests-remaining":      "0",
				"anthropic-ratelimit-requests-reset":          now.Add(5 * time.Second).Format(time.RFC3339),
				"anthropic-ratelimit-output-tokens-remaining": "0",
				"anthropic-ratelimit-output-tokens-reset":     now.Add(20 * time.Second).Format(time.RFC3339),
				"anthropic-ratelimit-input-tokens-remaining":  "12000",
				"anthropic-ratelimit-input-tokens-reset":      now.Add(50 * time.Second).Format(time.RFC3339),
			},
			20 * time.Second,
		},
		{"garbage", map[string]string{"retry-after": "soon"}, 0},
		{"no hint", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			for key, value := range tc.header {
				header.Set(key, value)
			}
			assert.Equal(t, tc.want, UpstreamRetryAfter(header, now))
		})
	}
}

func TestRelayErrorHandlerRecordsRetryAfterOnRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   time.Duration
	}{
		{http.StatusTooManyRequests, 12 * time.Second},
		{529, 12 * time.Second},
		{http.StatusServiceUnavailable, 12 * time.Second},
		{http.StatusBadRequest, 0},
	} {
		resp := &http.Response{
			StatusCode: tc.status,
			Header:     http.Header{"Retry-After": []string{"12"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)),
		}
		apiErr := RelayErrorHandler(context.Background(), resp, false)
		require.NotNil(t, apiErr)
		assert.Equal(t, tc.want, apiErr.RetryAfter, "status %d", tc.status)
	}
}

func TestChannelCooldownFor(t *testing.T) {
	withHint := types.NewOpenAIError(errors.New("slow down"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)
	withHint.RetryAfter = 42 * time.Second
	unavailableWithHint := types.NewOpenAIError(errors.New("maintenance"), types.ErrorCodeBadResponseStatusCode, http.StatusServiceUnavailable)
	unavailableWithHint.RetryAfter = 5 * time.Second

	cases := []struct {
		name string
		err  *types.NewAPIError
		want time.Duration
	}{
		{"upstream hint wins", withHint, 42 * time.Second},
		{"429 without hint", types.NewOpenAIError(errors.New("slow down"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests), defaultRateLimitCooldown},
		{"529 without hint", types.NewOpenAIError(errors.New("overloaded"), types.ErrorCodeBadResponseStatusCode, 529), defaultOverloadCooldown},
		{"overload inside a 200 stream", types.WithClaudeError(types.ClaudeError{Type: "overloaded_error", Message: "Overloaded"}, http.StatusInternalServerError), defaultOverloadCooldown},
		{"rate limit remapped to 500", types.WithOpenAIError(types.OpenAIError{Type: "rate_limit_error", Message: "slow down"}, http.StatusInternalServerError), defaultRateLimitCooldown},
		{"503 that names its window", unavailableWithHint, 5 * time.Second},
		{"plain 500", types.NewOpenAIError(errors.New("boom"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError), 0},
		{"client error", types.NewOpenAIError(errors.New("bad"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest), 0},
		{"no error", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ChannelCooldownFor(tc.err))
		})
	}
}

// One key of a multi-key channel cools for as long as a channel would for a
// refusal, for defaultKeyFaultCooldown after any other fault that moves the
// request on, and not at all when the request itself was at fault, including a
// request too large or malformed that is still retried on another channel.
func TestChannelKeyCooldownFor(t *testing.T) {
	withHint := types.NewOpenAIError(errors.New("slow down"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)
	withHint.RetryAfter = 42 * time.Second

	cases := []struct {
		name string
		err  *types.NewAPIError
		want time.Duration
	}{
		{"refusal with hint", withHint, 42 * time.Second},
		{"429 without hint", types.NewOpenAIError(errors.New("slow down"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests), defaultRateLimitCooldown},
		{"529 without hint", types.NewOpenAIError(errors.New("overloaded"), types.ErrorCodeBadResponseStatusCode, 529), defaultOverloadCooldown},
		{"key out of balance", types.NewOpenAIError(errors.New("insufficient balance"), types.ErrorCodeBadResponseStatusCode, http.StatusForbidden), defaultKeyFaultCooldown},
		{"upstream 502", types.NewOpenAIError(errors.New("bad gateway"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway), defaultKeyFaultCooldown},
		{"client error", types.NewOpenAIError(errors.New("bad"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest), 0},
		{"request too large", types.NewOpenAIError(errors.New("request_too_large"), types.ErrorCodeBadResponseStatusCode, http.StatusRequestEntityTooLarge), 0},
		{"unprocessable request", types.NewOpenAIError(errors.New("invalid parameter"), types.ErrorCodeBadResponseStatusCode, http.StatusUnprocessableEntity), 0},
		{"no error", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ChannelKeyCooldownFor(tc.err))
		})
	}
}
