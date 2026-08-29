package baidu

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/types"
)

// inBandError converts a Baidu error carried in the body of an HTTP 200
// response into a relay error whose status and error code reflect what actually
// went wrong.
//
// Every in-band Baidu error used to collapse into
// (ErrorCodeBadResponseBody, 500), which landed in a dead zone: that error code
// is in alwaysSkipRetryCodes so the request was never retried, and 500 is not in
// the default auto-disable allow-list (401 only) so the channel was never taken
// out of rotation either. A Baidu channel with an expired token therefore failed
// every request forever without ever being retried around or disabled.
//
// Only the two token codes map to 401, because that is the one status that can
// trigger auto-disable and a wrong guess there would remove a working channel.
// Everything unrecognized becomes 502: retryable, and not a disable trigger.
func inBandError(errorCode int, errorMsg string) *types.NewAPIError {
	err := fmt.Errorf("%s (error_code: %d)", errorMsg, errorCode)
	switch errorCode {
	case 110, 111: // access token invalid / expired
		return types.NewErrorWithStatusCode(err, types.ErrorCodeChannelInvalidKey, http.StatusUnauthorized)
	case 4, 17, 18, 336501: // qps, daily and tpm/rpm request limits
		return types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponse, http.StatusTooManyRequests)
	}
	return types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
}
