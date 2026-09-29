package channel

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Explicit Accept-Encoding disables net/http's automatic gzip decoding. CLI
// Messages advertise the encodings in the sample, so decode before the relay
// parses JSON or SSE. Readers remain streaming and close the upstream body.
type cliResponseBody struct {
	io.Reader
	closers []func() error
}

func (b *cliResponseBody) Close() error {
	var first error
	for i := len(b.closers) - 1; i >= 0; i-- {
		if err := b.closers[i](); first == nil {
			first = err
		}
	}
	return first
}

func decodeCLIResponse(resp *http.Response) error {
	encoding := resp.Header.Get("Content-Encoding")
	if encoding == "" || resp.Body == nil || resp.Body == http.NoBody {
		return nil
	}
	body := &cliResponseBody{Reader: resp.Body, closers: []func() error{resp.Body.Close}}
	// Decode stacked content codings in reverse application order.
	encodings := strings.Split(strings.ToLower(encoding), ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		var reader io.Reader
		var closeReader func() error
		var err error
		switch strings.TrimSpace(encodings[i]) {
		case "identity":
			continue
		case "gzip":
			var decoder *gzip.Reader
			decoder, err = gzip.NewReader(body.Reader)
			if err == nil {
				reader, closeReader = decoder, decoder.Close
			}
		case "deflate":
			var decoder io.ReadCloser
			decoder, err = zlib.NewReader(body.Reader)
			if err == nil {
				reader, closeReader = decoder, decoder.Close
			}
		case "br":
			reader = brotli.NewReader(body.Reader)
		case "zstd":
			var decoder *zstd.Decoder
			decoder, err = zstd.NewReader(body.Reader)
			if err == nil {
				reader = decoder
				closeReader = func() error { decoder.Close(); return nil }
			}
		default:
			err = fmt.Errorf("unsupported CLI response content encoding: %s", encoding)
		}
		if err != nil {
			_ = body.Close()
			return err
		}
		body.Reader = reader
		if closeReader != nil {
			body.closers = append(body.closers, closeReader)
		}
	}
	resp.Body = body
	resp.ContentLength = -1
	resp.Uncompressed = true
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}
