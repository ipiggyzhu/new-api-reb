package relay

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func TextHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	textReq, ok := info.Request.(*dto.GeneralOpenAIRequest)
	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected dto.GeneralOpenAIRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(textReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to GeneralOpenAIRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	if request.WebSearchOptions != nil {
		c.Set("chat_completion_web_search_context_size", request.WebSearchOptions.SearchContextSize)
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	includeUsage := true
	// 判断用户是否需要返回使用情况
	if request.StreamOptions != nil {
		includeUsage = request.StreamOptions.IncludeUsage
	}

	// 如果不支持StreamOptions，将StreamOptions设置为nil
	if !info.SupportStreamOptions || !lo.FromPtrOr(request.Stream, false) {
		request.StreamOptions = nil
	} else {
		// 如果支持StreamOptions，且请求中没有设置StreamOptions，根据配置文件设置StreamOptions
		if constant.ForceStreamOption {
			request.StreamOptions = &dto.StreamOptions{
				IncludeUsage: true,
			}
		}
	}

	info.ShouldIncludeUsage = includeUsage

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	passThroughGlobal := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	if info.RelayMode == relayconstant.RelayModeChatCompletions &&
		!passThroughGlobal &&
		!info.ChannelSetting.PassThroughBodyEnabled &&
		service.ShouldChatCompletionsUseResponsesGlobal(info.ChannelId, info.ChannelType, info.OriginModelName) {
		applySystemPromptIfNeeded(c, info, request)
		usage, newApiErr := chatCompletionsViaResponses(c, info, adaptor, request)
		if newApiErr != nil {
			return newApiErr
		}

		postAudioOrTextConsumeQuota(c, info, usage)
		return nil
	}

	var requestBody io.Reader

	// A channel test has no client request to pass through: channel-test.go builds
	// its payload as a Go struct and leaves the gin body nil, so reading it here
	// would send an empty body upstream and make a healthy channel look broken.
	if (passThroughGlobal || info.ChannelSetting.PassThroughBodyEnabled) && !info.IsChannelTest {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if common.DebugEnabled {
			if debugBytes, bErr := storage.Bytes(); bErr == nil {
				logger.LogDebug(c, "requestBody: %s", debugBytes)
			}
		}
		requestBody = common.ReaderOnly(storage)
	} else {
		convertedRequest, err := adaptor.ConvertOpenAIRequest(c, info, request)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)

		if req, ok := convertedRequest.(*dto.GeneralOpenAIRequest); ok {
			applySystemPromptIfNeeded(c, info, req)
		}

		jsonData, err := common.Marshal(convertedRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeJsonMarshalFailed, types.ErrOptionWithSkipRetry())
		}

		body, closer, apiErr := buildUpstreamRequestBody(c, info, jsonData, true, "text request body")
		if apiErr != nil {
			return apiErr
		}
		defer closer.Close()
		requestBody = body
	}

	usage, newApiErr := doUpstreamRoundTrip(c, info, adaptor, requestBody, true)
	if newApiErr != nil {
		return newApiErr
	}

	usageDto, newApiErr := adaptorUsage(usage)
	if newApiErr != nil {
		return newApiErr
	}
	if newApiErr = requireDeliveredOutput(usageDto); newApiErr != nil {
		return newApiErr
	}

	postAudioOrTextConsumeQuota(c, info, usageDto)
	return nil
}

// postAudioOrTextConsumeQuota settles a chat/completions response as audio usage
// when the usage carries audio tokens and the model has an audio ratio
// configured; otherwise it settles as ordinary text usage.
func postAudioOrTextConsumeQuota(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage) {
	containAudioTokens := usage.CompletionTokenDetails.AudioTokens > 0 || usage.PromptTokensDetails.AudioTokens > 0
	containsAudioRatios := ratio_setting.ContainsAudioRatio(info.OriginModelName) || ratio_setting.ContainsAudioCompletionRatio(info.OriginModelName)
	if containAudioTokens && containsAudioRatios {
		service.PostAudioConsumeQuota(c, info, usage, "")
	} else {
		service.PostTextConsumeQuota(c, info, usage, nil)
	}
}

// buildUpstreamRequestBody finalizes an already-marshaled upstream request body:
// optionally strips channel-disabled fields, applies any configured param
// override, logs the outgoing body under debugLabel, and wraps it for transport.
// It sets info.UpstreamRequestBodySize and returns the body reader together with
// the closer the caller must defer-close.
func buildUpstreamRequestBody(c *gin.Context, info *relaycommon.RelayInfo, jsonData []byte, removeDisabledFields bool, debugLabel string) (io.Reader, io.Closer, *types.NewAPIError) {
	var err error
	if removeDisabledFields {
		jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
		if err != nil {
			return nil, nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
	}
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, nil, newAPIErrorFromParamOverride(err)
		}
	}
	logger.LogDebug(c, "%s: %s", debugLabel, jsonData)
	body, size, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return nil, nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	info.UpstreamRequestBodySize = size
	return body, closer, nil
}

// doUpstreamRoundTrip sends the finalized request body through the adaptor and
// hands the upstream response back to the adaptor for parsing. A non-200
// upstream status becomes a relay error; that error and any DoResponse error
// both get the channel's status code mapping applied. With detectEventStream
// set, a text/event-stream upstream response switches the relay to stream mode
// before the status check.
func doUpstreamRoundTrip(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor, requestBody io.Reader, detectEventStream bool) (any, *types.NewAPIError) {
	var resp any
	var err error
	var rememberReasoningFallback func()
	if info.RelayMode == relayconstant.RelayModeResponses && info.ChannelSetting.ResponsesReasoningFallback {
		resp, rememberReasoningFallback, err = doResponsesReasoningFallback(c, info, adaptor, requestBody)
	} else {
		resp, err = adaptor.DoRequest(c, info, requestBody)
	}
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		if detectEventStream {
			info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		}
		if httpResp.StatusCode != http.StatusOK {
			newAPIError := service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return nil, newAPIError
		}
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return nil, newAPIError
	}
	// A partially delivered failed stream can return usage without an error so
	// billing still settles. It is not evidence of a completed recovery.
	if rememberReasoningFallback != nil && (!info.IsStream ||
		(info.StreamStatus.IsNormalEnd() && info.StreamStatus.FailureError() == nil)) {
		rememberReasoningFallback()
	}
	return usage, nil
}
