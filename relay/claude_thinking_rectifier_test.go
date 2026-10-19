package relay

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIsClaudeThinkingHistoryRejected(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		want    bool
	}{
		{"cross-account signature", 400, "messages.1.content.0: Invalid `signature` in `thinking` block", true},
		{"block bound elsewhere", 400, "Invalid `signature` in `thinking` block. The block is bound to a different conversation", true},
		{"missing signature", 400, "messages.1.content.0.thinking.signature: Field required", true},
		{"modified latest turn", 400, "thinking or redacted_thinking blocks in the latest assistant message cannot be modified", true},
		{"tool loop without thinking", 400, "Expected `thinking` or `redacted_thinking`, but found `tool_use`", true},
		{"final assistant prefill", 400, "a final `assistant` message must start with a thinking block", true},
		{"empty thinking block", 400, "messages.1.content.0.thinking: each thinking block must contain thinking", true},
		{"malformed budget is the client's error", 400, "thinking.budget_tokens: Input should be a valid integer, expected integer", false},
		{"aws sigv4 says signature without thinking", 400, "The request signature we calculated does not match the signature you provided", false},
		{"unrelated 400", 400, "max_tokens: 99999999 > 128000, which is the maximum allowed", false},
		{"not a 400", 500, "Invalid `signature` in `thinking` block", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewOpenAIError(errors.New(tc.message), types.ErrorCodeBadResponseStatusCode, tc.status)
			assert.Equal(t, tc.want, isClaudeThinkingHistoryRejected(err))
		})
	}
	assert.False(t, isClaudeThinkingHistoryRejected(nil))
}

func TestDowngradeClaudeThinkingHistory(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		withTools bool
		want      string
	}{
		{
			name: "thinking becomes text, redacted and empty dropped, enabled thinking removed",
			body: `{"model":"m","thinking":{"type":"enabled","budget_tokens":2048},"messages":[` +
				`{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"s1"},{"type":"redacted_thinking","data":"x"},{"type":"thinking","thinking":"  ","signature":"s2"},{"type":"text","text":"answer"}]},` +
				`{"role":"user","content":[{"type":"text","text":"next"}]}]}`,
			want: `{"model":"m","messages":[` +
				`{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":[{"type":"text","text":"plan"},{"type":"text","text":"answer"}]},` +
				`{"role":"user","content":[{"type":"text","text":"next"}]}]}`,
		},
		{
			name: "adaptive removed with its clear_thinking edits, other edits kept",
			body: `{"thinking":{"type":"adaptive"},"context_management":{"edits":[{"type":"clear_thinking_20251015"},{"type":"clear_tool_uses_20250919"}]},"messages":[]}`,
			want: `{"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},"messages":[]}`,
		},
		{
			name: "only clear_thinking edits leaves no edits",
			body: `{"thinking":{"type":"adaptive"},"context_management":{"edits":[{"type":"clear_thinking_20251015"}]},"messages":[]}`,
			want: `{"messages":[]}`,
		},
		{
			name: "disabled thinking is the client's choice and stays",
			body: `{"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"}]}]}`,
			want: `{"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[{"type":"text","text":"t"}]}]}`,
		},
		{
			name: "assistant left empty gets a placeholder",
			body: `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}]}`,
			want: `{"messages":[{"role":"assistant","content":[{"type":"text","text":"(assistant content removed)"}]}]}`,
		},
		{
			name: "tool blocks untouched in the first stage",
			body: `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"tu1","name":"ls","input":{"path":"."}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"a.go"}]}]}`,
			want: `{"messages":[{"role":"assistant","content":[{"type":"text","text":"t"},{"type":"tool_use","id":"tu1","name":"ls","input":{"path":"."}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"a.go"}]}]}`,
		},
		{
			name:      "tool blocks become text, cache breakpoint and images kept",
			withTools: true,
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"ls","input":{"path":"."}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","is_error":true,"cache_control":{"type":"ephemeral"},"content":[{"type":"text","text":"denied"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`,
			want: `{"messages":[{"role":"assistant","content":[{"type":"text","text":"(tool_use) name=ls id=tu1 input={\"path\":\".\"}"}]},` +
				`{"role":"user","content":[{"type":"text","text":"(tool_result) tool_use_id=tu1 is_error=true\ndenied","cache_control":{"type":"ephemeral"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`,
		},
		{
			name:      "string tool result joins the header",
			withTools: true,
			body:      `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"a.go"}]}]}`,
			want:      `{"messages":[{"role":"user","content":[{"type":"text","text":"(tool_result) tool_use_id=tu1\na.go"}]}]}`,
		},
		{
			name: "nothing to rewrite returns the same bytes",
			body: `{"thinking":{"type":"disabled"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: `{"thinking":{"type":"disabled"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.JSONEq(t, tc.want, string(downgradeClaudeThinkingHistory([]byte(tc.body), tc.withTools)))
		})
	}

	unchanged := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, unchanged, downgradeClaudeThinkingHistory(unchanged, true))
}

