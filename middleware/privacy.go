package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/privacy"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

const privacyBodyLimit = 4 << 20

var privacySessions *privacy.Store
var privacyStoreError error
var privacyStoreMu sync.Mutex

func getPrivacyStore() (*privacy.Store, error) {
	privacyStoreMu.Lock()
	defer privacyStoreMu.Unlock()
	if privacySessions == nil && privacyStoreError == nil {
		privacySessions, privacyStoreError = privacy.NewStoreWithSecret(1024, 30*time.Minute, common.CryptoSecret)
	}
	return privacySessions, privacyStoreError
}

// PromptPrivacy must run after authentication and before any body caching,
// distribution, token counting, billing, or debug logging.
func PromptPrivacy() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := *system_setting.GetPrivacySetting()
		mode := c.GetHeader("X-New-Api-Privacy")
		if !cfg.Enabled && mode == "" {
			c.Next()
			return
		}
		if mode != "" && mode != "on" {
			privacyAbort(c, http.StatusBadRequest, "privacy_invalid_mode", "X-New-Api-Privacy must be on")
			return
		}
		// A request that asks for protection itself is refused when the promise
		// cannot be kept. The site-wide switch covers every route of the gateway,
		// so there it protects the text it can read and lets the rest through,
		// answering "bypass" for a request it did not inspect.
		bestEffort := mode == ""
		if c.GetHeader("Upgrade") != "" || c.GetHeader("Sec-WebSocket-Key") != "" || c.Request.Method != http.MethodPost || !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
			if !bestEffort {
				privacyAbort(c, http.StatusBadRequest, "privacy_unsupported_request", privacy.ErrUnsupported.Error())
				return
			}
			c.Header("X-New-Api-Privacy", "bypass")
			c.Next()
			return
		}
		store, storeErr := getPrivacyStore()
		if storeErr != nil || c.GetInt("id") <= 0 {
			privacyAbort(c, http.StatusServiceUnavailable, "privacy_unavailable", "privacy unavailable")
			return
		}
		incoming := c.Request.Body
		body, err := io.ReadAll(io.LimitReader(incoming, privacyBodyLimit+1))
		if err == nil && len(body) > privacyBodyLimit && bestEffort {
			// Inline media easily outgrows the text limit; hand the request on as
			// it arrived, the part already read followed by the rest.
			c.Request.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), incoming), incoming}
			c.Header("X-New-Api-Privacy", "bypass")
			c.Next()
			return
		}
		_ = incoming.Close()
		if err != nil || len(body) > privacyBodyLimit {
			privacyAbort(c, http.StatusRequestEntityTooLarge, "privacy_body_limit", "privacy request exceeds the 4 MiB text limit")
			return
		}
		var object map[string]json.RawMessage
		if common.Unmarshal(body, &object) != nil || object == nil {
			if !bestEffort {
				privacyAbort(c, http.StatusBadRequest, "privacy_invalid_json", "invalid privacy JSON request")
				return
			}
			// The route's own handler decides what a malformed body deserves.
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Header("X-New-Api-Privacy", "bypass")
			c.Next()
			return
		}
		defer clear(body)
		var input struct {
			SensitiveValues []string `json:"sensitive_values"`
		}
		if raw, ok := object["new_api_privacy"]; ok {
			if common.Unmarshal(raw, &input) != nil {
				privacyAbort(c, http.StatusBadRequest, "privacy_invalid_options", "invalid new_api_privacy options")
				return
			}
			delete(object, "new_api_privacy")
		}
		toolMode := c.GetHeader("X-New-Api-Privacy-Tools")
		if toolMode != "" && toolMode != "opaque" && toolMode != "restore" {
			privacyAbort(c, http.StatusBadRequest, "privacy_invalid_tools_mode", "X-New-Api-Privacy-Tools must be opaque or restore")
			return
		}
		restoreTools := cfg.RestoreToolArguments || toolMode == "restore"
		sessionID := c.GetHeader("X-New-Api-Privacy-Session")
		if raw := object["previous_response_id"]; !bestEffort && len(raw) > 0 && string(raw) != "null" && string(raw) != `""` && sessionID == "" {
			privacyAbort(c, http.StatusConflict, "privacy_session_required", "previous_response_id requires X-New-Api-Privacy-Session from the preceding response")
			return
		}
		owner := fmt.Sprintf("%d:%d", c.GetInt("id"), c.GetInt("token_id"))
		session, sessionID, err := store.Open(owner, sessionID)
		if err != nil {
			privacyAbort(c, http.StatusConflict, "privacy_session_unavailable", privacy.ErrSession.Error())
			return
		}
		session.ProtectIPs = cfg.ProtectIPs
		session.BestEffort = bestEffort
		defer session.Clear()
		for _, value := range input.SensitiveValues {
			if err := session.AddDeclared(value); err != nil {
				code := "privacy_limit"
				if errors.Is(err, privacy.ErrDeclaredValue) {
					code = "privacy_invalid_declared_value"
				}
				privacyAbort(c, http.StatusBadRequest, code, err.Error())
				return
			}
		}
		data, err := common.Marshal(object)
		if err == nil && !bestEffort {
			err = privacy.ValidateTextRequest(data)
		}
		if err != nil {
			privacyAbort(c, http.StatusBadRequest, "privacy_unsupported_content", "privacy requires valid text content and client-side tools")
			return
		}
		protected, err := session.ProtectJSON(data, privacy.Options{ProtectIPs: cfg.ProtectIPs})
		clear(data)
		if err != nil {
			privacyAbort(c, http.StatusBadRequest, "privacy_protection_failed", err.Error())
			return
		}
		privacyOptions := privacy.Options{ProtectIPs: cfg.ProtectIPs}
		c.Request.URL.RawQuery, err = session.ProtectQuery(c.Request.URL.RawQuery, privacyOptions)
		if err != nil {
			privacyAbort(c, http.StatusBadRequest, "privacy_protection_failed", err.Error())
			return
		}
		c.Request.URL.Path, err = session.ProtectText("url", c.Request.URL.Path, privacyOptions)
		if err != nil {
			privacyAbort(c, http.StatusBadRequest, "privacy_protection_failed", err.Error())
			return
		}
		c.Request.URL.RawPath = ""
		c.Request.RequestURI = c.Request.URL.EscapedPath()
		if c.Request.URL.RawQuery != "" {
			c.Request.RequestURI += "?" + c.Request.URL.RawQuery
		}
		for name, values := range c.Request.Header {
			switch strings.ToLower(name) {
			case "authorization", "x-api-key", "api-key", "x-goog-api-key", "x-new-api-privacy", "x-new-api-privacy-session", "x-new-api-privacy-tools":
				continue
			default:
				for i, value := range values {
					prefix := ""
					if strings.EqualFold(name, "Content-Type") {
						// Keep the downstream parser's media type intact; parameters
						// use the same discovery and references as body text.
						separator := strings.IndexByte(value, ';')
						if separator < 0 {
							continue
						}
						prefix, value = value[:separator+1], value[separator+1:]
					}
					values[i], err = session.ProtectText(name, value, privacyOptions)
					if err != nil {
						privacyAbort(c, http.StatusBadRequest, "privacy_protection_failed", err.Error())
						return
					}
					values[i] = prefix + values[i]
				}
			}
		}
		finalProtected, err := session.ProtectJSON(protected, privacyOptions)
		clear(protected)
		if err != nil {
			privacyAbort(c, http.StatusBadRequest, "privacy_protection_failed", err.Error())
			return
		}
		protected = finalProtected
		if err = store.Save(owner, sessionID, session); err != nil {
			privacyAbort(c, http.StatusServiceUnavailable, "privacy_session_limit", "privacy session capacity exceeded")
			return
		}
		// Clear every legacy raw-body cache. Retries and passthrough read only
		// protected bytes; original input is never handed to disk BodyStorage.
		common.CleanupBodyStorage(c)
		c.Set(common.KeyRequestBody, nil)
		c.Set(gin.BodyBytesKey, nil)
		c.Request.Body = io.NopCloser(bytes.NewReader(protected))
		c.Request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(protected)), nil }
		c.Request.ContentLength = int64(len(protected))
		c.Request.Header.Set("Content-Length", fmt.Sprint(len(protected)))
		c.Request.Header.Del("X-New-Api-Privacy")
		c.Request.Header.Del("X-New-Api-Privacy-Session")
		c.Request.Header.Del("X-New-Api-Privacy-Tools")
		c.Set(privacy.ContextKey, session)
		c.Header("X-New-Api-Privacy", "on")
		c.Header("X-New-Api-Privacy-Session", sessionID)
		c.Header("Cache-Control", "no-store")
		toolsMode := "opaque"
		if restoreTools {
			toolsMode = "restore"
		}
		c.Header("X-New-Api-Privacy-Tools", toolsMode)
		original := c.Writer
		writer := &privacyResponseWriter{ResponseWriter: original, session: session, restoreTools: restoreTools}
		c.Writer = writer
		defer func() { c.Writer = original }()
		c.Next()
		_ = c.Request.Body.Close()
		defer clear(protected)
		// Only ciphertext is kept across requests. Egress sanitization can add
		// references for channel-provided system prompts/parameter overrides.
		if err := store.Save(owner, sessionID, session); err != nil {
			// The initial mapping is already saved; do not log any secret material.
			common.SysError("privacy session update failed")
		}
		if err := writer.finish(); err != nil {
			common.SysError("privacy response encoding failed")
		}
	}
}

