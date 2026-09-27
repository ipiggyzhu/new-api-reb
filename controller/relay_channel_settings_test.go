package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relayCall is one client request, relayed through a single channel whose
// upstream answers it with the reply fields.
type relayCall struct {
	format types.RelayFormat
	// route is the pattern the relay is mounted on and defaults to path.
	route, path, body string
	// replyStatus defaults to 200 and replyType to application/json.
	replyStatus          int
	replyType, replyBody string
}

// upstreamRequest is what the channel's upstream received.
type upstreamRequest struct {
	body          []byte
	readErr       error
	contentLength int64
}

// relayThroughChannel sends call through the real Distribute and Relay path to
// channel, which is the only channel serving its model, and returns the client's
// response together with the request the upstream received. Only the identity
// normally established by TokenAuth is supplied by the fixture.
func relayThroughChannel(t *testing.T, channel model.Channel, call relayCall) (*httptest.ResponseRecorder, upstreamRequest) {
	t.Helper()
	const initialQuota = 100_000
	originalMode := gin.Mode()
	originalRedis, originalCache := common.RedisEnabled, common.MemoryCacheEnabled
	originalBatch, originalDisable := common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled
	originalRetry, originalCount := common.RetryTimes, constant.CountToken
	originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
	originalScores := operation_setting.GetChannelDynamicScoreSetting()
	originalPing := operation_setting.GetGeneralSetting().PingIntervalEnabled
	originalRatios := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() {
		gin.SetMode(originalMode)
		common.RedisEnabled, common.MemoryCacheEnabled = originalRedis, originalCache
		common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = originalBatch, originalDisable
		common.RetryTimes, constant.CountToken = originalRetry, originalCount
		common.SetDatabaseTypes(originalMainType, originalLogType)
		operation_setting.SetChannelDynamicScoreSettingForTest(originalScores)
		operation_setting.GetGeneralSetting().PingIntervalEnabled = originalPing
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalRatios))
	})
	common.MemoryCacheEnabled = true
	common.BatchUpdateEnabled, common.AutomaticDisableChannelEnabled = false, false
	// A single attempt: every case is about what that attempt sends and returns.
	common.RetryTimes, constant.CountToken = 0, false
	operation_setting.GetGeneralSetting().PingIntervalEnabled = false
	scores := originalScores
	scores.Enabled = false
	operation_setting.SetChannelDynamicScoreSettingForTest(scores)
	ratios := ratio_setting.GetModelRatioCopy()
	ratios[channel.Models] = 1
	ratioJSON, err := common.Marshal(ratios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(ratioJSON)))
	require.NoError(t, i18n.Init())
	service.InitHttpClient()

	initModelListColumnNames(t)
	db := setupErrorLogTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Token{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, db.Where("1 = 1").Delete(&model.Channel{}).Error)
		model.InitChannelCache()
	})
	user := model.User{Id: 83, Username: "relay-settings-user", Group: "default", Quota: initialQuota, Status: common.UserStatusEnabled}
	token := model.Token{Id: 89, UserId: user.Id, Key: "local-settings-test-token", Name: "relay-settings-token", RemainQuota: initialQuota}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&token).Error)

	received := make(chan upstreamRequest, 1)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) == 1 {
			body, err := io.ReadAll(r.Body)
			received <- upstreamRequest{body: body, readErr: err, contentLength: r.ContentLength}
		}
		replyType, replyStatus := call.replyType, call.replyStatus
		if replyType == "" {
			replyType = "application/json"
		}
		if replyStatus == 0 {
			replyStatus = http.StatusOK
		}
		w.Header().Set("Content-Type", replyType)
		w.WriteHeader(replyStatus)
		_, _ = io.WriteString(w, call.replyBody)
	}))
	t.Cleanup(upstream.Close)
	channel.Id, channel.Name, channel.Key = 81, "settings-upstream", "local-settings-upstream-key"
	channel.Status, channel.Group, channel.BaseURL = common.ChannelStatusEnabled, "default", &upstream.URL
	channel.Priority, channel.Weight = common.GetPointer(int64(5)), common.GetPointer(uint(10))
	require.NoError(t, db.Create(&channel).Error)
	model.InitChannelCache()

	route := call.route
	if route == "" {
		route = call.path
	}
	router := gin.New()
	router.POST(route, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
		common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
		c.Set("token_name", token.Name)
		c.Set("token_quota", initialQuota)
		c.Set(common.RequestIdKey, "relay-settings-test")
		c.Next()
	}, middleware.Distribute(), func(c *gin.Context) { Relay(c, call.format) })
	request := httptest.NewRequest(http.MethodPost, call.path, strings.NewReader(call.body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	// Settlement and refunds enqueue background work. Drain it before earlier
	// cleanups close the DB or restore global settings.
	t.Cleanup(func() {
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 },
			2*time.Second, time.Millisecond, "relay background tasks must finish before fixture cleanup")
	})
	router.ServeHTTP(recorder, request)

	require.Equal(t, int32(1), upstreamCalls.Load(), "the request must reach the upstream exactly once: %s", recorder.Body.String())
	got := <-received
	require.NoError(t, got.readErr)
	return recorder, got
}

