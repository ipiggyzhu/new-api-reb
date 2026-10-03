package channel

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesForceHighEffortPreservesHistoryAndIdentity(t *testing.T) {
	const original = `{"model":"gpt-6-astra","reasoning":{"effort":"xhigh","context":"all_turns","summary":"auto"},"stream":false,"store":false,"service_tier":"priority","prompt_cache_key":"original-thread","client_metadata":{"session_id":"original-thread","x-codex-turn-metadata":"{\"reasoning_effort\":\"ultra\",\"turn_id\":\"turn-one\",\"n\":9007199254740993}"},"input":[{"role":"developer","content":"Keep these instructions."},{"role":"user","content":"Continue the plan."},{"type":"reasoning","id":"rs_old","encrypted_content":"old","summary":[{"type":"summary_text","text":"Existing plan"}]},{"type":"function_call","call_id":"call-one","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call-one","output":"Existing result"}],"tools":[{"type":"function","name":"read","parameters":{"const":9007199254740993}}]}`
	_, info := cliTestRequest("")
	info.ChannelSetting.ResponsesForceHighEffort = true
	headers := http.Header{}
	headers.Set("x-codex-turn-metadata", `{"reasoning_effort":"max","turn_id":"header-turn","n":9007199254740993}`)
	headers.Set("session-id", "original-thread")
	got, err := forceResponsesHighEffort(info, "https://upstream.test/v1/responses", strings.NewReader(original), headers)
	require.NoError(t, err)
	assert.Equal(t, "high", gjson.GetBytes(got, "reasoning.effort").String())
	assert.Equal(t, "high", gjson.Get(gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String(), "reasoning_effort").String())
	assert.JSONEq(t, `{"reasoning_effort":"high","turn_id":"header-turn","n":9007199254740993}`, headers.Get("x-codex-turn-metadata"))
	assert.Contains(t, headers.Get("x-codex-turn-metadata"), "9007199254740993")
	assert.Equal(t, "original-thread", headers.Get("session-id"))
	for _, path := range []string{"input", "tools", "reasoning.context", "reasoning.summary", "stream", "store", "service_tier", "prompt_cache_key", "client_metadata.session_id"} {
		assert.Equal(t, gjson.Get(original, path).Raw, gjson.GetBytes(got, path).Raw, path)
	}
	assert.Contains(t, string(got), "9007199254740993")
	assert.Equal(t, "high", info.ReasoningEffort)
	assert.Equal(t, int64(len(got)), info.UpstreamRequestBodySize)
	again, err := forceResponsesHighEffort(info, "https://upstream.test/v1/responses", bytes.NewReader(got), headers)
	require.NoError(t, err)
	assert.Equal(t, got, again, "HTTP fallback/retry is idempotent")
}

