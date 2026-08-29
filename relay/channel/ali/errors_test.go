package ali

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
)

func TestAliInBandErrorStatusCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		code           string
		respStatusCode int
		want           int
	}{
		{"throttling", "Throttling", http.StatusOK, http.StatusTooManyRequests},
		{"throttling rate quota", "Throttling.RateQuota", http.StatusOK, http.StatusTooManyRequests},
		{"throttling allocation quota", "Throttling.AllocationQuota", http.StatusOK, http.StatusTooManyRequests},
		{"invalid api key", "InvalidApiKey", http.StatusOK, http.StatusUnauthorized},
		{"unauthorized", "Unauthorized", http.StatusOK, http.StatusUnauthorized},
		{"access denied", "AccessDenied", http.StatusOK, http.StatusUnauthorized},
		{"unknown code on 200 falls back to bad gateway", "InternalError", http.StatusOK, http.StatusBadGateway},
		{"empty code on 200 falls back to bad gateway", "", http.StatusOK, http.StatusBadGateway},
		{"unknown code keeps a real upstream status", "InternalError", http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"classification wins over a real upstream status", "Throttling", http.StatusServiceUnavailable, http.StatusTooManyRequests},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, aliInBandErrorStatusCode(tc.code, tc.respStatusCode))
		})
	}
}

// Throttling and a rejected key must land on opposite sides of the auto-disable
// decision: a busy channel has to stay in rotation, a dead credential should not.
func TestAliInBandErrorStatusCode_DisableVerdict(t *testing.T) {
	t.Parallel()

	assert.False(t, operation_setting.ShouldDisableByStatusCode(aliInBandErrorStatusCode("Throttling", http.StatusOK)))
	assert.True(t, operation_setting.ShouldDisableByStatusCode(aliInBandErrorStatusCode("InvalidApiKey", http.StatusOK)))
	assert.False(t, operation_setting.ShouldDisableByStatusCode(aliInBandErrorStatusCode("InternalError", http.StatusOK)))

	for _, code := range []string{"Throttling", "InvalidApiKey", "InternalError"} {
		assert.True(t, operation_setting.ShouldRetryByStatusCode(aliInBandErrorStatusCode(code, http.StatusOK)),
			"%s must stay retryable", code)
	}
}
