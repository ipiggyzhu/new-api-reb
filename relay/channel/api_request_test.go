package channel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// gin.SetMode writes a package global. These tests run in parallel, so setting
// it per test made every -race run report a write/write race and drown out any
// real one.
func init() {
	gin.SetMode(gin.TestMode)
}

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}

func TestProcessHeaderOverride_WildcardPassthroughWithholdsCredentialHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Headers a caller authenticates to this gateway with, plus ones an
	// adaptor fills in with the channel's own credentials downstream.
	ctx.Request.Header.Set("Authorization", "Bearer sk-caller-token")
	ctx.Request.Header.Set("X-Api-Key", "sk-caller-token")
	ctx.Request.Header.Set("X-Goog-Api-Key", "sk-caller-token")
	ctx.Request.Header.Set("Api-Key", "sk-caller-token")
	ctx.Request.Header.Set("Mj-Api-Secret", "sk-caller-token")
	ctx.Request.Header.Set("New-Api-User", "1")
	ctx.Request.Header.Set("Sec-WebSocket-Protocol", "openai-insecure-api-key.sk-caller-token")
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			// The "*" rule: with send_original_request removed, the wildcard
			// override is the only way caller headers still reach this code path.
			HeadersOverride: map[string]any{"*": ""},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)

	for _, name := range []string{
		"authorization",
		"x-api-key",
		"x-goog-api-key",
		"api-key",
		"mj-api-secret",
		"new-api-user",
		"sec-websocket-protocol",
	} {
		_, exists := headers[name]
		require.Falsef(t, exists, "credential header %q must not be forwarded upstream", name)
	}

	// Non-credential headers are still passed through.
	require.Equal(t, "trace-123", headers["x-trace-id"])
}

func TestProcessHeaderOverride_NilChannelMetaDoesNotPanic(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	headers, err := processHeaderOverride(&relaycommon.RelayInfo{}, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

// A channel test builds its own request, and SetupApiRequestHeader copies only
// Content-Type/Accept from it. Without forwarding, an upstream sees no
// user-agent at all — and the upstreams that matter here are exactly the ones
// that reject a request whose client they cannot identify.
func TestSetupApiRequestHeaderForwardsChannelTestClientHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("user-agent", "claude-cli/1.0.0 (external, cli)")
	ctx.Request.Header.Set("x-app", "cli")

	info := &relaycommon.RelayInfo{IsChannelTest: true}

	header := http.Header{}
	SetupApiRequestHeader(info, ctx, &header)

	require.Equal(t, "claude-cli/1.0.0 (external, cli)", header.Get("user-agent"))
	require.Equal(t, "cli", header.Get("x-app"))
}

// Live traffic must not start forwarding caller headers implicitly: that is what
// the channel's "send original request" setting is for.
func TestSetupApiRequestHeaderDoesNotForwardClientHeadersForLiveTraffic(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Request.Header.Set("user-agent", "some-browser/1")
	ctx.Request.Header.Set("x-app", "cli")

	info := &relaycommon.RelayInfo{IsChannelTest: false}

	header := http.Header{}
	SetupApiRequestHeader(info, ctx, &header)

	require.Empty(t, header.Get("user-agent"))
	require.Empty(t, header.Get("x-app"))
}

// Credentials must never ride along: the channel's own key is set by the
// adaptor, and forwarding these would send the caller's key upstream.
func TestSetupApiRequestHeaderChannelTestSkipsCredentialAndHopHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Request.Header.Set("Authorization", "Bearer caller-secret")
	ctx.Request.Header.Set("x-api-key", "caller-key")
	ctx.Request.Header.Set("new-api-user", "42")
	ctx.Request.Header.Set("Connection", "keep-alive")
	ctx.Request.Header.Set("user-agent", "claude-cli/1.0.0 (external, cli)")

	info := &relaycommon.RelayInfo{IsChannelTest: true}

	header := http.Header{}
	SetupApiRequestHeader(info, ctx, &header)

	require.Empty(t, header.Get("Authorization"))
	require.Empty(t, header.Get("x-api-key"))
	require.Empty(t, header.Get("new-api-user"))
	require.Empty(t, header.Get("Connection"))
	require.Equal(t, "claude-cli/1.0.0 (external, cli)", header.Get("user-agent"))
}

// An adaptor that already chose a value owns it. Claude sets anthropic-version
// from its own default, and forwarding must not fight that.
func TestSetupApiRequestHeaderChannelTestDoesNotOverrideAdaptorValues(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Request.Header.Set("anthropic-version", "2023-06-01")

	info := &relaycommon.RelayInfo{IsChannelTest: true}

	header := http.Header{}
	header.Set("anthropic-version", "2100-01-01")
	SetupApiRequestHeader(info, ctx, &header)

	require.Equal(t, "2100-01-01", header.Get("anthropic-version"))
}
