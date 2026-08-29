package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	common2 "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// applyUpstreamContentLength populates req.ContentLength when the upstream
// body is wrapped in a BodyStorage (see relay/common/outbound_body.go).
//
// net/http.NewRequest only auto-detects ContentLength for *bytes.Reader,
// *bytes.Buffer and *strings.Reader. When the body is a type-erased io.Reader
// (which is the case for ReaderOnly(BodyStorage)), the Content-Length header
// would otherwise be omitted, forcing chunked transfer encoding and breaking
// some upstreams that require an explicit Content-Length.
func applyUpstreamContentLength(req *http.Request, info *common.RelayInfo) {
	if info == nil {
		return
	}
	if info.UpstreamRequestBodySize > 0 && req.ContentLength <= 0 {
		req.ContentLength = info.UpstreamRequestBodySize
	}
}

// newUpstreamRequest builds the outbound request bound to the caller's request
// context. RELAY_TIMEOUT defaults to 0 so the client has no overall deadline;
// without a context an upstream that accepts the connection and then goes silent
// would pin a goroutine and a connection forever, even after the caller hung up.
// The streaming path already treats c.Request.Context().Done() as a stop signal
// (see relay/helper/stream_scanner.go), so this only makes the outbound leg
// follow the same lifetime.
func newUpstreamRequest(c *gin.Context, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(c.Request.Context(), c.Request.Method, url, body)
}

func SetupApiRequestHeader(info *common.RelayInfo, c *gin.Context, req *http.Header) {
	if info.RelayMode == constant.RelayModeAudioTranscription || info.RelayMode == constant.RelayModeAudioTranslation {
		// multipart/form-data
	} else if info.RelayMode == constant.RelayModeRealtime {
		// websocket
	} else {
		req.Set("Content-Type", c.Request.Header.Get("Content-Type"))
		req.Set("Accept", c.Request.Header.Get("Accept"))
		if info.IsStream && c.Request.Header.Get("Accept") == "" {
			req.Set("Accept", "text/event-stream")
		}
	}

	if info.IsChannelTest {
		forwardChannelTestClientHeaders(c, req)
	}
}

// forwardChannelTestClientHeaders carries the client-identity headers of a
// channel test to upstream.
//
// A test builds its own request (controller/channel-test.go replaces
// c.Request outright) and only Content-Type and Accept are copied above, so
// without this an upstream sees no user-agent at all. Upstreams that gate on
// the client shape — the relay sites that only answer Claude Code, the ones
// expecting an official SDK — reject that with a 4xx that says nothing about
// whether the model works, and the model-validation task then reads a working
// model as a failing one.
//
// Restricted to channel tests on purpose: for live traffic, forwarding the
// caller's headers is what the "send original request" channel setting is for,
// and this must not start doing it implicitly for every channel. Because the
// test's c.Request is synthetic, the only headers here are the ones
// applyTestClientHeaders put there — never a real caller's.
//
// shouldSkipPassthroughHeader is reused rather than re-listing anything: it
// already excludes credentials (Authorization, x-api-key, ...), hop-by-hop
// headers, and Host/Content-Length, so the channel's own key still wins.
func forwardChannelTestClientHeaders(c *gin.Context, req *http.Header) {
	if c == nil || c.Request == nil || req == nil {
		return
	}
	for name := range c.Request.Header {
		if shouldSkipPassthroughHeader(name) {
			continue
		}
		// Content-Type/Accept are already set above, and an adaptor may have
		// deliberately chosen a different value than the test request carries.
		if req.Get(name) != "" {
			continue
		}
		value := strings.TrimSpace(c.Request.Header.Get(name))
		if value == "" {
			continue
		}
		req.Set(name, value)
	}
}

const clientHeaderPlaceholderPrefix = "{client_header:"

const (
	headerPassthroughAllKey        = "*"
	headerPassthroughRegexPrefix   = "re:"
	headerPassthroughRegexPrefixV2 = "regex:"
)

