package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
		errorLogged bool
	)

	// The distributor middleware pins channel affinity after this handler returns,
	// where a mid-stream failure is indistinguishable from success (the 200 is
	// already committed). Start from "failed" so every exit path below — including
	// the ones that return before a channel is even picked — leaves the truth
	// behind, and flip it only where the relay actually succeeded.
	service.SetChannelAffinityRelayOutcome(c, false)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		if newAPIError != nil {
			// Everything that fails before the retry loop reaches
			// processChannelError returned straight to the caller and produced no
			// error log — no available channel for the group, a rejected request
			// body, a pricing lookup failure. Those are exactly the failures an
			// operator needs to see, so record them here instead.
			//
			// Insufficient-quota errors are NOT among them: billing_session.go
			// tags every one of them with ErrOptionWithNoRecordErrorLog, and
			// recordRelayErrorLog honours that. That suppression is deliberate —
			// one out-of-credit client retrying in a loop would otherwise bury
			// the log table — so it stays, and this is not the place to see a
			// user running out of money.
			if !errorLogged {
				recordRelayErrorLog(c, nil, newAPIError)
			}
			logger.LogError(c, fmt.Sprintf("relay error: %s", common.LocalLogPreview(newAPIError.Error())))
			newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))

			var errorBody gin.H
			if relayFormat == types.RelayFormatClaude {
				errorBody = gin.H{"type": "error", "error": newAPIError.ToClaudeError()}
			} else {
				errorBody = gin.H{"error": newAPIError.ToOpenAIError()}
			}

			// A streaming attempt sets the event-stream headers before the upstream
			// request is even made, and gin's writeContentType only fills Content-Type
			// when it is absent — so a plain c.JSON here could neither replace the
			// text/event-stream label nor, once chunks were on the wire, avoid
			// appending a bare JSON object to a half-delivered SSE body. A
			// spec-compliant SSE parser reads that trailing object as an unknown field
			// and drops it, which is the silent truncation the StreamStatus work was
			// meant to eliminate. So: mid-stream failures are reported as a final SSE
			// frame, and failures with nothing yet written shed the streaming headers
			// so the JSON body is labelled as JSON.
			isEventStream := c.Writer.Header().Get("Content-Type") == "text/event-stream"

			switch {
			case relayFormat == types.RelayFormatOpenAIRealtime:
				helper.WssError(c, ws, newAPIError.ToOpenAIError())
			case isEventStream && c.Writer.Written():
				if errorJson, marshalErr := common.Marshal(errorBody); marshalErr != nil {
					logger.LogError(c, fmt.Sprintf("marshal stream error payload failed: %s", marshalErr.Error()))
				} else {
					_ = helper.StringData(c, string(errorJson))
				}
				helper.Done(c)
			default:
				if isEventStream {
					// The status code has not been committed yet, so a JSON body is still
					// possible — but only once these are gone.
					c.Writer.Header().Del("Content-Type")
					c.Writer.Header().Del("Transfer-Encoding")
					c.Writer.Header().Del("X-Accel-Buffering")
				}
				c.JSON(newAPIError.StatusCode, errorBody)
			}
		}
	}()

	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		// Map "request body too large" to 413 so clients can handle it correctly
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			newAPIError = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
		} else {
			newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			if relayInfo.Billing != nil {
				relayInfo.Billing.Refund(c)
			}
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
	}()

	// With PingIntervalEnabled the keepalive pinger usually writes before a
	// slow upstream fails; shouldRetry needs to tell those frames apart from
	// relayed payload.
	c.Writer = &pingAwareWriter{ResponseWriter: c.Writer}

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		relayInfo.RetryIndex = retryParam.GetRetry()
		// Clear the previous attempt's stream verdict. StreamScannerHandler
		// installs a fresh StreamStatus, but a retry can succeed through a path
		// that never reaches it, and a stale abnormal end reason would then fail
		// an attempt that actually completed.
		relayInfo.StreamStatus = nil
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			newAPIError = channelErr
			break
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			// Ensure consistent 413 for oversized bodies even when error occurs later (e.g., retry path)
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
			} else {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			newAPIError = relayHandler(c, relayInfo)
		}

		// A handler that returns nil has already settled billing, but for a stream
		// that means only "no error was raised while forwarding" — the scanner
		// records an upstream disconnect, stream timeout, or handler panic on
		// relayInfo.StreamStatus and every handler still returns nil. Consulting it
		// here converts that into a real failure for the one caller whose verdict
		// matters: without it the truncated stream counted as a success, so the
		// channel was marked healthy for affinity and never accrued a fault.
		//
		// Order matters. Billing stays settled (the upstream produced the tokens
		// that were delivered, and Refund no-ops once settled), and shouldRetry
		// declines below because the partial stream is already on the wire, so the
		// caller keeps what it received instead of getting a second response
		// appended to it.
		if newAPIError == nil {
			newAPIError = relayInfo.StreamStatus.FailureError()
		}
		if newAPIError == nil {
			relayInfo.LastError = nil
			service.SetChannelAffinityRelayOutcome(c, true)
			// Recorded here rather than in the distributor so it lands on the channel
			// that actually served the request, and only after StreamStatus has been
			// consulted above: a stream that committed a 200 and then died reaches
			// this point as an error, so it cannot be credited as a success.
			if !relayInfo.IsChannelTest && channel_score.Enabled() {
				channel_score.Report(channel.Id, relayInfo.UsingGroup, relayInfo.OriginModelName, channel_score.OutcomeSuccess)
			}
			return
		}

		newAPIError = service.NormalizeViolationFeeError(newAPIError)
		relayInfo.LastError = newAPIError

		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)
		errorLogged = true
		// Report this attempt, not just the request's final outcome. The
		// distributor's post-c.Next() hook only ever sees the last channel, so
		// without a report here a channel that failed and was retried away from
		// would never accrue a fault.
		//
		// Deliberately at the call site rather than inside processChannelError:
		// that function is also reached by synthetic channel tests and by the task
		// relay failure path, and neither should move production routing.
		//
		// AutoBan is not consulted. It governs whether a channel gets disabled,
		// which is a separate decision from whether a genuine fault should affect
		// ordering; a channel exempted from auto-ban can still be the worse choice.
		if service.IsChannelRoutingFaultError(newAPIError) {
			// Also tells the affinity layer that this specific channel faulted, which
			// is what lets a pin on a broken channel be retracted instead of surviving
			// until its TTL.
			service.MarkChannelAffinityChannelFault(c, channel.Id)
			// Guarded even though the synthetic channel test does not run this loop:
			// scheduled tests probe every channel including broken ones, so if that
			// ever changed, the periodic sweep would demote channels on traffic no
			// user ever sent.
			if !relayInfo.IsChannelTest && channel_score.Enabled() {
				channel_score.Report(channel.Id, relayInfo.UsingGroup, relayInfo.OriginModelName, channel_score.OutcomeFault)
			}
		}

		if !shouldRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
	if newAPIError != nil {
		gopool.Go(func() {
			perfmetrics.RecordRelaySample(relayInfo, false, 0)
		})
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		channelId := c.GetInt("channel_id")
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		// The distributor selected this channel before the relay retry loop
		// started. Record it as an attempted channel now; otherwise the first
		// retry has no exclusion set and can select the same failed channel again
		// (or skip healthy siblings by treating retry as a tier index).
		if channelId > 0 && retryParam != nil {
			retryParam.ExcludeChannel(channelId)
		}
		return &model.Channel{
			Id:      channelId,
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			AutoBan: &autoBanInt,
			ChannelInfo: model.ChannelInfo{
				IsMultiKey: common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey),
			},
		}, nil
	}
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		if errors.Is(err, model.ErrAllChannelsSaturated) {
			// Every remaining channel is busy, not broken. Retrying in place would
			// only spin against the same caps, so the request stops here and says so.
			return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的渠道均已达到并发上限（retry）", selectGroup, info.OriginModelName), types.ErrorCodeChannelsSaturated, types.ErrOptionWithSkipRetry())
		}
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	// Selection already took the slot; hand it to the request, which gives back
	// the previous attempt's slot at the same time.
	service.HoldChannelSlot(c, channel.Id)

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	// use_channel is log-only, so the selector never learned which channels this
	// request already burned; feed the attempt back so a retry skips it.
	retryParam.ExcludeChannel(channel.Id)
	return channel, nil
}

