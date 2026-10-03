package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type highEffortFallbackHTTPAdaptor struct {
	reasoningFallbackTestAdaptor
	url string
}

func (a *highEffortFallbackHTTPAdaptor) GetRequestURL(*relaycommon.RelayInfo) (string, error) {
	return a.url, nil
}

func (a *highEffortFallbackHTTPAdaptor) SetupRequestHeader(_ *gin.Context, header *http.Header, _ *relaycommon.RelayInfo) error {
	header.Set("x-codex-turn-metadata", `{"reasoning_effort":"ultra","session_id":"existing-thread"}`)
	return nil
}

func (a *highEffortFallbackHTTPAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, reader io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, reader)
}

func TestResponsesReasoningFallbackWithFixedHigh(t *testing.T) {
	service.InitHttpClient()
	type capture struct {
		body   []byte
		header http.Header
		err    error
	}
	captures := make(chan capture, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captures <- capture{body, r.Header.Clone(), err}
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte(`"encrypted_content":"old"`)) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_encrypted_content"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"reply","reasoning":{"effort":"high"},"output":[]}`))
	}))
	defer server.Close()
	const original = `{"model":"gpt-6-astra","reasoning":{"effort":"xhigh","context":"all_turns"},"client_metadata":{"x-codex-turn-metadata":"{\"reasoning_effort\":\"ultra\",\"session_id\":\"existing-thread\"}"},"input":[{"role":"user","content":"Keep the whole conversation."},{"type":"reasoning","id":"old-id","encrypted_content":"old","summary":[{"type":"summary_text","text":"Keep this plan."}]},{"type":"function_call","name":"read","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"Keep this result."}]}`
	c, info := reasoningFallbackFixture(t, original)
	info.ChannelSetting.ResponsesForceHighEffort = true
	a := &highEffortFallbackHTTPAdaptor{url: server.URL + "/v1/responses"}
	_, apiErr := doUpstreamRoundTrip(c, info, a, strings.NewReader(original), false)
	require.Nil(t, apiErr)
	first, retry := <-captures, <-captures
	require.NoError(t, first.err)
	require.NoError(t, retry.err)
	assert.Contains(t, string(first.body), `"encrypted_content":"old"`)
	assert.NotContains(t, string(retry.body), `"encrypted_content":"old"`)
	for _, got := range []capture{first, retry} {
		assert.Equal(t, "high", gjson.GetBytes(got.body, "reasoning.effort").String())
		assert.Equal(t, "high", gjson.Get(gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String(), "reasoning_effort").String())
		assert.JSONEq(t, `{"reasoning_effort":"high","session_id":"existing-thread"}`, got.header.Get("x-codex-turn-metadata"))
	}
	for _, path := range []string{"input.0", "input.1.summary", "input.2", "input.3", "reasoning.context"} {
		if gjson.Get(original, path).IsObject() || gjson.Get(original, path).IsArray() {
			assert.JSONEq(t, gjson.Get(original, path).Raw, gjson.GetBytes(retry.body, path).Raw, path)
		} else {
			assert.Equal(t, gjson.Get(original, path).Raw, gjson.GetBytes(retry.body, path).Raw, path)
		}
	}
	next := strings.Replace(original, `"input":[`, `"input":[{"type":"reasoning","id":"new-id","encrypted_content":"fresh","summary":[]},`, 1)
	c, info = reasoningFallbackFixture(t, next)
	info.ChannelSetting.ResponsesForceHighEffort = true
	_, apiErr = doUpstreamRoundTrip(c, info, a, strings.NewReader(next), false)
	require.Nil(t, apiErr)
	continued := <-captures
	require.NoError(t, continued.err)
	assert.Equal(t, "high", gjson.GetBytes(continued.body, "reasoning.effort").String())
	assert.Contains(t, string(continued.body), `"encrypted_content":"fresh"`)
	assert.NotContains(t, string(continued.body), `"encrypted_content":"old"`)
}

type reasoningFallbackTestAdaptor struct {
	channel.Adaptor
	bodies      [][]byte
	status      int
	errorBody   string
	responseErr *types.NewAPIError
	partialFail bool
}