// Every relay format finalizes its upstream body the same way: client fields
// the channel does not allow are dropped, then the channel's param override is
// applied, and the body goes out with an explicit Content-Length because some
// upstreams refuse chunked requests. Each case runs a different handler, so
// each proves its own call site.
func TestRelayChannelParamOverrideReachesUpstream(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		model       string
		call        relayCall
		// dropsDisabledFields marks the formats that filter client fields the
		// channel does not allow, such as a client-chosen service_tier.
		dropsDisabledFields bool
	}{
		{
			name: "chat completions", channelType: constant.ChannelTypeOpenAI, model: "gpt-4o", dropsDisabledFields: true,
			call: relayCall{
				format: types.RelayFormatOpenAI, path: "/v1/chat/completions",
				body: `{"model":"gpt-4o","max_tokens":16,"service_tier":"priority","messages":[{"role":"user","content":"hello"}]}`,
				replyBody: `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,` +
					`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
					`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
			},
		},
		{
			name: "responses", channelType: constant.ChannelTypeOpenAI, model: "gpt-4o", dropsDisabledFields: true,
			call: relayCall{
				format: types.RelayFormatOpenAIResponses, path: "/v1/responses",
				body: `{"model":"gpt-4o","input":"hello","max_output_tokens":16,"service_tier":"priority"}`,
				replyBody: `{"id":"resp_1","object":"response","model":"gpt-4o","status":"completed","output":[{"type":"message",` +
					`"id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
					`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`,
			},
		},
		{
			name: "claude messages", channelType: constant.ChannelTypeAnthropic, model: "claude-fable-5-1", dropsDisabledFields: true,
			call: relayCall{
				format: types.RelayFormatClaude, path: "/v1/messages",
				body: `{"model":"claude-fable-5-1","max_tokens":16,"service_tier":"priority","messages":[{"role":"user","content":"hello"}]}`,
				replyBody: `{"id":"msg_1","type":"message","role":"assistant","model":"claude-fable-5-1",` +
					`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`,
			},
		},
		{
			name: "image generation", channelType: constant.ChannelTypeOpenAI, model: "dall-e-3",
			call: relayCall{
				format: types.RelayFormatOpenAIImage, path: "/v1/images/generations",
				body:      `{"model":"dall-e-3","prompt":"a cat","n":1,"size":"1024x1024"}`,
				replyBody: `{"created":1700000000,"data":[{"url":"https://example.com/cat.png"}]}`,
			},
		},
		{
			name: "embeddings", channelType: constant.ChannelTypeOpenAI, model: "text-embedding-3-small",
			call: relayCall{
				format: types.RelayFormatEmbedding, path: "/v1/embeddings",
				body: `{"model":"text-embedding-3-small","input":"hello"}`,
				replyBody: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],` +
					`"model":"text-embedding-3-small","usage":{"prompt_tokens":2,"total_tokens":2}}`,
			},
		},
		{
			name: "rerank", channelType: constant.ChannelTypeOpenAI, model: "bge-reranker-v2-m3",
			call: relayCall{
				format: types.RelayFormatRerank, path: "/v1/rerank",
				body:      `{"model":"bge-reranker-v2-m3","query":"hello","documents":["hello world","goodbye"]}`,
				replyBody: `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"prompt_tokens":3,"total_tokens":3}}`,
			},
		},
		{
			name: "gemini generate content", channelType: constant.ChannelTypeGemini, model: "gemini-2.5-flash",
			call: relayCall{
				format: types.RelayFormatGemini, route: "/v1beta/models/*path", path: "/v1beta/models/gemini-2.5-flash:generateContent",
				body: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
				replyBody: `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],` +
					`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`,
			},
		},
		{
			name: "gemini embed content", channelType: constant.ChannelTypeGemini, model: "gemini-embedding-001",
			call: relayCall{
				format: types.RelayFormatGemini, route: "/v1beta/models/*path", path: "/v1beta/models/gemini-embedding-001:embedContent",
				body:      `{"content":{"parts":[{"text":"hello"}]}}`,
				replyBody: `{"embedding":{"values":[0.1,0.2]}}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{
				Type: tc.channelType, Models: tc.model,
				// inference_geo is also filtered from client requests by default, so
				// the override survives only if it is applied after that filter.
				ParamOverride: common.GetPointer(`{"inference_geo":"us"}`),
			}
			recorder, received := relayThroughChannel(t, channel, tc.call)

			assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.EqualValues(t, len(received.body), received.contentLength, "the upstream body must carry an explicit Content-Length")
			var body map[string]any
			require.NoError(t, common.Unmarshal(received.body, &body), string(received.body))
			assert.Equal(t, "us", body["inference_geo"], "the channel param override must reach the upstream")
			if tc.dropsDisabledFields {
				assert.NotContains(t, body, "service_tier", "a service_tier the channel does not allow must not reach the upstream")
			}
		})
	}
}

// A channel's status code mapping rewrites the status of a failed upstream
// attempt before the client sees it, on every format that relays through the
// shared round trip, both for an upstream error status and for a response the
// adaptor could not use.
func TestRelayChannelStatusCodeMappingRewritesUpstreamFailures(t *testing.T) {
	const upstreamError = `{"error":{"message":"rejected by upstream","type":"invalid_request_error","code":"rejected"}}`
	cases := []struct {
		name        string
		channelType int
		model       string
		call        relayCall
		wantStatus  int
	}{
		{
			name: "chat completions", channelType: constant.ChannelTypeOpenAI, model: "gpt-4o", wantStatus: http.StatusUnprocessableEntity,
			call: relayCall{
				format: types.RelayFormatOpenAI, path: "/v1/chat/completions",
				body:        `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
				replyStatus: http.StatusBadRequest, replyBody: upstreamError,
			},
		},
		{
			name: "responses", channelType: constant.ChannelTypeOpenAI, model: "gpt-4o", wantStatus: http.StatusUnprocessableEntity,
			call: relayCall{
				format: types.RelayFormatOpenAIResponses, path: "/v1/responses",
				body:        `{"model":"gpt-4o","input":"hello","max_output_tokens":16}`,
				replyStatus: http.StatusBadRequest, replyBody: upstreamError,
			},
		},
		{
			name: "claude messages", channelType: constant.ChannelTypeAnthropic, model: "claude-fable-5-1", wantStatus: http.StatusUnprocessableEntity,
			call: relayCall{
				format: types.RelayFormatClaude, path: "/v1/messages",
				body:        `{"model":"claude-fable-5-1","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
				replyStatus: http.StatusBadRequest,
				replyBody:   `{"type":"error","error":{"type":"invalid_request_error","message":"rejected by upstream"}}`,
			},
		},
		{
			name: "embeddings", channelType: constant.ChannelTypeOpenAI, model: "text-embedding-3-small", wantStatus: http.StatusUnprocessableEntity,
			call: relayCall{
				format: types.RelayFormatEmbedding, path: "/v1/embeddings",
				body:        `{"model":"text-embedding-3-small","input":"hello"}`,
				replyStatus: http.StatusBadRequest, replyBody: upstreamError,
			},
		},
		{
			name: "rerank", channelType: constant.ChannelTypeOpenAI, model: "bge-reranker-v2-m3", wantStatus: http.StatusUnprocessableEntity,
			call: relayCall{
				format: types.RelayFormatRerank, path: "/v1/rerank",
				body:        `{"model":"bge-reranker-v2-m3","query":"hello","documents":["hello world","goodbye"]}`,
				replyStatus: http.StatusBadRequest, replyBody: upstreamError,
			},
		},
		{
			name: "audio speech", channelType: constant.ChannelTypeOpenAI, model: "tts-1", wantStatus: http.StatusUnprocessableEntity,
			call: relayCall{
				format: types.RelayFormatOpenAIAudio, path: "/v1/audio/speech",
				body:        `{"model":"tts-1","input":"hello","voice":"alloy"}`,
				replyStatus: http.StatusBadRequest, replyBody: upstreamError,
			},
		},
		{
			name: "chat completions answered with an undecodable body", channelType: constant.ChannelTypeOpenAI, model: "gpt-4o",
			wantStatus: http.StatusServiceUnavailable,
			call: relayCall{
				format: types.RelayFormatOpenAI, path: "/v1/chat/completions",
				body:      `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
				replyBody: `not json`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{
				Type: tc.channelType, Models: tc.model,
				StatusCodeMapping: common.GetPointer(`{"400":"422","500":"503"}`),
			}
			recorder, _ := relayThroughChannel(t, channel, tc.call)

			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
		})
	}
}

