package middleware

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func abortWithOpenAiMessage(c *gin.Context, statusCode int, message string, code ...types.ErrorCode) {
	codeStr := ""
	if len(code) > 0 {
		codeStr = string(code[0])
	}
	userId := c.GetInt("id")
	errorBody := gin.H{
		"message": common.MessageWithRequestId(message, c.GetString(common.RequestIdKey)),
		"type":    "new_api_error",
		"code":    codeStr,
	}
	if c.Writer.Written() && c.Writer.Header().Get("Content-Type") == "text/event-stream" {
		// Keepalive pings sent while waiting for the pinned channel already
		// committed a 200 event stream, and a JSON body appended to it is a line
		// no SSE parser reads. An `error` event is what Anthropic SDKs raise on,
		// and its top-level "error" object is what OpenAI SDKs raise on.
		if eventJson, err := common.Marshal(gin.H{"type": "error", "error": errorBody}); err == nil {
			helper.ClaudeChunkData(c, dto.ClaudeResponse{Type: "error"}, string(eventJson))
		}
	} else {
		c.JSON(statusCode, gin.H{"error": errorBody})
	}
	c.Abort()
	logger.LogError(c.Request.Context(), fmt.Sprintf("user %d | %s", userId, message))
}

func abortWithMidjourneyMessage(c *gin.Context, statusCode int, code int, description string) {
	c.JSON(statusCode, gin.H{
		"description": description,
		"type":        "new_api_error",
		"code":        code,
	})
	c.Abort()
	logger.LogError(c.Request.Context(), description)
}
