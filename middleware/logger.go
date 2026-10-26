package middleware

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

// rotationAwareLogWriter forwards access logs to whichever writer gin.DefaultWriter
// currently points at. gin's LoggerWithConfig captures its output writer once, when
// the middleware is built, but log rotation swaps gin.DefaultWriter and closes the
// previous log file. Without this indirection the captured writer would keep pointing
// at a closed file descriptor and every [GIN] line would be silently dropped after the
// first rotation. The read lock mirrors common.SysLog so a rotation can never swap and
// close the file in the middle of a write.
type rotationAwareLogWriter struct{}

func (rotationAwareLogWriter) Write(p []byte) (int, error) {
	common.LogWriterMu.RLock()
	defer common.LogWriterMu.RUnlock()
	return gin.DefaultWriter.Write(p)
}

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		Output: rotationAwareLogWriter{},
		Formatter: func(param gin.LogFormatterParams) string {
			var requestID string
			if param.Keys != nil {
				requestID, _ = param.Keys[common.RequestIdKey].(string)
			}
			tag, _ := param.Keys[RouteTagKey].(string)
			if tag == "" {
				tag = "web"
			}
			// gin's Path carries the raw query, and callers put credentials there:
			// Gemini clients send ?key=sk-..., OAuth callbacks send one-time code and
			// state. Only the logged copy is redacted; handlers read the original.
			path, rawQuery, hasQuery := strings.Cut(param.Path, "?")
			if hasQuery && !strings.HasPrefix(path, "/api/oauth/") && !strings.HasPrefix(path, "/oauth/") {
				// A query that does not parse cleanly is dropped rather than logged
				// half-masked: a malformed pair could still hide a credential.
				if query, err := url.ParseQuery(rawQuery); err == nil {
					path += relaycommon.SanitizeURLForLog("?" + query.Encode())
				}
			}
			return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
				param.TimeStamp.Format("2006/01/02 - 15:04:05"),
				tag,
				requestID,
				param.StatusCode,
				param.Latency,
				param.ClientIP,
				param.Method,
				path,
			)
		},
	}))
}
