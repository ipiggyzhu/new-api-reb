package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpproxy"
)

// upstreamErrorBodyReadLimit caps how much of an upstream error body is read
// into memory. Real error payloads are a few hundred bytes of JSON; the largest
// legitimate ones seen in practice are Cloudflare block pages at roughly 8 KB.
// 1 MB leaves several orders of magnitude of headroom while still bounding a
// hostile upstream.
const upstreamErrorBodyReadLimit = 1 << 20

func MidjourneyErrorWrapper(code int, desc string) *dto.MidjourneyResponse {
	return &dto.MidjourneyResponse{
		Code:        code,
		Description: desc,
	}
}

func MidjourneyErrorWithStatusCodeWrapper(code int, desc string, statusCode int) *dto.MidjourneyResponseWithStatusCode {
	return &dto.MidjourneyResponseWithStatusCode{
		StatusCode: statusCode,
		Response:   *MidjourneyErrorWrapper(code, desc),
	}
}

//// OpenAIErrorWrapper wraps an error into an OpenAIErrorWithStatusCode
//func OpenAIErrorWrapper(err error, code string, statusCode int) *dto.OpenAIErrorWithStatusCode {
//	text := err.Error()
//	lowerText := strings.ToLower(text)
//	if !strings.HasPrefix(lowerText, "get file base64 from url") && !strings.HasPrefix(lowerText, "mime type is not supported") {
//		if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
//			common.SysLog(fmt.Sprintf("error: %s", text))
//			text = "请求上游地址失败"
//		}
//	}
//	openAIError := dto.OpenAIError{
//		Message: text,
//		Type:    "new_api_error",
//		Code:    code,
//	}
//	return &dto.OpenAIErrorWithStatusCode{
//		Error:      openAIError,
//		StatusCode: statusCode,
//	}
//}
//
//func OpenAIErrorWrapperLocal(err error, code string, statusCode int) *dto.OpenAIErrorWithStatusCode {
//	openaiErr := OpenAIErrorWrapper(err, code, statusCode)
//	openaiErr.LocalError = true
//	return openaiErr
//}

func ClaudeErrorWrapper(err error, code string, statusCode int) *dto.ClaudeErrorWithStatusCode {
	text := err.Error()
	lowerText := strings.ToLower(text)
	if !strings.HasPrefix(lowerText, "get file base64 from url") {
		if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
			common.SysLog(fmt.Sprintf("error: %s", text))
			text = "请求上游地址失败"
		}
	}
	claudeError := types.ClaudeError{
		Message: text,
		Type:    "new_api_error",
	}
	return &dto.ClaudeErrorWithStatusCode{
		Error:      claudeError,
		StatusCode: statusCode,
	}
}

func ClaudeErrorWrapperLocal(err error, code string, statusCode int) *dto.ClaudeErrorWithStatusCode {
	claudeErr := ClaudeErrorWrapper(err, code, statusCode)
	claudeErr.LocalError = true
	return claudeErr
}

func RelayErrorHandler(ctx context.Context, resp *http.Response, showBodyWhenFail bool) (newApiErr *types.NewAPIError) {
	newApiErr = types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, resp.StatusCode)
	// Every return below replaces newApiErr, so the hint is attached last.
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
		defer func() {
			newApiErr.RetryAfter = UpstreamRetryAfter(resp.Header, time.Now())
		}()
	}
	statusMessage := fmt.Sprintf("bad response status code %d", resp.StatusCode)
	if resp.StatusCode == 524 {
		statusMessage = "upstream response timed out (HTTP 524)"
		newApiErr.Err = errors.New(statusMessage)
	}

	defer CloseResponseBodyGracefully(resp)
	// Bounded: this is an *error* body from an upstream we do not control, and
	// every byte of it is buffered in memory before anything else happens. A
	// misbehaving or hostile upstream answering a 500 with a multi-gigabyte body
	// would otherwise be able to OOM the gateway one failed request at a time.
	// Nothing downstream needs more than the limit — the message is parsed from
	// the first JSON object and the admin copy is capped at
	// types.UpstreamBodyLogLimit.
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyReadLimit))
	if err != nil {
		return
	}
	var errResponse dto.GeneralErrorResponse
	responseBodyText := string(responseBody)
	responseBodyPreview := common.LocalLogPreview(responseBodyText)
	buildErrWithBody := func(message string) error {
		if message == "" {
			return fmt.Errorf("%s, body: %s", statusMessage, responseBodyText)
		}
		return fmt.Errorf("%s, message: %s, body: %s", statusMessage, message, responseBodyText)
	}

	err = common.Unmarshal(responseBody, &errResponse)
	if err != nil {
		// The body is not JSON at all: a Cloudflare block page, an nginx 502, a
		// plaintext "insufficient balance". Nothing of it survives into the
		// message, so keep a bounded copy for the admin error log — otherwise
		// this is recorded as nothing but "bad response status code NNN".
		newApiErr.SetUpstreamBody(responseBodyText)
		if showBodyWhenFail {
			newApiErr.Err = buildErrWithBody("")
		} else {
			logger.LogError(ctx, fmt.Sprintf("bad response status code %d, body: %s", resp.StatusCode, responseBodyPreview))
			newApiErr.Err = errors.New(statusMessage)
		}
		return
	}

	if common.GetJsonType(errResponse.Error) == "object" {
		// General format error (OpenAI, Anthropic, Gemini, etc.)
		oaiError := errResponse.TryToOpenAIError()
		if oaiError != nil {
			newApiErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
			if showBodyWhenFail {
				newApiErr.Err = buildErrWithBody(newApiErr.Error())
			}
			return
		}
	}
	// Valid JSON, but none of the shapes ToMessage knows about. Whatever it did
	// not recognise is only in the raw body, so keep that for the admin log too.
	message := errResponse.ToMessage()
	newApiErr = types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponseStatusCode, resp.StatusCode)
	if message == "" {
		newApiErr.SetUpstreamBody(responseBodyText)
		if resp.StatusCode == 524 {
			newApiErr.Err = errors.New(statusMessage)
		}
	}
	if showBodyWhenFail {
		newApiErr.Err = buildErrWithBody(newApiErr.Error())
	}
	return
}

