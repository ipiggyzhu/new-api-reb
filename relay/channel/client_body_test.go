package channel

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func cliTestRequest(profile string) (*gin.Context, *relaycommon.RelayInfo) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("session-id", "caller-session")
	return c, &relaycommon.RelayInfo{
		UserId: 42, TokenId: 7, RequestId: "request-one", IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: 9, ApiType: constant.APITypeOpenAI,
			ChannelSetting: dto.ChannelSettings{SyntheticClientHeadersProfile: profile},
		},
	}
}

func TestCLIProfileOffAndUnrelatedEndpointsPreserveBytes(t *testing.T) {
	for _, tc := range []struct{ profile, path string }{
		{"", "/v1/messages"}, {"off", "/v1/responses"}, {"openai", "/v1/responses"},
		{"gemini", "/v1/messages"}, {"generic", "/v1/messages"},
		{"codex", "/v1/responses/compact"}, {"codex", "/v1/images/generations"},
		{"claude", "/v1/chat/completions"}, {"codex", "/v1/messages"},
	} {
		t.Run(tc.profile+tc.path, func(t *testing.T) {
			c, info := cliTestRequest(tc.profile)
			reader := strings.NewReader("unchanged non-JSON body\n")
			url, got, headers, err := prepareCLIRequest(c, info, "https://upstream.test"+tc.path, reader)
			require.NoError(t, err)
			assert.Equal(t, "https://upstream.test"+tc.path, url)
			assert.Same(t, reader, got)
			assert.Empty(t, headers)
			assert.Zero(t, info.UpstreamRequestBodySize)
		})
	}
}

func TestClaudeCLIShapePreservesContentToolsAndExplicitParameters(t *testing.T) {
	c, info := cliTestRequest("claude")
	info.IsStream = false
	body := `{"model":"claude-opus-5-5","system":"tenant system","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}},{"name":"Bash","input_schema":{"type":"object","properties":{},"additionalProperties":true}},{"name":"tenant_tool","input_schema":{"type":"object"}}],"max_tokens":16,"temperature":0,"thinking":{"type":"disabled"},"metadata":{"user_id":"original-user","custom":"keep"}}`
	url, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test/v1/messages?existing=1", strings.NewReader(body))
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Contains(t, url, "beta=true")
	assert.Contains(t, url, "existing=1")
	assert.Equal(t, "tenant system", gjson.GetBytes(data, "system.0.text").String())
	assert.Equal(t, "ephemeral", gjson.GetBytes(data, "system.0.cache_control.type").String())
	assert.Equal(t, "hello", gjson.GetBytes(data, "messages.0.content.0.text").String())
	assert.Equal(t, "ephemeral", gjson.GetBytes(data, "messages.0.content.0.cache_control.type").String())
	assert.Equal(t, "Bash", gjson.GetBytes(data, "tools.0.name").String())
	assert.Equal(t, "Read", gjson.GetBytes(data, "tools.1.name").String())
	assert.Equal(t, "tenant_tool", gjson.GetBytes(data, "tools.2.name").String())
	assert.Equal(t, "9007199254740993", gjson.GetBytes(data, "tools.1.input_schema.properties.n.const").Raw)
	assert.True(t, gjson.GetBytes(data, "tools.0.input_schema.additionalProperties").Bool())
	assert.Equal(t, "false", gjson.GetBytes(data, "tools.1.input_schema.additionalProperties").Raw)
	assert.Equal(t, "https://json-schema.org/draft/2020-12/schema", gjson.GetBytes(data, "tools.1.input_schema.$schema").String())
	assert.Equal(t, "16", gjson.GetBytes(data, "max_tokens").Raw)
	assert.Equal(t, "0", gjson.GetBytes(data, "temperature").Raw)
	assert.Equal(t, "false", gjson.GetBytes(data, "stream").Raw)
	assert.Equal(t, AcceptJSON, headers.Get("Accept"))
	assert.Contains(t, headers.Get("anthropic-beta"), "claude-code-20250219")
	assert.False(t, gjson.GetBytes(data, "context_management").Exists())
	assert.Equal(t, "keep", gjson.GetBytes(data, "metadata.custom").String())
	identity := gjson.Parse(gjson.GetBytes(data, "metadata.user_id").String())
	assert.Equal(t, headers.Get("X-Claude-Code-Session-Id"), identity.Get("session_id").String())
	assert.NotEqual(t, "caller-session", identity.Get("session_id").String())
	assert.Len(t, identity.Get("device_id").String(), 64)
	assert.Empty(t, identity.Get("account_uuid").String())
	assert.Equal(t, int64(len(data)), info.UpstreamRequestBodySize)
	_, again, sameHeaders, err := prepareCLIRequest(c, info, url, bytes.NewReader(data))
	require.NoError(t, err)
	second, err := io.ReadAll(again)
	require.NoError(t, err)
	assert.JSONEq(t, string(data), string(second), "retries must not duplicate blocks or regenerate identities")
	assert.Equal(t, headers, sameHeaders)
}

