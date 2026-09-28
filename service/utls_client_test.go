package service

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientHelloIDForFingerprint(t *testing.T) {
	_, ok := clientHelloIDForFingerprint("chrome")
	assert.True(t, ok, "chrome must map to a ClientHelloID")

	_, ok = clientHelloIDForFingerprint("CHROME")
	assert.True(t, ok, "lookup must be case-insensitive")

	_, ok = clientHelloIDForFingerprint("  firefox ")
	assert.True(t, ok, "lookup must trim whitespace")

	_, ok = clientHelloIDForFingerprint("nope")
	assert.False(t, ok, "unknown fingerprint must not map")
}

func TestIsSupportedTLSFingerprint(t *testing.T) {
	assert.True(t, IsSupportedTLSFingerprint(""), "empty means default transport, treated as supported")
	assert.True(t, IsSupportedTLSFingerprint("chrome"))
	assert.True(t, IsSupportedTLSFingerprint("claude-code"), "the captured Claude Code preset must be selectable")
	assert.True(t, IsSupportedTLSFingerprint("codex-cli"), "the captured Codex preset must be selectable")
	assert.False(t, IsSupportedTLSFingerprint("randomized"), "a per-connection random handshake is not offered")
	assert.False(t, IsSupportedTLSFingerprint("mystery-client"))
}

// TestResolveTLSFingerprint pins the channel contract: an explicit fingerprint
// wins, otherwise the CLI header profile brings its own CLI's handshake, and
// anything else stays on the default transport.
func TestResolveTLSFingerprint(t *testing.T) {
	cases := []struct {
		name, explicit, family, want string
	}{
		{"claude headers follow claude handshake", "", constant.ClientHeaderFamilyClaude, "claude-code"},
		{"codex headers follow codex handshake", "", constant.ClientHeaderFamilyCodex, "codex-cli"},
		{"explicit overrides the header family", "chrome", constant.ClientHeaderFamilyClaude, "chrome"},
		{"explicit applies without a header profile", "codex-cli", "", "codex-cli"},
		{"header profile off keeps default transport", "", "", ""},
		{"family without a captured cli keeps default transport", "", constant.ClientHeaderFamilyOpenAI, ""},
		{"blank explicit falls back to the family", "  ", constant.ClientHeaderFamilyCodex, "codex-cli"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ResolveTLSFingerprint(tc.explicit, tc.family))
		})
	}
}

func TestGetFingerprintHTTPClientErrors(t *testing.T) {
	_, err := GetFingerprintHTTPClient("", "does-not-exist")
	require.Error(t, err, "unknown fingerprint must error rather than silently fall back")

	_, err = GetFingerprintHTTPClient("ftp://proxy:1080", "chrome")
	require.Error(t, err, "unsupported proxy scheme must error")
}

func TestGetFingerprintHTTPClientCachesPerKey(t *testing.T) {
	resetFingerprintClientCache()

	first, err := GetFingerprintHTTPClient("", "chrome")
	require.NoError(t, err)
	second, err := GetFingerprintHTTPClient("", "chrome")
	require.NoError(t, err)
	assert.Same(t, first, second, "same (fingerprint, proxy) must return the cached client")

	other, err := GetFingerprintHTTPClient("", "firefox")
	require.NoError(t, err)
	assert.NotSame(t, first, other, "a different fingerprint must be a different client")
}

// newFingerprintTestClient builds a client around the utls round tripper with
// certificate verification off, so it can talk to httptest's self-signed server.
func newFingerprintTestClient() *http.Client {
	h2Transport, err := http2.ConfigureTransports(&http.Transport{})
	if err != nil {
		panic(err)
	}
	rt := &utlsRoundTripper{
		helloID:  utls.HelloChrome_Auto,
		insecure: true,
		h2:       h2Transport,
	}
	return &http.Client{Transport: rt}
}

func TestUtlsRoundTripperOverHTTP2(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Proto", r.Proto)
		_, _ = w.Write(body)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	resp, err := newFingerprintTestClient().Post(srv.URL, "text/plain", bytes.NewBufferString("ping"))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "HTTP/2.0", resp.Proto, "ALPN must negotiate h2 against an HTTP/2 server")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(body), "request body must reach upstream and response body must be read back")
}

