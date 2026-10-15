package service

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
)

// Codex CLI 0.156.1 /v1/responses HTTP/1.1 capture. No authentication or
// identity values live here; net/http still supplies and validates all values.
var codexHeaderOrder = []string{"x-codex-beta-features", "x-codex-window-id", "x-codex-turn-metadata", "x-openai-internal-codex-responses-lite", "x-client-request-id", "session-id", "thread-id", "accept", "content-type", "authorization", "originator", "user-agent", "host", "content-length"}

// Only the first request head is reordered. net/http retains ownership of body
// framing, responses, deadlines and cancellation. The enclosing transport uses
// a fresh, non-pooled connection, so body/trailer bytes are never reinterpreted.
type codexHeaderOrderConn struct {
	net.Conn
	head  []byte
	sent  bool
	order []string
}

func (c *codexHeaderOrderConn) Write(p []byte) (int, error) {
	if c.sent {
		return c.Conn.Write(p)
	}
	c.head = append(c.head, p...)
	end := bytes.Index(c.head, []byte("\r\n\r\n"))
	const maxHeadBytes = 1 << 20
	if end < 0 {
		if len(c.head) > maxHeadBytes {
			return 0, fmt.Errorf("Codex request headers exceed %d bytes", maxHeadBytes)
		}
		return len(p), nil
	}
	if end > maxHeadBytes {
		return 0, fmt.Errorf("Codex request headers exceed %d bytes", maxHeadBytes)
	}
	lines := bytes.Split(c.head[:end], []byte("\r\n"))
	var out bytes.Buffer
	out.Write(lines[0])
	out.WriteString("\r\n")
	used := make([]bool, len(lines))
	order := c.order
	if order == nil {
		order = codexHeaderOrder
	}
	for _, name := range order {
		for i, line := range lines[1:] {
			key, value, found := bytes.Cut(line, []byte(":"))
			if found && strings.EqualFold(string(key), name) {
				out.WriteString(name)
				out.WriteByte(':')
				out.Write(value)
				out.WriteString("\r\n")
				used[i+1] = true
			}
		}
	}
	// Explicit administrator headers and transfer framing survive unchanged.
	for i, line := range lines[1:] {
		if !used[i+1] {
			out.Write(line)
			out.WriteString("\r\n")
		}
	}
	out.WriteString("\r\n")
	out.Write(c.head[end+4:])
	if _, err := io.Copy(c.Conn, &out); err != nil {
		return 0, err
	}
	c.sent = true
	c.head = nil
	return len(p), nil
}
