package channel

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLICompressedUpstreamResponses(t *testing.T) {
	service.InitHttpClient()
	for _, encoding := range []string{"gzip", "deflate", "br", "zstd", "gzip, br"} {
		t.Run(encoding, func(t *testing.T) {
			payload := []byte("event: error\ndata: {\"error\":\"upstream unavailable\"}\n\n")
			compressed := payload
			for _, coding := range strings.Split(encoding, ", ") {
				var buffer bytes.Buffer
				var writer io.WriteCloser
				switch coding {
				case "gzip":
					writer = gzip.NewWriter(&buffer)
				case "deflate":
					writer = zlib.NewWriter(&buffer)
				case "br":
					writer = brotli.NewWriter(&buffer)
				case "zstd":
					var err error
					writer, err = zstd.NewWriter(&buffer)
					require.NoError(t, err)
				}
				_, err := writer.Write(compressed)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				compressed = bytes.Clone(buffer.Bytes())
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", encoding)
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write(compressed)
			}))
			defer server.Close()
			c, info := cliTestRequest("claude")
			resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(`{"messages":[]}`))
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, body)
			assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
			assert.Empty(t, resp.Header.Get("Content-Encoding"))
			assert.EqualValues(t, -1, resp.ContentLength)
			assert.True(t, resp.Uncompressed)
		})
	}
}

type countedCLIResponseBody struct {
	io.Reader
	closed int
}

func (b *countedCLIResponseBody) Close() error { b.closed++; return nil }

func TestCLIResponseDecodingClosesTheUpstream(t *testing.T) {
	for _, encoding := range []string{"gzip", "br", "unknown"} {
		body := &countedCLIResponseBody{Reader: strings.NewReader("invalid compressed data")}
		resp := &http.Response{Header: http.Header{"Content-Encoding": []string{encoding}}, Body: body}
		err := decodeCLIResponse(resp)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, resp.Body.Close())
		}
		require.Error(t, err)
		assert.Equal(t, 1, body.closed)
	}
}
