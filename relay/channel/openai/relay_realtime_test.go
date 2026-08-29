package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenaiRealtimeHandlerReturnsWhenOnePeerIsSilent pins the exit guarantee
// that OpenaiRealtimeHandler's join depends on. The handler waits for both
// reader goroutines before reading the usage accumulators, but only one of them
// observes the event that ends the session — here the client hangs up while the
// upstream stays connected and never sends a frame, so the target reader is
// parked in ReadMessage on a peer that will never speak. If the handler stops
// forcing that read to fail, this call never returns and the test binary dies on
// its own timeout. There is deliberately no watchdog: hanging forever is the
// only failure mode, and a timer here would just be a flaky restatement of it.
func TestOpenaiRealtimeHandlerReturnsWhenOnePeerIsSilent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Upgrades and then holds the connection open without ever reading or
	// writing, so the dialed side blocks in ReadMessage indefinitely.
	upgrader := websocket.Upgrader{}
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-held
	}))
	t.Cleanup(func() {
		close(held)
		server.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	targetConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { targetConn.Close() })

	// The client hangs up before the handler starts, so the client reader ends
	// immediately and the target reader is the one left blocked.
	require.NoError(t, clientConn.Close())

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)

	apiErr, usage := OpenaiRealtimeHandler(c, &relaycommon.RelayInfo{
		ClientWs: clientConn,
		TargetWs: targetConn,
	})

	assert.Nil(t, apiErr)
	require.NotNil(t, usage)
	// Neither reader ever decoded a frame, so nothing was billable.
	assert.Zero(t, usage.TotalTokens)
}