func privacyAbort(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": gin.H{"type": code, "code": code, "message": message}})
}

type privacyResponseWriter struct {
	gin.ResponseWriter
	mu           sync.Mutex
	session      *privacy.Session
	restoreTools bool
	stream       *privacy.Stream
	body         []byte
	failed       error
	// direct is set once the response turned out to be something this layer
	// does not rewrite; from then on it is written as it comes.
	direct bool
}

// unreadable reports a response that carries no text to restore: generated
// speech, images and other files. Buffering it would delay or truncate it.
func (w *privacyResponseWriter) unreadable() bool {
	if w.direct {
		return true
	}
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	for _, prefix := range []string{"audio/", "image/", "video/", "application/octet-stream", "application/pdf", "application/zip"} {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	return false
}

func (w *privacyResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return 0, w.failed
	}
	if w.unreadable() {
		w.direct = true
		return w.ResponseWriter.Write(data)
	}
	w.Header().Del("Content-Length")
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		if w.stream == nil {
			w.stream = &privacy.Stream{Session: w.session, RestoreTools: w.restoreTools}
		}
		out, err := w.stream.Write(data, false)
		if err == nil && len(out) > 0 {
			_, err = w.ResponseWriter.Write(out)
		}
		w.failed = err
		if err != nil {
			return 0, err
		}
	} else {
		if len(w.body)+len(data) > 16<<20 {
			if !w.session.BestEffort {
				w.failed = privacy.ErrLimit
				return 0, w.failed
			}
			// Several generated images in one JSON answer outgrow the buffer;
			// deliver them unrestored rather than fail a paid request.
			w.direct = true
			if _, err := w.ResponseWriter.Write(w.body); err != nil {
				w.failed = err
				return 0, err
			}
			w.body = nil
			return w.ResponseWriter.Write(data)
		}
		w.body = append(w.body, data...)
	}
	return len(data), nil
}