func TestResponsesForceHighEffortOptInAndEndpointScope(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		enabled          bool
		format           types.RelayFormat
		want             string
	}{
		{name: "default off", path: "/v1/responses", body: `{"reasoning":{"effort":"max"}}`},
		{name: "compact excluded", path: "/v1/responses/compact", enabled: true, format: types.RelayFormatOpenAIResponses, body: `{"input":[]}`},
		{name: "chat unaffected", path: "/v1/chat/completions", enabled: true, body: `{"reasoning_effort":"low"}`},
		{name: "absent effort", path: "/v1/responses", enabled: true, body: `{"input":[]}`, want: `{"input":[],"reasoning":{"effort":"high"}}`},
		{name: "null reasoning", path: "/v1/responses", enabled: true, body: `{"reasoning":null}`, want: `{"reasoning":{"effort":"high"}}`},
		{name: "custom Responses route", path: "/custom", enabled: true, format: types.RelayFormatOpenAIResponses, body: `{"reasoning":{"effort":"low"}}`, want: `{"reasoning":{"effort":"high"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, info := cliTestRequest("")
			info.ChannelSetting.ResponsesForceHighEffort = tc.enabled
			info.RelayFormat = tc.format
			reader := strings.NewReader(tc.body)
			got, err := forceResponsesHighEffort(info, "https://upstream.test"+tc.path, reader, make(http.Header))
			require.NoError(t, err)
			if tc.want == "" {
				assert.Nil(t, got)
				remaining, err := io.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, tc.body, string(remaining))
			} else {
				assert.JSONEq(t, tc.want, string(got))
			}
		})
	}
}

func TestResponsesForceHighEffortAlignsConfigurationUpdates(t *testing.T) {
	const original = `{"reasoning":{"effort":"low"},"input":[{"role":"user","content":"Keep history"},{"type":"configuration_update","reasoning":{"effort":"max"}},{"role":"user","content":"Continue"},{"type":"configuration_update","reasoning":{"effort":"xhigh"}},{"type":"function_call_output","call_id":"keep","output":"9007199254740993"}]}`
	_, info := cliTestRequest("")
	info.ChannelSetting.ResponsesForceHighEffort = true
	got, err := forceResponsesHighEffort(info, "https://upstream.test/v1/responses", strings.NewReader(original), make(http.Header))
	require.NoError(t, err)
	assert.Equal(t, "high", gjson.GetBytes(got, "reasoning.effort").String())
	for _, path := range []string{"input.1.reasoning.effort", "input.3.reasoning.effort"} {
		assert.Equal(t, "high", gjson.GetBytes(got, path).String())
	}
	for _, path := range []string{"input.0", "input.2", "input.4"} {
		assert.Equal(t, gjson.Get(original, path).Raw, gjson.GetBytes(got, path).Raw)
	}
	info.ChannelSetting.ResponsesForceHighEffort = false
	reader := strings.NewReader(original)
	got, err = forceResponsesHighEffort(info, "https://upstream.test/v1/responses", reader, make(http.Header))
	require.NoError(t, err)
	assert.Nil(t, got)
	remaining, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, original, string(remaining))
}

func TestResponsesForceHighEffortRejectsMalformedMetadata(t *testing.T) {
	for _, body := range []string{`not json`, `[]`, `{"reasoning":"max"}`, `{"client_metadata":{"x-codex-turn-metadata":{}}}`, `{"client_metadata":{"x-codex-turn-metadata":"not json"}}`} {
		_, info := cliTestRequest("")
		info.ChannelSetting.ResponsesForceHighEffort = true
		_, err := forceResponsesHighEffort(info, "https://upstream.test/v1/responses", strings.NewReader(body), make(http.Header))
		require.Error(t, err)
	}
	_, info := cliTestRequest("")
	info.ChannelSetting.ResponsesForceHighEffort = true
	header := make(http.Header)
	header.Set("x-codex-turn-metadata", "broken")
	_, err := forceResponsesHighEffort(info, "https://upstream.test/v1/responses", strings.NewReader(`{}`), header)
	require.Error(t, err)
	assert.Empty(t, info.ReasoningEffort)
}

func TestResponsesForceHighEffortHTTPAndWebsocket(t *testing.T) {
	service.InitHttpClient()
	for _, tc := range []struct{ name, profile, transport string }{
		{"HTTP passthrough", "", "http"}, {"HTTP Codex profile", "codex", "http"},
		{"WebSocket", "", "ws"}, {"WebSocket HTTP fallback", "codex", "fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type capture struct {
				body    []byte
				headers http.Header
				length  int64
				err     error
			}
			captures := make(chan capture, 2)
			const reply = `{"type":"response.completed","response":{"id":"test-response","reasoning":{"effort":"low"},"output":[]}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					if tc.transport == "fallback" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						captures <- capture{err: err}
						return
					}
					defer conn.Close()
					_, body, err := conn.ReadMessage()
					captures <- capture{body: body, headers: r.Header.Clone(), err: err}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(reply))
					return
				}
				body, err := io.ReadAll(r.Body)
				captures <- capture{body, r.Header.Clone(), r.ContentLength, err}
				_, _ = w.Write([]byte(reply))
			}))
			defer server.Close()
			c, info := cliTestRequest(tc.profile)
			info.IsStream = false
			info.ChannelSetting.PassThroughBodyEnabled = true
			info.ChannelSetting.ResponsesForceHighEffort = true
			info.HeadersOverride = map[string]any{"x-codex-turn-metadata": `{"reasoning_effort":"max","turn_id":"override-turn"}`}
			const body = `{"model":"gpt-6-astra","reasoning":{"effort":"low"},"client_metadata":{"x-codex-turn-metadata":"{\"reasoning_effort\":\"ultra\"}"},"input":[{"role":"user","content":"Continue"}]}`
			a := cliTransportAdaptor{url: server.URL + "/v1/responses"}
			var resp *http.Response
			if tc.transport == "http" {
				var err error
				resp, err = DoApiRequest(a, c, info, strings.NewReader(body))
				require.NoError(t, err)
			} else {
				attempt, err := TryResponsesWebsocket(a, c, info, strings.NewReader(body))
				require.NoError(t, err)
				resp = attempt.Response
				if resp == nil {
					resp, err = DoApiRequest(a, c, info, attempt.FallbackBody)
					require.NoError(t, err)
				}
			}
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Contains(t, string(data), `"effort":"low"`, "never falsify upstream effort")
			got := <-captures
			require.NoError(t, got.err)
			assert.Equal(t, "high", gjson.GetBytes(got.body, "reasoning.effort").String())
			assert.Equal(t, "high", gjson.Get(gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String(), "reasoning_effort").String())
			assert.JSONEq(t, `{"reasoning_effort":"high","turn_id":"override-turn"}`, got.headers.Get("x-codex-turn-metadata"))
			assert.Equal(t, "Bearer channel-test-key", got.headers.Get("Authorization"))
			if tc.transport != "ws" {
				assert.Equal(t, int64(len(got.body)), got.length)
			}
		})
	}
}
