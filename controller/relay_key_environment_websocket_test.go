package controller

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type independentWebsocketFixture struct {
	server *httptest.Server
	db     *gorm.DB
	model  string
	keys   [2]string
	done   chan map[string]any
}

// The provider is local TLS; selection, profile assignment, both relay paths,
// retrying and binding use the real channel/ability tables and binding store.
func newIndependentWebsocketFixture(t *testing.T, upstream http.Handler) *independentWebsocketFixture {
	t.Helper()
	_, db := setupCrossGroupBillingTest(t, false, 100_000, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	f := &independentWebsocketFixture{db: db, model: "gpt-task21-" + strings.ReplaceAll(t.Name(), "/", "-"), done: make(chan map[string]any, 8)}
	f.keys = [2]string{"sk-" + f.model + "-a", "sk-" + f.model + "-b"}
	previousTLS := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true
	service.ResetProxyClientCache()
	service.ResetKeyEnvironmentBindingCacheForTest()
	t.Cleanup(func() {
		common.TLSInsecureSkipVerify = previousTLS
		service.ResetProxyClientCache()
		service.ResetKeyEnvironmentBindingCacheForTest()
	})
	provider := httptest.NewTLSServer(upstream)
	t.Cleanup(provider.Close)
	common.RetryTimes = 3
	scores := operation_setting.GetChannelDynamicScoreSetting()
	scores.Enabled = false
	operation_setting.SetChannelDynamicScoreSettingForTest(scores)
	*operation_setting.GetChannelAffinitySetting() = operation_setting.ChannelAffinitySetting{MaxEntries: 100, KeyEnvironmentBindingTTLSeconds: 3600}
	prices := ratio_setting.GetModelPriceCopy()
	prices[f.model] = 0.00002
	priceJSON, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceJSON)))
	require.NoError(t, db.AutoMigrate(&model.ChannelKeyEnvironment{}))
	var channel model.Channel
	require.NoError(t, db.First(&channel, 81).Error)
	channel.Type, channel.Models, channel.BaseURL = constant.ChannelTypeOpenAI, f.model, &provider.URL
	channel.Key = strings.Join(f.keys[:], "\n")
	channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}
	channel.SetSetting(dto.ChannelSettings{
		KeyEnvironments: true, SyntheticClientHeadersProfile: "codex", WebsocketTransport: true,
		TLSFingerprint: "claude-code", ClientDeviceSeed: "task21-device", ClientSessionSeed: "task21-session",
	})
	require.NoError(t, db.Save(&channel).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 82).Update("status", common.ChannelStatusAutoDisabled).Error)
	require.NoError(t, db.Where("channel_id IN ?", []int{81, 82}).Delete(&model.Ability{}).Error)
	require.NoError(t, channel.AddAbilities(nil))
	for index, profile := range []string{"codex-0.163.0-linux-x64", "codex-0.160.0-windows-x64"} {
		require.NoError(t, db.Create(&model.ChannelKeyEnvironment{ChannelId: 81, KeyTag: model.ChannelKeyTag(f.keys[index]), ProfileId: &profile}).Error)
	}
	model.InitChannelCache()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		defer common.CleanupBodyStorage(c)
		common.SetContextKey(c, constant.ContextKeyUserId, 991)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, 992)
		common.SetContextKey(c, constant.ContextKeyTokenKey, "local-task-routing-token")
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: -1_000_000_000})
		c.Set("token_name", "task21-websocket-token")
		c.Set("token_quota", 100_000)
		c.Set(common.RequestIdKey, "task21-websocket-request")
		c.Next()
	})
	handler := func(c *gin.Context) {
		var format types.RelayFormat = types.RelayFormatOpenAIRealtime
		if c.Request.Method == http.MethodPost {
			format = types.RelayFormatOpenAIResponses
		}
		Relay(c, format)
		audit, _ := c.Get(string(constant.ContextKeyCLIProfileAudit))
		fields, _ := audit.(map[string]any)
		f.done <- fields
	}
	router.GET("/v1/realtime", middleware.Distribute(), handler)
	router.POST("/v1/responses", middleware.Distribute(), handler)
	f.server = httptest.NewServer(router)
	t.Cleanup(f.server.Close)
	return f
}

