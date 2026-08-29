package ali

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/relay/channel"
)

// aliInBandErrorStatusCode maps a DashScope error code carried in an HTTP 200
// body to the status the relay should act on.
//
// DashScope reports throttling as a Throttling* code rather than a 429, and a
// bad credential as InvalidApiKey rather than a 401. Both used to reach the
// relay as the upstream's own 2xx, which made a throttled channel and a dead
// channel indistinguishable from a healthy one. Separating them matters because
// the two verdicts are opposites: a 429 is deliberately not treated as a
// channel fault (throttling is transient and blaming the channel would poison
// selection for everyone), while a rejected key is exactly the fault that
// should take the channel out of rotation.
func aliInBandErrorStatusCode(code string, respStatusCode int) int {
	switch {
	case strings.HasPrefix(code, "Throttling"):
		return http.StatusTooManyRequests
	case code == "InvalidApiKey", code == "Unauthorized", code == "AccessDenied":
		return http.StatusUnauthorized
	}
	return channel.UpstreamInBandErrorStatusCode(respStatusCode)
}
