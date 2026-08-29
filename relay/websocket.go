package relay

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func WssHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	//var requestBody io.Reader
	//firstWssRequest, _ := c.Get("first_wss_request")
	//requestBody = bytes.NewBuffer(firstWssRequest.([]byte))

	statusCodeMappingStr := c.GetString("status_code_mapping")
	resp, err := adaptor.DoRequest(c, info, nil)
	if err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed)
	}

	if resp != nil {
		info.TargetWs = resp.(*websocket.Conn)
		defer info.TargetWs.Close()
	}

	usage, newAPIError := adaptor.DoResponse(c, nil, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}
	// Same typed-nil hazard as adaptorUsage guards for the text paths: DoResponse
	// hands usage back as `any`, so a nil *dto.RealtimeUsage arrives as a non-nil
	// interface and PostWssConsumeQuota would deref it. A panic here unwinds past
	// controller.Relay's deferred refund, which only refunds when its error is
	// non-nil, so the pre-consumed quota would be silently kept.
	realtimeUsage, ok := usage.(*dto.RealtimeUsage)
	if !ok || realtimeUsage == nil {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("realtime adaptor returned no usage (%T)", usage),
			types.ErrorCodeEmptyResponse,
			http.StatusInternalServerError,
		)
	}
	service.PostWssConsumeQuota(c, info, info.UpstreamModelName, realtimeUsage, "")
	return nil
}
