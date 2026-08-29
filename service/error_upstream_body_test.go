package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func relayError(t *testing.T, status int, body string) *types.NewAPIError {
	t.Helper()
	resp := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	return RelayErrorHandler(context.Background(), resp, false)
}

// An upstream that answers with a block page, a gateway error page, or plain
// text leaves nothing in the error message but the status code. The raw body is
// the only thing that distinguishes a Cloudflare challenge from a dead origin,
// so it has to survive into the error for the admin log.
func TestRelayErrorHandlerKeepsUnparseableUpstreamBody(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantInBody  string
		wantMessage string
	}{
		{
			name:        "cloudflare block page",
			status:      http.StatusForbidden,
			body:        "<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head><body>Ray ID: 8f2b1c3d4e5f6a7b</body></html>",
			wantInBody:  "Cloudflare",
			wantMessage: "bad response status code 403",
		},
		{
			name:        "nginx bad gateway",
			status:      http.StatusBadGateway,
			body:        "<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center></body></html>",
			wantInBody:  "502 Bad Gateway",
			wantMessage: "bad response status code 502",
		},
		{
			name:        "plaintext refusal",
			status:      http.StatusPaymentRequired,
			body:        "insufficient balance",
			wantInBody:  "insufficient balance",
			wantMessage: "bad response status code 402",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := relayError(t, tc.status, tc.body)

			assert.Equal(t, tc.wantMessage, apiErr.Error(),
				"the caller-visible message must not start leaking upstream bodies")
			assert.Contains(t, apiErr.GetUpstreamBody(), tc.wantInBody,
				"the admin log needs the raw body to identify the blocker")
		})
	}
}

// A well-formed provider error already carries everything in its message, so
// storing the body again would just duplicate it in every error log row.
func TestRelayErrorHandlerDoesNotDuplicateStructuredErrors(t *testing.T) {
	apiErr := relayError(t, http.StatusTooManyRequests,
		`{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`)

	assert.Equal(t, "You exceeded your current quota", apiErr.Error())
	assert.Equal(t, types.ErrorCode("insufficient_quota"), apiErr.GetErrorCode())
	assert.Empty(t, apiErr.GetUpstreamBody())
}

// JSON that parses but matches none of the known error shapes yields an empty
// message, which is the worst case for diagnosis — keep the body for that too.
func TestRelayErrorHandlerKeepsUnrecognisedJSONShape(t *testing.T) {
	apiErr := relayError(t, http.StatusInternalServerError, `{"weird_field":{"nested":"upstream exploded"}}`)

	assert.Contains(t, apiErr.GetUpstreamBody(), "upstream exploded")
}

// An empty body carries no information; recording it would add noise to a
// column that is read by hand.
func TestRelayErrorHandlerIgnoresEmptyBody(t *testing.T) {
	assert.Empty(t, relayError(t, http.StatusInternalServerError, "").GetUpstreamBody())
	assert.Empty(t, relayError(t, http.StatusInternalServerError, "   \n  ").GetUpstreamBody())
}

// Block pages are full HTML documents and this value lands in every error log
// row, so it must be bounded and must not end mid-character.
func TestRelayErrorHandlerBoundsUpstreamBody(t *testing.T) {
	huge := "锁" + strings.Repeat("很长的中文错误页面内容", 5000)
	require.Greater(t, len(huge), types.UpstreamBodyLogLimit)

	stored := relayError(t, http.StatusForbidden, huge).GetUpstreamBody()

	assert.Less(t, len(stored), types.UpstreamBodyLogLimit+64)
	assert.True(t, utf8.ValidString(stored), "truncation must land on a rune boundary")
	assert.Contains(t, stored, "truncated")
}

// countingReader reports how many bytes were actually pulled off the wire,
// which is the only thing that proves the read is bounded — the parsed message
// and the admin copy are both capped elsewhere, so asserting on those passes
// whether or not the read itself is limited.
type countingReader struct {
	src  io.Reader
	read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.read += n
	return n, err
}

// A hostile or broken upstream can answer an error with an arbitrarily large
// body. RelayErrorHandler buffers that body in memory before doing anything
// else, so it has to be bounded — otherwise one failed request per upstream is
// enough to push the gateway into OOM.
func TestRelayErrorHandlerBoundsUpstreamBodyRead(t *testing.T) {
	oversized := upstreamErrorBodyReadLimit * 4
	counter := &countingReader{src: strings.NewReader(strings.Repeat("A", oversized))}
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(counter),
		Header:     http.Header{},
	}

	apiErr := RelayErrorHandler(context.Background(), resp, false)
	require.NotNil(t, apiErr)

	assert.LessOrEqual(t, counter.read, upstreamErrorBodyReadLimit,
		"RelayErrorHandler must stop reading at the limit instead of buffering the whole body")
	assert.Less(t, counter.read, oversized, "the whole body must not reach memory")
}
