package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/privacy"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func privacyRouter(t *testing.T, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	oldConfig := *system_setting.GetPrivacySetting()
	privacyStoreMu.Lock()
	oldStore, oldError := privacySessions, privacyStoreError
	*system_setting.GetPrivacySetting() = system_setting.PrivacySetting{ProtectIPs: true}
	privacySessions, privacyStoreError = privacy.NewStore(16, time.Minute)
	require.NoError(t, privacyStoreError)
	privacyStoreMu.Unlock()
	t.Cleanup(func() {
		*system_setting.GetPrivacySetting() = oldConfig
		privacyStoreMu.Lock()
		privacySessions, privacyStoreError = oldStore, oldError
		privacyStoreMu.Unlock()
	})
	router := gin.New()
	router.Use(DecompressRequestMiddleware(), BodyStorageCleanup(), func(c *gin.Context) {
		c.Set("id", 42)
		if c.GetHeader("X-Test-Other-User") != "" {
			c.Set("id", 43)
		}
		c.Set("token_id", 8)
		c.Next()
	}, PromptPrivacy())
	router.POST("/v1/chat/completions", handler)
	router.POST("/v1/responses", handler)
	router.POST("/v1/images/edits", handler)
	return router
}

func privacyRequest(t *testing.T, router *gin.Engine, body, id string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-New-Api-Privacy", "on")
	request.Header.Set("X-New-Api-Privacy-Tools", "restore")
	if id != "" {
		request.Header.Set("X-New-Api-Privacy-Session", id)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestPrivacyMiddlewareRoundTripAndSessionToolOutput(t *testing.T) {
	var received []string
	router := privacyRouter(t, func(c *gin.Context) {
		first, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		body, err := first.Bytes()
		require.NoError(t, err)
		received = append(received, string(body))
		assert.NotContains(t, string(body), "fixture-pass")
		assert.NotContains(t, string(body), "203.0.113.15")
		assert.NotContains(t, string(body), "new_api_privacy")
		assert.Empty(t, c.GetHeader("X-New-Api-Privacy-Session"))
		retry, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		replay, err := io.ReadAll(retry)
		require.NoError(t, err)
		assert.Equal(t, body, replay)
		text := gjson.GetBytes(body, "messages.0.content").String()
		c.JSON(http.StatusOK, gin.H{"choices": []any{gin.H{"message": gin.H{"content": text}}}, "usage": gin.H{"total_tokens": 17}})
	})
	body := `{"model":"fixture","messages":[{"role":"user","content":"连接 203.0.113.15，密码是 fixture-pass 检查磁盘"}],"temperature":0,"stream":false}`
	response := privacyRequest(t, router, body, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "fixture-pass")
	assert.Contains(t, response.Body.String(), "203.0.113.15")
	assert.Equal(t, int64(17), gjson.GetBytes(response.Body.Bytes(), "usage.total_tokens").Int())
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	id := response.Header().Get("X-New-Api-Privacy-Session")
	require.NotEmpty(t, id)
	// A later tool result lacks any password label. The encrypted session still
	// hides the value without requiring the original user message to be replayed.
	response = privacyRequest(t, router, `{"model":"fixture","messages":[{"role":"tool","content":"fixture-pass"}]}`, id)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "fixture-pass")
	assert.Len(t, received, 2)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-New-Api-Privacy", "on")
	request.Header.Set("X-New-Api-Privacy-Session", id)
	request.Header.Set("X-Test-Other-User", "yes")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Len(t, received, 2, "another user must not reach the upstream with this session")
}

func TestPrivacyMiddlewareSSEAndExplicitSecret(t *testing.T) {
	router := privacyRouter(t, func(c *gin.Context) {
		storage, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		body, err := storage.Bytes()
		require.NoError(t, err)
		ref := gjson.GetBytes(body, "messages.0.content").String()
		require.NotContains(t, ref, "unlabelled-fixture")
		c.Header("Content-Type", "text/event-stream")
		c.Header("Content-Length", "1")
		for _, part := range []string{ref[:8], ref[8:]} {
			data, err := common.Marshal(gin.H{"choices": []any{gin.H{"index": 0, "delta": gin.H{"content": part}}}})
			require.NoError(t, err)
			_, err = c.Writer.WriteString("data: " + string(data) + "\n\n")
			require.NoError(t, err)
			c.Writer.Flush()
		}
		_, err = c.Writer.WriteString("data: [DONE]\n\n")
		require.NoError(t, err)
	})
	response := privacyRequest(t, router, `{"model":"fixture","messages":[{"role":"user","content":"unlabelled-fixture"}],"new_api_privacy":{"sensitive_values":["unlabelled-fixture"]},"stream":true}`, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "unlabelled-fixture")
	assert.NotContains(t, response.Body.String(), privacy.Prefix)
	assert.Contains(t, response.Body.String(), "data: [DONE]")
	assert.Empty(t, response.Header().Get("Content-Length"))
}

func TestPrivacyMiddlewareLeavesDisabledTrafficUnchanged(t *testing.T) {
	body := `{"messages":[{"content":"password=fixture-pass"}],"temperature":0}`
	router := privacyRouter(t, func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		assert.Equal(t, body, string(data))
		assert.Nil(t, privacy.FromContext(c))
		c.Data(http.StatusOK, "application/json", data)
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	assert.Equal(t, body, response.Body.String())
	assert.Empty(t, response.Header().Get("X-New-Api-Privacy"))
}

func TestPrivacyInvalidProtectedRequestsNeverReachHandler(t *testing.T) {
	calls := 0
	router := privacyRouter(t, func(c *gin.Context) { calls++; c.JSON(http.StatusOK, gin.H{}) })
	for _, tc := range []struct {
		body, path, media string
		status            int
	}{
		{`{"messages":`, "/v1/chat/completions", "application/json", http.StatusBadRequest},
		{`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"https://example.test/a"}}]}]}`, "/v1/chat/completions", "application/json", http.StatusBadRequest},
		{`{"previous_response_id":"resp_old"}`, "/v1/responses", "application/json", http.StatusConflict},
		{"multipart fixture", "/v1/images/edits", "multipart/form-data", http.StatusBadRequest},
	} {
		t.Run(fmt.Sprint(tc.status, tc.path, tc.body[:8]), func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body))
			request.Header.Set("Content-Type", tc.media)
			request.Header.Set("X-New-Api-Privacy", "on")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
	assert.Zero(t, calls)
}

func TestPrivacyProtectsURLQueryAndCustomHeaders(t *testing.T) {
	var query, custom string
	router := privacyRouter(t, func(c *gin.Context) {
		query = c.Request.URL.RawQuery
		custom = c.GetHeader("X-Remote-URL")
		assert.NotContains(t, query, "fixture-url-pass")
		assert.NotContains(t, custom, "fixture-url-pass")
		c.JSON(http.StatusOK, gin.H{"echo": c.GetHeader("X-Remote-URL")})
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?password=fixture-url-pass", strings.NewReader("{\"messages\":[{\"role\":\"user\",\"content\":\"connect\"}]}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-New-Api-Privacy", "on")
	request.Header.Set("X-Remote-URL", "https://alice:fixture-url-pass@example.test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, query, "fixture-url-pass")
	assert.NotContains(t, custom, "fixture-url-pass")
	assert.Contains(t, response.Body.String(), "fixture-url-pass", "recognized values are restored only for the caller")
}

func TestPrivacySiteWideSwitchProtectsTextBesideUnreadableContent(t *testing.T) {
	// Everything a strict request is refused for rides along here: an image, a
	// document, a provider-side tool, encrypted reasoning and a continuation.
	body := `{"model":"fixture","previous_response_id":"resp_old",` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"password=fixture-pass 看这张图"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,Zml4dHVyZS1wYXNz"}},` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"Zml4dHVyZS1wYXNz"}}]}],` +
		`"input":[{"type":"reasoning","encrypted_content":"Zml4dHVyZS1wYXNz"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	var received []byte
	router := privacyRouter(t, func(c *gin.Context) {
		var err error
		received, err = io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		c.JSON(http.StatusOK, gin.H{"choices": []any{gin.H{"message": gin.H{"content": gjson.GetBytes(received, "messages.0.content.0.text").String()}}}})
	})
	system_setting.GetPrivacySetting().Enabled = true
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "on", response.Header().Get("X-New-Api-Privacy"))
	assert.NotContains(t, string(received), "fixture-pass")
	for _, path := range []string{"messages.0.content.1", "messages.0.content.2", "input.0", "tools.0", "previous_response_id"} {
		assert.JSONEq(t, gjson.Get(body, path).Raw, gjson.GetBytes(received, path).Raw, path)
	}
	assert.Contains(t, response.Body.String(), "password=fixture-pass 看这张图")
}

func TestPrivacySiteWideSwitchLetsUninspectableRequestsThrough(t *testing.T) {
	var received []byte
	router := privacyRouter(t, func(c *gin.Context) {
		var err error
		received, err = io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		assert.Nil(t, privacy.FromContext(c))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.GET("/v1/realtime", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	system_setting.GetPrivacySetting().Enabled = true
	for _, tc := range []struct {
		name, method, path, media, body string
		upgrade                         bool
	}{
		{"upload", http.MethodPost, "/v1/images/edits", "multipart/form-data; boundary=fixture", "--fixture\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\npassword=fixture-pass\r\n--fixture--\r\n", false},
		{"malformed JSON", http.MethodPost, "/v1/chat/completions", "application/json", `{"messages":`, false},
		{"body over the text limit", http.MethodPost, "/v1/chat/completions", "application/json", `{"messages":[{"role":"user","content":"` + strings.Repeat("A", privacyBodyLimit) + `"}]}`, false},
		{"websocket", http.MethodGet, "/v1/realtime", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received = nil
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.media != "" {
				request.Header.Set("Content-Type", tc.media)
			}
			if tc.upgrade {
				request.Header.Set("Upgrade", "websocket")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, "bypass", response.Header().Get("X-New-Api-Privacy"), "the caller is told nothing was protected")
			assert.True(t, tc.body == string(received), "the request reaches the handler as it was sent")
		})
	}
}

func TestPrivacyRequestedProtectionAcceptsEncryptedReasoning(t *testing.T) {
	var received []byte
	router := privacyRouter(t, func(c *gin.Context) {
		var err error
		received, err = io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		c.JSON(http.StatusOK, gin.H{})
	})
	response := privacyRequest(t, router, `{"model":"fixture","input":[{"type":"reasoning","encrypted_content":"Zml4dHVyZS1wYXNz"},{"role":"user","content":"password=fixture-pass"}]}`, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "Zml4dHVyZS1wYXNz", gjson.GetBytes(received, "input.0.encrypted_content").String())
	assert.NotContains(t, string(received), "fixture-pass")
}

func TestPrivacyResponsesWithoutTextArriveIntact(t *testing.T) {
	speech := string([]byte{0xff, 0xfb, 0x90, 0x00, '{', 0x00, 0x01})
	for _, tc := range []struct{ name, media, body string }{
		{"generated speech", "audio/mpeg", speech},
		{"unknown text-like type", "application/xml", "<ok>fixture</ok>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := privacyRouter(t, func(c *gin.Context) {
				c.Header("Content-Length", fmt.Sprint(len(tc.body)))
				c.Data(http.StatusOK, tc.media, []byte(tc.body))
			})
			response := privacyRequest(t, router, `{"model":"fixture","input":"password=fixture-pass"}`, "")
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, tc.media, response.Header().Get("Content-Type"))
			assert.Equal(t, tc.body, response.Body.String())
		})
	}
}

func TestPrivacyOversizedJSONResponse(t *testing.T) {
	// Several generated images in one answer outgrow the restore buffer.
	images := `{"data":[{"b64_json":"` + strings.Repeat("A", 17<<20) + `"}]}`
	router := privacyRouter(t, func(c *gin.Context) { c.Data(http.StatusOK, "application/json", []byte(images)) })
	response := privacyRequest(t, router, `{"model":"fixture","prompt":"password=fixture-pass"}`, "")
	assert.Equal(t, http.StatusBadGateway, response.Code, "a request that asked for protection never receives an unrestored answer")

	system_setting.GetPrivacySetting().Enabled = true
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fixture","prompt":"password=fixture-pass"}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	assert.True(t, images == response.Body.String(), "the site-wide switch delivers what the provider produced")
}