func (f *independentWebsocketFixture) dial(t *testing.T) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	return dialer.Dial(strings.Replace(f.server.URL, "http://", "ws://", 1)+"/v1/realtime?model="+url.QueryEscape(f.model),
		http.Header{"User-Agent": {"codex_exec/0.150.0"}, "Session-Id": {"task21-caller-session"}})
}

func (f *independentWebsocketFixture) finished(t *testing.T) map[string]any {
	t.Helper()
	select {
	case audit := <-f.done:
		return audit
	case <-time.After(5 * time.Second):
		t.Fatal("WebSocket relay did not finish")
		return nil
	}
}

func TestIndependentRealtimeEstablishmentRetryMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		recover  bool
		attempts int
	}{
		{"500 retries same key", 500, true, 2},
		{"502 retries same key", 502, true, 2},
		{"503 retries same key", 503, true, 2},
		{"only one retry", 503, false, 2},
		{"429 refuses without upgrading downstream", 429, false, 1},
		{"529 refuses without upgrading downstream", 529, false, 1},
		{"504 refuses without upgrading downstream", 504, false, 1},
		{"524 refuses without upgrading downstream", 524, false, 1},
		{"401 refuses without upgrading downstream", 401, false, 1},
		{"403 refuses without upgrading downstream", 403, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(chan http.Header, 4)
			var attempt atomic.Int32
			f := newIndependentWebsocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				currentAttempt := attempt.Add(1)
				headers <- r.Header.Clone()
				if !tc.recover || currentAttempt == 1 {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"message":"upstream handshake refused","type":"upstream_error"}}`)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","session":{"id":"local-session"}}`))
				_, _, _ = conn.ReadMessage()
			}))
			conn, resp, err := f.dial(t)
			if tc.recover {
				require.NoError(t, err)
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, frame, readErr := conn.ReadMessage()
				require.NoError(t, readErr)
				assert.Equal(t, "session.created", gjson.GetBytes(frame, "type").String())
				require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)))
				require.NoError(t, conn.Close())
			} else {
				require.Error(t, err)
				require.NotNil(t, resp)
				body, readErr := io.ReadAll(resp.Body)
				require.NoError(t, readErr)
				require.NoError(t, resp.Body.Close())
				assert.Equal(t, tc.status, resp.StatusCode, string(body))
				mediaType, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
				require.NoError(t, parseErr)
				assert.Equal(t, "application/json", mediaType)
				assert.Contains(t, string(body), "upstream handshake refused")
				if tc.status == 429 || tc.status == 529 {
					assert.Equal(t, "7", resp.Header.Get("Retry-After"))
				}
			}
			audit := f.finished(t)
			assert.Equal(t, true, audit["key_env_bound"])
			assert.NotEmpty(t, audit["cli_profile"])
			assert.NotNil(t, audit["cli_version_mismatch"])
			require.Len(t, headers, tc.attempts, "the establishment retry budget is exactly one")
			var first http.Header
			for index := 0; index < tc.attempts; index++ {
				got := <-headers
				if index == 0 {
					first = got
				} else {
					assert.Equal(t, first.Get("Authorization"), got.Get("Authorization"))
					assert.Equal(t, first.Get("session-id"), got.Get("session-id"))
					assert.Equal(t, first.Get("User-Agent"), got.Get("User-Agent"))
				}
			}
			assert.Empty(t, headers, "no third attempt or alternate-key fallback")
		})
	}
}

