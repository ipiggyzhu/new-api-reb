package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// TLS ClientHello fingerprinting for channels that talk to upstreams which gate
// on the TLS handshake shape, not just HTTP headers. Opt-in per channel via
// ChannelSettings.TLSFingerprint, or implied by the channel's synthetic client
// header profile (see ResolveTLSFingerprint): a channel that presents itself as
// Claude Code or Codex at the HTTP layer gets that CLI's captured handshake at
// the TLS layer too. Channels with neither keep the default net/http transport
// and never touch this file.
//
// The claude-code and codex-cli entries replay handshakes captured from the real
// CLIs (tls_fingerprint_captures.go). The selectable catalog is CLI-only.

const http2NextProto = "h2"

// tlsFingerprintOption is one selectable ClientHello preset. Either helloID (a
// utls browser preset) or rawHello (a captured full TLS record, hex) drives the
// handshake: when rawHello is set, helloID is utls.HelloCustom and the captured
// ClientHello is reconstructed with a Fingerprinter and replayed via ApplyPreset.
type tlsFingerprintOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// ClientFamily is the synthetic client header profile this fingerprint is
	// the default for, so a channel that sends that CLI's headers also sends its
	// handshake unless the admin picks a fingerprint explicitly.
	ClientFamily   string `json:"client_family,omitempty"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	helloID        utls.ClientHelloID
	rawHello       string
}

// supportedTLSFingerprints is the CLI-only catalog returned to the admin UI.
//
// Every entry is one fixed handshake. There is deliberately no randomized
// preset: a channel should look like one real client, consistently.
var supportedTLSFingerprints = []tlsFingerprintOption{
	{ID: "claude-code", Label: "Claude Code 2.1.282 / Node 26.3.0 / Windows x64 (JSON)", ClientFamily: constant.ClientHeaderFamilyClaude, RuntimeVersion: "v26.3.0", helloID: utls.HelloCustom, rawHello: claudeCodeClientHello},
	{ID: "codex-cli", Label: "Codex exec 0.156.1 / Windows x64 (JSON)", ClientFamily: constant.ClientHeaderFamilyCodex, helloID: utls.HelloCustom, rawHello: codexCLIClientHello},
	{ID: "claude-node-22.14.0", Label: "Node 22.14.0 / OpenSSL 3.0.15+quic / Windows x64 (Claude Code 2.1.76)", RuntimeVersion: "v22.14.0", helloID: utls.HelloCustom, rawHello: claudeNode22140Hello},
	{ID: "claude-node-24.19.0", Label: "Node 24.19.0 / OpenSSL 3.5.7 / Windows x64 (Claude Code 2.1.76)", RuntimeVersion: "v24.19.0", helloID: utls.HelloCustom, rawHello: claudeNode24190Hello},
}

// Already saved browser overrides remain readable, but are no longer offered
// for selection. This avoids silently changing an existing channel on upgrade.
var legacyTLSFingerprints = []tlsFingerprintOption{
	{ID: "chrome", Label: "Chrome (latest)", helloID: utls.HelloChrome_Auto},
	{ID: "firefox", Label: "Firefox (latest)", helloID: utls.HelloFirefox_Auto},
	{ID: "safari", Label: "Safari (latest)", helloID: utls.HelloSafari_Auto},
	{ID: "edge", Label: "Edge (latest)", helloID: utls.HelloEdge_Auto},
	{ID: "ios", Label: "iOS Safari (latest)", helloID: utls.HelloIOS_Auto},
	{ID: "android", Label: "Android OkHttp", helloID: utls.HelloAndroid_11_OkHttp},
}

// SupportedTLSFingerprints returns the selectable presets for the admin UI.
func SupportedTLSFingerprints() []tlsFingerprintOption {
	return supportedTLSFingerprints
}

// ResolveTLSFingerprint keeps synthesis-off on the original Go transport,
// including channels with a stale saved override. Otherwise explicit TLS wins;
// without it, the selected CLI supplies its sample handshake.
func ResolveTLSFingerprint(explicit, clientFamily string) string {
	if clientFamily == "" || clientFamily == "off" {
		return ""
	}
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit
	}
	for _, opt := range supportedTLSFingerprints {
		if opt.ClientFamily == clientFamily {
			return opt.ID
		}
	}
	return ""
}

// TLSFingerprintRuntimeHeaders contains only the runtime declarations that
// exist in the Claude sample. Callers must not add these to Codex requests.
func TLSFingerprintRuntimeHeaders(fingerprint string) map[string]string {
	opt, ok := lookupTLSFingerprint(fingerprint)
	if !ok || opt.RuntimeVersion == "" {
		return nil
	}
	return map[string]string{
		"x-stainless-runtime": "node", "x-stainless-runtime-version": opt.RuntimeVersion,
		"x-stainless-os": "Windows", "x-stainless-arch": "x64",
	}
}

// lookupTLSFingerprint resolves a fingerprint id (case-insensitive, trimmed) to
// its option. It is the single source both the ClientHelloID and the captured
// raw-hello lookups build on.
func lookupTLSFingerprint(fingerprint string) (tlsFingerprintOption, bool) {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	for _, opt := range supportedTLSFingerprints {
		if opt.ID == fingerprint {
			return opt, true
		}
	}
	for _, opt := range legacyTLSFingerprints {
		if opt.ID == fingerprint {
			return opt, true
		}
	}
	return tlsFingerprintOption{}, false
}

func clientHelloIDForFingerprint(fingerprint string) (utls.ClientHelloID, bool) {
	opt, ok := lookupTLSFingerprint(fingerprint)
	if !ok {
		return utls.ClientHelloID{}, false
	}
	return opt.helloID, true
}

// IsSupportedTLSFingerprint reports whether name maps to a known preset. Empty
// means "no fingerprint" and is treated as supported (the default transport).
func IsSupportedTLSFingerprint(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	_, ok := clientHelloIDForFingerprint(name)
	return ok
}

var (
	fingerprintClientLock sync.Mutex
	fingerprintClients    = make(map[string]*http.Client)
)

// resetFingerprintClientCache drops cached fingerprint clients. Called by
// ResetProxyClientCache so a settings change (proxy list, insecure-skip-verify)
// rebuilds these the same way it rebuilds the proxy clients.
func resetFingerprintClientCache() {
	fingerprintClientLock.Lock()
	defer fingerprintClientLock.Unlock()
	fingerprintClients = make(map[string]*http.Client)
}

// GetFingerprintHTTPClient returns a client whose TLS ClientHello matches the
// named preset, dialing through proxyURL when set. Clients are cached per
// (fingerprint, proxy) pair. An unknown fingerprint is an error so a channel
// misconfiguration surfaces on the channel test rather than silently sending
// Go's default handshake.
func GetFingerprintHTTPClient(proxyURL, fingerprint string) (*http.Client, error) {
	opt, ok := lookupTLSFingerprint(fingerprint)
	if !ok {
		return nil, fmt.Errorf("unsupported tls fingerprint: %q", fingerprint)
	}
	var customHello []byte
	if opt.rawHello != "" {
		decoded, err := hex.DecodeString(opt.rawHello)
		if err != nil {
			return nil, fmt.Errorf("decode captured client hello for %q: %w", fingerprint, err)
		}
		customHello = decoded
	}
	if proxyURL != "" {
		if err := validateFingerprintProxyURL(proxyURL); err != nil {
			return nil, err
		}
	}

	key := fingerprint + "\x00" + proxyURL
	fingerprintClientLock.Lock()
	if client, ok := fingerprintClients[key]; ok {
		fingerprintClientLock.Unlock()
		return client, nil
	}
	fingerprintClientLock.Unlock()

	rt := &utlsRoundTripper{
		helloID:     opt.helloID,
		customHello: customHello,
		proxyURL:    proxyURL,
		insecure:    common.TLSInsecureSkipVerify,
	}
	// ConfigureTransports links a fresh http.Transport to an http2.Transport so
	// NewClientConn can drive HTTP/2 over the utls conn. A bare &http2.Transport{}
	// no longer works: since Go bundled HTTP/2 into net/http, NewClientConn
	// dereferences the linked http.Transport and panics without one. The linked
	// transport's ResponseHeaderTimeout is what the HTTP/2 conn honours.
	h1Transport := &http.Transport{}
	applyRelayTransportTimeouts(h1Transport)
	h2Transport, err := http2.ConfigureTransports(h1Transport)
	if err != nil {
		return nil, fmt.Errorf("configure http2 transport: %w", err)
	}
	h2Transport.DisableCompression = opt.rawHello != ""
	rt.h2 = h2Transport
	client := &http.Client{Transport: rt, CheckRedirect: checkRedirect}
	if common.RelayTimeout > 0 {
		client.Timeout = time.Duration(common.RelayTimeout) * time.Second
	}

	fingerprintClientLock.Lock()
	if existing, ok := fingerprintClients[key]; ok {
		fingerprintClientLock.Unlock()
		return existing, nil
	}
	fingerprintClients[key] = client
	fingerprintClientLock.Unlock()
	return client, nil
}

func validateFingerprintProxyURL(proxyURL string) error {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return err
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme: %s, must be http, https, socks5 or socks5h", parsed.Scheme)
	}
}

// utlsRoundTripper dials a fresh connection per request, performs the utls
// handshake with the selected ClientHello, then dispatches over HTTP/2 or
// HTTP/1.1 based on the negotiated ALPN protocol. It does not pool connections:
// every request pays a handshake, which is negligible against LLM request
// lifetimes and keeps the ClientHello identical on every dial.
type utlsRoundTripper struct {
	helloID utls.ClientHelloID
	// customHello, when non-nil, is a captured full TLS record (header+ClientHello)
	// reconstructed into a spec per dial and replayed via HelloCustom+ApplyPreset.
	// It is parsed fresh each handshake because a ClientHelloSpec holds mutable
	// extension state (key shares) that must not be shared across connections.
	customHello []byte
	proxyURL    string
	insecure    bool
	h2          *http2.Transport
}

// connClosingBody closes an extra resource (the HTTP/2 ClientConn) when the
// response body is closed, so the non-pooled connection and its read loop do not
// leak after a single request.
type connClosingBody struct {
	io.ReadCloser
	closer io.Closer
}

func (b *connClosingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.closer != nil {
		_ = b.closer.Close()
	}
	return err
}

// uTLS v1.8.2 always writes 0x0301 on the first record, independently of
// ApplyPreset. The Windows Codex capture uses 0x0303. Adjust only the plaintext
// record header, never the ClientHello transcript or later encrypted records.
type clientHelloRecordConn struct {
	net.Conn
	version [2]byte
	written int
}

func (c *clientHelloRecordConn) Write(p []byte) (int, error) {
	if c.written >= 3 {
		return c.Conn.Write(p)
	}
	p = bytes.Clone(p)
	for i := range p {
		position := c.written + i
		if position >= 3 {
			break
		}
		if position > 0 {
			p[i] = c.version[position-1]
		}
	}
	n, err := c.Conn.Write(p)
	c.written += n
	return n, err
}

func (rt *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, "https") {
		// No TLS layer to fingerprint: the ordinary client for this proxy sends
		// it, forwarding plain HTTP through a proxy instead of tunnelling.
		client, err := NewProxyHttpClient(rt.proxyURL)
		if err != nil {
			return nil, err
		}
		if client.Transport == nil {
			return http.DefaultTransport.RoundTrip(req)
		}
		return client.Transport.RoundTrip(req)
	}

	// The channel's own proxy, else HTTP_PROXY/HTTPS_PROXY/NO_PROXY like the
	// default relay client. nil dials directly.
	var proxyURL *url.URL
	var err error
	if rt.proxyURL != "" {
		proxyURL, err = url.Parse(rt.proxyURL)
	} else {
		proxyURL, err = http.ProxyFromEnvironment(req)
	}
	if err != nil {
		return nil, err
	}

	ctx := req.Context()
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)

	raw, err := rt.dialRaw(ctx, proxyURL, addr)
	if err != nil {
		return nil, err
	}

	if len(rt.customHello) >= 3 && rt.customHello[2] != 1 {
		raw = &clientHelloRecordConn{Conn: raw, version: [2]byte{rt.customHello[1], rt.customHello[2]}}
	}
	uconn := utls.UClient(raw, &utls.Config{ServerName: host, InsecureSkipVerify: rt.insecure}, rt.helloID)
	if rt.customHello != nil {
		// AllowBluntMimicry passes any extension utls doesn't natively model
		// through as raw bytes, so a captured hello reconstructs even if it
		// carries an extension (e.g. compress_certificate) utls can't rebuild.
		spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(rt.customHello)
		if err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("reconstruct captured client hello: %w", err)
		}
		if err := uconn.ApplyPreset(spec); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("apply captured client hello: %w", err)
		}
		// Fingerprinter omits the legacy session-ID length. ApplyPreset always
		// generates 32 bytes, but the Codex TLS 1.2 capture sends an empty ID.
		// Offset: record(5) + handshake(4) + version(2) + random(32).
		if len(rt.customHello) > 43 && rt.customHello[43] == 0 {
			uconn.HandshakeState.Hello.SessionId = nil
		}
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, relayTLSHandshakeTimeout())
	err = uconn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}

	if uconn.ConnectionState().NegotiatedProtocol == http2NextProto {
		cc, err := rt.h2.NewClientConn(uconn)
		if err != nil {
			_ = uconn.Close()
			return nil, err
		}
		resp, err := cc.RoundTrip(req)
		if err != nil {
			_ = cc.Close()
			return nil, err
		}
		resp.Body = &connClosingBody{ReadCloser: resp.Body, closer: cc}
		return resp, nil
	}

	return rt.roundTripOverConn(req, uconn)
}

// roundTripOverConn speaks HTTP/1.1 over a connection this round tripper already
// established, handing it to a throwaway std transport so gzip, chunked bodies,
// context cancellation and connection close are handled by net/http rather than
// reimplemented.
func (rt *utlsRoundTripper) roundTripOverConn(req *http.Request, conn net.Conn) (*http.Response, error) {
	// Not DisableKeepAlives: that makes net/http add "Connection: close", which
	// the real CLIs do not send, so the fingerprint would change the headers too.
	// A negative per-host idle limit refuses the conn back into the pool instead,
	// so it is still closed once the response body is.
	tr := &http.Transport{MaxIdleConnsPerHost: -1, DisableCompression: rt.customHello != nil}
	applyRelayTransportTimeouts(tr)

	handedOut := false
	tr.DialTLSContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		if handedOut {
			return nil, errors.New("utls http/1.1 connection already consumed")
		}
		handedOut = true
		return conn, nil
	}
	resp, err := tr.RoundTrip(req)
	if err != nil && !handedOut {
		_ = conn.Close()
	}
	return resp, err
}

// dialRaw opens a plain TCP connection to addr, through proxyURL when non-nil,
// mirroring the schemes NewProxyHttpClient supports.
func (rt *utlsRoundTripper) dialRaw(ctx context.Context, parsed *url.URL, addr string) (net.Conn, error) {
	if parsed == nil {
		return relayDialer.DialContext(ctx, "tcp", addr)
	}

	switch parsed.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if parsed.User != nil {
			auth = &proxy.Auth{User: parsed.User.Username()}
			if password, ok := parsed.User.Password(); ok {
				auth.Password = password
			}
		}
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, relayDialer)
		if err != nil {
			return nil, err
		}
		if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
			return ctxDialer.DialContext(ctx, "tcp", addr)
		}
		return dialer.Dial("tcp", addr)
	case "http", "https":
		return rt.dialHTTPProxyConnect(ctx, parsed, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", parsed.Scheme)
	}
}

// dialHTTPProxyConnect tunnels to addr through an HTTP(S) proxy via CONNECT.
func (rt *utlsRoundTripper) dialHTTPProxyConnect(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		defaultPort := "80"
		if proxyURL.Scheme == "https" {
			defaultPort = "443"
		}
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), defaultPort)
	}

	conn, err := relayDialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	// The CONNECT exchange reads and writes the raw conn, which the context
	// cannot interrupt; bound it like net/http bounds its own proxy setup.
	_ = conn.SetDeadline(time.Now().Add(relayTLSHandshakeTimeout()))
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), InsecureSkipVerify: rt.insecure})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credential := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+credential)
	}
	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s failed: %s", addr, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
