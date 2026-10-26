package service

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
)

const keyEnvironmentBindingNamespace = "new-api:key_env_binding:v1"

// KeyEnvironmentBinding pins a conversation to one key of an independent key
// environment channel, so the upstream account sees one device per
// conversation. It names the key by tag, never by index or profile, so list
// edits and version reassignment do not move it.
type KeyEnvironmentBinding struct {
	ChannelId int
	KeyTag    string
}

func (b KeyEnvironmentBinding) String() string {
	return fmt.Sprintf("%d:%s", b.ChannelId, b.KeyTag)
}

func parseKeyEnvironmentBinding(value string) (KeyEnvironmentBinding, bool) {
	id, tag, ok := strings.Cut(value, ":")
	if !ok || tag == "" {
		return KeyEnvironmentBinding{}, false
	}
	channelId, err := strconv.Atoi(id)
	if err != nil || channelId <= 0 {
		return KeyEnvironmentBinding{}, false
	}
	return KeyEnvironmentBinding{ChannelId: channelId, KeyTag: tag}, true
}

var (
	keyEnvironmentBindingCacheOnce sync.Once
	keyEnvironmentBindingCache     *cachex.HybridCache[string]
)

func getKeyEnvironmentBindingCache() *cachex.HybridCache[string] {
	keyEnvironmentBindingCacheOnce.Do(func() {
		capacity := operation_setting.GetChannelAffinitySetting().MaxEntries
		if capacity <= 0 {
			capacity = 100_000
		}
		keyEnvironmentBindingCache = cachex.NewHybridCache[string](cachex.HybridCacheConfig[string]{
			Namespace: cachex.Namespace(keyEnvironmentBindingNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.StringCodec{},
			Memory: func() *hot.HotCache[string, string] {
				return hot.NewHotCache[string, string](hot.LRU, capacity).
					WithTTL(operation_setting.GetChannelAffinitySetting().KeyEnvironmentBindingTTL()).
					WithJanitor().
					Build()
			},
		})
	})
	return keyEnvironmentBindingCache
}

// KeyEnvironmentBindingKey is the conversation a request belongs to: the
// affinity rule's key when one matched (Claude's metadata.user_id, Codex's
// prompt_cache_key, the token fallback rule's token), otherwise the caller's
// token in its group. The model is never part of it, not even when the matched
// rule keys its pins by model, so switching models keeps the key.
func KeyEnvironmentBindingKey(c *gin.Context, usingGroup string) string {
	if meta, ok := getChannelAffinityMeta(c); ok && meta.ConversationKey != "" {
		return "kenv:" + meta.ConversationKey
	}
	return fmt.Sprintf("kenv:token:%d:%s", common.GetContextKeyInt(c, constant.ContextKeyTokenId), usingGroup)
}

// GetKeyEnvironmentBinding reads a conversation's binding. A stored value that
// does not parse reads as no binding.
func GetKeyEnvironmentBinding(bindingKey string) (KeyEnvironmentBinding, bool, error) {
	value, found, err := getKeyEnvironmentBindingCache().Get(bindingKey)
	if err != nil || !found {
		return KeyEnvironmentBinding{}, false, err
	}
	binding, ok := parseKeyEnvironmentBinding(value)
	return binding, ok, nil
}

// ClaimKeyEnvironmentBinding binds a conversation to next. With no previous
// binding it only places one if none exists; with one it migrates only from
// that value. It reports whether this request's value is now stored; on false
// another request bound the conversation first and the caller reads it.
func ClaimKeyEnvironmentBinding(bindingKey string, previous *KeyEnvironmentBinding, next KeyEnvironmentBinding) (bool, error) {
	ttl := operation_setting.GetChannelAffinitySetting().KeyEnvironmentBindingTTL()
	if previous == nil {
		return getKeyEnvironmentBindingCache().SetIfAbsent(bindingKey, next.String(), ttl)
	}
	return getKeyEnvironmentBindingCache().CompareAndSwap(bindingKey, previous.String(), next.String(), ttl)
}

// RenewKeyEnvironmentBinding extends a binding after a request, only while it
// still holds the value the request served under.
func RenewKeyEnvironmentBinding(bindingKey string, binding KeyEnvironmentBinding) error {
	ttl := operation_setting.GetChannelAffinitySetting().KeyEnvironmentBindingTTL()
	_, err := getKeyEnvironmentBindingCache().ExpireIfEquals(bindingKey, binding.String(), ttl)
	return err
}

// KeyEnvironmentBindingState is what the distributor decided for a request on
// an independent key environment channel, read again on retries.
type KeyEnvironmentBindingState struct {
	// Key is the conversation's binding key.
	Key string
	// Previous is the binding found when the request arrived; a migration
	// replaces only this value.
	Previous *KeyEnvironmentBinding
	// Bound is the binding the request serves under; nil until claimed.
	Bound *KeyEnvironmentBinding
}

const ginKeyKeyEnvironmentBinding = "key_environment_binding"

func SetKeyEnvironmentBindingState(c *gin.Context, state *KeyEnvironmentBindingState) {
	c.Set(ginKeyKeyEnvironmentBinding, state)
}

func GetKeyEnvironmentBindingState(c *gin.Context) *KeyEnvironmentBindingState {
	value, ok := c.Get(ginKeyKeyEnvironmentBinding)
	if !ok {
		return nil
	}
	state, _ := value.(*KeyEnvironmentBindingState)
	return state
}

// KeyEnvironmentBoundTo reports whether the request is bound to channelId, and
// to which key tag.
func KeyEnvironmentBoundTo(c *gin.Context, channelId int) (string, bool) {
	state := GetKeyEnvironmentBindingState(c)
	if state == nil || state.Bound == nil || state.Bound.ChannelId != channelId {
		return "", false
	}
	return state.Bound.KeyTag, true
}

// KeyEnvironmentRetryAfter is the Retry-After for a bound request that cannot
// be served now: the time left on its key's cooldown, at least a second.
func KeyEnvironmentRetryAfter(until time.Time) int {
	seconds := int(time.Until(until).Seconds() + 0.999)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// ResetKeyEnvironmentBindingCacheForTest drops the binding store so the next
// use rebuilds it against the current Redis settings.
func ResetKeyEnvironmentBindingCacheForTest() {
	keyEnvironmentBindingCacheOnce = sync.Once{}
	keyEnvironmentBindingCache = nil
}
