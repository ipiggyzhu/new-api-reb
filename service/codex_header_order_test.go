package service

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type headerOrderTestConn struct {
	net.Conn
	output bytes.Buffer
	err    error
}

func (c *headerOrderTestConn) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.output.Write(p)
}

func TestCodexHeaderOrderRetainsFramingAndOpaqueBody(t *testing.T) {
	const head = "POST /v1/responses HTTP/1.1\r\nHost: upstream.test\r\nUser-Agent: cli\r\nTransfer-Encoding: chunked\r\nAuthorization: Bearer test\r\nX-Extra: one\r\nX-Extra: two\r\nAccept: text/event-stream\r\n\r\n"
	const body = "3\r\n\x00\r\n\r\n0\r\nX-Trailer: retained\r\n\r\n"
	const expected = "POST /v1/responses HTTP/1.1\r\naccept: text/event-stream\r\nauthorization: Bearer test\r\nuser-agent: cli\r\nhost: upstream.test\r\nTransfer-Encoding: chunked\r\nX-Extra: one\r\nX-Extra: two\r\n\r\n" + body
	for _, writes := range [][]string{{head + body}, {head[:9], head[9 : len(head)-1], head[len(head)-1:] + body[:4], body[4:]}} {
		raw := &headerOrderTestConn{}
		conn := &codexHeaderOrderConn{Conn: raw}
		for _, part := range writes {
			n, err := conn.Write([]byte(part))
			require.NoError(t, err)
			assert.Equal(t, len(part), n)
		}
		assert.Equal(t, expected, raw.output.String())
	}
	raw := &headerOrderTestConn{err: io.ErrClosedPipe}
	_, err := (&codexHeaderOrderConn{Conn: raw}).Write([]byte(head))
	assert.ErrorIs(t, err, io.ErrClosedPipe)
}

func TestCodexHeaderOrderOverTLSUsesCaptureOrderAndChannelAuth(t *testing.T) {
	old := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true
	resetFingerprintClientCache()
	t.Cleanup(func() { common.TLSInsecureSkipVerify = old; resetFingerprintClientCache() })
	seed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	config := seed.TLS.Clone()
	config.NextProtos = []string{"http/1.1"}
	seed.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	require.NoError(t, err)
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
	client, err := GetFingerprintHTTPClient("", "codex-cli", constant.ClientHeaderFamilyCodex)
	require.NoError(t, err)
	unshaped, err := GetFingerprintHTTPClient("", "codex-cli")
	require.NoError(t, err)
	assert.NotSame(t, client, unshaped, "TLS-only clients must not inherit a header profile from the cache")
	request, err := http.NewRequest(http.MethodPost, "https://"+listener.Addr().String()+"/v1/responses", strings.NewReader(`{"input":[]}`))
	require.NoError(t, err)
	for _, name := range codexHeaderOrder {
		if name != "host" && name != "content-length" {
			request.Header.Set(name, "sample-value")
		}
	}
	request.Header.Set("Authorization", "Bearer channel-test-key")
	resp, err := client.Do(request)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	got := <-captured
	require.NoError(t, got.err)
	var names []string
	for _, line := range strings.Split(strings.TrimSuffix(got.head, "\r\n\r\n"), "\r\n")[1:] {
		name, _, ok := strings.Cut(line, ":")
		require.True(t, ok)
		names = append(names, name)
	}
	assert.Equal(t, codexHeaderOrder, names)
	assert.Contains(t, got.head, "authorization: Bearer channel-test-key\r\n")
	assert.Equal(t, `{"input":[]}`, got.body)
}
