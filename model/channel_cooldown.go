package model

import (
	"strconv"
	"sync"
	"time"
)

// channelCooldowns records, per channel and model, when an upstream that just
// answered 429 or 529 is expected to take requests again. Selection skips a
// cooling channel while any other candidate remains, so the next requests stop
// spending their first attempt — and its latency — on a channel that already
// said it would refuse them. With nothing else left, a cooling channel is still
// chosen: a probably-refused request beats a guaranteed 503.
//
// The key carries the model because Anthropic and OpenAI limit each model
// separately, so one model being exhausted says nothing about another on the
// same channel.
//
// Per process, like the concurrency gauge: production runs a single instance
// without Redis. With several instances each one learns the cooldown from its
// own first refusal.
//
// A multi-key channel is never cooled as a whole, since its other keys may still
// serve; the key that failed cools instead, under its own entry.
var channelCooldowns sync.Map // map[string]time.Time

func channelCooldownKey(channelId int, modelName string) string {
	return strconv.Itoa(channelId) + ":" + modelName
}

// channelKeyCooldownKey names one key of a multi-key channel by its
// fingerprint. The "/" after the channel id keeps it apart from every
// channelCooldownKey.
func channelKeyCooldownKey(channelId int, fingerprint string, modelName string) string {
	return strconv.Itoa(channelId) + "/" + fingerprint + ":" + modelName
}

// MarkChannelCooldown keeps the channel out of selection for modelName until
// until.
func MarkChannelCooldown(channelId int, modelName string, until time.Time) {
	markCooldown(channelCooldownKey(channelId, modelName), until)
}

// MarkChannelKeyCooldown keeps one key of a multi-key channel out of its
// conversations' sticky selection (GetStickyEnabledKey) for modelName until
// until.
func MarkChannelKeyCooldown(channelId int, key string, modelName string, until time.Time) {
	markCooldown(channelKeyCooldownKey(channelId, KeyFingerprint(key), modelName), until)
}

// markCooldown records until for key. A later deadline already recorded is
// kept: a short retry-after from one request must not cut short a longer window
// another request learned.
func markCooldown(key string, until time.Time) {
	for {
		current, loaded := channelCooldowns.LoadOrStore(key, until)
		if !loaded {
			return
		}
		if !until.After(current.(time.Time)) {
			return
		}
		if channelCooldowns.CompareAndSwap(key, current, until) {
			return
		}
	}
}

// ChannelCoolingDown reports whether the channel is still inside a cooldown for
// modelName.
func ChannelCoolingDown(channelId int, modelName string) bool {
	return coolingDown(channelCooldownKey(channelId, modelName))
}

// coolingDown reports whether key is still inside its cooldown. An expired
// entry is removed on the way out, which keeps the map bounded by the entries
// that were refused recently.
func coolingDown(key string) bool {
	value, ok := channelCooldowns.Load(key)
	if !ok {
		return false
	}
	if time.Now().Before(value.(time.Time)) {
		return true
	}
	channelCooldowns.CompareAndDelete(key, value)
	return false
}
