package channel

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type keyHelloListener struct {
	net.Listener
	hellos chan []byte
}

func (l *keyHelloListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &keyHelloConn{Conn: conn, hellos: l.hellos}, nil
}

type keyHelloConn struct {
	net.Conn
	hellos chan []byte
	first  []byte
	done   bool
}

func (c *keyHelloConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.done {
		c.first = append(c.first, p[:n]...)
		if len(c.first) >= 5 {
			length := 5 + int(binary.BigEndian.Uint16(c.first[3:5]))
			if len(c.first) >= length {
				c.hellos <- bytes.Clone(c.first[:length])
				c.done = true
			}
		}
	}
	return n, err
}

// Ignore fresh random/key material and destination SNI, while comparing every
// other wire field, extension payload and extension position with the capture.
func keyHelloWireShape(t *testing.T, raw []byte) []string {
	t.Helper()
	take := func(n int) []byte {
		require.GreaterOrEqual(t, len(raw), n)
		part := raw[:n]
		raw = raw[n:]
		return part
	}
	shape := []string{hex.EncodeToString(take(3))}
	take(6)
	shape = append(shape, hex.EncodeToString(take(2)))
	take(32)
	sessionSize := int(take(1)[0])
	shape = append(shape, hex.EncodeToString([]byte{byte(sessionSize)}))
	take(sessionSize)
	cipherSize := int(binary.BigEndian.Uint16(take(2)))
	shape = append(shape, hex.EncodeToString(take(cipherSize)))
	compressionSize := int(take(1)[0])
	shape = append(shape, hex.EncodeToString(take(compressionSize)))
	extensionSize := int(binary.BigEndian.Uint16(take(2)))
	require.Len(t, raw, extensionSize)
	for len(raw) > 0 {
		id := take(2)
		size := int(binary.BigEndian.Uint16(take(2)))
		data := bytes.Clone(take(size))
		switch binary.BigEndian.Uint16(id) {
		case 0:
			data = nil
		case 51:
			for pos := 2; pos < len(data); {
				require.GreaterOrEqual(t, len(data)-pos, 4)
				keySize := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
				pos += 4
				require.GreaterOrEqual(t, len(data)-pos, keySize)
				clear(data[pos : pos+keySize])
				pos += keySize
			}
		}
		shape = append(shape, hex.EncodeToString(id)+":"+hex.EncodeToString(data))
	}
	return shape
}

func keyTransportTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, chan []byte) {
	t.Helper()
	previous := appcommon.TLSInsecureSkipVerify
	appcommon.TLSInsecureSkipVerify = true
	service.InitHttpClient()
	service.ResetProxyClientCache()
	t.Cleanup(func() { appcommon.TLSInsecureSkipVerify = previous; service.ResetProxyClientCache() })
	hellos := make(chan []byte, 8)
	server := httptest.NewUnstartedServer(handler)
	server.Listener = &keyHelloListener{Listener: server.Listener, hellos: hellos}
	server.StartTLS()
	// CLI captures include SNI; use a loopback hostname so the fixture does too.
	server.URL = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	t.Cleanup(server.Close)
	return server, hellos
}

