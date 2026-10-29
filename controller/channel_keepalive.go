package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	ChannelId int `json:"channel_id"`
	// KeyIndex is the failing key's index on a multi-key channel.
	KeyIndex *int   `json:"key_index,omitempty"`
	Error    string `json:"error"`
}

// channelKeepaliveKey is one key of a channel, named by its identity slot
// (model.Channel.KeyIdentitySlot).
type channelKeepaliveKey struct {
	channelId int
	slot      string
}

// findDueKeepalives lists, in channel id order, the keys of enabled channels
// whose upstream keepalive is due at now, each judged against that key's own
// real-traffic activity record. A key that is gone or disabled is never due.
// The LIKE only narrows the rows to parse; the parsed settings decide, so a
// stored `"upstream_keepalive_enabled":false` or different JSON spacing cannot
// change the answer.
func findDueKeepalives(now int64) ([]channelKeepaliveKey, error) {
	var channels []*model.Channel
	err := model.DB.
		Select("id", "key", "channel_info", "settings").
		Where("status = ?", common.ChannelStatusEnabled).
		Where("settings LIKE ?", "%upstream_keepalive_enabled%").
		Order("id asc").
		Find(&channels).Error
	if err != nil {
		return nil, err
	}
	activities := service.ChannelKeyActivities(now)
	var due []channelKeepaliveKey
	for _, channel := range channels {
		settings := channel.GetOtherSettings()
		keys := activities[channel.Id]
		sort.Slice(keys, func(i, j int) bool { return keys[i].Slot < keys[j].Slot })
		for _, key := range keys {
			if _, _, enabled := channel.EnabledKeyForSlot(key.Slot); enabled &&
				settings.UpstreamKeepaliveDue(channel.Id, key.Slot, key.LastActive, key.LastKeepalive, now) {
				due = append(due, channelKeepaliveKey{channelId: channel.Id, slot: key.Slot})
			}
		}
	}
	return due, nil
}

// runChannelKeepalivePass sends every keepalive that is due, one key at a time.
// Failures are recorded and logged only: they never disable a channel or a key,
// and the key is retried after its normal interval.
func runChannelKeepalivePass(ctx context.Context) (channelKeepaliveSummary, error) {
	summary := channelKeepaliveSummary{}
	due, err := findDueKeepalives(common.GetTimestamp())
	if err != nil {
		return summary, err
	}
	for _, unit := range due {
		if ctx.Err() != nil {
			break
		}
		channel, err := model.GetChannelById(unit.channelId, true)
		if err != nil {
			summary.recordFailure(unit.channelId, nil, err)
			continue
		}
		// The channel may have been disabled or edited since the scan, the key
		// removed or disabled, or real traffic may have reached the key while
		// earlier keys were being sent.
		now := common.GetTimestamp()
		settings := channel.GetOtherSettings()
		activity := service.ChannelKeyActivityAt(channel.Id, unit.slot, now)
		key, index, enabled := channel.EnabledKeyForSlot(unit.slot)
		if channel.Status != common.ChannelStatusEnabled || !enabled ||
			!settings.UpstreamKeepaliveDue(channel.Id, unit.slot, activity.LastActive, activity.LastKeepalive, now) {
			continue
		}
		var keyIndex *int
		if channel.ChannelInfo.IsMultiKey {
			keyIndex = &index
		}
		summary.Due++
		// Stamp before sending, so a failed keepalive waits the same interval as
		// a successful one. The channel's stored stamp is the one the admin sees;
		// one that cannot be persisted means no send.
		service.StampChannelKeyKeepalive(channel.Id, unit.slot, now)
		if err := model.MutateChannelSettings(channel.Id, func(stored *dto.ChannelOtherSettings) bool {
			stored.UpstreamKeepaliveLastTime = now
			return true
		}); err != nil {
			summary.recordFailure(channel.Id, keyIndex, err)
			continue
		}
		if err := sendChannelKeepalive(ctx, channel, unit.slot, key, index); err != nil {
			summary.recordFailure(channel.Id, keyIndex, err)
			continue
		}
		summary.Succeeded++
		common.SysLog(fmt.Sprintf("channel keepalive succeeded: %s", channelKeepaliveLabel(channel.Id, keyIndex)))
	}
	return summary, nil
}

// channelKeepaliveLabel names a keepalive in the logs: "channel_id=3" for a
// single-key channel, "channel_id=3 key_index=1" for one key of a multi-key
// channel.
func channelKeepaliveLabel(channelId int, keyIndex *int) string {
	if keyIndex == nil {
		return fmt.Sprintf("channel_id=%d", channelId)
	}
	return fmt.Sprintf("channel_id=%d key_index=%d", channelId, *keyIndex)
}

func (s *channelKeepaliveSummary) recordFailure(channelId int, keyIndex *int, err error) {
	message := common.MaskSensitiveInfo(err.Error())
	common.SysError(fmt.Sprintf("channel keepalive failed: %s err=%s", channelKeepaliveLabel(channelId, keyIndex), message))
	if len(message) > channelKeepaliveErrorLimit {
		message = message[:channelKeepaliveErrorLimit] + "..."
	}
	s.Failed++
	s.Failures = append(s.Failures, channelKeepaliveFailure{ChannelId: channelId, KeyIndex: keyIndex, Error: message})
}