func TestClaudeCLIHonorsCacheBreakpointsAndModelCapabilities(t *testing.T) {
	c, info := cliTestRequest("claude")
	_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/messages", strings.NewReader(`{"model":"claude-3-haiku","system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"aW1hZ2U="}},{"type":"text","text":"hi"}]}],"tools":[{"name":"Read","cache_control":{"type":"ephemeral"},"input_schema":{"type":"object"}},{"name":"Bash","input_schema":{"type":"object"}}]}`))
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "1h", gjson.GetBytes(data, "system.0.cache_control.ttl").String())
	assert.Equal(t, "Read", gjson.GetBytes(data, "tools.0.name").String(), "explicit cache boundary must not move")
	assert.False(t, gjson.GetBytes(data, "messages.0.content.1.cache_control").Exists())
	assert.Equal(t, "aW1hZ2U=", gjson.GetBytes(data, "messages.0.content.0.source.data").String())
	assert.False(t, gjson.GetBytes(data, "thinking").Exists())
}

func TestCodexCLIShapeAndIsolatedMetadata(t *testing.T) {
	c, info := cliTestRequest("codex")
	body := `{"model":"gpt-6-astra","instructions":"system instruction","input":[{"role":"user","content":"hello"},{"type":"function_call_output","call_id":"call_original","output":"result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}}}},{"type":"namespace","name":"custom","tools":[{"type":"function","name":"run","strict":true,"parameters":{"type":"object"}},{"type":"custom","name":"patch","format":{"type":"grammar","definition":"start: /.+/","syntax":"lark"}}]}],"max_output_tokens":123,"parallel_tool_calls":true,"reasoning":{"effort":"low"},"store":true,"service_tier":"default","text":{"format":{"type":"json_object"}},"client_metadata":{"session_id":"untrusted-id","secret":"do-not-forward"}}`
	_, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "additional_tools", gjson.GetBytes(data, "input.0.type").String())
	assert.Equal(t, "lookup", gjson.GetBytes(data, "input.0.tools.0.name").String())
	assert.Equal(t, "false", gjson.GetBytes(data, "input.0.tools.0.strict").Raw)
	assert.Equal(t, "9007199254740993", gjson.GetBytes(data, "input.0.tools.0.parameters.properties.n.const").Raw)
	assert.True(t, gjson.GetBytes(data, "input.0.tools.1.tools.0.strict").Bool())
	assert.Equal(t, "lark", gjson.GetBytes(data, "input.0.tools.1.tools.1.format.syntax").String())
	assert.False(t, gjson.GetBytes(data, "tools").Exists())
	assert.Equal(t, "developer", gjson.GetBytes(data, "input.1.role").String())
	assert.Equal(t, "system instruction", gjson.GetBytes(data, "input.1.content.0.text").String())
	assert.Equal(t, "input_text", gjson.GetBytes(data, "input.2.content.0.type").String())
	assert.Equal(t, "call_original", gjson.GetBytes(data, "input.3.call_id").String())
	assert.Equal(t, "result", gjson.GetBytes(data, "input.3.output").String())
	assert.Equal(t, "123", gjson.GetBytes(data, "max_output_tokens").Raw)
	assert.True(t, gjson.GetBytes(data, "parallel_tool_calls").Bool())
	assert.True(t, gjson.GetBytes(data, "store").Bool())
	assert.Equal(t, "low", gjson.GetBytes(data, "reasoning.effort").String())
	assert.Equal(t, "default", gjson.GetBytes(data, "service_tier").String())
	assert.Equal(t, "json_object", gjson.GetBytes(data, "text.format.type").String())
	assert.False(t, gjson.GetBytes(data, "client_metadata.secret").Exists())
	assert.Equal(t, headers.Get("session-id"), gjson.GetBytes(data, "client_metadata.session_id").String())
	assert.Equal(t, headers.Get("thread-id"), gjson.GetBytes(data, "prompt_cache_key").String())
	assert.Equal(t, headers.Get("x-codex-turn-metadata"), gjson.GetBytes(data, "client_metadata.x-codex-turn-metadata").String())
	assert.Equal(t, "true", headers.Get("x-openai-internal-codex-responses-lite"))
	_, again, sameHeaders, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", bytes.NewReader(data))
	require.NoError(t, err)
	second, err := io.ReadAll(again)
	require.NoError(t, err)
	assert.JSONEq(t, string(data), string(second))
	assert.Equal(t, headers, sameHeaders)
}

func TestCLIIdentityStaysWithinTenantAndSession(t *testing.T) {
	c, info := cliTestRequest("codex")
	_, _, first, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(`{"input":"hello"}`))
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		user     int
		session  string
		wantSame bool
	}{
		{"same conversation next turn", 42, "caller-session", true},
		{"different user same incoming id", 99, "caller-session", false},
		{"different conversation", 42, "another-session", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := cliTestRequest("codex")
			info.UserId, info.RequestId = tc.user, "request-two"
			c.Request.Header.Set("session-id", tc.session)
			_, _, got, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(`{"input":"hello"}`))
			require.NoError(t, err)
			assert.Equal(t, tc.wantSame, first.Get("session-id") == got.Get("session-id"))
			assert.NotEqual(t, first.Get("x-client-request-id"), got.Get("x-client-request-id"))
		})
	}
}

