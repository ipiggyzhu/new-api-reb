package baidu

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInBandError_StatusAndCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		errorCode      int
		errorMsg       string
		wantStatus     int
		wantErrorCode  types.ErrorCode
		wantMsgContain string
	}{
		{"access token invalid", 110, "Access token invalid or no longer valid",
			http.StatusUnauthorized, types.ErrorCodeChannelInvalidKey, "Access token invalid"},
		{"access token expired", 111, "Access token expired",
			http.StatusUnauthorized, types.ErrorCodeChannelInvalidKey, "Access token expired"},
		{"qps limit", 18, "Open api qps request limit reached",
			http.StatusTooManyRequests, types.ErrorCodeBadResponse, "qps request limit"},
		{"daily limit", 17, "Open api daily request limit reached",
			http.StatusTooManyRequests, types.ErrorCodeBadResponse, "daily request limit"},
		{"request limit", 4, "Open api request limit reached",
			http.StatusTooManyRequests, types.ErrorCodeBadResponse, "request limit"},
		{"qianfan rate limit", 336501, "rate limit exceeded",
			http.StatusTooManyRequests, types.ErrorCodeBadResponse, "rate limit"},
		{"unrecognized code", 336003, "invalid parameter",
			http.StatusBadGateway, types.ErrorCodeBadResponse, "invalid parameter"},
		{"zero code with message", 0, "something went wrong",
			http.StatusBadGateway, types.ErrorCodeBadResponse, "something went wrong"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := inBandError(tc.errorCode, tc.errorMsg)
			require.NotNil(t, err)
			assert.Equal(t, tc.wantStatus, err.StatusCode)
			assert.Equal(t, tc.wantErrorCode, err.GetErrorCode())
			assert.Contains(t, err.Error(), tc.wantMsgContain)
			assert.Contains(t, err.Error(), "error_code", "the upstream code must stay visible in the message")
		})
	}
}

// Every in-band Baidu error used to collapse into
// (ErrorCodeBadResponseBody, 500). That pair is a dead zone: the error code is
// in alwaysSkipRetryCodes so the request is never retried, and 500 is not in the
// default auto-disable allow-list so the channel is never taken out of rotation
// either. Assert the escape from that pair directly — a status check alone would
// still pass if the error code regressed.
func TestInBandError_EscapesNeverRetryNeverDisableDeadZone(t *testing.T) {
	t.Parallel()

	require.True(t, operation_setting.IsAlwaysSkipRetryCode(types.ErrorCodeBadResponseBody),
		"guards the premise: ErrorCodeBadResponseBody is the never-retry code")

	for _, errorCode := range []int{110, 111, 18, 17, 4, 336501, 336003, 0} {
		err := inBandError(errorCode, "upstream failure")
		require.NotNil(t, err)

		assert.False(t, operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()),
			"error_code %d must not map to a never-retry error code", errorCode)
		assert.True(t, operation_setting.ShouldRetryByStatusCode(err.StatusCode),
			"error_code %d must produce a retryable status, got %d", errorCode, err.StatusCode)
	}
}

// A rejected credential is the one in-band error that should pull the channel
// out of rotation; throttling deliberately must not, because blaming a channel
// for being busy poisons selection for every caller.
func TestInBandError_OnlyAuthFailureCanDisableChannel(t *testing.T) {
	t.Parallel()

	for _, errorCode := range []int{110, 111} {
		err := inBandError(errorCode, "token rejected")
		assert.True(t, operation_setting.ShouldDisableByStatusCode(err.StatusCode),
			"error_code %d is a dead credential and should be able to disable the channel", errorCode)
	}

	for _, errorCode := range []int{4, 17, 18, 336501, 336003, 0} {
		err := inBandError(errorCode, "busy or unknown")
		assert.False(t, operation_setting.ShouldDisableByStatusCode(err.StatusCode),
			"error_code %d must not disable the channel", errorCode)
	}
}
