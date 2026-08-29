package channel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	appcommon "github.com/QuantumNous/new-api/common"
	appconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

// This file carries the Responses API WebSocket transport.
//
// Upstream sends one JSON event per text frame, and those events are byte-identical
// to the payload of an SSE `data:` line. That is what makes this cheap to graft onto
// a gateway whose whole downstream pipeline (StreamScannerHandler, the responses
// handlers, usage accounting) is written against an *http.Response: the frames are
// re-encoded as SSE into a pipe and handed back as a synthetic response, so nothing
// downstream needs to know the transport changed.
//
// What this buys is one saved round trip per request. The larger prize — upstream's
// connection-local cache of the previous response's rendered tokens — needs
// consecutive turns pinned to one socket, which a stateless HTTP gateway cannot
// promise, so every request here opens its own socket and that cache stays cold.

const (
	// websocketResponsesBetaHeader opts into WebSocket mode. Upstream rejects the
	// upgrade outright without it.
	websocketResponsesBetaHeader = "responses_websockets=2026-02-06"
	// websocketResponsesBetaMarker detects an existing opt-in on a caller- or
	// override-supplied OpenAI-Beta header, whose pinned date must be preserved.
	websocketResponsesBetaMarker = "responses_websockets="
	// websocketHandshakeTimeout bounds the upgrade attempt. It is deliberately
	// short: a failed handshake is followed by a full HTTP attempt, so this delay
	// is paid on top of the normal request and must not approach the caller's own
	// deadline.
	websocketHandshakeTimeout = 10 * time.Second
)

// ShouldTryResponsesWebsocket reports whether this request may attempt the
// WebSocket transport.
//
// The admin's switch is necessary but not sufficient: WebsocketUnsupported, written
// after a handshake proved the upstream refuses the upgrade, overrides it, and only
// /v1/responses has a WebSocket form at all.
func ShouldTryResponsesWebsocket(info *common.RelayInfo) bool {
	// ChannelMeta is an embedded pointer, so reading the promoted settings fields
	// dereferences it. InitChannelMeta runs before DoRequest on every relay path
	// that reaches here, but the guard is free and the alternative is a panic on
	// the request path if that ordering ever changes.
	if info == nil || info.ChannelMeta == nil {
		return false
	}
	if !info.ChannelSetting.WebsocketTransport || info.ChannelOtherSettings.WebsocketUnsupported {
		return false
	}
	// /v1/responses/compact has no WebSocket form; upstream's own guidance is to
	// call it over HTTP and start a fresh chain from the compacted window.
	if info.RelayMode != relayconstant.RelayModeResponses {
		return false
	}
	// A channel test exists to report what the channel does over its normal path,
	// and it reuses this relay code. Probing an upgrade here would turn "the model
	// works" into "the upgrade failed" and could persist a downgrade off a
	// synthetic request.
	if info.IsChannelTest {
		return false
	}
	switch info.ApiType {
	case appconstant.APITypeOpenAI, appconstant.APITypeCodex:
		return true
	default:
		return false
	}
}

// buildResponsesWebsocketURL rewrites an https/http Responses URL to wss/ws.
func buildResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", fmt.Errorf("parse responses url failed: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported scheme for websocket transport: %q", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("responses websocket url has no host")
	}
	return parsed.String(), nil
}

// applyResponsesWebsocketBetaHeader ensures the opt-in is present without
// discarding a pinned date the caller or a header override already chose.
func applyResponsesWebsocketBetaHeader(header http.Header) {
	if header == nil {
		return
	}
	if existing := strings.TrimSpace(header.Get("OpenAI-Beta")); existing != "" &&
		strings.Contains(existing, websocketResponsesBetaMarker) {
		return
	}
	header.Set("OpenAI-Beta", websocketResponsesBetaHeader)
}