// sendChannelKeepalive sends one tiny request to the channel's upstream with
// one of its keys, through the same adaptor steps as a channel test.
//
// It is system traffic, so it deliberately leaves out everything a channel
// test adds on top of the adaptor: no test user, no pricing or quota, no
// consume log, no response-time update, and no verdict on the channel. The
// relay controller is not involved, so nothing reaches channel scoring,
// affinity or auto-disable either, and no concurrency slot is taken, so the
// keepalive is not recorded as key activity and cannot keep its own active
// window open.
//
// To the upstream it is that key's own client: it wears the key's client
// environment (the channel's headers, TLS fingerprint and seeds, with the
// key's own CLI version and proxy laid over the channel's) and identity, and
// on a channel whose real traffic is a Codex or Claude Code CLI it continues
// the last real session on that key, with its model, user and token scope and
// conversation seed, streams like the CLI does, and sends no output limit the
// CLI would not send.
func sendChannelKeepalive(ctx context.Context, channel *model.Channel, slot string, key string, keyIndex int) error {
	if lo.Contains(unsupportedTestChannelTypes, channel.Type) {
		return fmt.Errorf("%s channels have no chat endpoint for a keepalive", constant.GetChannelTypeName(channel.Type))
	}
	apiType, _ := common.ChannelType2APIType(channel.Type)
	channelSetting := channel.GetSetting()
	channelSetting.Normalize(apiType)
	family := channelSetting.SyntheticClientHeadersProfile
	cliProfile := family == constant.ClientHeaderFamilyCodex || family == constant.ClientHeaderFamilyClaude
	session, continued := service.LastChannelCLISession(channel.Id, slot)
	continued = continued && cliProfile

	modelName := ""
	if continued {
		modelName = session.ModelName
	}
	if modelName == "" && channel.TestModel != nil {
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
	// The CLIs always stream and never cap the output, so a CLI-profiled
	// keepalive does neither. The ChatGPT Codex backend also refuses
	// non-streaming requests (the reason the automatic channel test streams for
	// Codex, commit 5f67d2a2).
	isStream := cliProfile || channel.Type == constant.ChannelTypeCodex
	var maxTokens *uint
	if !cliProfile {
		maxTokens = lo.ToPtr(uint(channelKeepaliveMaxTokens))
	}
	requestPath := "/v1/chat/completions"
	relayFormat := types.RelayFormatOpenAI
	var request dto.Request = &dto.GeneralOpenAIRequest{
		Model:     modelName,
		Stream:    lo.ToPtr(isStream),
		Messages:  []dto.Message{{Role: "user", Content: prompt}},
		MaxTokens: maxTokens,
	}
	// Codex channels and codex models only exist on the Responses endpoint, and
	// an OpenAI channel wearing the Codex profile is a Codex CLI upstream, as in
	// the channel test.
	if channel.Type == constant.ChannelTypeCodex || strings.Contains(strings.ToLower(modelName), "codex") ||
		(channel.Type == constant.ChannelTypeOpenAI && family == constant.ClientHeaderFamilyCodex) {
		input, err := common.Marshal([]dto.Message{{Role: "user", Content: prompt}})
		if err != nil {
			return err
		}
		requestPath = "/v1/responses"
		relayFormat = types.RelayFormatOpenAIResponses
		responsesRequest := &dto.OpenAIResponsesRequest{
			Model:           modelName,
			Input:           input,
			Stream:          lo.ToPtr(isStream),
			MaxOutputTokens: maxTokens,
		}
		if continued && len(session.Reasoning) > 0 {
			var reasoning dto.Reasoning
			if err := common.Unmarshal(session.Reasoning, &reasoning); err == nil {
				responsesRequest.Reasoning = &reasoning
			}
		}
		request = responsesRequest
	}
	originalBody, err := common.Marshal(request)
	if err != nil {
		return err
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, requestPath, bytes.NewReader(originalBody))
	c.Request.Header.Set("Content-Type", "application/json")
	// The CLI shaping reads the caller's own body to tell what the caller asked
	// for (an output limit, an effort) from what conversion filled in.
	if _, err := common.GetBodyStorage(c); err != nil {
		return err
	}
	defer common.CleanupBodyStorage(c)
	if family != "" {
		// Dress the synthetic request as the configured client, as the channel
		// test does; the relay forwards these for channel-test requests.
		applyTestClientHeaders(c.Request.Header, apiType, family, isStream)
	} else if !isStream {
		// The adaptor copies this request's Accept upstream; streaming fills in
		// text/event-stream itself when it is absent.
		c.Request.Header.Set("Accept", "application/json")
	}
	if newAPIError := middleware.SetupContextForSelectedChannel(c, channel, modelName); newAPIError != nil {
		return newAPIError
	}
	// Selection may have picked another key; the keepalive belongs to this one.
	middleware.SetupContextForChannelKey(c, channel, channel.GetSetting(), key, keyIndex)

	info, err := relaycommon.GenRelayInfo(c, relayFormat, request, nil)
	if err != nil {
		return err
	}
	// The channel-test flag gives the synthetic-request semantics the relay
	// already implements for tests: no caller body or headers to pass through and
	// no WebSocket upgrade probe. Nothing that scores, bills or disables a channel
	// reads it here, because none of those run on this path. It also keeps this
	// request from being recorded as the channel's last real CLI session.
	info.IsChannelTest = true
	if continued {
		info.UserId, info.TokenId = session.UserId, session.TokenId
		info.CLISessionSeed = session.Seed
	}
	// Nobody reads the response, so there is no one to keep alive with pings.
	info.DisablePing = true
	info.InitChannelMeta(c)
	if err := helper.ModelMappedHelper(c, info, request); err != nil {
		return err
	}
	request.SetModelName(info.UpstreamModelName)

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