func TestCustomCLIIdentityRotationIsStableAndIndependent(t *testing.T) {
	originalSecret := appcommon.CryptoSecret
	t.Cleanup(func() { appcommon.CryptoSecret = originalSecret })
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			settings := dto.ChannelSettings{
				SyntheticClientHeadersProfile: family,
				ClientDeviceSeed:              "device-profile-one", ClientSessionSeed: "session-profile-one",
			}
			// Exercise the persisted ChannelSettings representation, not just a
			// form-only value which could disappear while saving a channel.
			serialized, err := appcommon.Marshal(settings)
			require.NoError(t, err)
			var stored dto.ChannelSettings
			require.NoError(t, appcommon.Unmarshal(serialized, &stored))
			stored.Normalize(constant.APITypeOpenAI)
			capture := func(setting dto.ChannelSettings, user, channel int) (string, string) {
				c, info := cliTestRequest(family)
				info.ChannelSetting, info.UserId, info.ChannelId = setting, user, channel
				path, body := "/v1/responses", `{"input":"hello"}`
				if family == "claude" {
					path, body = "/v1/messages", `{"messages":[{"role":"user","content":"hello"}]}`
				}
				_, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test"+path, strings.NewReader(body))
				require.NoError(t, err)
				data, err := io.ReadAll(reader)
				require.NoError(t, err)
				if family == "claude" {
					identity := gjson.Parse(gjson.GetBytes(data, "metadata.user_id").String())
					assert.Equal(t, headers.Get("X-Claude-Code-Session-Id"), identity.Get("session_id").String())
					return identity.Get("device_id").String(), identity.Get("session_id").String()
				}
				assert.Equal(t, headers.Get("session-id"), gjson.GetBytes(data, "client_metadata.session_id").String())
				return gjson.GetBytes(data, "client_metadata.x-codex-installation-id").String(), headers.Get("session-id")
			}
			device, session := capture(stored, 42, 9)
			againDevice, againSession := capture(stored, 42, 9)
			assert.Equal(t, device, againDevice)
			assert.Equal(t, session, againSession)

			appcommon.CryptoSecret = "different-process-secret"
			restartedDevice, restartedSession := capture(stored, 42, 9)
			appcommon.CryptoSecret = originalSecret
			assert.Equal(t, device, restartedDevice, "saved custom device survives a restart")
			assert.Equal(t, session, restartedSession, "saved custom session survives a restart")

			rotated := stored
			rotated.ClientDeviceSeed = "device-profile-two"
			newDevice, sameSession := capture(rotated, 42, 9)
			assert.NotEqual(t, device, newDevice)
			assert.Equal(t, session, sameSession)
			rotated = stored
			rotated.ClientSessionSeed = "session-profile-two"
			sameDevice, newSession := capture(rotated, 42, 9)
			assert.Equal(t, device, sameDevice)
			assert.NotEqual(t, session, newSession)
			for _, identity := range [][2]int{{99, 9}, {42, 10}} {
				otherDevice, otherSession := capture(stored, identity[0], identity[1])
				assert.NotEqual(t, device, otherDevice)
				assert.NotEqual(t, session, otherSession)
			}

			defaults := dto.ChannelSettings{SyntheticClientHeadersProfile: family}
			defaultDevice, defaultSession := capture(defaults, 42, 9)
			stored.ClientDeviceSeed, stored.ClientSessionSeed = "", ""
			resetDevice, resetSession := capture(stored, 42, 9)
			assert.Equal(t, defaultDevice, resetDevice)
			assert.Equal(t, defaultSession, resetSession)
		})
	}
}