func (w *privacyResponseWriter) WriteString(value string) (int, error) { return w.Write([]byte(value)) }

func (w *privacyResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *privacyResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.unreadable() {
		w.ResponseWriter.Flush()
		return
	}
	w.Header().Del("Content-Length")
	// Keep deferred JSON responses uncommitted; SSE headers may be flushed for
	// heartbeats while a reference waits for its next delta.
	if w.stream != nil || strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.ResponseWriter.Flush()
	}
}

func (w *privacyResponseWriter) finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		if !w.ResponseWriter.Written() {
			w.ResponseWriter.Header().Set("Content-Type", "application/json")
			w.ResponseWriter.WriteHeader(http.StatusBadGateway)
			_, _ = w.ResponseWriter.Write([]byte(`{"error":{"type":"privacy_response_failed","message":"privacy response processing failed"}}`))
		}
		return w.failed
	}
	if w.direct {
		return nil
	}
	w.Header().Del("Content-Length")
	if w.stream != nil {
		out, err := w.stream.Write(nil, true)
		if err != nil {
			return err
		}
		if len(out) > 0 {
			if _, err = w.ResponseWriter.Write(out); err != nil {
				return err
			}
		}
		w.ResponseWriter.Flush()
		return nil
	}
	if len(w.body) == 0 {
		return nil
	}
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	var out []byte
	var err error
	trimmed := bytes.TrimSpace(w.body)
	if strings.Contains(contentType, "json") || (len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) {
		out, err = w.session.RestoreJSON(w.body, w.restoreTools)
		if err != nil {
			return errors.New("invalid privacy response JSON")
		}
	} else if strings.HasPrefix(contentType, "text/") {
		out = []byte(w.session.Restore(string(w.body)))
	} else {
		out = w.body
	}
	_, err = w.ResponseWriter.Write(out)
	return err
}
