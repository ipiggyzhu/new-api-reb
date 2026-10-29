package service

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// ginKeyHeldChannelSlot names the channel whose concurrency slot this request
	// currently owns.
	ginKeyHeldChannelSlot = "held_channel_slot"
	// ginKeyHeldChannelKey holds the *channelKeyActivity of the key this request
	// is using on that channel.
	ginKeyHeldChannelKey = "held_channel_key"
)

// channelKey names one key of one channel by the identity slot it wears
// upstream (model.Channel.KeyIdentitySlot), which, unlike its index, survives
// the re-indexing that deleting another key causes.
type channelKey struct {
	channelId int
	slot      string
}

// channelKeyActivity is one key's real-traffic activity record: how many relay
// requests are using the key right now, the unix second one last started or
// finished on it, and when its last upstream keepalive went out. The keepalive
// uses it to tell a key in use from an idle one, so every key of a multi-key
// channel is kept alive in the gaps of its own traffic and of nothing else.
//
// Traffic is stamped only by HoldChannelKey and the release paths: binding a
// key is a step every relay attempt takes, and the channel test and the
// keepalive never hold a slot, so neither counts as activity. The record lives
// in process memory, like the channel in-flight gauge, so the request path
// pays no database or Redis write. A restart forgets it, and a key then waits
// for its next real request before keepalives resume.
type channelKeyActivity struct {
	inFlight      atomic.Int64
	lastActive    atomic.Int64
	lastKeepalive atomic.Int64
}

var channelKeyActivities sync.Map // map[channelKey]*channelKeyActivity

func loadChannelKeyActivity(key channelKey) *channelKeyActivity {
	if value, ok := channelKeyActivities.Load(key); ok {
		return value.(*channelKeyActivity)
	}
	value, _ := channelKeyActivities.LoadOrStore(key, new(channelKeyActivity))
	return value.(*channelKeyActivity)
}

func (a *channelKeyActivity) markActive() {
	now := common.GetTimestamp()
	for {
		current := a.lastActive.Load()
		if current >= now || a.lastActive.CompareAndSwap(current, now) {
			return
		}
	}
}

// ChannelKeyActivity is one key's activity record as the keepalive reads it.
type ChannelKeyActivity struct {
	Slot string
	// LastActive is now while a request is using the key (a long stream is
	// activity for as long as it runs), else the last time a request started or
	// finished on it.
	LastActive    int64
	LastKeepalive int64
}

func (a *channelKeyActivity) snapshot(slot string, now int64) ChannelKeyActivity {
	lastActive := a.lastActive.Load()
	if a.inFlight.Load() > 0 {
		lastActive = now
	}
	return ChannelKeyActivity{Slot: slot, LastActive: lastActive, LastKeepalive: a.lastKeepalive.Load()}
}

// ChannelKeyActivities lists, per channel id, every key real traffic has used
// since the process started.
func ChannelKeyActivities(now int64) map[int][]ChannelKeyActivity {
	activities := make(map[int][]ChannelKeyActivity)
	channelKeyActivities.Range(func(key, value any) bool {
		unit := key.(channelKey)
		activities[unit.channelId] = append(activities[unit.channelId], value.(*channelKeyActivity).snapshot(unit.slot, now))
		return true
	})
	return activities
}

// ChannelKeyActivityAt reads the record of the key in identity slot of
// channelId; both stamps are 0 when no real request has used it.
func ChannelKeyActivityAt(channelId int, slot string, now int64) ChannelKeyActivity {
	value, ok := channelKeyActivities.Load(channelKey{channelId, slot})
	if !ok {
		return ChannelKeyActivity{Slot: slot}
	}
	return value.(*channelKeyActivity).snapshot(slot, now)
}

// StampChannelKeyKeepalive records that a keepalive went out on the key at now.
func StampChannelKeyKeepalive(channelId int, slot string, now int64) {
	loadChannelKeyActivity(channelKey{channelId, slot}).lastKeepalive.Store(now)
}

// SetChannelKeyActivityForTest overwrites one key's record, so tests can place
// real traffic and keepalives in the past.
func SetChannelKeyActivityForTest(channelId int, slot string, lastActive, lastKeepalive int64) {
	activity := new(channelKeyActivity)
	activity.lastActive.Store(lastActive)
	activity.lastKeepalive.Store(lastKeepalive)
	channelKeyActivities.Store(channelKey{channelId, slot}, activity)
}

// ForgetChannelKeysForTest drops the activity records and CLI sessions of
// every key of channelId.
func ForgetChannelKeysForTest(channelId int) {
	for _, store := range []*sync.Map{&channelKeyActivities, &channelLastCLISession} {
		store.Range(func(key, _ any) bool {
			if key.(channelKey).channelId == channelId {
				store.Delete(key)
			}
			return true
		})
	}
}

