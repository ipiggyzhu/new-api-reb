package claude

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdaptorDoRequestPreservesAnthropicBetaHeaders(t *testing.T) {
	service.InitHttpClient()
	settings := model_setting.GetClaudeSettings()
	originalHeaders := settings.HeadersSettings
	settings.HeadersSettings = map[string]map[string][]string{}
	t.Cleanup(func() { settings.HeadersSettings = originalHeaders })

	const combinedBeta = "claude-code-20250219,context-1m-2025-08-07"
	cases := []struct {
		name            string
		clientBeta      []string
		headersOverride map[string]interface{}
		wantBeta        string
	}{
		{
			name:       "1m beta on the second header line",
			clientBeta: []string{"claude-code-20250219", "context-1m-2025-08-07"},
			wantBeta:   combinedBeta,
		},
		{
			name:       "single comma-separated header stays unchanged",
			clientBeta: []string{combinedBeta},
			wantBeta:   combinedBeta,
		},
		{
			name:            "wildcard passthrough preserves every beta value",
			clientBeta:      []string{"claude-code-20250219", "context-1m-2025-08-07"},
			headersOverride: map[string]interface{}{"*": ""},
			wantBeta:        combinedBeta,
		},
		{
			name:            "matching passthrough preserves every beta value",
			clientBeta:      []string{"claude-code-20250219", "context-1m-2025-08-07"},
			headersOverride: map[string]interface{}{"re:(?i)^anthropic-beta$": ""},
			wantBeta:        combinedBeta,
		},
		{
			name:            "client header placeholder preserves every beta value",
			clientBeta:      []string{"claude-code-20250219", "context-1m-2025-08-07"},
			headersOverride: map[string]interface{}{"anthropic-beta": "{client_header:anthropic-beta}"},
			wantBeta:        combinedBeta,
		},
		{
			name:       "explicit channel override wins over passthrough",
			clientBeta: []string{"claude-code-20250219", "context-1m-2025-08-07"},
			headersOverride: map[string]interface{}{
				"*":              "",
				"anthropic-beta": "computer-use-2025-01-24",
			},
			wantBeta: "computer-use-2025-01-24",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type capturedRequest struct {
				header  http.Header
				body    []byte
				readErr error
			}
			captured := make(chan capturedRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				captured <- capturedRequest{header: r.Header.Clone(), body: body, readErr: err}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)

			request := &dto.ClaudeRequest{Model: "fable-route", MaxTokens: common.GetPointer(uint(1))}
			clientBody, err := common.Marshal(request)
			require.NoError(t, err)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(clientBody))
			ctx.Request.Header.Set("Content-Type", "application/json")
			for _, value := range tc.clientBeta {
				ctx.Request.Header.Add("anthropic-beta", value)
			}
			ctx.Set("model_mapping", `{"fable-route":"claude-fable-5-1"}`)
			info := &relaycommon.RelayInfo{
				OriginModelName: request.Model,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelBaseUrl:    upstream.URL,
					UpstreamModelName: request.Model,
					ApiKey:            "local-test-key",
					HeadersOverride:   tc.headersOverride,
				},
			}
			require.NoError(t, helper.ModelMappedHelper(ctx, info, request))
			upstreamBody, err := common.Marshal(request)
			require.NoError(t, err)
			result, err := (&Adaptor{}).DoRequest(ctx, info, bytes.NewReader(upstreamBody))
			require.NoError(t, err)
			response, ok := result.(*http.Response)
			require.True(t, ok)
			require.NoError(t, response.Body.Close())

			received := <-captured
			require.NoError(t, received.readErr)
			assert.Equal(t, []string{tc.wantBeta}, received.header.Values("anthropic-beta"))
			var receivedBody dto.ClaudeRequest
			require.NoError(t, common.Unmarshal(received.body, &receivedBody))
			assert.Equal(t, "claude-fable-5-1", receivedBody.Model)
		})
	}
}