func TestUtlsRoundTripperOverHTTP1(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Request-Connection", r.Header.Get("Connection"))
		_, _ = w.Write(body)
	})
	srv := httptest.NewTLSServer(handler) // HTTP/1.1 only
	defer srv.Close()

	resp, err := newFingerprintTestClient().Post(srv.URL, "text/plain", bytes.NewBufferString("pong"))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "HTTP/1.1", resp.Proto, "an HTTP/1.1-only server must fall through to the h1 path")
	assert.Empty(t, resp.Header.Get("X-Request-Connection"), "a fingerprint changes only TLS, so no Connection: close may be added")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(body))
}

// TestClaudeCodeCapturedHello proves the shipped Claude Code capture both
// reconstructs into the expected handshake shape and completes a real TLS
// handshake carrying a request, i.e. the HelloCustom+ApplyPreset replay path
// works end to end, not just that the bytes parse.
func TestClaudeCodeCapturedHello(t *testing.T) {
	raw, err := hex.DecodeString(claudeCodeClientHello)
	require.NoError(t, err)

	spec, err := (&utls.Fingerprinter{}).FingerprintClientHello(raw)
	require.NoError(t, err, "captured Claude Code hello must parse without blunt mimicry")
	assert.Len(t, spec.CipherSuites, 17, "Claude Code offers 17 cipher suites")
	var alpn []string
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			alpn = a.AlpnProtocols
		}
	}
	assert.Equal(t, []string{"http/1.1"}, alpn, "Claude Code offers only http/1.1 in ALPN")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
	srv := httptest.NewTLSServer(handler) // HTTP/1.1 only, matching the ALPN offer
	defer srv.Close()

	h2Transport, err := http2.ConfigureTransports(&http.Transport{})
	require.NoError(t, err)
	rt := &utlsRoundTripper{
		helloID:     utls.HelloCustom,
		customHello: raw,
		insecure:    true,
		h2:          h2Transport,
	}
	client := &http.Client{Transport: rt}
	resp, err := client.Post(srv.URL, "text/plain", bytes.NewBufferString("claude"))
	require.NoError(t, err, "captured Claude Code hello must complete a real handshake")
	defer resp.Body.Close()

	assert.Equal(t, "HTTP/1.1", resp.Proto)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "claude", string(got))
}

// TestCodexCapturedHello does the same for the Codex capture, which is a TLS
// 1.2-only SChannel hello with no ALPN: it must still complete a real handshake
// against a server that also speaks TLS 1.3, and fall through to HTTP/1.1.
func TestCodexCapturedHello(t *testing.T) {
	raw, err := hex.DecodeString(codexCLIClientHello)
	require.NoError(t, err)

	spec, err := (&utls.Fingerprinter{}).FingerprintClientHello(raw)
	require.NoError(t, err, "captured Codex hello must parse without blunt mimicry")
	assert.Len(t, spec.CipherSuites, 18, "Codex on Windows offers 18 cipher suites")
	assert.Equal(t, uint16(utls.VersionTLS12), spec.TLSVersMax, "SChannel via native-tls caps at TLS 1.2")
	for _, ext := range spec.Extensions {
		_, isALPN := ext.(*utls.ALPNExtension)
		assert.False(t, isALPN, "Codex on Windows sends no ALPN")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-TLS-Version", fmt.Sprintf("%x", r.TLS.Version))
		_, _ = w.Write(body)
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()

	h2Transport, err := http2.ConfigureTransports(&http.Transport{})
	require.NoError(t, err)
	rt := &utlsRoundTripper{
		helloID:     utls.HelloCustom,
		customHello: raw,
		insecure:    true,
		h2:          h2Transport,
	}
	resp, err := (&http.Client{Transport: rt}).Post(srv.URL, "text/plain", bytes.NewBufferString("codex"))
	require.NoError(t, err, "captured Codex hello must complete a real handshake")
	defer resp.Body.Close()

	assert.Equal(t, "HTTP/1.1", resp.Proto)
	assert.Equal(t, "303", resp.Header.Get("X-TLS-Version"), "the handshake must stay on TLS 1.2 as captured")
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "codex", string(got))
}