// ChannelCLISession is what the last real request shaped by a CLI profile
// (Codex or Claude Code) on one key looked like to the upstream: the model
// asked for, the user and token its client identity is scoped to, and the
// conversation seed its session id derives from. The upstream keepalive of that
// key continues this session, so to the upstream it is one more turn of the
// same client rather than a second client on the same key.
type ChannelCLISession struct {
	ModelName string
	UserId    int
	TokenId   int
	Seed      string
	// Reasoning is the Codex request's reasoning object as the caller sent it.
	Reasoning json.RawMessage
}

// channelLastCLISession lives in process memory for the same reason as the
// activity record: a restart forgets it along with the activity it belongs to.
var channelLastCLISession sync.Map // map[channelKey]ChannelCLISession

func RecordChannelCLISession(channelId int, slot string, session ChannelCLISession) {
	if channelId <= 0 {
		return
	}
	channelLastCLISession.Store(channelKey{channelId, slot}, session)
}

func LastChannelCLISession(channelId int, slot string) (ChannelCLISession, bool) {
	value, ok := channelLastCLISession.Load(channelKey{channelId, slot})
	if !ok {
		return ChannelCLISession{}, false
	}
	return value.(ChannelCLISession), true
}

// CLIConversationSeed is the conversation a CLI request belongs to, read in the
// order the CLI identity reads it: the session headers Codex and Claude Code
// send, then the body fields that carry the same session (prompt_cache_key,
// Codex's client_metadata.session_id, Claude Code's metadata.user_id). Empty
// when the request names no conversation.
func CLIConversationSeed(header http.Header, body []byte) string {
	for _, name := range []string{"session-id", "thread-id", "x-claude-code-session-id"} {
		if seed := header.Get(name); seed != "" {
			return seed
		}
	}
	if seed := gjson.GetBytes(body, "prompt_cache_key").String(); seed != "" {
		return seed
	}
	if seed := gjson.GetBytes(body, "client_metadata.session_id").String(); seed != "" {
		return seed
	}
	return gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String()
}

// HoldChannelSlot records that the request now owns a concurrency slot on
// channelId, giving back the slot, and the key, it held before.
//
// A request holds at most one slot at a time: the retry loop moves to another
// channel only after the previous attempt has finished, so keeping the old slot
// would make a request that retried across three channels count against all
// three for the rest of its life. Selection is what takes the slot (atomically,
// so a race for the last one has a single winner); this only tracks it so it can
// be given back.
func HoldChannelSlot(c *gin.Context, channelId int) {
	if c == nil {
		return
	}
	if previous := c.GetInt(ginKeyHeldChannelSlot); previous > 0 && previous != channelId {
		releaseHeldChannelKey(c)
		model.ReleaseChannelSlot(previous)
	}
	c.Set(ginKeyHeldChannelSlot, channelId)
}

// HoldChannelKey records that the request is now using the key in identity
// slot of channelId, giving back the key it used before. Only a request that
// holds channelId's concurrency slot is real traffic; a channel test or a
// keepalive holds none and records nothing.
func HoldChannelKey(c *gin.Context, channelId int, slot string) {
	if c == nil || channelId <= 0 || c.GetInt(ginKeyHeldChannelSlot) != channelId {
		return
	}
	activity := loadChannelKeyActivity(channelKey{channelId, slot})
	if held, _ := c.Get(ginKeyHeldChannelKey); held == activity {
		return
	}
	releaseHeldChannelKey(c)
	activity.inFlight.Add(1)
	activity.markActive()
	c.Set(ginKeyHeldChannelKey, activity)
}

func releaseHeldChannelKey(c *gin.Context) {
	held, _ := c.Get(ginKeyHeldChannelKey)
	activity, ok := held.(*channelKeyActivity)
	if !ok || activity == nil {
		return
	}
	c.Set(ginKeyHeldChannelKey, nil)
	activity.inFlight.Add(-1)
	activity.markActive()
}

// ReleaseHeldChannelSlot gives back whatever slot and key the request holds.
// It is idempotent, so the deferred call at the end of the request is safe
// even when the slot was already released.
func ReleaseHeldChannelSlot(c *gin.Context) {
	if c == nil {
		return
	}
	releaseHeldChannelKey(c)
	channelId := c.GetInt(ginKeyHeldChannelSlot)
	if channelId <= 0 {
		return
	}
	c.Set(ginKeyHeldChannelSlot, 0)
	model.ReleaseChannelSlot(channelId)
}