func (a *reasoningFallbackTestAdaptor) DoRequest(_ *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (any, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	a.bodies = append(a.bodies, data)
	if info.UpstreamRequestBodySize != int64(len(data)) {
		return nil, errors.New("outbound Content-Length does not match body")
	}
	status, content := a.status, a.errorBody
	if status == 0 {
		status = http.StatusOK
		if bytes.Contains(data, []byte(`"encrypted_content":"old"`)) {
			status = http.StatusBadRequest
			content = `{"error":{"code":"invalid_encrypted_content","message":"rejected"}}`
		}
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(content))}, nil
}

func (a *reasoningFallbackTestAdaptor) DoResponse(_ *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	_ = resp.Body.Close()
	if a.partialFail {
		info.StreamStatus.MarkMissingTerminator()
	}
	return &dto.Usage{}, a.responseErr
}

func reasoningFallbackFixture(t *testing.T, body string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	return c, &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeResponses,
		UserId:    11,
		TokenId:   22,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:      33,
			ChannelBaseUrl: "https://upstream.example/v1",
			ApiKey:         fmt.Sprintf("fixture-%p", t), // Isolate repeated runs without a global purge.
			ChannelSetting: dto.ChannelSettings{
				ResponsesReasoningFallback: true,
			},
		},
		UpstreamRequestBodySize: int64(len(body)),
	}
}

func TestResponsesReasoningFallbackPreservesHistoryAndNewReasoning(t *testing.T) {
	original := `{"model":"gpt-6-astra","reasoning":{"effort":"xhigh","context":"all_turns"},"service_tier":"priority","stream":true,"store":false,"prompt_cache_key":"original-thread","client_metadata":{"session_id":"original-thread"},"input":[{"role":"user","content":"keep all prior requirements"},{"type":"reasoning","id":"rs_old","encrypted_content":"old","summary":[{"type":"summary_text","text":"keep this plan"}]},{"type":"custom_tool_call","call_id":"call_1","input":"keep command"},{"type":"custom_tool_call_output","call_id":"call_1","output":"keep result"},{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"keep progress"}]}],"tools":[{"type":"function","name":"test","parameters":{"const":9007199254740993}}]}`
	c, info := reasoningFallbackFixture(t, original)
	a := &reasoningFallbackTestAdaptor{}
	_, apiErr := doUpstreamRoundTrip(c, info, a, strings.NewReader(original), false)
	require.Nil(t, apiErr)
	require.Len(t, a.bodies, 2)
	assert.Equal(t, original, string(a.bodies[0]), "first request must be byte-identical")
	expected := strings.Replace(original, `"id":"rs_old","encrypted_content":"old",`, "", 1)
	// JSONEq uses float64; also check the exact large number independently.
	assert.JSONEq(t, expected, string(a.bodies[1]))
	assert.Contains(t, string(a.bodies[1]), "9007199254740993")
	assert.Contains(t, original, `"encrypted_content":"old"`, "input was not rewritten in place")

	// The next turn contains the same old snapshot plus newly generated, valid
	// reasoning. Only the former should be omitted, without another failed send.
	next := strings.Replace(original, `"input":[`, `"input":[{"type":"reasoning","id":"rs_new","encrypted_content":"fresh","summary":[]},`, 1)
	c, info = reasoningFallbackFixture(t, next)
	a = &reasoningFallbackTestAdaptor{}
	_, apiErr = doUpstreamRoundTrip(c, info, a, strings.NewReader(next), false)
	require.Nil(t, apiErr)
	require.Len(t, a.bodies, 1)
	assert.NotContains(t, string(a.bodies[0]), `"encrypted_content":"old"`)
	assert.Contains(t, string(a.bodies[0]), `"encrypted_content":"fresh"`)
	assert.Contains(t, string(a.bodies[0]), `"id":"rs_new"`)

	// Disabling the opt-in restores original behavior even while the cache exists.
	c, info = reasoningFallbackFixture(t, original)
	info.ChannelSetting.ResponsesReasoningFallback = false
	a = &reasoningFallbackTestAdaptor{}
	_, apiErr = doUpstreamRoundTrip(c, info, a, strings.NewReader(original), false)
	require.NotNil(t, apiErr)
	require.Len(t, a.bodies, 1)
	assert.Equal(t, original, string(a.bodies[0]))

	// Another tenant must still try its own ciphertext unchanged first.
	c, info = reasoningFallbackFixture(t, original)
	info.UserId++
	a = &reasoningFallbackTestAdaptor{}
	_, apiErr = doUpstreamRoundTrip(c, info, a, strings.NewReader(original), false)
	require.Nil(t, apiErr)
	require.Len(t, a.bodies, 2)
	assert.Equal(t, original, string(a.bodies[0]))
}