func ResetStatusCode(newApiErr *types.NewAPIError, statusCodeMappingStr string) {
	if newApiErr == nil {
		return
	}
	if statusCodeMappingStr == "" || statusCodeMappingStr == "{}" {
		return
	}
	statusCodeMapping := make(map[string]any)
	err := common.Unmarshal([]byte(statusCodeMappingStr), &statusCodeMapping)
	if err != nil {
		return
	}
	if newApiErr.StatusCode == http.StatusOK {
		return
	}
	codeStr := strconv.Itoa(newApiErr.StatusCode)
	if value, ok := statusCodeMapping[codeStr]; ok {
		intCode, ok := parseStatusCodeMappingValue(value)
		if !ok {
			return
		}
		newApiErr.StatusCode = intCode
	}
}

func parseStatusCodeMappingValue(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return 0, false
		}
		statusCode, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return statusCode, true
	case float64:
		if v != math.Trunc(v) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case json.Number:
		statusCode, err := strconv.Atoi(v.String())
		if err != nil {
			return 0, false
		}
		return statusCode, true
	default:
		return 0, false
	}
}

// presetUpstreamHosts are the hosts, with any port, of the default base URLs
// new-api presets for channel types. They are public endpoints, so errors keep
// them readable. The port keeps a configured localhost upstream apart from the
// Ollama preset.
var presetUpstreamHosts = sync.OnceValue(func() map[string]struct{} {
	hosts := make(map[string]struct{}, len(constant.ChannelBaseURLs))
	for _, baseURL := range constant.ChannelBaseURLs {
		if parsed, err := url.Parse(baseURL); err == nil && parsed.Host != "" {
			hosts[strings.ToLower(parsed.Host)] = struct{}{}
		}
	}
	return hosts
})

// isPresetUpstreamHost reports whether address points at one of the preset
// default base URL hosts.
func isPresetUpstreamHost(address string) bool {
	rawURL := strings.TrimSpace(address)
	if !strings.Contains(rawURL, "://") {
		rawURL = "//" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	_, preset := presetUpstreamHosts()[strings.ToLower(parsed.Host)]
	return preset
}

// UpstreamAddresses returns the addresses masked in errors shown to users: the
// upstream the admin configured for the selected channel and the proxies its
// requests may go through. Preset default base URLs are public and stay
// readable. The origin actually requested only counts for a configured base
// URL or an Advanced Custom channel, whose routes may target absolute URLs;
// other adaptors build official endpoints themselves.
func UpstreamAddresses(c *gin.Context) []string {
	// A channel without its own proxy dials through the environment proxy.
	environmentProxy := httpproxy.FromEnvironment()
	candidates := []string{environmentProxy.HTTPProxy, environmentProxy.HTTPSProxy}
	if setting, ok := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting); ok {
		candidates = append(candidates, setting.Proxy)
	}
	baseURL := strings.TrimSpace(common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl))
	candidates = append(candidates, baseURL)
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeAdvancedCustom || (baseURL != "" && !isPresetUpstreamHost(baseURL)) {
		candidates = append(candidates, common.GetContextKeyString(c, constant.ContextKeyUpstreamRequestOrigin))
	}
	addresses := make([]string, 0, len(candidates))
	for _, address := range candidates {
		if strings.TrimSpace(address) != "" && !isPresetUpstreamHost(address) {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

func TaskErrorWrapperLocal(err error, code string, statusCode int) *dto.TaskError {
	openaiErr := TaskErrorWrapper(err, code, statusCode)
	openaiErr.LocalError = true
	return openaiErr
}

func TaskErrorWrapper(err error, code string, statusCode int) *dto.TaskError {
	text := err.Error()
	lowerText := strings.ToLower(text)
	if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
		common.SysLog(fmt.Sprintf("error: %s", text))
		//text = "请求上游地址失败"
		text = common.MaskSensitiveInfo(text)
	}
	//避免暴露内部错误
	taskError := &dto.TaskError{
		Code:       code,
		Message:    text,
		StatusCode: statusCode,
		Error:      err,
	}

	return taskError
}

// TaskErrorFromAPIError 将 PreConsumeBilling 返回的 NewAPIError 转换为 TaskError。
func TaskErrorFromAPIError(apiErr *types.NewAPIError) *dto.TaskError {
	if apiErr == nil {
		return nil
	}
	return &dto.TaskError{
		Code:       string(apiErr.GetErrorCode()),
		Message:    apiErr.Err.Error(),
		StatusCode: apiErr.StatusCode,
		LocalError: true,
		Error:      apiErr.Err,
	}
}