// pingFrame must stay byte-identical to what helper.PingData writes in a
// single Write call; a mismatch degrades safely (pings count as payload and
// block the retry).
var pingFrame = []byte(": PING\n\n")

// pingAwareWriter tells keepalive ping frames apart from real response data.
// With PingIntervalEnabled the ping goroutines (relay/channel/api_request.go,
// relay/helper/stream_scanner.go) commit a 200 and write SSE comments while
// the upstream is still working, which would otherwise make every slow
// upstream failure look non-retryable to shouldRetry.
type pingAwareWriter struct {
	gin.ResponseWriter
	pingFrames  int
	dataWritten bool
}

func (w *pingAwareWriter) Write(data []byte) (int, error) {
	if bytes.Equal(data, pingFrame) {
		w.pingFrames++
	} else if len(data) > 0 {
		w.dataWritten = true
	}
	return w.ResponseWriter.Write(data)
}

func (w *pingAwareWriter) WriteString(s string) (int, error) {
	if len(s) > 0 {
		w.dataWritten = true
	}
	return w.ResponseWriter.WriteString(s)
}

// Unwrap keeps http.NewResponseController able to reach the connection's
// write-deadline support through this wrapper (helper.ExtendWriteDeadline).
func (w *pingAwareWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// onlyPingsWritten reports that every byte on the wire is a keepalive frame.
// The Size cross-check keeps the answer exact even for bytes that reached the
// connection without passing through this wrapper (e.g. after a hijack).
func (w *pingAwareWriter) onlyPingsWritten() bool {
	return w.pingFrames > 0 && !w.dataWritten && w.Size() == w.pingFrames*len(pingFrame)
}

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	if openaiErr == nil {
		return false
	}
	// Part of the response is already on the wire — a streamed prefix, or a
	// hijacked realtime connection. Retrying on another channel would append a
	// second, complete response to the partial one the caller is still reading,
	// so the request has to fail as it stands. Upstreams that report an error
	// mid-stream (Anthropic overloaded_error and friends) reach here with a
	// retryable 500 and used to do exactly that.
	//
	// Keepalive ping frames are the one exception: the pinger can start
	// writing before the upstream has produced anything, and SSE clients
	// discard comment lines, so a retried attempt can still deliver a
	// well-formed stream after them. The headers are already committed at 200,
	// but that is no worse for the retry than for the ping-bearing attempt
	// itself. Anything else on the wire means giving up beats retrying blind.
	if c != nil && c.Writer != nil && c.Writer.Written() {
		pw, ok := c.Writer.(*pingAwareWriter)
		if !ok || !pw.onlyPingsWritten() {
			return false
		}
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	// These four guards outrank every error-class judgement below them. An error
	// tagged ErrOptionWithSkipRetry, an exhausted retry budget, a token pinned to
	// one channel, and an error code configured as always-skip each mean "do not
	// retry" regardless of which namespace the error code happens to live in.
	//
	// IsChannelError only tests the "channel:" prefix on the error code, and that
	// namespace mixes genuinely retryable upstream faults (channel:invalid_key on
	// a dead key) with configuration errors that were raised with
	// ErrOptionWithSkipRetry (channel:model_mapped_error). Judging the prefix
	// first burned the whole retry budget on errors that had already asked not to
	// be retried, re-running the auto-disable check on every attempt — and worse,
	// it let a sk-xxx-<channelId> token that must stay on one channel be rerouted
	// to another, because the specific_channel_id guard never got a say.
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if c != nil {
		if _, ok := c.Get("specific_channel_id"); ok {
			return false
		}
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	// A channel-level fault is retryable on a different channel by definition, so
	// it answers before the status code it happens to carry is consulted. The
	// status-code checks stay below it: these errors are raised internally and
	// carry whatever status the construction site stamped on them, which says
	// nothing about whether another channel could serve the request.
	if types.IsChannelError(openaiErr) {
		return true
	}
	code := openaiErr.StatusCode
	if code >= 200 && code < 300 {
		return false
	}
	if code < 100 || code > 599 {
		return true
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, common.LocalLogPreview(err.Error())))
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if service.ShouldDisableChannel(err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, err.ErrorWithStatusCode())
		})
	}

	recordRelayErrorLog(c, &channelError, err)
}

