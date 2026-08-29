package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRetryTestContext(t *testing.T) *gin.Context {
	t.Helper()
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func TestGetChannelRecordsDistributorSelectedChannelForRetry(t *testing.T) {
	c := newRetryTestContext(t)
	c.Set("channel_id", 42)
	c.Set("channel_type", 8)
	c.Set("channel_name", "primary")
	c.Set("auto_ban", true)
	common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, true)

	retryParam := &service.RetryParam{Ctx: c}
	channel, apiErr := getChannel(c, &relaycommon.RelayInfo{}, retryParam)

	require.Nil(t, apiErr)
	require.NotNil(t, channel)
	assert.Equal(t, 42, channel.Id)
	assert.True(t, channel.ChannelInfo.IsMultiKey)
	assert.True(t, retryParam.ExcludedChannelIds[42],
		"the channel selected by the distributor must be skipped by the next retry")
}

// A mid-stream upstream failure arrives as a plain retryable 500 (Anthropic's
// overloaded_error is the common case). Retrying it once the caller has already
// received part of the stream would append a second, complete response to the
// partial one, so a response that has begun must never be retried.
func TestShouldRetryStopsOnceResponseBytesAreCommitted(t *testing.T) {
	upstreamMidStreamError := types.WithClaudeError(
		types.ClaudeError{Type: "overloaded_error", Message: "Overloaded"},
		http.StatusInternalServerError,
	)

	fresh := newRetryTestContext(t)
	require.False(t, fresh.Writer.Written(), "nothing written yet")
	assert.True(t, shouldRetry(fresh, upstreamMidStreamError, 3),
		"a retryable upstream error must still retry while the response is untouched")

	started := newRetryTestContext(t)
	_, err := started.Writer.WriteString("data: {\"type\":\"content_block_delta\"}\n\n")
	require.NoError(t, err)
	require.True(t, started.Writer.Written())
	assert.False(t, shouldRetry(started, upstreamMidStreamError, 3),
		"the caller is mid-stream; a retry would concatenate two responses")
}

// With PingIntervalEnabled the keepalive pinger usually writes ": PING\n\n"
// before a slow upstream fails. Those frames are SSE comments the client
// discards, so a retry on another channel can still deliver a well-formed
// stream and must not be blocked by them.
func TestShouldRetrySurvivesKeepalivePingFrames(t *testing.T) {
	upstreamErr := types.WithClaudeError(
		types.ClaudeError{Type: "overloaded_error", Message: "Overloaded"},
		http.StatusInternalServerError,
	)

	pinged := newRetryTestContext(t)
	pinged.Writer = &pingAwareWriter{ResponseWriter: pinged.Writer}
	// Go through helper.PingData so this test breaks if the real ping frame
	// ever diverges from the one the retry guard recognises.
	require.NoError(t, helper.PingData(pinged))
	require.NoError(t, helper.PingData(pinged))
	require.True(t, pinged.Writer.Written(), "pings commit the response")
	assert.True(t, shouldRetry(pinged, upstreamErr, 3),
		"keepalive frames alone must not consume the request's retries")
}

func TestShouldRetryStopsOncePayloadFollowsPings(t *testing.T) {
	upstreamErr := types.WithClaudeError(
		types.ClaudeError{Type: "overloaded_error", Message: "Overloaded"},
		http.StatusInternalServerError,
	)

	mixed := newRetryTestContext(t)
	mixed.Writer = &pingAwareWriter{ResponseWriter: mixed.Writer}
	require.NoError(t, helper.PingData(mixed))
	_, err := mixed.Writer.Write([]byte("data: {\"choices\":[]}\n\n"))
	require.NoError(t, err)
	assert.False(t, shouldRetry(mixed, upstreamErr, 3),
		"payload after a ping means the caller holds a partial response")

	viaString := newRetryTestContext(t)
	viaString.Writer = &pingAwareWriter{ResponseWriter: viaString.Writer}
	require.NoError(t, helper.PingData(viaString))
	_, err = viaString.Writer.WriteString("data: {\"choices\":[]}\n\n")
	require.NoError(t, err)
	assert.False(t, shouldRetry(viaString, upstreamErr, 3),
		"WriteString payload must count as real data too")
}

