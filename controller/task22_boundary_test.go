package controller

import (
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryBudgetBoundary(t *testing.T) {
	for _, budget := range []int{0, -1, -2147483648} {
		c := newRetryTestContext(t)
		err := types.NewOpenAIError(errors.New("temporary upstream failure"), types.ErrorCodeBadResponseStatusCode, 503)
		assert.False(t, shouldRetry(c, err, budget))
		assert.False(t, shouldRetryBoundKeyEnvironment(c, err, budget))
	}
}

func TestAffinityInvalidPinAndTTLBoundary(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, ttl := range []int{0, -1} {
			for _, deleted := range []bool{false, true} {
				t.Run(fmt.Sprintf("cache=%t/ttl=%d/deleted=%t", memory, ttl, deleted), func(t *testing.T) {
					const name = "gpt-task22-pin"
					channels := ordinarySelectionChannels(t, memory, name)
					settings := operation_setting.GetChannelAffinitySetting()
					settings.DefaultTTLSeconds = ttl
					for i := range settings.Rules {
						settings.Rules[i].TTLSeconds = ttl
					}
					body := `{"model":"gpt-task22-pin","prompt_cache_key":"task22-session","input":"hi"}`
					cold := ordinarySelectionRequest(t, "/v1/responses", body)
					_, found := service.GetPreferredChannelByAffinity(cold, name, "default")
					require.False(t, found)
					service.SetChannelAffinityRelayOutcome(cold, true)
					service.RecordChannelAffinity(cold, channels[0].Id)
					warm := ordinarySelectionRequest(t, "/v1/responses", body)
					pinned, found := service.GetPreferredChannelByAffinity(warm, name, "default")
					require.True(t, found)
					assert.Equal(t, channels[0].Id, pinned, "nonpositive TTL falls back to one hour")
					if deleted {
						require.NoError(t, model.DB.Delete(channels[0]).Error)
					} else {
						require.NoError(t, model.DB.Model(channels[0]).Update("status", common.ChannelStatusAutoDisabled).Error)
					}
					model.InitChannelCache()
					next := ordinarySelectionRequest(t, "/v1/responses", body)
					_, found = service.GetPreferredChannelByAffinity(next, name, "default")
					assert.False(t, found, "disabled/deleted pin cannot serve")
				})
			}
		}
	}
}

func TestWebSocketOversizedHandshakeHeaderBoundary(t *testing.T) {
	f := newIndependentWebsocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversized downstream header reached upstream")
		w.WriteHeader(503)
	}))
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial(strings.Replace(f.server.URL, "http://", "ws://", 1)+"/v1/realtime?model="+url.QueryEscape(f.model), http.Header{"X-Boundary-Large": {strings.Repeat("x", 2*1024*1024)}})
	require.Error(t, err)
	assert.Nil(t, conn)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode)
}

func TestNegativeConfiguredRetryBudgetBoundary(t *testing.T) {
	for _, budget := range []int{0, -1} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			var calls atomic.Int32
			f := newIndependentWebsocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"message":"budget-boundary","type":"upstream_error"}}`))
			}))
			common.RetryTimes = budget
			conn, resp, err := f.dial(t)
			require.Error(t, err)
			assert.Nil(t, conn)
			require.NotNil(t, resp)
			defer resp.Body.Close()
			assert.Equal(t, 503, resp.StatusCode, "negative configuration disables retries, preserving one initial attempt")
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}