// An upstream may answer a request that did not ask for streaming with an event
// stream. Chat completions and Claude messages relay that answer as a stream
// instead of failing to decode it as a single JSON body.
func TestRelayServesEventStreamAnswerToNonStreamRequest(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		model       string
		call        relayCall
	}{
		{
			name: "chat completions", channelType: constant.ChannelTypeOpenAI, model: "gpt-4o",
			call: relayCall{
				format: types.RelayFormatOpenAI, path: "/v1/chat/completions",
				body:      `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
				replyType: "text/event-stream",
				replyBody: "data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,` +
					`"delta":{"role":"assistant","content":"streamed hello"},"finish_reason":null}]}` + "\n\n" +
					"data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,` +
					`"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
					"data: " + `{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],` +
					`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
					"data: [DONE]\n\n",
			},
		},
		{
			name: "claude messages", channelType: constant.ChannelTypeAnthropic, model: "claude-fable-5-1",
			call: relayCall{
				format: types.RelayFormatClaude, path: "/v1/messages",
				body:      `{"model":"claude-fable-5-1","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
				replyType: "text/event-stream",
				replyBody: "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_1","type":"message",` +
					`"role":"assistant","model":"claude-fable-5-1","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
					"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
					"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed hello"}}` + "\n\n" +
					"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
					"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
					"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{Type: tc.channelType, Models: tc.model}
			recorder, _ := relayThroughChannel(t, channel, tc.call)

			assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			assert.Contains(t, recorder.Body.String(), "streamed hello")
		})
	}
}
