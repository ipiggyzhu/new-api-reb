package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}
	responseJSON := gjson.ParseBytes(responseBody)
	if !responsesResponseHasOutput(responseJSON) && !responsesResponseIsDeferred(responseJSON) {
		return nil, emptyOutputError()
	}

	if responsesResponse.HasImageGenerationCall() {
		c.Set("image_generation_call", true)
		c.Set("image_generation_call_quality", responsesResponse.GetQuality())
		c.Set("image_generation_call_size", responsesResponse.GetSize())
	}

	// compute usage
	usage := dto.Usage{ResponseValidated: true}
	if responsesResponse.Usage != nil {
		usage.PromptTokens = responsesResponse.Usage.InputTokens
		usage.CompletionTokens = responsesResponse.Usage.OutputTokens
		usage.TotalTokens = responsesResponse.Usage.TotalTokens
		if responsesResponse.Usage.InputTokensDetails != nil {
			usage.PromptTokensDetails.CachedTokens = responsesResponse.Usage.InputTokensDetails.CachedTokens
			usage.PromptTokensDetails.CacheWriteTokens = responsesResponse.Usage.InputTokensDetails.CacheWriteTokens
		}
	}
	service.IOCopyBytesGracefully(c, resp, responseBody)
	if info == nil || info.ResponsesUsageInfo == nil || info.ResponsesUsageInfo.BuiltInTools == nil {
		return &usage, nil
	}
	// 解析 Tools 用量
	for _, tool := range responsesResponse.Tools {
		buildToolinfo, ok := info.ResponsesUsageInfo.BuiltInTools[common.Interface2String(tool["type"])]
		if !ok || buildToolinfo == nil {
			logger.LogError(c, fmt.Sprintf("BuiltInTools not found for tool type: %v", tool["type"]))
			continue
		}
		buildToolinfo.CallCount++
	}
	return &usage, nil
}

func responsesStreamError(streamResponse dto.ResponsesStreamResponse, data string) *types.NewAPIError {
	var upstreamError *types.OpenAIError
	if streamResponse.Response != nil {
		upstreamError = streamResponse.Response.GetOpenAIError()
	}
	if upstreamError == nil {
		if rawError := gjson.Get(data, "error"); rawError.Exists() && rawError.Type != gjson.Null {
			var errorField any
			if err := common.UnmarshalJsonStr(rawError.Raw, &errorField); err != nil {
				return types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
			}
			upstreamError = dto.GetOpenAIError(errorField)
		}
	}
	if upstreamError == nil {
		switch streamResponse.Type {
		case "response.failed", "response.error", "error":
			upstreamError = &types.OpenAIError{}
			if err := common.UnmarshalJsonStr(data, upstreamError); err != nil {
				return types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
			}
		default:
			return nil
		}
	}
	if upstreamError.Message == "" {
		upstreamError.Message = "upstream responses stream returned an error event"
	}
	return types.WithOpenAIError(*upstreamError, http.StatusBadGateway)
}

func updateResponsesStreamUsage(usage *dto.Usage, response *dto.OpenAIResponsesResponse) {
	if response == nil || response.Usage == nil {
		return
	}
	if response.Usage.InputTokens != 0 {
		usage.PromptTokens = response.Usage.InputTokens
	}
	if response.Usage.OutputTokens != 0 {
		usage.CompletionTokens = response.Usage.OutputTokens
	}
	if response.Usage.TotalTokens != 0 {
		usage.TotalTokens = response.Usage.TotalTokens
	}
	if response.Usage.InputTokensDetails != nil {
		usage.PromptTokensDetails.CachedTokens = response.Usage.InputTokensDetails.CachedTokens
		usage.PromptTokensDetails.CacheWriteTokens = response.Usage.InputTokensDetails.CacheWriteTokens
	}
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	var hasOutput, streamComplete bool
	var preamble outputPreamble
	var outputErr *types.NewAPIError
	var upstreamErr *types.NewAPIError

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}
		// Error events are failures, not lifecycle frames waiting for output.
		// After delivery, keep the original event and usage so the caller can
		// settle the partial response before reporting its failed stream status.
		if upstreamErr = responsesStreamError(streamResponse, data); upstreamErr != nil {
			if hasOutput {
				updateResponsesStreamUsage(usage, streamResponse.Response)
				sendResponsesStreamData(c, streamResponse, data)
			}
			errBody := upstreamErr.ToOpenAIError()
			sr.Stop(fmt.Errorf("%s (%v): %s", errBody.Type, errBody.Code, upstreamErr.Error()))
			return
		}
		hasOutput = hasOutput || responsesStreamEventHasOutput(gjson.Parse(data))
		if isResponsesTerminalEvent(streamResponse.Type) {
			if !hasOutput {
				outputErr = emptyOutputError()
				sr.Stop(outputErr)
				return
			}
			streamComplete = true
		}
		if !hasOutput {
			if !preamble.add(data) {
				outputErr = emptyOutputError()
				sr.Stop(outputErr)
			}
			return
		}
		for _, pending := range preamble.chunks {
			pendingResponse := dto.ResponsesStreamResponse{Type: gjson.Get(pending, "type").String()}
			sendResponsesStreamData(c, pendingResponse, pending)
		}
		preamble = outputPreamble{}
		sendResponsesStreamData(c, streamResponse, data)
		switch streamResponse.Type {
		case "response.completed", "response.done", "response.incomplete":
			updateResponsesStreamUsage(usage, streamResponse.Response)
			if streamResponse.Response != nil {
				if streamResponse.Response.HasImageGenerationCall() {
					c.Set("image_generation_call", true)
					c.Set("image_generation_call_quality", streamResponse.Response.GetQuality())
					c.Set("image_generation_call_size", streamResponse.Response.GetSize())
				}
			}
		case "response.output_text.delta":
			// 处理输出文本
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			// 函数调用处理
			if streamResponse.Item != nil {
				switch streamResponse.Item.Type {
				case dto.BuildInCallWebSearchCall:
					if info != nil && info.ResponsesUsageInfo != nil && info.ResponsesUsageInfo.BuiltInTools != nil {
						if webSearchTool, exists := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists && webSearchTool != nil {
							webSearchTool.CallCount++
						}
					}
				}
			}
		}
	})

	// Before any terminator reaches the client: once something is on the wire
	// shouldRetry can no longer move the request to another channel.
	if streamErr := info.StreamStatus.NoStreamBodyError(); streamErr != nil {
		return nil, streamErr
	}
	if upstreamErr != nil && !hasOutput {
		return nil, upstreamErr
	}
	if outputErr != nil {
		return nil, outputErr
	}
	if !hasOutput {
		return nil, emptyOutputError()
	}
	if !streamComplete || upstreamErr != nil {
		// The scanner has joined here; its end reason must not be read from
		// the callback while the scanner may still be setting it.
		info.StreamStatus.MarkMissingTerminator()
	}

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	usage.ResponseValidated = true

	return usage, nil
}
