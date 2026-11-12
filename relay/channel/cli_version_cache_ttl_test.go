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

// The same for Linux 2.1.288 and 2.1.282 (/tmp/t20/cap/<version>-5m-net and
// -1h-net), whose 1h captures append the same flag.
const (
	claudeCode2288Beta = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,effort-2025-11-24"
	claudeCode2282Beta = "claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,advisor-tool-2026-03-01,effort-2025-11-24"
)

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
			assert.Equal(t, `{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.306.aba; cc_entrypoint=sdk-cli;"}`,
				gjson.GetBytes(got.body, "system.0").Raw, `the client's suffix for "first question"`)
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

// A native CLI uses the selected profile's cache layout and TTL, just like a
// converted request. Caller breakpoints cannot bypass the profile.
func TestClaudeVersionProfileTTLModeOwnsNativeClaudeCodeBreakpoints(t *testing.T) {
	const profile = "claude-2.1.306-linux-x64-1h"
	c, info := nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = profile
	sent := []byte(withClientCache.Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1)))
	shaped := shapeClaudeMessagesBody(t, c, info, sent)
	assert.Equal(t, map[string]string{
		"system.1.cache_control":             "ephemeral 1h",
		"system.2.cache_control":             "ephemeral 1h",
		"messages.4.content.0.cache_control": "ephemeral 1h",
	}, claudeCacheBreakpoints(t, shaped))
	assertProfileBillingLeads(t, "2.1.306", string(sent), shaped)
	assert.JSONEq(t, string(shaped),
		string(shapeClaudeMessagesBody(t, c, info, []byte(withoutClientCache.Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1))))),
		"the same history receives the same profile shape regardless of caller breakpoints")
	assert.False(t, gjson.GetBytes(shaped, "messages.0.content.0.cache_control").Exists())
	assert.Equal(t, "file body", gjson.GetBytes(shaped, "messages.2.content.0.content.0.text").String())
	assert.Equal(t, "Bash", gjson.GetBytes(shaped, "tools.0.name").String(), "caller breakpoints cannot pin tool order")

	c, info = nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = profile
	uncached := shapeClaudeMessagesBody(t, c, info, []byte(`{"model":"claude-sonnet-4-5","max_tokens":32000,
		"system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"project instructions"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"thanks"}]}]}`))
	breakpoints := []string{"system.1.cache_control", "system.2.cache_control", "messages.0.content.0.cache_control"}
	require.Len(t, claudeCacheBreakpoints(t, uncached), len(breakpoints))
	for _, path := range breakpoints {
		assert.Equal(t, `{"type":"ephemeral","ttl":"1h"}`, gjson.GetBytes(uncached, path).Raw, path)
	}
}

// The channel-wide TTL choice controls native and converted requests alike.
func TestClaudeVersionProfileTTLModeControlsNativeRequests(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	bodies := map[string]string{
		"client sends 1h and 5m": withClientCache.Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1)),
		"client sends only 5m": strings.NewReplacer("@ttl1h", `,"cache_control":{"type":"ephemeral"}`, "@ttl5m", `,"cache_control":{"type":"ephemeral"}`).
			Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1)),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			wire := make(map[string]cliWireCapture)
			for _, profile := range []string{"claude-2.1.306-linux-x64", "claude-2.1.306-linux-x64-1h"} {
				c, info := nativeClaudeCodeRequest()
				info.ChannelSetting.CLIVersionProfile = profile
				withOriginalRequest(t, c, body)
				resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(body))
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				wire[profile] = <-captures
			}
			def, hour := wire["claude-2.1.306-linux-x64"], wire["claude-2.1.306-linux-x64-1h"]
			assert.NotEqual(t, string(def.body), string(hour.body))
			assert.Equal(t, claudeCode2306Beta, def.header.Get("anthropic-beta"))
			assert.Equal(t, claudeCode2306Beta+",extended-cache-ttl-2025-04-11", hour.header.Get("anthropic-beta"))
			assert.Equal(t, map[string]string{
				"system.1.cache_control": "ephemeral", "system.2.cache_control": "ephemeral",
				"messages.4.content.0.cache_control": "ephemeral",
			}, claudeCacheBreakpoints(t, def.body))
			assert.Equal(t, map[string]string{
				"system.1.cache_control": "ephemeral 1h", "system.2.cache_control": "ephemeral 1h",
				"messages.4.content.0.cache_control": "ephemeral 1h",
			}, claudeCacheBreakpoints(t, hour.body))
			assert.Equal(t, def.header.Get("X-Claude-Code-Session-Id"), hour.header.Get("X-Claude-Code-Session-Id"))
			assert.Equal(t, gjson.GetBytes(def.body, "messages.2.content.0.content.0.text").String(),
				gjson.GetBytes(hour.body, "messages.2.content.0.content.0.text").String())
		})
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
