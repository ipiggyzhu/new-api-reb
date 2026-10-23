package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

const (
	// channelKeepaliveInstruction follows the question drawn from the shared
	// channel-test prompt pool, so the reply stays a few tokens long.
	channelKeepaliveInstruction = "只回复 ok，不要解释"
	channelKeepaliveMaxTokens   = 16
	// channelKeepaliveRequestTimeout bounds one keepalive. Channels are sent one
	// after another, so an upstream that accepts the connection and never answers
	// would otherwise hold back every channel queued behind it.
	channelKeepaliveRequestTimeout = 2 * time.Minute
	// channelKeepaliveErrorLimit caps an error kept in the task result.
	channelKeepaliveErrorLimit = 300
)

// channelKeepaliveSummary is the result persisted on a channel_keepalive task row.
type channelKeepaliveSummary struct {
	Due       int                       `json:"due"`
	Succeeded int                       `json:"succeeded"`
	Failed    int                       `json:"failed"`
	Failures  []channelKeepaliveFailure `json:"failures,omitempty"`
}

type channelKeepaliveFailure struct {
	ChannelId int    `json:"channel_id"`
	Error     string `json:"error"`
}

// findDueKeepaliveChannelIDs lists the enabled channels whose upstream keepalive
// is due at now, judged against each channel's real-traffic activity record.
// The LIKE only narrows the rows to parse; the parsed settings decide, so a
// stored `"upstream_keepalive_enabled":false` or different JSON spacing cannot
// change the answer.
func findDueKeepaliveChannelIDs(now int64) ([]int, error) {
	var channels []*model.Channel
	err := model.DB.
		Select("id", "settings").
		Where("status = ?", common.ChannelStatusEnabled).
		Where("settings LIKE ?", "%upstream_keepalive_enabled%").
		Order("id asc").
		Find(&channels).Error
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		settings := channel.GetOtherSettings()
		if settings.UpstreamKeepaliveDue(channel.Id, service.ChannelLastActiveAt(channel.Id, now), now) {
			ids = append(ids, channel.Id)
		}
	}
	return ids, nil
}

// runChannelKeepalivePass sends every keepalive that is due, one channel at a
// time. Failures are recorded and logged only: they never disable a channel and
// the channel is retried after its normal interval.
func runChannelKeepalivePass(ctx context.Context) (channelKeepaliveSummary, error) {
	summary := channelKeepaliveSummary{}
	ids, err := findDueKeepaliveChannelIDs(common.GetTimestamp())
	if err != nil {
		return summary, err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		channel, err := model.GetChannelById(id, true)
		if err != nil {
			summary.recordFailure(id, err)
			continue
		}
		// The channel may have been disabled or edited since the scan, or real
		// traffic may have reached it while earlier channels were being sent.
		now := common.GetTimestamp()
		settings := channel.GetOtherSettings()
		lastActive := service.ChannelLastActiveAt(channel.Id, now)
		if channel.Status != common.ChannelStatusEnabled || !settings.UpstreamKeepaliveDue(channel.Id, lastActive, now) {
			continue
		}
		summary.Due++
		// Stamp before sending. A failed keepalive then waits the same interval
		// as a successful one, and a stamp that cannot be persisted means no
		// send, so neither a crash nor a database outage turns into a burst of
		// requests against the upstream every minute.
		if err := model.MutateChannelSettings(channel.Id, func(stored *dto.ChannelOtherSettings) bool {
			stored.UpstreamKeepaliveLastTime = now
			return true
		}); err != nil {
			summary.recordFailure(channel.Id, err)
			continue
		}
		if err := sendChannelKeepalive(ctx, channel); err != nil {
			summary.recordFailure(channel.Id, err)
			continue
		}
		summary.Succeeded++
		common.SysLog(fmt.Sprintf("channel keepalive succeeded: channel_id=%d", channel.Id))
	}
	return summary, nil
}

func (s *channelKeepaliveSummary) recordFailure(channelId int, err error) {
	message := common.MaskSensitiveInfo(err.Error())
	common.SysError(fmt.Sprintf("channel keepalive failed: channel_id=%d err=%s", channelId, message))
	if len(message) > channelKeepaliveErrorLimit {
		message = message[:channelKeepaliveErrorLimit] + "..."
	}
	s.Failed++
	s.Failures = append(s.Failures, channelKeepaliveFailure{ChannelId: channelId, Error: message})
}

