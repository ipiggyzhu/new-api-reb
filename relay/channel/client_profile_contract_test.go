package channel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCLIRequestSampleContractAndTLSOnlyOverride(t *testing.T) {
	previous := appcommon.TLSInsecureSkipVerify
	appcommon.TLSInsecureSkipVerify = true // local httptest certificate only
	service.InitHttpClient()
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		appcommon.TLSInsecureSkipVerify = previous
		service.InitHttpClient()
		service.ResetProxyClientCache()
	})
	type capture struct {
		header http.Header
		body   []byte
	}
	got := make(chan capture, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- capture{r.Header.Clone(), body}
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			path, input := "/v1/messages", `{"system":"keep prompt","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"custom","input_schema":{"type":"object","properties":{"v":{"type":"string"}}}}]}`
			if family == "codex" {
				path, input = "/v1/responses", `{"instructions":"keep prompt","input":"hi","tools":[{"type":"function","name":"custom","parameters":{"type":"object","properties":{"v":{"type":"string"}}}}]}`
			}
			var baseline capture
			for _, preset := range []struct{ id, runtime string }{
				{"", "v26.3.0"}, {"claude-node-22.14.0", "v22.14.0"}, {"claude-node-24.19.0", "v24.19.0"},
			} {
				c, info := cliTestRequest(family)
				info.ChannelSetting.TLSFingerprint = preset.id
				resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + path}, c, info, strings.NewReader(input))
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				received := <-got
				assert.Empty(t, received.header.Get("Accept-Language"), "neither JSON sample sends this header")
				if family == "claude" {
					// Stable field values read from claude_code_capture.json; dynamic
					// auth, destination, lengths and isolated IDs are checked separately.
					for key, want := range map[string]string{
						"Accept": "application/json", "Content-Type": "application/json",
						"User-Agent":          "claude-cli/2.1.282 (external, sdk-cli)",
						"X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": preset.runtime,
						"X-Stainless-OS": "Windows", "X-Stainless-Arch": "x64",
						"X-Stainless-Lang": "js", "X-Stainless-Package-Version": "0.112.1",
						"X-Stainless-Retry-Count": "0", "X-Stainless-Timeout": "600",
						"anthropic-version": "2023-06-01", "anthropic-dangerous-direct-browser-access": "true",
						"x-app": "cli", "Connection": "keep-alive", "Accept-Encoding": "gzip, deflate, br, zstd",
						"anthropic-beta": claudeCodeBeta,
					} {
						assert.Equal(t, want, received.header.Get(key), key)
					}
					identity := gjson.Parse(gjson.GetBytes(received.body, "metadata.user_id").String())
					assert.Regexp(t, `^[0-9a-f]{64}$`, identity.Get("device_id").String())
					assert.Empty(t, identity.Get("account_uuid").String())
					assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, identity.Get("session_id").String())
					assert.Equal(t, identity.Get("session_id").String(), received.header.Get("X-Claude-Code-Session-Id"))
				} else {
					assert.Equal(t, "text/event-stream", received.header.Get("Accept"))
					assert.Equal(t, "codex_exec", received.header.Get("Originator"))
					assert.Equal(t, "codex_exec/0.156.1 (Windows 10.0.26100; x86_64) WindowsTerminal (codex_exec; 0.156.1)", received.header.Get("User-Agent"))
					assert.Empty(t, received.header.Get("Accept-Encoding"))
					metadata := gjson.Parse(received.header.Get("X-Codex-Turn-Metadata"))
					assert.Equal(t, 21, len(metadata.Map()), "nested metadata field set from the JSON sample")
					assert.Equal(t, "/root", metadata.Get("agent_name").String())
					assert.Equal(t, "0", metadata.Get("window_number").Raw)
					assert.Equal(t, "user", metadata.Get("thread_source").String())
					assert.Equal(t, "exec", metadata.Get("turn_trigger").String())
					assert.Equal(t, "windows_elevated", metadata.Get("sandbox").String())
					assert.Equal(t, "workspace-write", metadata.Get("sandbox_mode").String())
					for key, value := range map[string]string{
						"auto_review_enabled": "false", "node_repl_auto_review_required": "true",
						"node_repl_disabled": "false", "analytics_enabled": "true",
					} {
						assert.Equal(t, value, metadata.Get(key).Raw)
					}
					assert.Equal(t, info.StartTime.UnixMilli(), metadata.Get("turn_started_at_unix_ms").Int())
					assert.NotEmpty(t, metadata.Get("context_window_id").String())
					assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, metadata.Get("installation_id").String())
					for _, name := range []string{"session_id", "thread_id", "turn_id", "root_turn_id", "context_window_id"} {
						assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, metadata.Get(name).String(), name)
					}
					assert.Equal(t, metadata.Get("thread_id").String()+":0", metadata.Get("window_id").String())
					assert.Len(t, received.header.Get("X-Codex-Window-Id"), 38)
					assert.Equal(t, metadata.Get("window_id").String(), received.header.Get("X-Codex-Window-Id"))
					assert.Regexp(t, `^at_[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, gjson.GetBytes(received.body, "input.0.id").String())
					assert.Regexp(t, `^msg_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, gjson.GetBytes(received.body, "input.2.id").String())
					for key := range received.header {
						assert.False(t, strings.HasPrefix(strings.ToLower(key), "x-stainless-"), key)
					}
				}
				if preset.id == "" {
					baseline = received
				} else {
					assert.Equal(t, baseline.body, received.body, "TLS selection must not change prompt/tools/identity/cache keys")
					if family == "claude" {
						received.header.Set("X-Stainless-Runtime-Version", "v26.3.0")
					}
					assert.Equal(t, baseline.header, received.header, "only existing runtime declarations may change")
				}
			}
		})
	}

	// A stale, even invalid, TLS selection must not enter the custom transport
	// when synthesis is off; request bytes retain the ordinary relay path.
	for _, profile := range []string{"", "off"} {
		c, info := cliTestRequest(profile)
		info.ChannelSetting.TLSFingerprint = "stale-unknown-preset"
		info.ChannelSetting.ClientDeviceSeed = "stale-device"
		info.ChannelSetting.ClientSessionSeed = "stale-session"
		input := "{ \"input\": \"unchanged\" }\n"
		resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(input))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		received := <-got
		assert.Equal(t, input, string(received.body))
		assert.Equal(t, "Go-http-client/1.1", received.header.Get("User-Agent"))
		assert.Empty(t, received.header.Get("Session-Id"))
		assert.Empty(t, received.header.Get("X-Stainless-Runtime-Version"))
	}
}

func TestCLIRuntimeHeadersRespectAdminOverrides(t *testing.T) {
	monitor := *operation_setting.GetMonitorSetting()
	monitor.ChannelTestClientHeaders = map[string]map[string]string{
		"claude": {"x-stainless-runtime-version": "admin-runtime"},
	}
	t.Cleanup(operation_setting.SetMonitorSettingForTest(monitor))
	for _, channelTest := range []bool{false, true} {
		c, info := cliTestRequest("claude")
		info.IsChannelTest = channelTest
		info.ChannelSetting.TLSFingerprint = "claude-node-22.14.0"
		headers, err := processHeaderOverride(info, c)
		require.NoError(t, err)
		assert.Equal(t, "admin-runtime", headers["x-stainless-runtime-version"])
		info.HeadersOverride = map[string]any{"x-stainless-runtime-version": "channel-runtime"}
		headers, err = processHeaderOverride(info, c)
		require.NoError(t, err)
		assert.Equal(t, "channel-runtime", headers["x-stainless-runtime-version"])
	}
}