// stripHTTPOnlyHandshakeHeaders removes headers that describe an HTTP body
// exchange. The handshake is a GET with no body, and Accept: text/event-stream
// would advertise a transport this request is not using.
func stripHTTPOnlyHandshakeHeaders(header http.Header) {
	if header == nil {
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Accept"} {
		header.Del(name)
	}
}

// newResponsesWebsocketDialer builds a dialer honouring the channel's proxy
// setting. The scheme handling mirrors service.NewProxyHttpClient so a channel
// behind a proxy behaves the same on either transport; an unusable proxy value
// degrades to a direct dial rather than failing the request, since the HTTP path
// would have surfaced that misconfiguration already.
func newResponsesWebsocketDialer(info *common.RelayInfo) *websocket.Dialer {
	dialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: websocketHandshakeTimeout,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if appcommon.TLSInsecureSkipVerify {
		dialer.TLSClientConfig = appcommon.InsecureTLSConfig
	}

	proxyURL := ""
	if info != nil {
		proxyURL = strings.TrimSpace(info.ChannelSetting.Proxy)
	}
	if proxyURL == "" {
		return dialer
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		logger.LogError(nil, "websocket transport: unusable channel proxy, dialing direct: "+err.Error())
		return dialer
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		dialer.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if parsed.User != nil {
			auth = &proxy.Auth{User: parsed.User.Username()}
			if password, ok := parsed.User.Password(); ok {
				auth.Password = password
			}
		}
		socksDialer, errSocks := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
		if errSocks != nil {
			logger.LogError(nil, "websocket transport: socks5 dialer failed, dialing direct: "+errSocks.Error())
			return dialer
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	default:
		logger.LogError(nil, fmt.Sprintf("websocket transport: unsupported proxy scheme %q, dialing direct", parsed.Scheme))
	}
	return dialer
}

// isDefinitiveUpgradeRefusal reports whether a handshake status proves the
// endpoint has no WebSocket form, making the downgrade worth persisting.
//
// Deliberately narrow. This flag survives until an admin saves the channel, so a
// false positive is a silent permanent regression, while a false negative costs
// one wasted handshake per request. Timeouts and transport errors never reach here
// (they arrive with no status at all); 5xx and 429 mean the endpoint exists and is
// unwell; 401/403 are about the credential, which says nothing about whether the
// upstream speaks WebSocket.
func isDefinitiveUpgradeRefusal(status int) bool {
	switch status {
	case http.StatusUpgradeRequired, // 426: reached an HTTP-only handler
		http.StatusNotFound,      // 404: no route at the wss path
		http.StatusNotImplemented, // 501: route exists, upgrade unimplemented
		http.StatusMethodNotAllowed: // 405: path is POST-only, i.e. HTTP-only
		return true
	default:
		return false
	}
}

// closeHandshakeBody drains and closes a rejected handshake's response body so
// the connection can be reused rather than abandoned.
func closeHandshakeBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if err := resp.Body.Close(); err != nil {
		logger.LogError(nil, "websocket transport: close handshake body failed: "+err.Error())
	}
}

// closeWebsocketConn closes an upstream socket, logging rather than returning the
// error: every caller is already on an error or teardown path.
func closeWebsocketConn(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	if err := conn.Close(); err != nil {
		logger.LogError(nil, "websocket transport: close upstream socket failed: "+err.Error())
	}
}

// readWebsocketFrame reads one frame, re-arming the idle deadline first.
//
// The deadline is per-frame rather than per-stream: a long reasoning turn can
// legitimately go quiet between events, and only silence longer than one interval
// means the socket is dead. Without it a half-open connection would pin this
// goroutine and the caller's request forever.
func readWebsocketFrame(conn *websocket.Conn) (int, []byte, error) {
	idle := time.Duration(appconstant.StreamingTimeout) * time.Second
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
		return 0, nil, err
	}
	return conn.ReadMessage()
}

