package channel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// anthropic-beta of Claude Code 2.1.306 in its default cache mode
// (/tmp/capture/002.json). With ENABLE_PROMPT_CACHING_1H=1 the same client
// appends extended-cache-ttl-2025-04-11 (/tmp/capture/005.json).
const claudeCode2306Beta = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,advanced-tool-use-2025-11-20,effort-2025-11-24"

type cliWireCapture struct {
	header http.Header
	body   []byte
}

// cliTLSUpstream records what a versioned profile puts on the wire; profiles
// bind a captured TLS handshake, so they only send over HTTPS.
func cliTLSUpstream(t *testing.T) (*httptest.Server, chan cliWireCapture) {
	t.Helper()
	old := appcommon.TLSInsecureSkipVerify
	appcommon.TLSInsecureSkipVerify = true // only this test's local self-signed server
	service.InitHttpClient()
	service.ResetProxyClientCache()
	t.Cleanup(func() { appcommon.TLSInsecureSkipVerify = old; service.ResetProxyClientCache() })
	captures := make(chan cliWireCapture, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captures <- cliWireCapture{r.Header.Clone(), body}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	return server, captures
}

// A converted (non-native) request is laid out by the selected profile: the
// same request gets the capture's breakpoint positions under both 2.1.306
// profiles, and only the cache mode differs — every breakpoint's TTL in the
// body and the extended-cache-ttl beta flag in the headers, together.
func TestClaudeVersionProfileTTLModeShapesConvertedRequest(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	cases := []struct {
		profile, cacheControl, beta string
	}{
		{"claude-2.1.306-linux-x64", `{"type":"ephemeral"}`, claudeCode2306Beta},
		{"claude-2.1.306-linux-x64-1h", `{"type":"ephemeral","ttl":"1h"}`, claudeCode2306Beta + ",extended-cache-ttl-2025-04-11"},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			converted, _, c, info := convertAndShapeOpenAIRequest(t, "claude", withClientCache.Replace(oddlyCachedOpenAIRequest))
			info.ChannelSetting.CLIVersionProfile = tc.profile
			resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(string(converted)))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			got := <-captures

			// The capture's layout: the bare billing block first, then the
			// last two system blocks and the last message block, nothing on
			// tools. Exact JSON, key order included.
			assert.Equal(t, `{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.306.813; cc_entrypoint=sdk-cli;"}`,
				gjson.GetBytes(got.body, "system.0").Raw)
			breakpoints := []string{"system.1.cache_control", "system.2.cache_control", "messages.3.content.0.cache_control"}
			require.Len(t, claudeCacheBreakpoints(t, got.body), len(breakpoints))
			for _, path := range breakpoints {
				assert.Equal(t, tc.cacheControl, gjson.GetBytes(got.body, path).Raw, path)
			}
			assert.Equal(t, "Bash", gjson.GetBytes(got.body, "tools.0.name").String(), "captured tool order")
			assert.Equal(t, "lookup", gjson.GetBytes(got.body, "tools.1.name").String())

			assert.Equal(t, tc.beta, got.header.Get("anthropic-beta"))
			assert.Equal(t, "claude-cli/2.1.306 (external, sdk-cli)", got.header.Get("User-Agent"))
			assert.Equal(t, "Linux", got.header.Get("X-Stainless-OS"))
			assert.Equal(t, "0.128.0", got.header.Get("X-Stainless-Package-Version"))
		})
	}
}

