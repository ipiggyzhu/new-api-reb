package service

import (
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReasoningLogsPreserveOriginalSettingsOnSuccessAndFailure(t *testing.T) {
	cases := []struct {
		name       string
		request    *relaycommon.RequestReasoning
		effort     string
		wantEffort string
	}{
		{name: "model-derived effort without request settings", effort: "high", wantEffort: "high"},
		{name: "empty request effort preserves model-derived effort", request: &relaycommon.RequestReasoning{Mode: "adaptive"}, effort: "high", wantEffort: "high"},
		{name: "explicit none wins", request: &relaycommon.RequestReasoning{Effort: "none", Mode: "adaptive"}, effort: "high", wantEffort: "none"},
		{name: "explicit max wins", request: &relaycommon.RequestReasoning{Effort: "max", Mode: "adaptive"}, effort: "high", wantEffort: "max"},
		{name: "missing effort stays absent", request: &relaycommon.RequestReasoning{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, ReasoningEffort: tc.effort, UpstreamReasoningEffort: "low", RequestReasoning: tc.request}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			require.NotNil(t, c)
			success := GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 0, 1)
			failure := map[string]interface{}{}
			AppendRelayErrorStreamInfo(info, failure)
			for _, other := range []map[string]interface{}{success, failure} {
				if tc.request == nil {
					assert.NotContains(t, other, "request_reasoning")
				} else {
					assert.Equal(t, tc.request, other["request_reasoning"])
				}
				assert.Equal(t, "low", other["upstream_reasoning_effort"])
				if tc.wantEffort == "" {
					assert.NotContains(t, other, "reasoning_effort")
				} else {
					assert.Equal(t, tc.wantEffort, other["reasoning_effort"])
				}
			}
		})
	}
}