// websocketFrameErrorStatus reports whether a frame is an upstream error and, if
// so, the HTTP status to give the synthetic response.
//
// This must be consulted before the synthetic response is built. Once a 200 is
// committed the status cannot be revised, and the relay's error handling keys off
// resp.StatusCode — so an error frame that slipped through as a 200 would reach the
// caller as a malformed event instead of a billable-free failure.
func websocketFrameErrorStatus(payload []byte) (int, bool) {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return 0, false
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		// An error frame with no status still terminates the request; 500 keeps it
		// an error rather than letting it read as success.
		status = http.StatusInternalServerError
	}
	return status, true
}

// normalizeWebsocketCompletion rewrites the WebSocket-only `response.done`
// terminator to the `response.completed` that the SSE handlers and usage parsing
// match on. Left alone, a stream would end with no usage recorded.
func normalizeWebsocketCompletion(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "response.done" {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "type", "response.completed")
	if err != nil || len(updated) == 0 {
		return payload
	}
	return updated
}

// isWebsocketTerminalEvent reports whether an event ends the turn.
//
// response.failed and response.incomplete are terminal too: upstream sends no
// further frames after them, so omitting either would leave the reader blocking
// until the idle deadline and turn a fast failure into a stall.
func isWebsocketTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

// encodeWebsocketFrameAsSSE wraps a frame payload as an SSE data line. The frame
// body is already exactly what an SSE `data:` line carries, so this is a framing
// change only — no re-encoding of the JSON.
func encodeWebsocketFrameAsSSE(payload []byte) []byte {
	line := make([]byte, 0, len("data: ")+len(payload)+2)
	line = append(line, "data: "...)
	line = append(line, payload...)
	line = append(line, '\n', '\n')
	return line
}

// websocketResponseBody is the synthetic response's Body: a pipe fed by the frame
// pump, tied to the socket that feeds it.
//
// Binding the two is the point. Every downstream handler closes resp.Body on all
// exit paths — normal completion, scanner error, client disconnect — and that is
// the only teardown signal this transport gets. If Close only closed the pipe, an
// abandoned stream would leak the socket and leave the pump blocked in ReadMessage
// until the idle deadline, holding an upstream connection per dropped client.
//
// Closing the pipe reader also unblocks the pump: its next write fails with
// io.ErrClosedPipe, so it returns on its own instead of needing to be signalled.
type websocketResponseBody struct {
	reader    *io.PipeReader
	conn      *websocket.Conn
	closeOnce sync.Once
}

func (b *websocketResponseBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *websocketResponseBody) Close() error {
	b.closeOnce.Do(func() {
		// Pipe first: it releases the reader and unblocks a pump parked on a write
		// before the socket close makes its in-flight read fail.
		_ = b.reader.Close()
		closeWebsocketConn(b.conn)
	})
	return nil
}

// synthesizeWebsocketResponse builds the *http.Response the relay pipeline expects.
// Nothing downstream inspects the transport, so a synthetic response with the right
// status, Content-Type and Body is indistinguishable from a real one.
func synthesizeWebsocketResponse(status int, contentType string, body io.ReadCloser) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", contentType)
	return &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Header:        header,
		Body:          body,
		ContentLength: -1,
	}
}

// streamWebsocketAsSSE turns the socket into an SSE response.
//
// The first frame is read synchronously, before any response is synthesized. That
// ordering is what lets an upstream rejection still become a real HTTP status: once
// a 200 and its headers are committed, an error can only be delivered as an event
// inside a stream the caller already believes succeeded.
func streamWebsocketAsSSE(conn *websocket.Conn) (*http.Response, error) {
	var first []byte
	for {
		msgType, payload, err := readWebsocketFrame(conn)
		if err != nil {
			closeWebsocketConn(conn)
			return nil, fmt.Errorf("read first websocket frame failed: %w", err)
		}
		// Control frames carry no events; gorilla handles ping/pong itself. A binary
		// frame is off-protocol here, but skipping is safer than failing the request.
		if msgType != websocket.TextMessage {
			continue
		}
		if payload = bytes.TrimSpace(payload); len(payload) == 0 {
			continue
		}
		first = payload
		break
	}

	if status, isErr := websocketFrameErrorStatus(first); isErr {
		closeWebsocketConn(conn)
		return synthesizeWebsocketResponse(status, "application/json",
			io.NopCloser(bytes.NewReader(first))), nil
	}

	reader, writer := io.Pipe()
	go pumpWebsocketToSSE(conn, writer, first)

	return synthesizeWebsocketResponse(http.StatusOK, "text/event-stream",
		&websocketResponseBody{reader: reader, conn: conn}), nil
}

