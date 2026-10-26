package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// keyEnvironmentRetryAfter is the Retry-After of a bound request that cannot be
// served now for a reason with no deadline of its own (a full channel, a
// channel-wide cooldown, the binding store failing).
const keyEnvironmentRetryAfter = 5

// refuseKeyEnvironment is the answer to a bound request that cannot be served
// now. The Retry-After goes on the response here, so the distributor and the
// relay's retry loop both send it with the error.
func refuseKeyEnvironment(c *gin.Context, status int, retryAfter int, message string) *types.NewAPIError {
	c.Header("Retry-After", strconv.Itoa(retryAfter))
	return types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeChannelsSaturated, status, types.ErrOptionWithSkipRetry())
}

func abortKeyEnvironment(c *gin.Context, refusal *types.NewAPIError) {
	if c.IsAborted() {
		// The client went away while the request waited for a slot.
		return
	}
	abortWithOpenAiMessage(c, refusal.StatusCode, refusal.Error(), refusal.GetErrorCode())
}

// keyEnvironmentGroups are the groups a request of usingGroup may be served
// from.
func keyEnvironmentGroups(c *gin.Context, usingGroup string) []string {
	if usingGroup == "auto" {
		return service.GetUserAutoGroup(common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	return []string{usingGroup}
}

// selectBoundKeyEnvironmentChannel serves a conversation bound to a key of an
// independent key environment channel from that key. Only hard signals move a
// conversation: the channel or key disabled, removed or unassigned, the switch
// turned off, the channel no longer serving the group, model or path. Those
// fall through to ordinary selection, which migrates the binding. Soft signals
// (a cooling key or channel, a full channel) are waited out or refused with
// Retry-After, never switched away from, because the real CLI backs off on its
// own account the same way.
//
// It returns the bound channel and group, or nil to select as usual. The
// request is aborted when it can neither be served nor moved.
func selectBoundKeyEnvironmentChannel(c *gin.Context, modelName string, usingGroup string) (*model.Channel, string) {
	if !model.GroupModelHasKeyEnvironmentChannel(keyEnvironmentGroups(c, usingGroup), modelName) {
		return nil, ""
	}
	state := &service.KeyEnvironmentBindingState{Key: service.KeyEnvironmentBindingKey(c, usingGroup)}
	service.SetKeyEnvironmentBindingState(c, state)

	binding, found, err := service.GetKeyEnvironmentBinding(state.Key)
	if err != nil {
		// Fail closed: treating a store error as "unbound" would move every
		// conversation to a fresh key, the one thing a binding must prevent.
		common.SysError("key environment binding lookup failed: " + err.Error())
		abortKeyEnvironment(c, refuseKeyEnvironment(c, http.StatusServiceUnavailable, keyEnvironmentRetryAfter, "key environment binding store unavailable"))
		return nil, ""
	}
	if !found {
		return nil, ""
	}
	state.Previous = &binding

	channel, selectGroup, refusal := serveKeyEnvironmentBinding(c, binding, modelName, usingGroup, true)
	if refusal != nil {
		abortKeyEnvironment(c, refusal)
		return nil, ""
	}
	return channel, selectGroup
}

// serveKeyEnvironmentBinding takes a slot on the channel of binding for this
// request and records the binding as the one it serves under. It returns nil
// with no refusal on a hard signal (the binding can no longer serve this
// request), and a refusal on a soft one. Only the distributor's own lookup
// waits for a full channel; it is the one place nothing has been sent yet and
// the wait's pings may start the response.
func serveKeyEnvironmentBinding(c *gin.Context, binding service.KeyEnvironmentBinding, modelName string, usingGroup string, wait bool) (*model.Channel, string, *types.NewAPIError) {
	channel, err := model.CacheGetChannel(binding.ChannelId)
	if err != nil || channel == nil || channel.Status != common.ChannelStatusEnabled || !channel.GetSetting().KeyEnvironments ||
		!channelSupportsRequestPath(channel, c.Request.URL.Path, modelName) {
		return nil, "", nil
	}
	selectGroup := ""
	for _, group := range keyEnvironmentGroups(c, usingGroup) {
		if model.IsChannelEnabledForGroupModel(group, modelName, channel.Id) {
			selectGroup = group
			break
		}
	}
	if selectGroup == "" {
		return nil, "", nil
	}
	index, _, coolingUntil := channel.KeyEnvironmentKey(binding.KeyTag, modelName)
	if index < 0 {
		return nil, "", nil
	}

	if !coolingUntil.IsZero() {
		retryAfter := service.KeyEnvironmentRetryAfter(coolingUntil)
		return nil, "", refuseKeyEnvironment(c, http.StatusTooManyRequests, retryAfter,
			fmt.Sprintf("key environment is rate limited; retry after %ds (session stays on its key)", retryAfter))
	}
	if model.ChannelCoolingDown(channel.Id, modelName) {
		return nil, "", refuseKeyEnvironment(c, http.StatusTooManyRequests, keyEnvironmentRetryAfter,
			fmt.Sprintf("key environment is rate limited; retry after %ds (session stays on its key)", keyEnvironmentRetryAfter))
	}
	if !model.AcquireChannelSlot(channel.Id, channel.GetMaxConcurrency()) && (!wait || !waitForAffinitySlot(c, channel, modelName)) {
		return nil, "", refuseKeyEnvironment(c, http.StatusServiceUnavailable, keyEnvironmentRetryAfter,
			"the conversation's channel is at its concurrency limit")
	}
	if usingGroup == "auto" {
		common.SetContextKey(c, constant.ContextKeyAutoGroup, selectGroup)
	}
	// Holding the bound channel's slot gives back the one a losing request took
	// on the channel it had selected.
	service.HoldChannelSlot(c, channel.Id)
	if state := service.GetKeyEnvironmentBindingState(c); state != nil {
		state.Bound = &binding
	}
	return channel, selectGroup, nil
}

// BindKeyEnvironment binds the conversation to the key channel setup selected
// on an independent key environment channel: placing it the first time, or
// migrating it from a binding that a hard signal invalidated. It returns the
// channel the request is served from.
//
// A concurrent request may have bound the conversation first. This request
// then follows the winner: its key on the same channel, or the winner's
// channel, whose slot replaces the one taken here. Only when the winner's
// binding cannot serve this request at all (a hard signal) is it served once,
// unbound, from the channel it selected, rather than overwrite the winner.
// Retries that land on such a channel claim it the same way.
func BindKeyEnvironment(c *gin.Context, channel *model.Channel, modelName string) (*model.Channel, *types.NewAPIError) {
	state := service.GetKeyEnvironmentBindingState(c)
	if state == nil || state.Bound != nil || channel == nil || !channel.GetSetting().KeyEnvironments {
		return channel, nil
	}
	tag := common.GetContextKeyString(c, constant.ContextKeyChannelKeyTag)
	if tag == "" {
		return channel, nil
	}
	next := service.KeyEnvironmentBinding{ChannelId: channel.Id, KeyTag: tag}
	claimed, err := service.ClaimKeyEnvironmentBinding(state.Key, state.Previous, next)
	if err == nil && !claimed && state.Previous != nil {
		// The binding being migrated may have expired meanwhile.
		claimed, err = service.ClaimKeyEnvironmentBinding(state.Key, nil, next)
	}
	if err != nil {
		common.SysError("key environment binding claim failed: " + err.Error())
		return nil, refuseKeyEnvironment(c, http.StatusServiceUnavailable, keyEnvironmentRetryAfter, "key environment binding store unavailable")
	}
	if claimed {
		state.Bound = &next
		return channel, nil
	}

	winner, found, err := service.GetKeyEnvironmentBinding(state.Key)
	if err != nil || !found {
		logger.LogWarn(c, "key environment binding lost and unreadable; serving this request unbound")
		return channel, nil
	}
	serving := channel
	if winner.ChannelId == channel.Id {
		state.Bound = &winner
	} else {
		usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		followed, _, refusal := serveKeyEnvironmentBinding(c, winner, modelName, usingGroup, false)
		if refusal != nil {
			return nil, refusal
		}
		if followed == nil {
			logger.LogWarn(c, fmt.Sprintf("key environment binding lost to channel %d, which cannot serve this request; serving it unbound", winner.ChannelId))
			return channel, nil
		}
		serving = followed
	}
	if setupErr := SetupContextForSelectedChannel(c, serving, modelName); setupErr != nil {
		// The winner's key stopped serving between its claim and this setup.
		c.Header("Retry-After", strconv.Itoa(keyEnvironmentRetryAfter))
		return nil, types.NewErrorWithStatusCode(setupErr.Err, setupErr.GetErrorCode(), http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	return serving, nil
}

// renewKeyEnvironmentBinding extends the conversation's binding after its
// request, whatever the outcome, while it still names the key served.
func renewKeyEnvironmentBinding(c *gin.Context) {
	state := service.GetKeyEnvironmentBindingState(c)
	if state == nil || state.Bound == nil {
		return
	}
	if err := service.RenewKeyEnvironmentBinding(state.Key, *state.Bound); err != nil {
		common.SysError("key environment binding renewal failed: " + err.Error())
	}
}

// boundKeyEnvironmentKey is the key a bound request must use on channel, or ""
// when the request is not bound to it.
func boundKeyEnvironmentKey(c *gin.Context, channel *model.Channel, modelName string) (string, int, bool, *types.NewAPIError) {
	tag, bound := service.KeyEnvironmentBoundTo(c, channel.Id)
	if !bound {
		return "", 0, false, nil
	}
	index, key, _ := channel.KeyEnvironmentKey(tag, modelName)
	if index < 0 {
		return "", 0, true, types.NewError(errors.New("the conversation's key can no longer serve"), types.ErrorCodeChannelNoAvailableKey)
	}
	return key, index, true, nil
}