var passthroughSkipHeaderNamesLower = map[string]struct{}{
	// RFC 7230 hop-by-hop headers.
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},

	"cookie": {},

	// Additional headers that should not be forwarded by name-matching passthrough rules.
	"host":            {},
	"content-length":  {},
	"accept-encoding": {},

	// Do not passthrough credentials by wildcard/regex.
	"authorization":  {},
	"x-api-key":      {},
	"x-goog-api-key": {},
	"api-key":        {},
	"mj-api-secret":  {},
	"new-api-user":   {},

	// WebSocket handshake headers are generated by the client/dialer.
	// sec-websocket-protocol carries the caller's key for realtime, and the
	// adaptor rewrites it with the channel key.
	"sec-websocket-key":        {},
	"sec-websocket-version":    {},
	"sec-websocket-extensions": {},
	"sec-websocket-protocol":   {},
}

var headerPassthroughRegexCache sync.Map // map[string]*regexp.Regexp

func getHeaderPassthroughRegex(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, errors.New("empty regex pattern")
	}
	if v, ok := headerPassthroughRegexCache.Load(pattern); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		headerPassthroughRegexCache.Delete(pattern)
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	actual, _ := headerPassthroughRegexCache.LoadOrStore(pattern, compiled)
	if re, ok := actual.(*regexp.Regexp); ok {
		return re, nil
	}
	return compiled, nil
}

func IsHeaderPassthroughRuleKey(key string) bool {
	return isHeaderPassthroughRuleKey(key)
}
func isHeaderPassthroughRuleKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	if key == headerPassthroughAllKey {
		return true
	}
	lower := strings.ToLower(key)
	return strings.HasPrefix(lower, headerPassthroughRegexPrefix) || strings.HasPrefix(lower, headerPassthroughRegexPrefixV2)
}

func shouldSkipPassthroughHeader(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	lower := strings.ToLower(name)
	if _, ok := passthroughSkipHeaderNamesLower[lower]; ok {
		return true
	}
	return false
}

func applyHeaderOverridePlaceholders(template string, c *gin.Context, apiKey string) (string, bool, error) {
	trimmed := strings.TrimSpace(template)
	if strings.HasPrefix(trimmed, clientHeaderPlaceholderPrefix) {
		afterPrefix := trimmed[len(clientHeaderPlaceholderPrefix):]
		end := strings.Index(afterPrefix, "}")
		if end < 0 || end != len(afterPrefix)-1 {
			return "", false, fmt.Errorf("client_header placeholder must be the full value: %q", template)
		}

		name := strings.TrimSpace(afterPrefix[:end])
		if name == "" {
			return "", false, fmt.Errorf("client_header placeholder name is empty: %q", template)
		}
		if c == nil || c.Request == nil {
			return "", false, fmt.Errorf("missing request context for client_header placeholder")
		}
		clientHeaderValue := c.Request.Header.Get(name)
		if strings.TrimSpace(clientHeaderValue) == "" {
			return "", false, nil
		}
		// Do not interpolate {api_key} inside client-supplied content.
		return clientHeaderValue, true, nil
	}

	if strings.Contains(template, "{api_key}") {
		template = strings.ReplaceAll(template, "{api_key}", apiKey)
	}
	if strings.TrimSpace(template) == "" {
		return "", false, nil
	}
	return template, true, nil
}

