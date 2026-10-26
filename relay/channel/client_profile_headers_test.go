package channel

import (
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// These requests use channel-level versions and the real default affinity
// templates. The captured TLS upstream observes the final headers after every
// override layer, including derived session headers that are absent in a capture.
func TestChannelVersionKeepsProfileHeadersAfterOverrides(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	for _, family := range []string{"codex", "claude"} {
		for _, source := range []string{"affinity", "static", "set_header", "global"} {
			t.Run(family+"/"+source, func(t *testing.T) {
				profileID, path, body := "codex-0.163.0-linux-x64", "/v1/responses", `{"model":"gpt-5","input":"hi"}`
				caller := map[string]string{
					"User-Agent": "codex_cli_rs/0.150.0 (Windows; x86_64)", "Originator": "caller-originator",
					"Session_id": "caller-session", "Thread-Id": "caller-thread", "X-Client-Request-Id": "caller-request",
					"X-Codex-Beta-Features": "caller-beta", "X-Codex-Window-Id": "caller-window",
					"X-Codex-Turn-Metadata": "caller-metadata", "X-OpenAI-Subagent": "review",
				}
				if family == "claude" {
					profileID, path, body = "claude-2.1.306-linux-x64", "/v1/messages", `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`
					caller = map[string]string{
						"User-Agent": "claude-cli/2.0.0 (external, sdk-cli)", "Anthropic-Beta": "caller-beta",
						"X-Stainless-Package-Version": "caller-package", "X-Stainless-Runtime-Version": "caller-runtime",
						"X-Claude-Code-Session-Id": "caller-session", "X-Stainless-Extra": "caller-extra",
					}
				}
				c, info := cliTestRequest(family)
				info.ChannelSetting.CLIVersionProfile = profileID
				require.False(t, info.ChannelSetting.KeyEnvironments)
				info.RequestHeaders = caller
				for name, value := range caller {
					c.Request.Header.Set(name, value)
				}
				info.HeadersOverride = map[string]any{"x-tenant": "tenant-a"}
				switch source {
				case "affinity":
					for _, rule := range operation_setting.GetChannelAffinitySetting().Rules {
						if rule.Name == family+" cli trace" {
							info.ParamOverride = rule.ParamOverrideTemplate
						}
					}
					require.NotNil(t, info.ParamOverride)
					_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(body), info)
					require.NoError(t, err)
					require.Equal(t, caller["User-Agent"], info.RuntimeHeadersOverride["user-agent"])
				case "static":
					for name, value := range caller {
						info.HeadersOverride[name] = value
					}
				case "set_header":
					operations := make([]map[string]any, 0, len(caller))
					for name, value := range caller {
						operations = append(operations, map[string]any{"mode": "set_header", "path": name, "value": value})
					}
					info.ParamOverride = map[string]any{"operations": operations}
					_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(body), info)
					require.NoError(t, err)
				case "global":
					monitor := *operation_setting.GetMonitorSetting()
					monitor.ChannelTestClientHeaders = map[string]map[string]string{"*": {"user-agent": "blanket-ua"}, family: caller}
					t.Cleanup(operation_setting.SetMonitorSettingForTest(monitor))
				}
				resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + path}, c, info, strings.NewReader(body))
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				got := <-captures
				profile := service.LookupCLIVersionProfile(profileID)
				require.NotNil(t, profile)
				for name, want := range profile.Headers {
					if service.IsProtectedProfileHeader(family, name) && name != "accept" {
						assert.Equal(t, want, got.header.Get(name), name)
					}
				}
				assert.Equal(t, "tenant-a", got.header.Get("X-Tenant"), "non-profile static headers still apply")
				if family == "codex" {
					session := gjson.GetBytes(got.body, "client_metadata.session_id").String()
					require.NotEmpty(t, session)
					assert.Equal(t, session, got.header.Get("Session-Id"))
					assert.Equal(t, session, got.header.Get("Thread-Id"))
					assert.Equal(t, session+":0", got.header.Get("X-Codex-Window-Id"))
					assert.JSONEq(t, gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String(), got.header.Get("X-Codex-Turn-Metadata"))
					assert.Empty(t, got.header.Values("Session_id"))
					assert.Equal(t, "review", got.header.Get("X-OpenAI-Subagent"), "a non-profile caller header still applies")
				} else {
					session := gjson.Parse(gjson.GetBytes(got.body, "metadata.user_id").String()).Get("session_id").String()
					require.NotEmpty(t, session)
					assert.Equal(t, session, got.header.Get("X-Claude-Code-Session-Id"))
					assert.Empty(t, got.header.Get("X-Stainless-Extra"), "protected prefixes cannot gain uncaptured headers")
				}
			})
		}
	}
}
