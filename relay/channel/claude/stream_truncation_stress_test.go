package claude

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

type truncationCase struct {
	name           string
	body           string
	wantTruncated  bool
	wantRelayError bool
}

// Concurrency load over the real handler, mixing stream shapes that must reach
// opposite verdicts.
//
// StreamStatus is written by three goroutines inside StreamScannerHandler (the
// scanner, the ping loop, the data handler) and read by the verdict layer after
// it returns, so the truncation flag shares its mutex with the soft-error list.
// The risk this covers is not throughput: it is a per-request verdict being lost
// or leaking across concurrent relays, which under load would look like random
// channel demotions on healthy traffic. Each case therefore carries its own
// expected verdict and every goroutine asserts only its own.
//
// Run under -race for this to test what it is for.
func TestClaudeStreamHandler_ConcurrentVerdictsStayIsolated(t *testing.T) {
	t.Parallel()

	cases := []truncationCase{
		{
			name:          "truncated after text",
			body:          claudeMessageStart + claudeTextDelta,
			wantTruncated: true,
		},
		{
			name: "complete message",
			body: claudeMessageStart + claudeTextDelta + claudeMessageDelta + claudeMessageStop,
		},
		{
			name: "closed by message_delta only",
			body: claudeMessageStart + claudeTextDelta + claudeMessageDelta,
		},
		{
			// Either terminating frame is enough on its own. This one carries no
			// usage, so it also covers the local-estimate fallback path.
			name: "closed by message_stop only",
			body: claudeMessageStart + claudeTextDelta + claudeMessageStop,
		},
		{
			name:           "no content at all",
			body:           claudeMessageStart,
			wantRelayError: true,
		},
		{
			name: "truncated mid tool call",
			body: claudeMessageStart +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\n",
			wantTruncated: true,
		},
		{
			name:          "truncated after a long body",
			body:          claudeMessageStart + strings.Repeat(claudeTextDelta, 200),
			wantTruncated: true,
		},
	}

	const roundsPerCase = 40

	var wg sync.WaitGroup
	for _, tc := range cases {
		for round := 0; round < roundsPerCase; round++ {
			wg.Add(1)
			go func(tc truncationCase, round int) {
				defer wg.Done()

				c, resp, info := newClaudeStreamTest(t, tc.body)
				usage, apiErr := ClaudeStreamHandler(c, resp, info)

				label := fmt.Sprintf("%s round %d", tc.name, round)

				if tc.wantRelayError {
					if apiErr == nil {
						t.Errorf("%s: an upstream that answered nothing must fail the relay", label)
					}
					return
				}
				if apiErr != nil {
					t.Errorf("%s: unexpected relay error %v", label, apiErr)
					return
				}
				if usage == nil {
					t.Errorf("%s: delivered content must still report usage", label)
					return
				}
				if got := info.StreamStatus.MissingTerminator(); got != tc.wantTruncated {
					t.Errorf("%s: MissingTerminator = %v, want %v", label, got, tc.wantTruncated)
				}
				if got := info.StreamStatus.IsNormalEnd(); got != !tc.wantTruncated {
					t.Errorf("%s: IsNormalEnd = %v, want %v", label, got, !tc.wantTruncated)
				}
				if failure := info.StreamStatus.FailureError(); (failure != nil) != tc.wantTruncated {
					t.Errorf("%s: FailureError present = %v, want %v", label, failure != nil, tc.wantTruncated)
				}
			}(tc, round)
		}
	}
	wg.Wait()
}

// The scanner forwards each frame to the data handler while the verdict is read
// after it returns. A body split across many small reads exercises that handoff
// without changing the shape of the message, so the truncation verdict must come
// out identical however the bytes arrive.
func TestClaudeStreamHandler_TruncationVerdictIndependentOfChunking(t *testing.T) {
	t.Parallel()

	body := claudeMessageStart + strings.Repeat(claudeTextDelta, 50)

	for _, chunk := range []int{1, 7, 64, 4096} {
		c, resp, info := newClaudeStreamTest(t, "")
		resp.Body = io.NopCloser(&chunkedReader{data: body, chunk: chunk})

		usage, apiErr := ClaudeStreamHandler(c, resp, info)

		if apiErr != nil {
			t.Fatalf("chunk=%d: unexpected relay error %v", chunk, apiErr)
		}
		if usage == nil {
			t.Fatalf("chunk=%d: expected usage", chunk)
		}
		if !info.StreamStatus.MissingTerminator() {
			t.Errorf("chunk=%d: a message with no message_delta must be recorded as truncated", chunk)
		}
		if info.StreamStatus.IsNormalEnd() {
			t.Errorf("chunk=%d: a truncated message must not read as a normal end", chunk)
		}
	}
}

// chunkedReader hands out at most chunk bytes per Read, so the scanner has to
// reassemble SSE frames across reads the way a real socket makes it.
type chunkedReader struct {
	data  string
	chunk int
	pos   int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	end := r.pos + r.chunk
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}
