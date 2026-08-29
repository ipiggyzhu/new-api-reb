package channel

import "net/http"

// UpstreamInBandErrorStatusCode resolves the HTTP status to report for an error
// the upstream embedded in the body of a response it still sent with a success
// status.
//
// Several providers (Baidu, Tencent Hunyuan, Zhipu, Ali DashScope) answer a
// failed request with HTTP 200 and an error object in the payload. The adaptors
// that detect those objects used to forward resp.StatusCode verbatim, so the
// relay saw a 2xx failure: shouldRetry declines on 2xx, the channel accrued no
// fault, and channel affinity kept steering traffic at a provider that was
// answering nothing but errors.
//
// A 2xx therefore becomes 502 — the upstream did respond, but not usably, which
// is exactly what a bad gateway is. Any non-2xx the provider did set is honored
// as-is, since it already carries the provider's own verdict.
func UpstreamInBandErrorStatusCode(respStatusCode int) int {
	if respStatusCode >= http.StatusOK && respStatusCode < http.StatusMultipleChoices {
		return http.StatusBadGateway
	}
	return respStatusCode
}
