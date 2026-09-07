package types

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToClaudeErrorPreservesUpstreamTypeAndCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantType string
	}{
		{
			name:     "missing code uses upstream type",
			body:     `{"message":"Service Unavailable","type":"error"}`,
			wantType: "error",
		},
		{
			name:     "null code uses upstream type",
			body:     `{"message":"Service Unavailable","type":"error","code":null}`,
			wantType: "error",
		},
		{
			name:     "empty code uses upstream type",
			body:     `{"message":"Service Unavailable","type":"error","code":""}`,
			wantType: "error",
		},
		{
			name:     "missing code and type use upstream default",
			body:     `{"message":"Service Unavailable"}`,
			wantType: "upstream_error",
		},
		{
			name:     "explicit string code stays preferred",
			body:     `{"message":"Service Unavailable","type":"error","code":"upstream_unavailable"}`,
			wantType: "upstream_unavailable",
		},
		{
			name:     "numeric code stays formatted",
			body:     `{"message":"Service Unavailable","type":"error","code":503}`,
			wantType: "503",
		},
		{
			name:     "zero numeric code stays formatted",
			body:     `{"message":"Service Unavailable","type":"error","code":0}`,
			wantType: "0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstream OpenAIError
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &upstream))
			apiErr := WithOpenAIError(upstream, http.StatusServiceUnavailable)

			assert.Equal(t, ClaudeError{Type: tc.wantType, Message: upstream.Message}, apiErr.ToClaudeError())
			assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
		})
	}
}