// The "channel:" namespace mixes genuinely retryable upstream faults
// (channel:invalid_key on a dead key) with configuration errors raised with
// ErrOptionWithSkipRetry (channel:model_mapped_error), so membership in that
// namespace cannot be the first thing consulted. Every guard that means "do not
// retry" — an explicit skip-retry tag, an exhausted budget, a token pinned to one
// channel — has to answer before it. Judging the prefix first burned the whole
// retry budget on errors that had already asked not to be retried, and rerouted a
// sk-xxx-<channelId> token onto a channel its caller never selected.
func TestShouldRetryGuardsOutrankChannelErrorClass(t *testing.T) {
	cases := []struct {
		name       string
		err        *types.NewAPIError
		setup      func(*gin.Context)
		retryTimes int
		want       bool
		reason     string
	}{
		{
			name: "skip-retry tag on a channel error",
			err: types.NewErrorWithStatusCode(
				errors.New("model mapped error: no target model for gpt-4o"),
				types.ErrorCodeChannelModelMappedError,
				http.StatusInternalServerError,
				types.ErrOptionWithSkipRetry(),
			),
			retryTimes: 3,
			want:       false,
			reason:     "an error that asked not to be retried must not spend the retry budget",
		},
		{
			name: "token pinned to one channel",
			err: types.NewErrorWithStatusCode(
				errors.New("invalid api key"),
				types.ErrorCodeChannelInvalidKey,
				http.StatusUnauthorized,
			),
			setup:      func(c *gin.Context) { c.Set("specific_channel_id", 42) },
			retryTimes: 3,
			want:       false,
			reason:     "a sk-xxx-<channelId> token must fail on its channel, not be rerouted",
		},
		{
			name: "retry budget already exhausted",
			err: types.NewErrorWithStatusCode(
				errors.New("invalid api key"),
				types.ErrorCodeChannelInvalidKey,
				http.StatusUnauthorized,
			),
			retryTimes: 0,
			want:       false,
			reason:     "no retries left means no retry, whatever class the error belongs to",
		},
		{
			name: "channel fault with no guard hit",
			err: types.NewErrorWithStatusCode(
				errors.New("invalid api key"),
				types.ErrorCodeChannelInvalidKey,
				http.StatusBadRequest,
			),
			retryTimes: 3,
			want:       true,
			reason:     "a channel fault is still retryable on another channel; the reordering must not disable retries wholesale",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, types.IsChannelError(tc.err),
				"fixture must be a channel-class error for this ordering to be under test")

			c := newRetryTestContext(t)
			if tc.setup != nil {
				tc.setup(c)
			}
			require.False(t, c.Writer.Written(), "no bytes on the wire yet")

			assert.Equal(t, tc.want, shouldRetry(c, tc.err, tc.retryTimes), tc.reason)
		})
	}
}

// A committed response with zero ping frames — a hijacked realtime connection
// marks the writer written the same way — must keep blocking retries even
// though no payload passed through the wrapper.
func TestShouldRetryStopsOnCommittedResponseWithoutPings(t *testing.T) {
	upstreamErr := types.WithClaudeError(
		types.ClaudeError{Type: "overloaded_error", Message: "Overloaded"},
		http.StatusInternalServerError,
	)

	committed := newRetryTestContext(t)
	committed.Writer = &pingAwareWriter{ResponseWriter: committed.Writer}
	committed.Writer.WriteHeaderNow()
	require.True(t, committed.Writer.Written())
	assert.False(t, shouldRetry(committed, upstreamErr, 3),
		"a committed response with no attributable ping bytes is not retryable")
}