// Under a 1h profile the real Claude Code client still decides its own cache
// layout (task 4): its 1h and 5m breakpoints reach upstream unchanged. Only
// when it sent none does the shaping layer place the profile's breakpoints.
func TestClaudeVersionProfileTTLModeKeepsNativeClaudeCodeBreakpoints(t *testing.T) {
	const profile = "claude-2.1.306-linux-x64-1h"
	c, info := nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = profile
	sent := []byte(withClientCache.Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1)))
	shaped := shapeClaudeMessagesBody(t, c, info, sent)
	assert.Equal(t, map[string]string{
		"tools.0.cache_control":                        "ephemeral 1h",
		"system.1.cache_control":                       "ephemeral 1h",
		"messages.0.content.0.cache_control":           "ephemeral",
		"messages.2.content.0.content.0.cache_control": "ephemeral",
	}, claudeCacheBreakpoints(t, shaped))
	assert.JSONEq(t, gjson.GetBytes(sent, "system").Raw, gjson.GetBytes(shaped, "system").Raw)
	assert.JSONEq(t, gjson.GetBytes(sent, "messages").Raw, gjson.GetBytes(shaped, "messages").Raw)
	assert.Equal(t, `{"type":"ephemeral"}`, gjson.GetBytes(shaped, "messages.0.content.0.cache_control").Raw, "a native 5m breakpoint must not gain the profile TTL")
	assert.Equal(t, "Write", gjson.GetBytes(shaped, "tools.0.name").String(), "a native cache boundary must not move")

	c, info = nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = profile
	uncached := shapeClaudeMessagesBody(t, c, info, []byte(`{"model":"claude-sonnet-4-5","max_tokens":32000,
		"system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"project instructions"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"thanks"}]}]}`))
	breakpoints := []string{"system.0.cache_control", "system.1.cache_control", "messages.0.content.0.cache_control"}
	require.Len(t, claudeCacheBreakpoints(t, uncached), len(breakpoints))
	for _, path := range breakpoints {
		assert.Equal(t, `{"type":"ephemeral","ttl":"1h"}`, gjson.GetBytes(uncached, path).Raw, path)
	}
}

// Codex 0.163.0 (/tmp/capture/004.json) has no cache breakpoints: its cache
// identity is prompt_cache_key == session, and the body's top-level fields
// are exactly the capture's, in the capture's order.
func TestCodexVersionProfileOwnsPromptCacheAndMatchesCapture(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	c, info := cliTestRequest("codex")
	info.ChannelSetting.CLIVersionProfile = "codex-0.163.0-linux-x64"
	body := `{"model":"gpt-6.1-sol","prompt_cache_key":"caller-cache-key","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"explicit"},"cache_control":{"type":"ephemeral"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"Say ok.","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"reasoning":{"effort":"low","context":"all_turns"}}`
	resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	got := <-captures

	keys := func(value gjson.Result) []string {
		var names []string
		value.ForEach(func(key, _ gjson.Result) bool {
			names = append(names, key.String())
			return true
		})
		return names
	}
	assert.Equal(t, []string{"model", "stream", "input", "tool_choice", "parallel_tool_calls", "reasoning", "store",
		"include", "prompt_cache_key", "text", "client_metadata"}, keys(gjson.ParseBytes(got.body)))
	session := got.header.Get("Session-Id")
	require.NotEmpty(t, session)
	assert.NotEqual(t, "caller-cache-key", session)
	assert.Equal(t, session, gjson.GetBytes(got.body, "prompt_cache_key").String())
	for _, field := range []string{`"cache_control"`, `"prompt_cache_retention"`, `"prompt_cache_options"`} {
		assert.NotContains(t, string(got.body), field)
	}
	assert.Equal(t, "additional_tools", gjson.GetBytes(got.body, "input.0.type").String(), "0.163 sends tools inside input")
	assert.Equal(t, "false", gjson.GetBytes(got.body, "store").Raw)

	assert.Equal(t, "codex_exec/0.163.0 (Debian 13.0.0; x86_64) unknown (codex_exec; 0.163.0)", got.header.Get("User-Agent"))
	assert.Equal(t, session, got.header.Get("Thread-Id"))
	assert.Equal(t, session, got.header.Get("X-Client-Request-Id"), "0.163 sends the session as the request id")
	assert.Equal(t, session+":0", got.header.Get("X-Codex-Window-Id"))
	assert.Equal(t, "true", got.header.Get("X-Openai-Internal-Codex-Responses-Lite"))
	assert.Equal(t, []string{"x-codex-installation-id", "thread_id", "session_id", "x-codex-window-id", "root_turn_id",
		"turn_id", "x-codex-turn-metadata"}, keys(gjson.GetBytes(got.body, "client_metadata")))
	turn := gjson.Parse(got.header.Get("X-Codex-Turn-Metadata"))
	assert.Equal(t, gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String(), turn.Raw)
	assert.Equal(t, []string{"installation_id", "session_id", "thread_id", "agent_name", "turn_id", "window_id",
		"window_number", "context_window_id", "request_kind", "history_initialization", "root_turn_id", "thread_source",
		"turn_trigger", "sandbox", "sandbox_mode", "auto_review_enabled", "node_repl_auto_review_required",
		"node_repl_disabled", "turn_started_at_unix_ms", "analytics_enabled", "model", "reasoning_effort"}, keys(turn))
	assert.Equal(t, "new", turn.Get("history_initialization").String())
}
