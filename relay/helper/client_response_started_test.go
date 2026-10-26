package helper

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Same-channel re-attempts (the Claude thinking rectifier, the Responses
// reasoning fallback) may only run while the client has received nothing it
// would read. Pings written while waiting for the pinned channel are SSE
// comments, so they alone must not count as a started response; every other
// write — including relay keepalive pings, as before — does.
func TestClientResponseStarted(t *testing.T) {
	cases := []struct {
		name      string
		waitPings int
		write     func(c *gin.Context)
		want      bool
	}{
		{name: "nothing written", write: func(*gin.Context) {}, want: false},
		{name: "relay keepalive ping", write: func(c *gin.Context) { require.NoError(t, PingData(c)) }, want: true},
		{
			name: "only affinity wait pings", waitPings: 2,
			write: func(c *gin.Context) {
				require.NoError(t, PingData(c))
				require.NoError(t, PingData(c))
			},
			want: false,
		},
		{
			name: "payload after affinity wait pings", waitPings: 1,
			write: func(c *gin.Context) {
				require.NoError(t, PingData(c))
				require.NoError(t, StringData(c, `{"type":"message_start"}`))
			},
			want: true,
		},
		{
			name: "relay ping after affinity wait pings", waitPings: 1,
			write: func(c *gin.Context) {
				require.NoError(t, PingData(c))
				require.NoError(t, PingData(c))
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			tc.write(c)
			if tc.waitPings > 0 {
				common.SetContextKey(c, constant.ContextKeyAffinityWaitPings, tc.waitPings)
			}
			assert.Equal(t, tc.want, ClientResponseStarted(c))
		})
	}
}
