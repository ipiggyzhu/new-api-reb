package service

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type helloCaptureListener struct {
	net.Listener
	hellos chan []byte
}

func (l *helloCaptureListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &helloCaptureConn{Conn: conn, hellos: l.hellos}, nil
}

type helloCaptureConn struct {
	net.Conn
	hellos chan []byte
	first  []byte
	done   bool
}

func (c *helloCaptureConn) Read(p []byte) (int, error) {
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

// Compare wire fields, not just JA3 or a parsed cipher count. Dynamic random,
// session bytes, destination SNI and key material differ; their formats remain
// checked. Every other extension's payload and position must match the sample.
func helloWireShape(t *testing.T, raw []byte) []string {
	t.Helper()
	take := func(n int) []byte {
		require.GreaterOrEqual(t, len(raw), n)
		part := raw[:n]
		raw = raw[n:]
		return part
	}
	shape := []string{hex.EncodeToString(take(3))} // record type/version
	take(6)                                        // record/handshake lengths and type
	shape = append(shape, hex.EncodeToString(take(2)))
	take(32) // random
	sessionSize := take(1)[0]
	shape = append(shape, hex.EncodeToString([]byte{sessionSize}))
	take(int(sessionSize))
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
		case 0: // SNI changes with destination length.
			data = nil
		case 51: // retain group IDs and key lengths, zero fresh key material.
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

func TestCLIWireHelloMatchesCapture(t *testing.T) {
	for _, preset := range SupportedTLSFingerprints() {
		t.Run(preset.ID, func(t *testing.T) {
			require.NotEmpty(t, preset.rawHello, "only captured CLI presets are selectable")
			raw, err := hex.DecodeString(preset.rawHello)
			require.NoError(t, err)
			hellos := make(chan []byte, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Accept-Encoding", r.Header.Get("Accept-Encoding"))
				_, _ = io.Copy(w, r.Body)
			}))
			server.Listener = &helloCaptureListener{Listener: server.Listener, hellos: hellos}
			server.StartTLS()
			defer server.Close()
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(t, err)
			client := &http.Client{Timeout: 5 * time.Second, Transport: &utlsRoundTripper{
				helloID: utls.HelloCustom, customHello: raw, insecure: true,
			}}
			resp, err := client.Post("https://localhost:"+port, "text/plain", bytes.NewBufferString("capture"))
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, "capture", string(body))
			assert.Empty(t, resp.Header.Get("X-Accept-Encoding"), "transport must not add gzip absent from the Codex sample")
			assert.Equal(t, helloWireShape(t, raw), helloWireShape(t, <-hellos))
		})
	}
}