func TestCLIRejectsMalformedBodyBeforeSending(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{`, `{"input":42}`, `{"input":[null]}`, `{"input":[{"role":"user","content":[null]}]}`, `{"input":"hello","tools":[null]}`, `{"input":"hello","tools":"invalid"}`} {
		c, info := cliTestRequest("codex")
		_, _, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
		require.Error(t, err)
	}
}

// Embed the interface so only the two hooks used by the real transport need
// test implementations. All request building/sending stays production code.
type cliTransportAdaptor struct {
	Adaptor
	url string
}

func (a cliTransportAdaptor) GetRequestURL(*relaycommon.RelayInfo) (string, error) { return a.url, nil }
func (a cliTransportAdaptor) SetupRequestHeader(_ *gin.Context, header *http.Header, _ *relaycommon.RelayInfo) error {
	header.Set("Authorization", "Bearer channel-test-key")
	header.Set("anthropic-beta", "caller-beta-must-not-leak")
	return nil
}

func TestCLIHTTPTransportShapesPassthroughAndHonorsHeaderOverrides(t *testing.T) {
	service.InitHttpClient()
	for _, channelTest := range []bool{false, true} {
		t.Run("channelTest="+strconv.FormatBool(channelTest), func(t *testing.T) {
			type capture struct {
				body   []byte
				header http.Header
				query  string
				length int64
				err    error
			}
			got := make(chan capture, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				got <- capture{data, r.Header.Clone(), r.URL.RawQuery, r.ContentLength, err}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c, info := cliTestRequest("claude")
			info.IsStream, info.IsChannelTest = false, channelTest
			info.ChannelSetting.PassThroughBodyEnabled = true
			info.UpstreamRequestBodySize = 1 // stale size from the conversion pipeline
			info.HeadersOverride = map[string]any{"anthropic-beta": "admin-beta"}
			resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(`{"model":"claude-opus-5-5","system":"instructions","messages":[{"role":"user","content":"hi"}]}`))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			received := <-got
			require.NoError(t, received.err)
			assert.Equal(t, int64(len(received.body)), received.length)
			assert.Equal(t, "admin-beta", received.header.Get("anthropic-beta"))
			assert.Equal(t, "Bearer channel-test-key", received.header.Get("Authorization"))
			assert.Equal(t, "beta=true", received.query)
			assert.Equal(t, "instructions", gjson.GetBytes(received.body, "system.0.text").String())
			assert.Equal(t, "128000", gjson.GetBytes(received.body, "max_tokens").Raw)
		})
	}
}

func TestCLIWebsocketUsesSameBodyAndIdentityAsHTTPFallback(t *testing.T) {
	service.InitHttpClient()
	for _, upgrade := range []bool{true, false} {
		t.Run("upgrade="+strconv.FormatBool(upgrade), func(t *testing.T) {
			type capture struct {
				body   []byte
				header http.Header
				err    error
			}
			got := make(chan capture, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					if !upgrade {
						got <- capture{header: r.Header.Clone()}
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						got <- capture{err: err}
						return
					}
					defer conn.Close()
					_, body, err := conn.ReadMessage()
					got <- capture{body, r.Header.Clone(), err}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"response-test","output":[]}}`))
					return
				}
				body, err := io.ReadAll(r.Body)
				got <- capture{body, r.Header.Clone(), err}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c, info := cliTestRequest("codex")
			info.IsStream = false
			a := cliTransportAdaptor{url: server.URL + "/v1/responses"}
			attempt, err := TryResponsesWebsocket(a, c, info, strings.NewReader(`{"input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
			require.NoError(t, err)
			received := <-got
			require.NoError(t, received.err)
			if !upgrade {
				require.Nil(t, attempt.Response)
				fallback, err := DoApiRequest(a, c, info, attempt.FallbackBody)
				require.NoError(t, err)
				require.NoError(t, fallback.Body.Close())
				httpReceived := <-got
				require.NoError(t, httpReceived.err)
				assert.Equal(t, received.header.Get("session-id"), httpReceived.header.Get("session-id"))
				received = httpReceived
			} else {
				require.NotNil(t, attempt.Response)
				require.NoError(t, attempt.Response.Body.Close())
				assert.Equal(t, "response.create", gjson.GetBytes(received.body, "type").String())
			}
			assert.Equal(t, "additional_tools", gjson.GetBytes(received.body, "input.0.type").String())
			assert.Equal(t, "lookup", gjson.GetBytes(received.body, "input.0.tools.0.name").String())
			assert.Equal(t, received.header.Get("session-id"), gjson.GetBytes(received.body, "client_metadata.session_id").String())
			assert.Equal(t, "Bearer channel-test-key", received.header.Get("Authorization"))
		})
	}
}
