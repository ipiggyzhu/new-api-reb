package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A CLI request's conversation picks its key on a multi-key channel and the
// session its upstream identity wears, so every client shape that names one
// must yield it, and a request that names none must yield nothing.
func TestCLIConversationSeed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		body   string
		want   string
	}{
		{"codex session header", http.Header{"Session-Id": {"thread-1"}}, `{"prompt_cache_key":"cache-1"}`, "thread-1"},
		{"claude code session header", http.Header{"X-Claude-Code-Session-Id": {"session-1"}}, `{}`, "session-1"},
		{"json user_id", nil, `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"account_uuid\":\"\",\"session_id\":\"s1\"}"}}`, "s1"},
		{"legacy user_id", nil, `{"metadata":{"user_id":"user_d1_account__session_s1"}}`, "s1"},
		{"legacy user_id with account", nil, `{"metadata":{"user_id":"user_d1_account_a1_session_s1"}}`, "s1"},
		{"other client's user_id", nil, `{"metadata":{"user_id":"user_42_session_s1"}}`, ""},
		{"no conversation", nil, `{"metadata":{"user_id":"user-42"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CLIConversationSeed(tc.header, []byte(tc.body)))
		})
	}
}
