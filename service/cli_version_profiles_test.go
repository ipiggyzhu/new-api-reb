package service

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishedCLICapturesExcludeCapturedIdentities(t *testing.T) {
	captures := map[string]string{}
	for _, profile := range cliVersionProfiles {
		captures[profile.ID] = profile.ClientHelloHex
		for _, name := range []string{"authorization", "x-api-key", "cookie", "session-id", "x-session-id", "x-device-id", "chatgpt-account-id"} {
			assert.Empty(t, profile.Headers[name], "%s contains captured %s", profile.ID, name)
		}
	}
	for _, profile := range supportedTLSFingerprints {
		captures[profile.ID] = profile.rawHello
	}
	for name, encoded := range captures {
		t.Run(name, func(t *testing.T) {
			raw, err := hex.DecodeString(encoded)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(raw), 44)
			assert.Equal(t, make([]byte, 32), raw[11:43], "captured client random")
			size := int(raw[43])
			assert.Equal(t, make([]byte, size), raw[44:44+size], "captured TLS session ID")
			pos := 44 + size
			pos += 2 + int(binary.BigEndian.Uint16(raw[pos:pos+2]))
			pos += 1 + int(raw[pos])
			pos += 2
			for pos < len(raw) {
				require.GreaterOrEqual(t, len(raw)-pos, 4)
				kind := binary.BigEndian.Uint16(raw[pos : pos+2])
				n := int(binary.BigEndian.Uint16(raw[pos+2 : pos+4]))
				pos += 4
				require.GreaterOrEqual(t, len(raw)-pos, n)
				data := raw[pos : pos+n]
				pos += n
				switch kind {
				case 0:
					assert.Equal(t, append([]byte{0, 12, 0, 0, 9}, []byte("localhost")...), data, "captured upstream hostname")
				case 35, 41, 44:
					assert.Empty(t, data, "captured session ticket, PSK or cookie")
				case 51:
					for i := 2; i < len(data); {
						require.GreaterOrEqual(t, len(data)-i, 4)
						n := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
						i += 4
						require.GreaterOrEqual(t, len(data)-i, n)
						assert.Equal(t, make([]byte, n), data[i:i+n], "captured key-share bytes")
						i += n
					}
				}
			}
		})
	}
}

func TestCLIVersionProfilesAreFamilyBoundAndOptIn(t *testing.T) {
	counts := map[string]int{}
	for _, option := range SupportedCLIVersionProfiles() {
		counts[option.Family]++
		p, err := ResolveCLIVersionProfile(option.ID, option.Family, "", false)
		require.NoError(t, err)
		require.NotNil(t, p)
		_, err = ResolveCLIVersionProfile(option.ID, "openai", "", false)
		require.Error(t, err)
		_, err = ResolveCLIVersionProfile(option.ID, option.Family, "chrome", false)
		require.Error(t, err)
		_, err = ResolveCLIVersionProfile(option.ID, option.Family, "", true)
		require.Error(t, err)
		p, err = ResolveCLIVersionProfile(option.ID, "", "", false)
		require.NoError(t, err)
		assert.Nil(t, p)
	}
	assert.Equal(t, map[string]int{"claude": 7, "codex": 6}, counts)
	p, err := ResolveCLIVersionProfile("", "codex", "chrome", true)
	require.NoError(t, err)
	assert.Nil(t, p, "legacy configuration remains unchanged")
	_, err = ResolveCLIVersionProfile("codex-made-up", "codex", "", false)
	require.Error(t, err)
}

// A profile is one client build in one cache mode: the breakpoint TTL and the
// beta flag that enables it were captured together and must not drift apart.
func TestCLIVersionProfileCacheTTLMatchesCapturedBetaSet(t *testing.T) {
	const extendedTTLBeta = "extended-cache-ttl-2025-04-11"
	for _, p := range cliVersionProfiles {
		t.Run(p.ID, func(t *testing.T) {
			betas := strings.Split(p.Headers["anthropic-beta"], ",")
			if p.CacheTTL == "1h" {
				assert.Contains(t, betas, extendedTTLBeta)
				assert.JSONEq(t, `{"type":"ephemeral","ttl":"1h"}`, string(p.CacheControl()))
				assert.Equal(t, []string{"type", "ttl"}, p.BodyOrder["cache_control"])
				return
			}
			assert.Empty(t, p.CacheTTL)
			assert.NotContains(t, betas, extendedTTLBeta)
			assert.JSONEq(t, `{"type":"ephemeral"}`, string(p.CacheControl()))
		})
	}
	var unversioned *CLIVersionProfile
	assert.JSONEq(t, `{"type":"ephemeral"}`, string(unversioned.CacheControl()))
}

func TestCLIVersionProfilesActualTLSAndHTTPWire(t *testing.T) {
	old := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true // only this test's local self-signed server
	resetFingerprintClientCache()
	t.Cleanup(func() { common.TLSInsecureSkipVerify = old; resetFingerprintClientCache() })
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	config := seed.TLS.Clone()
	config.NextProtos = []string{"http/1.1"}
	seed.Close()
	for _, p := range cliVersionProfiles {
		t.Run(p.ID, func(t *testing.T) {
			rawListener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			hellos := make(chan []byte, 1)
			listener := tls.NewListener(&helloCaptureListener{Listener: rawListener, hellos: hellos}, config)
			defer listener.Close()
			type capture struct {
				head, body string
				err        error
			}
			captured := make(chan capture, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					captured <- capture{err: err}
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				var head bytes.Buffer
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						captured <- capture{err: err}
						return
					}
					head.WriteString(line)
					if line == "\r\n" {
						break
					}
				}
				request, err := http.ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader(head.String()), reader)))
				if err != nil {
					captured <- capture{err: err}
					return
				}
				body, err := io.ReadAll(request.Body)
				_ = request.Body.Close()
				_, writeErr := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
				captured <- capture{head.String(), string(body), errors.Join(err, writeErr)}
			}()
			_, port, err := net.SplitHostPort(listener.Addr().String())
			require.NoError(t, err)
			request, err := http.NewRequest(http.MethodPost, "https://localhost:"+port+"/v1/test", strings.NewReader(`{"opaque":9007199254740993}`))
			require.NoError(t, err)
			for _, name := range p.HeaderOrder {
				if strings.EqualFold(name, "host") || strings.EqualFold(name, "content-length") {
					continue
				}
				value, ok := p.Headers[strings.ToLower(name)]
				if !ok {
					value = "isolated-test-value"
				}
				request.Header.Set(name, value)
			}
			client, err := GetFingerprintHTTPClient("", p.ID, p.Family)
			require.NoError(t, err)
			tlsOnly, err := GetFingerprintHTTPClient("", p.ID)
			require.NoError(t, err)
			assert.NotSame(t, client, tlsOnly)
			response, err := client.Do(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			got := <-captured
			require.NoError(t, got.err)
			var names []string
			for _, line := range strings.Split(strings.TrimSuffix(got.head, "\r\n\r\n"), "\r\n")[1:] {
				name, _, ok := strings.Cut(line, ":")
				require.True(t, ok)
				names = append(names, name)
			}
			assert.Equal(t, p.HeaderOrder, names)
			assert.Equal(t, `{"opaque":9007199254740993}`, got.body)
			raw, err := hex.DecodeString(p.ClientHelloHex)
			require.NoError(t, err)
			assert.Equal(t, helloWireShape(t, raw), helloWireShape(t, <-hellos))
		})
	}
}