func TestResponsesReasoningFallbackDoesNotRetryUnrelatedOrOpaqueHistory(t *testing.T) {
	base := `{"model":"gpt-6-astra","input":[{"type":"reasoning","id":"rs_old","encrypted_content":"old","summary":[]}]}`
	invalid := `{"error":{"code":"invalid_encrypted_content"}}`
	for _, tc := range []struct {
		name, body, errorBody string
		status                int
		written               bool
	}{
		{name: "other 400", body: base, status: 400, errorBody: `{"error":{"code":"invalid_request_error","message":"invalid_encrypted_content"}}`},
		{name: "429", body: base, status: 429, errorBody: invalid},
		{name: "401", body: base, status: 401, errorBody: invalid},
		{name: "500", body: base, status: 500, errorBody: invalid},
		{name: "html", body: base, status: 400, errorBody: "<html>invalid_encrypted_content</html>"},
		{name: "oversized error", body: base, status: 400, errorBody: invalid + strings.Repeat(" ", 65536)},
		{name: "already streaming", body: base, status: 400, errorBody: invalid, written: true},
		{name: "SSE failure with 200", body: base, status: 200, errorBody: "data: " + invalid + "\n\n"},
		{name: "compaction", body: strings.Replace(base, `"input":[`, `"input":[{"type":"compaction","encrypted_content":"history"},`, 1), status: 400, errorBody: invalid},
		{name: "item reference", body: strings.Replace(base, `"input":[`, `"input":[{"type":"item_reference","id":"history"},`, 1), status: 400, errorBody: invalid},
		{name: "previous response", body: strings.Replace(base, `"input":`, `"previous_response_id":"resp_old","input":`, 1), status: 400, errorBody: invalid},
		{name: "conversation", body: strings.Replace(base, `"input":`, `"conversation":{"id":"conv_old"},"input":`, 1), status: 400, errorBody: invalid},
		{name: "no ciphertext", body: `{"input":[{"role":"user","content":"hi"}]}`, status: 400, errorBody: invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := reasoningFallbackFixture(t, tc.body)
			if tc.written {
				_, err := c.Writer.Write([]byte(": ping\n\n"))
				require.NoError(t, err)
			}
			a := &reasoningFallbackTestAdaptor{status: tc.status, errorBody: tc.errorBody}
			resp, remember, err := doResponsesReasoningFallback(c, info, a, strings.NewReader(tc.body))
			require.NoError(t, err)
			assert.Nil(t, remember)
			require.Len(t, a.bodies, 1)
			assert.Equal(t, tc.body, string(a.bodies[0]))
			httpResp := resp.(*http.Response)
			defer httpResp.Body.Close()
			got, err := io.ReadAll(httpResp.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.errorBody, string(got), "non-matching errors must remain readable and unchanged")
		})
	}
}

func TestResponsesReasoningFallbackFailedRetryIsNotCached(t *testing.T) {
	original := `{"model":"gpt-6-astra","input":[{"type":"reasoning","id":"rs_old","encrypted_content":"old","summary":[]}]}`
	for _, terminal := range []string{"HTTP 400", "SSE failed", "partial SSE failed"} {
		t.Run(terminal, func(t *testing.T) {
			c, info := reasoningFallbackFixture(t, original)
			a := &reasoningFallbackTestAdaptor{}
			if terminal == "HTTP 400" {
				a.status = 400
				a.errorBody = `{"error":{"code":"invalid_encrypted_content"}}`
			} else if terminal == "SSE failed" {
				a.responseErr = types.NewError(errors.New("response.failed"), types.ErrorCodeBadResponse)
			} else {
				info.IsStream = true
				info.StreamStatus = relaycommon.NewStreamStatus()
				a.partialFail = true
			}
			_, apiErr := doUpstreamRoundTrip(c, info, a, strings.NewReader(original), false)
			if a.partialFail {
				require.Nil(t, apiErr, "partial output is settled before reporting stream failure")
				require.NotNil(t, info.StreamStatus.FailureError())
			} else {
				require.NotNil(t, apiErr)
			}
			require.Len(t, a.bodies, 2, "never make a third attempt")
			c, info = reasoningFallbackFixture(t, original)
			a = &reasoningFallbackTestAdaptor{}
			_, apiErr = doUpstreamRoundTrip(c, info, a, strings.NewReader(original), false)
			require.Nil(t, apiErr)
			require.Len(t, a.bodies, 2, "a failed retry must not retire ciphertext")
			assert.Equal(t, original, string(a.bodies[0]))
		})
	}
}

