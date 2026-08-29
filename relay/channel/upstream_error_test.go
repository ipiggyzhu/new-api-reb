package channel

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
)

func TestUpstreamInBandErrorStatusCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		respStatusCode int
		want           int
	}{
		{"200 becomes bad gateway", http.StatusOK, http.StatusBadGateway},
		{"201 becomes bad gateway", http.StatusCreated, http.StatusBadGateway},
		{"299 becomes bad gateway", 299, http.StatusBadGateway},
		{"upstream 401 is honored", http.StatusUnauthorized, http.StatusUnauthorized},
		{"upstream 429 is honored", http.StatusTooManyRequests, http.StatusTooManyRequests},
		{"upstream 500 is honored", http.StatusInternalServerError, http.StatusInternalServerError},
		{"redirect is honored", http.StatusFound, http.StatusFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, UpstreamInBandErrorStatusCode(tc.respStatusCode))
		})
	}
}

// The whole point of remapping a 2xx is that the relay acts on the result, so
// assert the downstream consequence rather than only the number: a 2xx is not
// retryable, which is why an in-band error reported as 200 silently became a
// success, and the substituted status must be.
func TestUpstreamInBandErrorStatusCode_IsRetryable(t *testing.T) {
	t.Parallel()

	assert.False(t, operation_setting.ShouldRetryByStatusCode(http.StatusOK),
		"a 2xx is not retryable, which is the bug this remap exists to fix")
	assert.True(t, operation_setting.ShouldRetryByStatusCode(UpstreamInBandErrorStatusCode(http.StatusOK)))
	assert.False(t, operation_setting.ShouldDisableByStatusCode(UpstreamInBandErrorStatusCode(http.StatusOK)),
		"an unclassified upstream error must not take the channel out of rotation")
}