// pumpWebsocketToSSE relays frames to the pipe as SSE lines until the turn ends.
//
// Owns the writer and closes it on every exit: the scanner downstream treats EOF as
// a normal end, so failing to close would leave it blocked until its own timeout.
// The socket is not closed here — websocketResponseBody.Close owns it, and closing
// it twice would race the reader.
func pumpWebsocketToSSE(conn *websocket.Conn, writer *io.PipeWriter, first []byte) {
	defer func() {
		if r := recover(); r != nil {
			_ = writer.CloseWithError(fmt.Errorf("websocket transport: pump panic: %v", r))
			return
		}
		_ = writer.Close()
	}()

	write := func(payload []byte) bool {
		payload = normalizeWebsocketCompletion(payload)
		if _, err := writer.Write(encodeWebsocketFrameAsSSE(payload)); err != nil {
			// The reader is gone (client disconnected, handler returned). Nothing to
			// report: Close already tore down the socket.
			return false
		}
		return !isWebsocketTerminalEvent(strings.TrimSpace(gjson.GetBytes(payload, "type").String()))
	}

	if !write(first) {
		return
	}

	for {
		msgType, payload, err := readWebsocketFrame(conn)
		if err != nil {
			// Surfaced through the pipe so the scanner ends as an error rather than a
			// clean EOF, which would bill a truncated stream as a complete one.
			_ = writer.CloseWithError(err)
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}
		if payload = bytes.TrimSpace(payload); len(payload) == 0 {
			continue
		}
		if !write(payload) {
			return
		}
	}
}

// collectWebsocketResponse drains the socket for a non-streaming caller.
//
// Upstream always streams events over WebSocket — `stream` is not part of the
// transport — so a non-streaming request is served by consuming the event stream
// here and handing back only the final response object. OaiResponsesHandler
// unmarshals the body as a bare response, so the `response` field is unwrapped from
// the terminal event rather than passing the event envelope through.
func collectWebsocketResponse(conn *websocket.Conn) (*http.Response, error) {
	defer closeWebsocketConn(conn)

	for {
		msgType, payload, err := readWebsocketFrame(conn)
		if err != nil {
			return nil, fmt.Errorf("read websocket frame failed: %w", err)
		}
		if msgType != websocket.TextMessage {
			continue
		}
		if payload = bytes.TrimSpace(payload); len(payload) == 0 {
			continue
		}

		if status, isErr := websocketFrameErrorStatus(payload); isErr {
			return synthesizeWebsocketResponse(status, "application/json",
				io.NopCloser(bytes.NewReader(payload))), nil
		}

		payload = normalizeWebsocketCompletion(payload)
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		if !isWebsocketTerminalEvent(eventType) {
			continue
		}

		body := payload
		if response := gjson.GetBytes(payload, "response"); response.Exists() {
			body = []byte(response.Raw)
		}
		resp := synthesizeWebsocketResponse(http.StatusOK, "application/json",
			io.NopCloser(bytes.NewReader(body)))
		resp.ContentLength = int64(len(body))
		return resp, nil
	}
}