func TestResponsesReasoningFallbackDefaultOff(t *testing.T) {
	var settings dto.ChannelSettings
	require.NoError(t, common.Unmarshal([]byte(`{}`), &settings))
	assert.False(t, settings.ResponsesReasoningFallback)
}

func TestResponsesReasoningFallbackPreservesNativeUltraProtocol(t *testing.T) {
	// Captured with Codex Desktop engine 0.158.0-alpha.2.1: selecting Ultra
	// sends xhigh on the wire, ultra in turn metadata, and enables proactive
	// delegation in the input instructions. These are not interchangeable.
	metadata := `{"model":"gpt-6-astra","reasoning_effort":"ultra"}`
	metadataJSON, err := common.Marshal(metadata)
	require.NoError(t, err)
	original := fmt.Sprintf(`{"model":"gpt-6-astra","reasoning":{"effort":"xhigh","context":"all_turns"},"service_tier":"priority","stream":true,"store":false,"client_metadata":{"x-codex-turn-metadata":%s},"input":[{"role":"developer","content":[{"type":"input_text","text":"Proactive multi-agent delegation is active."}]},{"role":"user","content":"Continue the existing task."},{"type":"reasoning","id":"rs_old","encrypted_content":"old","summary":[{"type":"summary_text","text":"Keep the existing plan."}]}]}`, metadataJSON)
	c, info := reasoningFallbackFixture(t, original)
	c.Request.Header.Set("x-codex-turn-metadata", metadata)
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(original), &request))
	converted, err := (&openai.Adaptor{}).ConvertOpenAIResponsesRequest(c, info, request)
	require.NoError(t, err)
	outbound, err := common.Marshal(converted)
	require.NoError(t, err)
	assert.JSONEq(t, original, string(outbound), "DTO conversion must preserve the native protocol")
	assert.Equal(t, "xhigh", info.ReasoningEffort, "wire effort must not be replaced by the UI label")
	info.UpstreamRequestBodySize = int64(len(outbound))
	a := &reasoningFallbackTestAdaptor{}
	_, apiErr := doUpstreamRoundTrip(c, info, a, bytes.NewReader(outbound), false)
	require.Nil(t, apiErr)
	require.Len(t, a.bodies, 2)
	assert.Equal(t, outbound, a.bodies[0])
	expected := strings.Replace(original, `"id":"rs_old","encrypted_content":"old",`, "", 1)
	assert.JSONEq(t, expected, string(a.bodies[1]), "fallback must retain Ultra metadata and delegation instructions")
	assert.Equal(t, metadata, c.Request.Header.Get("x-codex-turn-metadata"))

	// The retired-history cache must not pin the next turn to the previous
	// effort or discard newly generated reasoning when the user changes effort.
	next := strings.Replace(original, `"effort":"xhigh"`, `"effort":"max"`, 1)
	next = strings.Replace(next, `\"reasoning_effort\":\"ultra\"`, `\"reasoning_effort\":\"max\"`, 1)
	next = strings.Replace(next, `"input":[`, `"input":[{"type":"reasoning","id":"rs_new","encrypted_content":"fresh","summary":[]},`, 1)
	c, info = reasoningFallbackFixture(t, next)
	a = &reasoningFallbackTestAdaptor{}
	_, apiErr = doUpstreamRoundTrip(c, info, a, strings.NewReader(next), false)
	require.Nil(t, apiErr)
	require.Len(t, a.bodies, 1)
	expected = strings.Replace(next, `"id":"rs_old","encrypted_content":"old",`, "", 1)
	assert.JSONEq(t, expected, string(a.bodies[0]))
}
