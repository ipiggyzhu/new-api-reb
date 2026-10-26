package controller

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The retry matrix of a conversation bound to one key (task19 §5.6): only what
// the real CLI would retry on its own account is retried, and on the same key.
func TestBoundKeyEnvironmentRetryMatrix(t *testing.T) {
	upstream := func(status int) *types.NewAPIError {
		return types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, status)
	}
	streamed := func(c *gin.Context) {
		c.Writer = &pingAwareWriter{ResponseWriter: c.Writer}
		_, _ = c.Writer.Write([]byte("data: {}\n\n"))
	}
	pinged := func(c *gin.Context) {
		c.Writer = &pingAwareWriter{ResponseWriter: c.Writer}
		require.NoError(t, helper.PingData(c))
	}
	for _, tc := range []struct {
		name    string
		err     *types.NewAPIError
		budget  int
		written func(*gin.Context)
		want    bool
	}{
		{"request never reached the upstream", types.NewError(errors.New("dial tcp: refused"), types.ErrorCodeDoRequestFailed), 3, nil, true},
		{"upstream 500 before any byte", upstream(http.StatusInternalServerError), 3, nil, true},
		{"upstream 502 before any byte", upstream(http.StatusBadGateway), 3, nil, true},
		{"upstream 503 before any byte", upstream(http.StatusServiceUnavailable), 3, nil, true},
		{"upstream 500 after keepalive pings only", upstream(http.StatusInternalServerError), 3, pinged, true},
		{"rate limited", upstream(http.StatusTooManyRequests), 3, nil, false},
		{"overloaded", upstream(529), 3, nil, false},
		{"gateway timeout", upstream(http.StatusGatewayTimeout), 3, nil, false},
		{"cloudflare timeout", upstream(524), 3, nil, false},
		{"key rejected", types.NewError(errors.New("invalid key"), types.ErrorCodeChannelInvalidKey), 3, nil, false},
		{"upstream 500 mid-stream", upstream(http.StatusInternalServerError), 3, streamed, false},
		{"retries disabled", upstream(http.StatusInternalServerError), 0, nil, false},
		{"skip-retry error", types.NewError(errors.New("refused"), types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry()), 3, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRetryTestContext(t)
			if tc.written != nil {
				tc.written(c)
			}
			assert.Equal(t, tc.want, shouldRetryBoundKeyEnvironment(c, tc.err, tc.budget))
		})
	}

	t.Run("status ranges an admin excluded govern upstream answers, not unreached requests", func(t *testing.T) {
		original := operation_setting.AutomaticRetryStatusCodeRanges
		operation_setting.AutomaticRetryStatusCodeRanges = nil
		t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = original })
		c := newRetryTestContext(t)
		assert.False(t, shouldRetryBoundKeyEnvironment(c, upstream(http.StatusBadGateway), 3))
		assert.True(t, shouldRetryBoundKeyEnvironment(c, types.NewError(errors.New("dial tcp: refused"), types.ErrorCodeDoRequestFailed), 3))
	})
}

// A bound retry goes back to the conversation's own channel and key, never to
// another channel, and a key that stopped serving ends the request instead.
func TestGetChannelRetriesABoundConversationOnItsKey(t *testing.T) {
	db := setupKeyEnvironmentControllerTest(t)
	channel := &model.Channel{
		Id: 1701, Type: constant.ChannelTypeOpenAI, Name: "bound", Status: common.ChannelStatusEnabled,
		Key: "sk-a\nsk-b", Models: "gpt-5", Group: "default",
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling},
	}
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: "codex", KeyEnvironments: true})
	require.NoError(t, db.Create(channel).Error)
	for key, profile := range map[string]string{"sk-a": "codex-0.163.0-linux-x64", "sk-b": "codex-0.160.0-windows-x64"} {
		require.NoError(t, db.Create(&model.ChannelKeyEnvironment{ChannelId: channel.Id, KeyTag: model.ChannelKeyTag(key), ProfileId: &profile}).Error)
	}
	retry := func(t *testing.T) (*model.Channel, *types.NewAPIError, *service.RetryParam, *gin.Context) {
		c := newRetryTestContext(t)
		service.SetKeyEnvironmentBindingState(c, &service.KeyEnvironmentBindingState{
			Key: "kenv:test", Bound: &service.KeyEnvironmentBinding{ChannelId: channel.Id, KeyTag: model.ChannelKeyTag("sk-b")},
		})
		retryParam := &service.RetryParam{Ctx: c, ModelName: "gpt-5", TokenGroup: "default", Retry: common.GetPointer(1)}
		got, apiErr := getChannel(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, OriginModelName: "gpt-5"}, retryParam)
		return got, apiErr, retryParam, c
	}

	for round := 0; round < 3; round++ {
		got, apiErr, retryParam, c := retry(t)
		require.Nil(t, apiErr)
		assert.Equal(t, channel.Id, got.Id)
		assert.Equal(t, "sk-b", common.GetContextKeyString(c, constant.ContextKeyChannelKey), "round %d", round)
		assert.Empty(t, retryParam.ExcludedChannelIds, "the bound channel is not excluded from its own retry")
	}

	require.NoError(t, db.Model(channel).Update("channel_info", model.ChannelInfo{
		IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling, MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled},
	}).Error)
	_, apiErr, _, _ := retry(t)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeGetChannelFailed, apiErr.GetErrorCode(), "the upstream's error stays the request's answer")
	assert.True(t, types.IsSkipRetryError(apiErr))
}