// sendChannelKeepalive sends one tiny request to the channel's upstream with
// the channel's own key, through the same adaptor steps as a channel test.
//
// It is system traffic, so it deliberately leaves out everything a channel
// test adds on top of the adaptor: no test user, no pricing or quota, no
// consume log, no response-time update, and no verdict on the channel. The
// relay controller is not involved, so nothing reaches channel scoring,
// affinity or auto-disable either, and no concurrency slot is taken, so the
// keepalive is not recorded as channel activity and cannot keep its own active
// window open. The client profile does not apply: the
// request goes out without synthesized client headers, TLS fingerprint or CLI
// body shaping.
func sendChannelKeepalive(ctx context.Context, channel *model.Channel) error {
	if lo.Contains(unsupportedTestChannelTypes, channel.Type) {
		return fmt.Errorf("%s channels have no chat endpoint for a keepalive", constant.GetChannelTypeName(channel.Type))
	}
	modelName := ""
	if channel.TestModel != nil {
		modelName = strings.TrimSpace(*channel.TestModel)
	}
	if modelName == "" {
		if models := channel.GetModels(); len(models) > 0 {
			modelName = strings.TrimSpace(models[0])
		}
	}
	if modelName == "" {
		return errors.New("channel has neither a test model nor any model")
	}

	ctx, cancel := context.WithTimeout(ctx, channelKeepaliveRequestTimeout)
	defer cancel()

	prompt := operation_setting.PickChannelTestPrompt() + "\n" + channelKeepaliveInstruction
	requestPath := "/v1/chat/completions"
	relayFormat := types.RelayFormatOpenAI
	isStream := false
	var request dto.Request = &dto.GeneralOpenAIRequest{
		Model:     modelName,
		Stream:    lo.ToPtr(false),
		Messages:  []dto.Message{{Role: "user", Content: prompt}},
		MaxTokens: lo.ToPtr(uint(channelKeepaliveMaxTokens)),
	}
	// Codex channels and codex models only exist on the Responses endpoint, as
	// in the channel test. The ChatGPT Codex backend also refuses non-streaming
	// requests (the reason the automatic channel test streams for Codex, commit
	// 5f67d2a2), so that one channel type streams.
	if channel.Type == constant.ChannelTypeCodex || strings.Contains(strings.ToLower(modelName), "codex") {
		input, err := common.Marshal([]dto.Message{{Role: "user", Content: prompt}})
		if err != nil {
			return err
		}
		isStream = channel.Type == constant.ChannelTypeCodex
		requestPath = "/v1/responses"
		relayFormat = types.RelayFormatOpenAIResponses
		request = &dto.OpenAIResponsesRequest{
			Model:           modelName,
			Input:           input,
			Stream:          lo.ToPtr(isStream),
			MaxOutputTokens: lo.ToPtr(uint(channelKeepaliveMaxTokens)),
		}
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, requestPath, nil)
	c.Request.Header.Set("Content-Type", "application/json")
	if !isStream {
		// The adaptor copies this request's Accept upstream; streaming fills in
		// text/event-stream itself when it is absent.
		c.Request.Header.Set("Accept", "application/json")
	}
	if newAPIError := middleware.SetupContextForSelectedChannel(c, channel, modelName); newAPIError != nil {
		return newAPIError
	}
	// Keepalive is system traffic and does not wear the channel's client profile.
	// Clearing the profile on this request's copy of the settings makes every
	// shaping step see "no profile"; the shaping code itself is untouched.
	channelSetting := channel.GetSetting()
	channelSetting.SyntheticClientHeadersProfile = ""
	channelSetting.SyntheticClientHeaders = false
	channelSetting.CLIVersionProfile = ""
	channelSetting.TLSFingerprint = ""
	channelSetting.ClientDeviceSeed = ""
	channelSetting.ClientSessionSeed = ""
	common.SetContextKey(c, constant.ContextKeyChannelSetting, channelSetting)

	info, err := relaycommon.GenRelayInfo(c, relayFormat, request, nil)
	if err != nil {
		return err
	}
	// The channel-test flag gives the synthetic-request semantics the relay
	// already implements for tests: no caller body or headers to pass through and
	// no WebSocket upgrade probe. Nothing that scores, bills or disables a channel
	// reads it here, because none of those run on this path.
	info.IsChannelTest = true
	// Nobody reads the response, so there is no one to keep alive with pings.
	info.DisablePing = true
	info.InitChannelMeta(c)
	if err := helper.ModelMappedHelper(c, info, request); err != nil {
		return err
	}
	request.SetModelName(info.UpstreamModelName)

	apiType, _ := common.ChannelType2APIType(channel.Type)
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return fmt.Errorf("invalid api type: %d, adaptor is nil", apiType)
	}
	adaptor.Init(info)

	var converted any
	switch req := request.(type) {
	case *dto.OpenAIResponsesRequest:
		converted, err = adaptor.ConvertOpenAIResponsesRequest(c, info, *req)
	case *dto.GeneralOpenAIRequest:
		converted, err = adaptor.ConvertOpenAIRequest(c, info, req)
	}
	if err != nil {
		return err
	}
	jsonData, err := common.Marshal(converted)
	if err != nil {
		return err
	}
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return err
		}
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(jsonData))
	resp, err := adaptor.DoRequest(c, info, bytes.NewReader(jsonData))
	if err != nil {
		return err
	}
	httpResp, _ := resp.(*http.Response)
	if httpResp == nil {
		return errors.New("upstream returned no response")
	}
	if httpResp.StatusCode != http.StatusOK {
		if newAPIError := service.RelayErrorHandler(ctx, httpResp, true); newAPIError != nil {
			return errors.New(newAPIError.ErrorWithStatusCode())
		}
		return fmt.Errorf("upstream status %d", httpResp.StatusCode)
	}
	if _, newAPIError := adaptor.DoResponse(c, httpResp, info); newAPIError != nil {
		return errors.New(newAPIError.ErrorWithStatusCode())
	}
	// Some upstreams answer 200 with an error envelope.
	respBody, err := readTestResponseBody(w.Result().Body, isStream)
	if err != nil {
		return err
	}
	return validateTestResponseBody(respBody, isStream)
}