// processHeaderOverride applies channel header overrides, with placeholder substitution.
// Supported placeholders:
//   - {api_key}: resolved to the channel API key
//   - {client_header:<name>}: resolved to the incoming request header value
//
// Header passthrough rules (keys only; values are ignored):
//   - "*": passthrough all incoming headers by name (excluding unsafe headers)
//   - "re:<regex>" / "regex:<regex>": passthrough headers whose names match the regex (Go regexp)
//
// Passthrough rules are applied first, then normal overrides are applied, so explicit overrides win.
func processHeaderOverride(info *common.RelayInfo, c *gin.Context) (map[string]string, error) {
	headerOverride := make(map[string]string)
	if info == nil {
		return headerOverride, nil
	}

	headerOverrideSource := common.GetEffectiveHeaderOverride(info)

	// A synthetic profile replaces passthrough entirely: upstream gets generated
	// client headers instead of anything the caller sent. Passthrough's deny-list
	// only strips the credential header names it happens to know, so a caller's
	// anthropic-api-key / x-auth-token / x-forwarded-for all reached upstream
	// under it. Injecting a profile is the allow-list version of the same idea and
	// still satisfies upstreams that gate on the client's shape.
	syntheticFamily := ""
	if info.ChannelMeta != nil && !info.IsChannelTest {
		profile := info.ChannelSetting.SyntheticClientHeadersProfile
		if profile == "" && info.ChannelSetting.SyntheticClientHeaders {
			// Normalize maps the legacy bool to auto. Repeating it here means a
			// RelayInfo assembled without Normalize still gets the profile rather
			// than silently falling back to forwarding the caller's headers.
			profile = dto.SyntheticClientHeadersProfileAuto
		}
		switch profile {
		case "":
		case dto.SyntheticClientHeadersProfileAuto:
			syntheticFamily = ClientHeaderFamilyForAPIType(info.ChannelMeta.ApiType)
		default:
			syntheticFamily = profile
		}
	}
	syntheticHeaders := syntheticFamily != ""
	if syntheticHeaders {
		// EffectiveClientHeaders rather than the raw profile: the built-in client
		// versions go stale and the admin's override map is where they get
		// corrected, so live traffic and channel tests wear the same headers.
		for name, value := range EffectiveClientHeaders(syntheticFamily) {
			headerOverride[strings.ToLower(strings.TrimSpace(name))] = value
		}
		if info.IsStream {
			headerOverride["accept"] = AcceptSSE
		} else {
			headerOverride["accept"] = AcceptJSON
		}
	}

	passAll := false
	var passthroughRegex []*regexp.Regexp
	if !info.IsChannelTest && !syntheticHeaders {
		for k := range headerOverrideSource {
			key := strings.TrimSpace(strings.ToLower(k))
			if key == "" {
				continue
			}
			if key == headerPassthroughAllKey {
				passAll = true
				continue
			}

			var pattern string
			switch {
			case strings.HasPrefix(key, headerPassthroughRegexPrefix):
				pattern = strings.TrimSpace(key[len(headerPassthroughRegexPrefix):])
			case strings.HasPrefix(key, headerPassthroughRegexPrefixV2):
				pattern = strings.TrimSpace(key[len(headerPassthroughRegexPrefixV2):])
			default:
				continue
			}

			if pattern == "" {
				return nil, types.NewError(fmt.Errorf("header passthrough regex pattern is empty: %q", k), types.ErrorCodeChannelHeaderOverrideInvalid, types.ErrOptionWithSkipRetry())
			}
			compiled, err := getHeaderPassthroughRegex(pattern)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeChannelHeaderOverrideInvalid, types.ErrOptionWithSkipRetry())
			}
			passthroughRegex = append(passthroughRegex, compiled)
		}
	}

	if passAll || len(passthroughRegex) > 0 {
		if c == nil || c.Request == nil {
			return nil, types.NewError(fmt.Errorf("missing request context for header passthrough"), types.ErrorCodeChannelHeaderOverrideInvalid, types.ErrOptionWithSkipRetry())
		}
		for name := range c.Request.Header {
			if shouldSkipPassthroughHeader(name) {
				continue
			}
			if !passAll {
				matched := false
				for _, re := range passthroughRegex {
					if re.MatchString(name) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			value := strings.TrimSpace(c.Request.Header.Get(name))
			if value == "" {
				continue
			}
			headerOverride[strings.ToLower(strings.TrimSpace(name))] = value
		}
	}

	for k, v := range headerOverrideSource {
		if isHeaderPassthroughRuleKey(k) {
			continue
		}
		key := strings.TrimSpace(strings.ToLower(k))
		if key == "" {
			continue
		}

		str, ok := v.(string)
		if !ok {
			return nil, types.NewError(nil, types.ErrorCodeChannelHeaderOverrideInvalid, types.ErrOptionWithSkipRetry())
		}
		if info.IsChannelTest && strings.HasPrefix(strings.TrimSpace(str), clientHeaderPlaceholderPrefix) {
			continue
		}
		// {client_header:<name>} copies a caller header to upstream, which is
		// exactly what synthetic headers exist to prevent. Without this an
		// explicit override could reintroduce the leak the setting removes.
		if syntheticHeaders && strings.HasPrefix(strings.TrimSpace(str), clientHeaderPlaceholderPrefix) {
			continue
		}

		value, include, err := applyHeaderOverridePlaceholders(str, c, info.ApiKey)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeChannelHeaderOverrideInvalid, types.ErrOptionWithSkipRetry())
		}
		if !include {
			continue
		}

		headerOverride[key] = value
	}
	return headerOverride, nil
}