// thinkingRectifierTestAdaptor answers like an upstream that cannot verify the
// replayed history: thinking blocks fail the signature check, and tool blocks
// without a leading thinking block fail the tool-loop rule.
type thinkingRectifierTestAdaptor struct {
	channel.Adaptor
	bodies       [][]byte
	rejectTools  bool
	otherMessage string
}

func (a *thinkingRectifierTestAdaptor) DoRequest(_ *gin.Context, _ *relaycommon.RelayInfo, body io.Reader) (any, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	a.bodies = append(a.bodies, data)
	message := ""
	switch {
	case a.otherMessage != "":
		message = a.otherMessage
	case bytes.Contains(data, []byte(`"type":"thinking"`)):
		message = "messages.1.content.0: Invalid `signature` in `thinking` block"
	case a.rejectTools && bytes.Contains(data, []byte(`"type":"tool_use"`)):
		message = "messages.1.content.0.type: Expected `thinking` or `redacted_thinking`, but found `tool_use`"
	}
	if message == "" {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	errorBody := `{"type":"error","error":{"type":"invalid_request_error","message":"` + message + `"}}`
	return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(errorBody))}, nil
}

func (a *thinkingRectifierTestAdaptor) DoResponse(_ *gin.Context, resp *http.Response, _ *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	_ = resp.Body.Close()
	return &dto.Usage{TotalTokens: 1}, nil
}

func TestRectifyClaudeThinkingHistory(t *testing.T) {
	const history = `{"model":"m","thinking":{"type":"adaptive"},"messages":[` +
		`{"role":"user","content":"list"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"other-account"},{"type":"tool_use","id":"tu1","name":"ls","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"a.go"}]}]}`
	cases := []struct {
		name         string
		adaptor      *thinkingRectifierTestAdaptor
		written      bool
		wantRequests int
		wantStatus   int
	}{
		{name: "thinking as text is enough", adaptor: &thinkingRectifierTestAdaptor{}, wantRequests: 2},
		{name: "tool loop needs the second stage", adaptor: &thinkingRectifierTestAdaptor{rejectTools: true}, wantRequests: 3},
		{name: "unrelated 400 is not retried", adaptor: &thinkingRectifierTestAdaptor{otherMessage: "max_tokens: too large"}, wantRequests: 1, wantStatus: http.StatusBadRequest},
		{name: "nothing retried once the client got bytes", adaptor: &thinkingRectifierTestAdaptor{}, written: true, wantRequests: 1, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.written {
				c.Writer.WriteHeaderNow()
			}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7}}

			usage, apiErr := doUpstreamRoundTrip(c, info, tc.adaptor, bytes.NewReader([]byte(history)), true)
			usage, apiErr = rectifyClaudeThinkingHistory(c, info, tc.adaptor, []byte(history), usage, apiErr)

			require.Len(t, tc.adaptor.bodies, tc.wantRequests)
			if tc.wantStatus != 0 {
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.wantStatus, apiErr.StatusCode)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, &dto.Usage{TotalTokens: 1}, usage)
			last := tc.adaptor.bodies[len(tc.adaptor.bodies)-1]
			assert.False(t, gjson.GetBytes(last, "thinking").Exists())
			assert.Equal(t, int64(len(last)), info.UpstreamRequestBodySize)
		})
	}
}