// recordRelayErrorLog writes one error log row for a failed relay attempt.
//
// channelError is the channel the attempt actually ran against and is preferred
// over the request context, which can disagree once the retry loop has moved on.
// It is nil for failures raised before a channel was ever picked — an exhausted
// user balance, no available channel for the group — which previously returned
// straight to the caller and left no trace at all.
func recordRelayErrorLog(c *gin.Context, channelError *types.ChannelError, err *types.NewAPIError) {
	if !constant.ErrorLogEnabled || !types.IsRecordErrorLog(err) {
		return
	}

	channelId := c.GetInt("channel_id")
	channelName := c.GetString("channel_name")
	channelType := c.GetInt("channel_type")
	if channelError != nil {
		channelId = channelError.ChannelId
		channelName = channelError.ChannelName
		channelType = channelError.ChannelType
	}

	other := make(map[string]interface{})
	if c.Request != nil && c.Request.URL != nil {
		other["request_path"] = c.Request.URL.Path
	}
	other["error_type"] = err.GetErrorType()
	other["error_code"] = err.GetErrorCode()
	other["status_code"] = err.StatusCode
	other["channel_id"] = channelId
	other["channel_name"] = channelName
	other["channel_type"] = channelType

	adminInfo := make(map[string]interface{})
	adminInfo["use_channel"] = c.GetStringSlice("use_channel")
	if common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey) {
		adminInfo["is_multi_key"] = true
		adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
	}
	// A block page or plaintext refusal leaves nothing in the message, so the raw
	// upstream body is the only way to tell a Cloudflare challenge apart from a
	// dead origin. admin_info is stripped from non-admin log views.
	if upstreamBody := err.GetUpstreamBody(); upstreamBody != "" {
		adminInfo["upstream_body"] = upstreamBody
	}
	service.AppendChannelAffinityAdminInfo(c, adminInfo)
	other["admin_info"] = adminInfo

	startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	if startTime.IsZero() {
		startTime = time.Now()
	}
	useTimeSeconds := int(time.Since(startTime).Seconds())
	model.RecordErrorLog(c, c.GetInt("id"), channelId, c.GetString("original_model"), c.GetString("token_name"),
		err.MaskSensitiveErrorWithStatusCode(), c.GetInt("token_id"), useTimeSeconds,
		common.GetContextKeyBool(c, constant.ContextKeyIsStream), c.GetString("group"), other)
}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *dto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	if taskErr := relay.ResolveOriginTask(c, relayInfo); taskErr != nil {
		respondTaskError(c, taskErr)
		return
	}

	var result *relay.TaskSubmitResult
	var taskErr *dto.TaskError
	defer func() {
		if taskErr != nil && relayInfo.Billing != nil {
			relayInfo.Billing.Refund(c)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		var channel *model.Channel

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			if retryParam.GetRetry() > 0 {
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				break
			}
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusRequestEntityTooLarge)
			} else {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusBadRequest)
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		result, taskErr = relay.RelayTaskSubmit(c, relayInfo)
		if taskErr == nil {
			break
		}

		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				types.NewOpenAIError(taskErr.Error, types.ErrorCodeBadResponseStatusCode, taskErr.StatusCode))
		}

		if !shouldRetryTaskRelay(c, channel.Id, taskErr, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	// ── 成功：结算 + 日志 + 插入任务 ──
	if taskErr == nil {
		if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
			common.SysError("settle task billing error: " + settleErr.Error())
		}
		service.LogTaskConsumption(c, relayInfo)

		task := model.InitTask(result.Platform, relayInfo)
		task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
		task.PrivateData.BillingSource = relayInfo.BillingSource
		task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
		task.PrivateData.TokenId = relayInfo.TokenId
		task.PrivateData.NodeName = common.NodeName
		task.PrivateData.BillingContext = &model.TaskBillingContext{
			ModelPrice:      relayInfo.PriceData.ModelPrice,
			GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ModelRatio:      relayInfo.PriceData.ModelRatio,
			OtherRatios:     relayInfo.PriceData.OtherRatios(),
			OriginModelName: relayInfo.OriginModelName,
			PerCallBilling:  common.StringsContains(constant.TaskPricePatches, relayInfo.OriginModelName) || relayInfo.PriceData.UsePrice,
		}
		task.Quota = result.Quota
		task.Data = result.TaskData
		task.Action = relayInfo.Action
		if insertErr := task.Insert(); insertErr != nil {
			common.SysError("insert task error: " + insertErr.Error())
		}
	}

	if taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *dto.TaskError) {
	if taskErr.StatusCode == http.StatusTooManyRequests {
		taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	c.JSON(taskErr.StatusCode, taskErr)
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if operation_setting.IsAlwaysSkipRetryStatusCode(taskErr.StatusCode) {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}