func ResolveHeaderOverride(info *common.RelayInfo, c *gin.Context) (map[string]string, error) {
	return processHeaderOverride(info, c)
}

func applyHeaderOverrideToRequest(req *http.Request, headerOverride map[string]string) {
	if req == nil {
		return
	}
	for key, value := range headerOverride {
		req.Header.Set(key, value)
		// set Host in req
		if strings.EqualFold(key, "Host") {
			req.Host = value
		}
	}
}

func DoApiRequest(a Adaptor, c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	fullRequestURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	logger.LogDebug(c, "fullRequestURL: %s", common.SanitizeURLForLog(fullRequestURL))
	req, err := newUpstreamRequest(c, fullRequestURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	applyUpstreamContentLength(req, info)
	headers := req.Header
	err = a.SetupRequestHeader(c, &headers, info)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	// 在 SetupRequestHeader 之后应用 Header Override，确保用户设置优先级最高
	// 这样可以覆盖默认的 Authorization header 设置
	headerOverride, err := processHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	applyHeaderOverrideToRequest(req, headerOverride)
	resp, err := doRequest(c, req, info)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}

func DoFormRequest(a Adaptor, c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	fullRequestURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	logger.LogDebug(c, "fullRequestURL: %s", common.SanitizeURLForLog(fullRequestURL))
	req, err := newUpstreamRequest(c, fullRequestURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	applyUpstreamContentLength(req, info)
	// set form data
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	headers := req.Header
	err = a.SetupRequestHeader(c, &headers, info)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	// 在 SetupRequestHeader 之后应用 Header Override，确保用户设置优先级最高
	// 这样可以覆盖默认的 Authorization header 设置
	headerOverride, err := processHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	applyHeaderOverrideToRequest(req, headerOverride)
	resp, err := doRequest(c, req, info)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}

func DoWssRequest(a Adaptor, c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (*websocket.Conn, error) {
	fullRequestURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	targetHeader := http.Header{}
	err = a.SetupRequestHeader(c, &targetHeader, info)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	// 在 SetupRequestHeader 之后应用 Header Override，确保用户设置优先级最高
	// 这样可以覆盖默认的 Authorization header 设置
	headerOverride, err := processHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	for key, value := range headerOverride {
		targetHeader.Set(key, value)
	}
	targetHeader.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	targetConn, _, err := websocket.DefaultDialer.Dial(fullRequestURL, targetHeader)
	if err != nil {
		return nil, fmt.Errorf("dial failed to %s: %w", common.SanitizeURLForLog(fullRequestURL), err)
	}
	// send request body
	//all, err := io.ReadAll(requestBody)
	//err = service.WssString(c, targetConn, string(all))
	return targetConn, nil
}

func startPingKeepAlive(c *gin.Context, pingInterval time.Duration) (context.CancelFunc, <-chan struct{}) {
	pingerCtx, stopPinger := context.WithCancel(context.Background())
	done := make(chan struct{})

	gopool.Go(func() {
		defer close(done)
		defer func() {
			// 增加panic恢复处理
			if r := recover(); r != nil {
				logger.LogDebug(c, "SSE ping goroutine panic recovered: %v", r)
			}
			logger.LogDebug(c, "SSE ping goroutine stopped")
		}()

		if pingInterval <= 0 {
			pingInterval = helper.DefaultPingInterval
		}

		ticker := time.NewTicker(pingInterval)
		// 确保在任何情况下都清理ticker
		defer func() {
			ticker.Stop()
			logger.LogDebug(c, "SSE ping ticker stopped")
		}()

		var pingMutex sync.Mutex
		logger.LogDebug(c, "SSE ping goroutine started")

		// 增加超时控制，防止goroutine长时间运行
		maxPingDuration := 120 * time.Minute // 最大ping持续时间
		pingTimeout := time.NewTimer(maxPingDuration)
		defer pingTimeout.Stop()

		for {
			select {
			// 发送 ping 数据
			case <-ticker.C:
				if err := sendPingData(c, &pingMutex); err != nil {
					logger.LogDebug(c, "SSE ping error, stopping goroutine: %s", err.Error())
					return
				}
			// 收到退出信号
			case <-pingerCtx.Done():
				return
			// request 结束
			case <-c.Request.Context().Done():
				return
			// 超时保护，防止goroutine无限运行
			case <-pingTimeout.C:
				logger.LogDebug(c, "SSE ping goroutine timeout, stopping")
				return
			}
		}
	})

	return stopPinger, done
}

func sendPingData(c *gin.Context, mutex *sync.Mutex) error {
	mutex.Lock()
	defer mutex.Unlock()

	// Bound the write so a slow client cannot block this goroutine forever;
	// doRequest's defer waits for the pinger to exit before returning.
	helper.ExtendWriteDeadline(c)
	err := helper.PingData(c)
	if err != nil {
		logger.LogError(c, "SSE ping error: "+err.Error())
		return err
	}

	logger.LogDebug(c, "SSE ping data sent")
	return nil
}

func DoRequest(c *gin.Context, req *http.Request, info *common.RelayInfo) (*http.Response, error) {
	return doRequest(c, req, info)
}
func doRequest(c *gin.Context, req *http.Request, info *common.RelayInfo) (*http.Response, error) {
	var client *http.Client
	var err error
	if info.ChannelSetting.Proxy != "" {
		client, err = service.NewProxyHttpClient(info.ChannelSetting.Proxy)
		if err != nil {
			return nil, fmt.Errorf("new proxy http client failed: %w", err)
		}
	} else {
		client = service.GetHttpClient()
	}

	var stopPinger context.CancelFunc
	var pingerDone <-chan struct{}
	if info.IsStream {
		helper.SetEventStreamHeaders(c)
		// 处理流式请求的 ping 保活
		generalSettings := operation_setting.GetGeneralSetting()
		if generalSettings.PingIntervalEnabled && !info.DisablePing {
			pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
			stopPinger, pingerDone = startPingKeepAlive(c, pingInterval)
			// 使用defer确保在任何情况下都能停止ping goroutine
			defer func() {
				if stopPinger != nil {
					stopPinger()
					<-pingerDone
					// The pinger armed per-write deadlines on this connection. When
					// the upstream request fails the stream scanner never runs, so
					// nothing else clears them and the next request on the same
					// keep-alive connection would inherit an expired deadline. The
					// pinger has fully exited here, so no concurrent write can
					// re-arm after the clear; the scanner re-arms its own if the
					// stream does proceed.
					_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
					logger.LogDebug(c, "SSE ping goroutine stopped by defer")
				}
			}()
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.LogError(c, "do request failed: "+err.Error())
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithHideErrMsg("upstream error: do request failed"))
	}
	if resp == nil {
		return nil, errors.New("resp is nil")
	}

	if upID := resp.Header.Get(common2.RequestIdKey); upID != "" {
		c.Set(common2.UpstreamRequestIdKey, upID)
	}

	_ = req.Body.Close()
	_ = c.Request.Body.Close()
	return resp, nil
}

func DoTaskApiRequest(a TaskAdaptor, c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	fullRequestURL, err := a.BuildRequestURL(info)
	if err != nil {
		return nil, err
	}
	req, err := newUpstreamRequest(c, fullRequestURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	applyUpstreamContentLength(req, info)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(requestBody), nil
	}

	err = a.BuildRequestHeader(c, req, info)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	resp, err := doRequest(c, req, info)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}