func TestKeyEnvironmentTLSFollowsEachKeyOnHTTPAndWebSocket(t *testing.T) {
	for _, transport := range []string{"HTTP", "Responses WebSocket", "Realtime WebSocket"} {
		t.Run(transport, func(t *testing.T) {
			type capture struct {
				header http.Header
				body   []byte
				err    error
			}
			captures := make(chan capture, 2)
			server, hellos := keyTransportTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						captures <- capture{err: err}
						return
					}
					defer conn.Close()
					var body []byte
					if transport == "Responses WebSocket" {
						_, body, err = conn.ReadMessage()
					}
					captures <- capture{r.Header.Clone(), body, err}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"test","output":[]}}`))
					return
				}
				body, err := io.ReadAll(r.Body)
				captures <- capture{r.Header.Clone(), body, err}
				_, _ = io.WriteString(w, `{}`)
			}))
			profiles := []string{"codex-0.163.0-linux-x64", "codex-0.160.0-windows-x64"}
			var firstHello []string
			for index, profileID := range profiles {
				key := []string{"sk-a", "sk-b"}[index]
				c, info := keyEnvironmentRequest(key, index, 42)
				info.ChannelSetting.CLIVersionProfile = profileID
				info.ChannelSetting.TLSFingerprint = "claude-code" // must never override either key
				info.ChannelSetting.WebsocketTransport = true
				info.IsStream = false
				info.RelayMode = relayconstant.RelayModeResponses
				info.HeadersOverride = map[string]any{"Authorization": "Bearer " + key}
				c.Request.Header.Set("User-Agent", "codex_exec/0.150.0")
				service.SetKeyEnvironmentBindingState(c, &service.KeyEnvironmentBindingState{
					Key: "kenv:test", Bound: &service.KeyEnvironmentBinding{ChannelId: info.ChannelId, KeyTag: model.ChannelKeyTag(key)},
				})
				adaptor := cliTransportAdaptor{url: server.URL + "/v1/responses"}
				body := strings.NewReader(`{"model":"gpt-5","input":"hi"}`)
				switch transport {
				case "HTTP":
					resp, err := DoApiRequest(adaptor, c, info, body)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
				case "Responses WebSocket":
					attempt, err := TryResponsesWebsocket(adaptor, c, info, body)
					require.NoError(t, err)
					require.NotNil(t, attempt.Response)
					require.NoError(t, attempt.Response.Body.Close())
				case "Realtime WebSocket":
					info.RelayFormat = types.RelayFormatOpenAIRealtime
					info.RelayMode = relayconstant.RelayModeRealtime
					adaptor.url = strings.Replace(server.URL, "https://", "wss://", 1) + "/v1/realtime"
					conn, err := DoWssRequest(adaptor, c, info, nil)
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				}
				got := <-captures
				require.NoError(t, got.err)
				profile := service.LookupCLIVersionProfile(profileID)
				require.NotNil(t, profile)
				raw, err := hex.DecodeString(profile.ClientHelloHex)
				require.NoError(t, err)
				shape := keyHelloWireShape(t, <-hellos)
				assert.Equal(t, keyHelloWireShape(t, raw), shape, "the outgoing hello belongs to key %d", index)
				if index == 0 {
					firstHello = shape
				} else {
					assert.NotEqual(t, firstHello, shape, "the channel override cannot collapse two distinct captures")
				}
				assert.Equal(t, profile.Headers["user-agent"], got.header.Get("User-Agent"))
				assert.Equal(t, "Bearer "+key, got.header.Get("Authorization"))
				assert.NotEmpty(t, got.header.Get("session-id"))
				assert.NotEqual(t, "caller-session", got.header.Get("session-id"))
				if len(got.body) > 0 {
					assert.Equal(t, got.header.Get("session-id"), gjson.GetBytes(got.body, "client_metadata.session_id").String())
				}
				audit, _ := c.Get(string(constant.ContextKeyCLIProfileAudit))
				assert.Equal(t, map[string]any{"cli_profile": profileID, "key_env_bound": true,
					"cli_version_mismatch": map[string]string{"client": "0.150.0", "profile": profile.Version}}, audit)
				assert.Equal(t, transport != "HTTP", appcommon.GetContextKeyBool(c, constant.ContextKeyUpstreamWebsocketEstablished))
			}
		})
	}
}

func TestKeyEnvironmentWebSocketPreservesHandshakeRefusals(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 502, 503, 504, 524, 529} {
		for _, realtime := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/realtime=%t", status, realtime), func(t *testing.T) {
				server, _ := keyTransportTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"refused","type":"upstream_error"}}`)
				}))
				c, info := keyEnvironmentRequest("sk-a", 0, 42)
				info.ChannelSetting.WebsocketTransport = true
				var err error
				if realtime {
					info.RelayFormat = types.RelayFormatOpenAIRealtime
					_, err = DoWssRequest(cliTransportAdaptor{url: strings.Replace(server.URL, "https://", "wss://", 1)}, c, info, nil)
				} else {
					var attempt *WebsocketAttempt
					attempt, err = TryResponsesWebsocket(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(`{"input":"hi"}`))
					assert.Nil(t, attempt, "a refusal must not bypass the same-key retry matrix via HTTP")
				}
				require.Error(t, err)
				var apiErr *types.NewAPIError
				require.True(t, errors.As(err, &apiErr))
				assert.Equal(t, status, apiErr.StatusCode)
				if status == 429 || status == 503 || status == 529 {
					assert.Equal(t, 7*time.Second, apiErr.RetryAfter)
				}
				assert.False(t, appcommon.GetContextKeyBool(c, constant.ContextKeyUpstreamWebsocketEstablished))
			})
		}
	}
}

// A capability refusal may fall back without changing the selected key or body.
func TestKeyEnvironmentWebSocketCapabilityRefusalKeepsHTTPIdentity(t *testing.T) {
	headers := make(chan http.Header, 2)
	server, _ := keyTransportTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	c, info := keyEnvironmentRequest("sk-a", 0, 42)
	info.ChannelId = 0 // no persistent channel exists in this transport-only fixture
	info.ChannelSetting.WebsocketTransport = true
	adaptor := cliTransportAdaptor{url: server.URL + "/v1/responses"}
	attempt, err := TryResponsesWebsocket(adaptor, c, info, strings.NewReader(`{"input":"hi"}`))
	require.NoError(t, err)
	require.Nil(t, attempt.Response)
	require.NotNil(t, attempt.FallbackBody)
	resp, err := DoApiRequest(adaptor, c, info, attempt.FallbackBody)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	wsHeader, httpHeader := <-headers, <-headers
	assert.Equal(t, wsHeader.Get("session-id"), httpHeader.Get("session-id"))
	assert.Equal(t, wsHeader.Get("Authorization"), httpHeader.Get("Authorization"))
}

func TestKeyEnvironmentWebSocketUsesChannelProxyTunnel(t *testing.T) {
	server, hellos := keyTransportTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"proxied","output":[]}}`))
	}))
	targets := make(chan string, 1)
	connectProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		targets <- r.Host
		upstream, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, client)
			_ = upstream.Close()
			close(copied)
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-copied
	}))
	t.Cleanup(connectProxy.Close)
	c, info := keyEnvironmentRequest("sk-a", 0, 42)
	info.ChannelSetting.Proxy = connectProxy.URL
	info.ChannelSetting.WebsocketTransport = true
	info.IsStream = false
	attempt, err := TryResponsesWebsocket(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(`{"input":"hi"}`))
	require.NoError(t, err)
	require.NotNil(t, attempt.Response)
	body, err := io.ReadAll(attempt.Response.Body)
	require.NoError(t, err)
	require.NoError(t, attempt.Response.Body.Close())
	assert.Equal(t, "proxied", gjson.GetBytes(body, "id").String())
	require.Len(t, targets, 1)
	assert.Equal(t, strings.TrimPrefix(server.URL, "https://"), <-targets)
	raw, err := hex.DecodeString(service.LookupCLIVersionProfile(info.ChannelSetting.CLIVersionProfile).ClientHelloHex)
	require.NoError(t, err)
	assert.Equal(t, keyHelloWireShape(t, raw), keyHelloWireShape(t, <-hellos), "the tunnel carries the selected key's TLS, without a second proxy layer")
}