// WebsocketAttempt is the outcome of one attempt at the WebSocket transport.
//
// Exactly one field is set. FallbackBody carries the buffered request bytes for the
// caller to replay over HTTP; it is set only when nothing was sent upstream, so
// replaying cannot duplicate a charged request.
type WebsocketAttempt struct {
	Response     *http.Response
	FallbackBody io.Reader
}

// TryResponsesWebsocket attempts to serve a /v1/responses request over the
// Responses API WebSocket transport.
//
// Callers gate on ShouldTryResponsesWebsocket first, then fall back to
// DoApiRequest with attempt.FallbackBody whenever Response is nil.
func TryResponsesWebsocket(a Adaptor, c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (*WebsocketAttempt, error) {
	// Buffered up front because io.Reader is single-read: a failed handshake has to
	// replay these exact bytes over HTTP, and by then the caller's reader is drained.
	// Cheap here — responses_handler already held this payload in memory to build it.
	body, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, fmt.Errorf("read request body for websocket transport failed: %w", err)
	}
	fallback := func() *WebsocketAttempt {
		return &WebsocketAttempt{FallbackBody: bytes.NewReader(body)}
	}

	httpURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	wsURL, err := buildResponsesWebsocketURL(httpURL)
	if err != nil {
		// A base URL this transport cannot express is a configuration shape the HTTP
		// path may still handle, so fall back rather than failing the request.
		logger.LogError(c, "websocket transport: unusable url, falling back to HTTP: "+err.Error())
		return fallback(), nil
	}

	header := make(http.Header)
	if err := a.SetupRequestHeader(c, &header, info); err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	// Same precedence as DoApiRequest: overrides win over adaptor defaults, so an
	// admin's Authorization or Host reaches the handshake too.
	headerOverride, err := processHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	for key, value := range headerOverride {
		header.Set(key, value)
	}
	stripHTTPOnlyHandshakeHeaders(header)
	applyResponsesWebsocketBetaHeader(header)

	// Bound to the caller's context so a client disconnect mid-handshake tears the
	// dial down instead of holding it to the handshake timeout.
	dialer := newResponsesWebsocketDialer(info)
	conn, handshakeResp, errDial := dialer.DialContext(c.Request.Context(), wsURL, header)
	if errDial != nil {
		status := 0
		if handshakeResp != nil {
			status = handshakeResp.StatusCode
		}
		closeHandshakeBody(handshakeResp)

		if isDefinitiveUpgradeRefusal(status) {
			service.MarkChannelWebsocketUnsupported(
				info.ChannelId,
				appcommon.GetContextKeyString(c, appconstant.ContextKeyChannelName),
				fmt.Sprintf("websocket upgrade rejected with HTTP %d", status),
			)
		}
		// Ambiguous failures — timeout, 5xx, 429, 401/403 — fall back for this
		// request only, leaving the switch to be retried next time.
		logger.LogError(c, fmt.Sprintf("websocket transport: handshake failed (status=%d), falling back to HTTP: %s", status, errDial.Error()))
		return fallback(), nil
	}
	closeHandshakeBody(handshakeResp)

	// Past this point the upstream has proven it speaks WebSocket, so no later
	// failure may set the unsupported flag or fall back: the request has been sent,
	// and replaying it over HTTP could bill the caller twice.
	wsBody, errSet := sjson.SetBytes(body, "type", "response.create")
	if errSet != nil {
		closeWebsocketConn(conn)
		return nil, fmt.Errorf("build websocket request failed: %w", errSet)
	}
	if err := conn.WriteMessage(websocket.TextMessage, wsBody); err != nil {
		closeWebsocketConn(conn)
		return nil, fmt.Errorf("send websocket request failed: %w", err)
	}

	if info.IsStream {
		resp, err := streamWebsocketAsSSE(conn)
		if err != nil {
			return nil, err
		}
		return &WebsocketAttempt{Response: resp}, nil
	}
	resp, err := collectWebsocketResponse(conn)
	if err != nil {
		return nil, err
	}
	return &WebsocketAttempt{Response: resp}, nil
}