func TestIndependentWebSocketBindingMigratesOnlyNewConnections(t *testing.T) {
	providerHeaders := make(chan http.Header, 4)
	f := newIndependentWebsocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		providerHeaders <- r.Header.Clone()
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		created, _ := common.Marshal(map[string]any{"type": "session.created", "session": map[string]string{"id": key}})
		_ = conn.WriteMessage(websocket.TextMessage, created)
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			updated, _ := common.Marshal(map[string]any{"type": "session.updated", "session": map[string]string{"id": key}, "event_id": gjson.GetBytes(frame, "event_id").String()})
			if err := conn.WriteMessage(websocket.TextMessage, updated); err != nil {
				return
			}
		}
	}))
	// Polling would first choose key a. Prebinding b makes bypassing the binding
	// lookup observably wrong instead of being hidden by deterministic placement.
	claimed, err := service.ClaimKeyEnvironmentBinding("kenv:token:992:default", nil,
		service.KeyEnvironmentBinding{ChannelId: 81, KeyTag: model.ChannelKeyTag(f.keys[1])})
	require.NoError(t, err)
	require.True(t, claimed)
	connections := make([]*websocket.Conn, 0, 4)
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	open := func(wantKey string) *websocket.Conn {
		conn, _, err := f.dial(t)
		require.NoError(t, err)
		connections = append(connections, conn)
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, frame, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, wantKey, gjson.GetBytes(frame, "session.id").String())
		return conn
	}
	first := open(f.keys[1])
	second := open(f.keys[1])
	firstHeader, secondHeader := <-providerHeaders, <-providerHeaders
	assert.Equal(t, firstHeader.Get("session-id"), secondHeader.Get("session-id"))
	assert.Equal(t, firstHeader.Get("User-Agent"), secondHeader.Get("User-Agent"))
	require.True(t, model.UpdateChannelStatus(81, f.keys[1], common.ChannelStatusAutoDisabled, "local test"))
	third := open(f.keys[0])
	thirdHeader := <-providerHeaders
	assert.NotEqual(t, firstHeader.Get("User-Agent"), thirdHeader.Get("User-Agent"))
	assert.NotEqual(t, firstHeader.Get("session-id"), thirdHeader.Get("session-id"))
	for _, conn := range []*websocket.Conn{first, second} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update","event_id":"after-disable","session":{}}`)))
		_, frame, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, f.keys[1], gjson.GetBytes(frame, "session.id").String(), "an existing socket stays on the disabled key")
		assert.Equal(t, "after-disable", gjson.GetBytes(frame, "event_id").String())
	}
	require.True(t, model.UpdateChannelStatus(81, f.keys[1], common.ChannelStatusEnabled, "local test"))
	fourth := open(f.keys[0])
	<-providerHeaders
	for _, conn := range []*websocket.Conn{first, second, third, fourth} {
		require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)))
		require.NoError(t, conn.Close())
		audit := f.finished(t)
		assert.Equal(t, true, audit["key_env_bound"])
		assert.NotEmpty(t, audit["cli_profile"])
		assert.NotNil(t, audit["cli_version_mismatch"])
	}
}

func TestIndependentResponsesDoesNotReplayEstablishedWebSocket(t *testing.T) {
	for _, firstFrame := range []string{"upstream error", "upstream disconnect"} {
		t.Run(firstFrame, func(t *testing.T) {
			calls := make(chan struct{}, 4)
			f := newIndependentWebsocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls <- struct{}{}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				if firstFrame == "upstream error" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":503,"error":{"message":"no replay","type":"upstream_error"}}`))
				}
			}))
			body := fmt.Sprintf(`{"model":%q,"input":"hi","stream":false}`, f.model)
			req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := f.server.Client().Do(req)
			require.NoError(t, err)
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			wantStatus := http.StatusInternalServerError
			if firstFrame == "upstream error" {
				wantStatus = http.StatusServiceUnavailable
			}
			assert.Equal(t, wantStatus, resp.StatusCode, string(data))
			f.finished(t)
			require.Len(t, calls, 1, "an established socket cannot replay on this key or fall back to HTTP")
		})
	}
}
