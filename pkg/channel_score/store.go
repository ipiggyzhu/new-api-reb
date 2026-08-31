package channel_score

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/go-redis/redis/v8"
)

const scoreKeyPrefix = "new-api:channel_score:v1:"

//go:embed lua/score_update.lua
var scoreUpdateScript string

// scoreUpdate is safe for concurrent use: redis.Script caches the SHA and falls
// back to a plain EVAL on NOSCRIPT, so a Redis restart repairs itself.
var scoreUpdate = redis.NewScript(scoreUpdateScript)

// snapshot is the immutable view the selection path reads. Handing out a
// snapshot rather than the live record keeps the hot path off the write lock.
type snapshot struct {
	tierOffset int
	total      int64
	success    int64
}

// entry is one key's record plus the lock that serializes its transitions and
// the published snapshot readers take.
type entry struct {
	mu    sync.Mutex
	state scoreState
	// published is read by the selection path without taking mu.
	published atomic.Pointer[snapshot]
	// generation increments on every Reset. A report that reached Redis before the
	// reset must not publish its now-obsolete result afterwards: the script had
	// already run, so its return value still carries the pre-reset streak and
	// window, and storing it would put the tier offset an admin just cleared right
	// back. Reports carry the generation they started under and are dropped when it
	// no longer matches.
	generation uint64
}

// resetLocked clears the record in place and invalidates any report already in
// flight. The entry is kept rather than deleted so the generation survives; a
// deleted entry would be recreated by that in-flight report with no way to tell
// that a reset had happened in between.
func (e *entry) resetLocked() {
	e.generation++
	e.state = scoreState{}
	e.published.Store(nil)
}

var localStore sync.Map // string -> *entry

// nowFunc is swappable so tests can drive the sliding window and idle reset
// deterministically instead of sleeping.
var nowFunc = time.Now

// scoreKey builds the per-(channel, group, model) key.
//
// The group is length-prefixed because both a group and a model name may contain
// a colon. Joining them with a bare separator made the key ambiguous: group "a"
// with model "b:c" and group "a:b" with model "c" produced the identical key, so
// two unrelated routes shared one streak, one success window and one tier offset.
// Model names with colons are ordinary (vendor-prefixed ids), which made this
// reachable rather than theoretical. The length prefix makes the split
// unambiguous without needing to escape anything; the model can then hold any
// bytes at all, since it is the final segment.
func scoreKey(channelId int, group string, model string) string {
	return scoreKeyPrefix + strconv.Itoa(channelId) + ":" +
		strconv.Itoa(len(group)) + ":" + group + ":" + model
}

// sharedStoreHealthTTLSeconds is how long one Redis reachability verdict is
// reused. The check exists to notice that Redis went away, which does not need
// per-request resolution; a few seconds of staleness only means scoring stays on
// (or off) slightly longer than strictly necessary.
const sharedStoreHealthTTLSeconds = 5

var (
	sharedStoreHealthy   atomic.Bool
	sharedStoreCheckedAt atomic.Int64
)

// sharedStoreUsable reports whether the shared store is available.
//
// When Redis is configured but unreachable, dynamic scoring switches off
// entirely rather than letting each instance accumulate its own counters. Split
// windows across instances produce routing that cannot be explained from any
// single instance's state, which is worse than the feature simply not applying.
// A deployment with no Redis at all is single-instance by construction, so the
// in-process store is the whole truth and stays in use.
//
// The verdict is cached because this sits on the request path three times over
// (candidate selection, score application, outcome reporting). Pinging per call
// added three Redis round trips to every relay request purely to ask a question
// whose answer changes on the timescale of an outage, not a request.
func sharedStoreUsable() bool {
	if !common.RedisEnabled || common.RDB == nil {
		// No Redis configured: single instance, local store is authoritative.
		return true
	}
	now := nowFunc().Unix()
	if checkedAt := sharedStoreCheckedAt.Load(); checkedAt > 0 && now-checkedAt < sharedStoreHealthTTLSeconds {
		return sharedStoreHealthy.Load()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	healthy := common.RDB.Ping(ctx).Err() == nil
	// Concurrent callers may each run a ping before the first result is stored.
	// That costs a few redundant pings on the very first request after the TTL
	// lapses and converges immediately; serializing them behind a lock would put
	// a Redis round trip in the path of every request that arrived meanwhile.
	sharedStoreHealthy.Store(healthy)
	sharedStoreCheckedAt.Store(now)
	return healthy
}

func redisActive() bool {
	return common.RedisEnabled && common.RDB != nil
}

// Report records one attempt's outcome for a channel.
//
// It never returns an error to the caller: scoring is an optimization, and a
// failure to record must not turn into a failed request. Problems surface
// through SysError instead.
func Report(channelId int, group string, model string, outcome Outcome) {
	setting := operation_setting.GetChannelDynamicScoreSetting()
	if !setting.Enabled || channelId <= 0 {
		return
	}
	// An outcome that is neither of the two defined values is a caller bug, and the
	// two backends used to disagree about what to do with it: the Redis path mapped
	// "not fault" to success while the local path mapped "not success" to fault, so
	// the same call promoted a channel on one deployment and demoted it on another.
	// Dropping it keeps the two paths identical and never invents a verdict.
	if outcome != OutcomeSuccess && outcome != OutcomeFault {
		common.SysError(fmt.Sprintf("channel score report ignored, unknown outcome: channel=%d, outcome=%d", channelId, int(outcome)))
		return
	}
	key := scoreKey(channelId, group, model)
	now := nowFunc().Unix()
	halfWidth := int64(setting.SuccessWindowSeconds) / 2
	if halfWidth <= 0 {
		halfWidth = 1
	}

	if redisActive() {
		outcomeArg := 1
		if outcome == OutcomeFault {
			outcomeArg = 2
		}
		// Read before the round trip: a Reset that lands while the script runs must
		// invalidate this result, and only a generation taken beforehand can detect
		// that.
		generation := entryGeneration(key)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := scoreUpdate.Run(ctx, common.RDB, []string{key},
			outcomeArg,
			now,
			halfWidth,
			setting.SuccessesToPromote,
			setting.FaultsToDemote,
			setting.MaxPromoteTiers,
			setting.MaxDemoteTiers,
			setting.IdleResetSeconds,
			scoreKeyTTLSeconds(setting),
		).Int64Slice()
		if err != nil {
			common.SysError(fmt.Sprintf("channel score redis update failed: key=%s, err=%v", key, err))
			return
		}
		if len(result) == 3 {
			// Mirror the shared result locally so the selection path can read it
			// without a Redis round trip per candidate.
			publishLocal(key, snapshot{
				tierOffset: int(result[0]),
				total:      result[1],
				success:    result[2],
			}, generation)
		}
		return
	}

	applyLocal(key, outcome, now, halfWidth, setting)
}

// scoreKeyTTLSeconds is how long a score record lives in Redis without being
// touched.
//
// It must outlast a full decay, not one idle period. The TTL used to equal
// IdleResetSeconds, so a demoted channel's record was deleted exactly one idle
// period after its last request — and since demotion is what stops traffic
// reaching a channel, that deletion was guaranteed. The verdict was destroyed
// before it could decay a single tier, which is the same defect the wholesale idle
// reset had, one layer down.
//
// The depth is bounded by MaxDemoteTiers, so a record needs at most that many idle
// periods to reach neutral. One extra period is added so the final decay is
// observed rather than racing the expiry, and the promote bound is included for
// symmetry with a positive offset.
func scoreKeyTTLSeconds(setting operation_setting.ChannelDynamicScoreSetting) int {
	if setting.IdleResetSeconds <= 0 {
		return 0
	}
	depth := setting.MaxDemoteTiers
	if setting.MaxPromoteTiers > depth {
		depth = setting.MaxPromoteTiers
	}
	if depth < 0 {
		depth = 0
	}
	return setting.IdleResetSeconds * (depth + 1)
}

// applyLocal performs the same state transition as the Lua script, in process.
// The whole transition happens under the entry's lock, which is what makes the
// idle-reset decision safe: reading updatedAt and acting on it cannot be split
// by another goroutine's update.
func applyLocal(key string, outcome Outcome, now int64, halfWidth int64, setting operation_setting.ChannelDynamicScoreSetting) {
	loaded, _ := localStore.LoadOrStore(key, &entry{})
	e := loaded.(*entry)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state.updatedAt > 0 && setting.IdleResetSeconds > 0 &&
		now-e.state.updatedAt >= int64(setting.IdleResetSeconds) {
		e.state.decayIdle(now, int64(setting.IdleResetSeconds))
	}

	e.state.rotate(now, halfWidth)
	e.state.curTotal++

	if outcome == OutcomeSuccess {
		e.state.curSuccess++
		e.state.faultCount = 0
		e.state.consecutiveSuccess++
		if setting.SuccessesToPromote > 0 && e.state.consecutiveSuccess >= setting.SuccessesToPromote {
			e.state.consecutiveSuccess = 0
			if e.state.tierOffset < setting.MaxPromoteTiers {
				e.state.tierOffset++
			}
		}
	} else {
		e.state.consecutiveSuccess = 0
		e.state.faultCount++
		if setting.FaultsToDemote > 0 && e.state.faultCount >= setting.FaultsToDemote {
			e.state.faultCount = 0
			if e.state.tierOffset > -setting.MaxDemoteTiers {
				e.state.tierOffset--
			}
		}
	}
	e.state.updatedAt = now

	total, success := e.state.sampleTotals()
	e.published.Store(&snapshot{
		tierOffset: e.state.tierOffset,
		total:      total,
		success:    success,
	})
}

// publishLocal mirrors a Redis-computed result into the local snapshot readers
// use. generation is the value read before the Redis call; a mismatch means a
// Reset landed while the script was running and this result describes state the
// admin has already discarded.
func publishLocal(key string, snap snapshot, generation uint64) {
	loaded, _ := localStore.LoadOrStore(key, &entry{})
	e := loaded.(*entry)
	e.mu.Lock()
	if e.generation != generation {
		e.mu.Unlock()
		return
	}
	e.state.updatedAt = nowFunc().Unix()
	e.state.tierOffset = snap.tierOffset
	// Stored under the lock so it cannot land after a concurrent reset has cleared
	// it.
	e.published.Store(&snap)
	e.mu.Unlock()
}

// entryGeneration returns the current generation for key, creating the entry if
// needed. Called before a Redis round trip so the result can be matched against
// the generation on the way back.
func entryGeneration(key string) uint64 {
	loaded, _ := localStore.LoadOrStore(key, &entry{})
	e := loaded.(*entry)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// lookup returns the published snapshot for a key, or nil when there is none or
// nothing survives the idle lapse. Nothing is mutated: the selection path must not
// take a write lock, and the record is rewritten on the next report anyway.
//
// An idle record is decayed on read rather than treated as absent. Reporting it
// absent is what let a demotion vanish from routing one idle period after it was
// applied — and since demoting a channel is what stops traffic reaching it, that
// period always elapsed. The offset therefore had no effect beyond the first idle
// window, no matter how deep it was, and a channel failing 100% of requests was
// returned to the top tier over and over.
//
// The sample window is reported empty in the idle case even though the offset
// survives. The two answer different questions: the offset is a standing verdict
// about where a channel belongs, while the rate is a measurement of how it is
// behaving right now, and a rate recorded before a long silence is not evidence
// about the present. An empty sample yields a neutral weight factor, so weight
// returns to the admin baseline while the demotion persists.
func lookup(key string, idleResetSeconds int) *snapshot {
	loaded, ok := localStore.Load(key)
	if !ok {
		return nil
	}
	e := loaded.(*entry)
	snap := e.published.Load()
	if snap == nil {
		return nil
	}
	if idleResetSeconds <= 0 {
		return snap
	}

	e.mu.Lock()
	updatedAt := e.state.updatedAt
	e.mu.Unlock()
	if updatedAt <= 0 {
		return snap
	}
	elapsed := nowFunc().Unix() - updatedAt
	if elapsed < int64(idleResetSeconds) {
		return snap
	}

	decayed := decayedOffset(snap.tierOffset, elapsed, int64(idleResetSeconds))
	if decayed == 0 {
		// Fully forgiven: identical to never having been scored.
		return nil
	}
	return &snapshot{tierOffset: decayed}
}

// decayedOffset moves offset one step toward neutral per elapsed idle period. It
// is the read-side counterpart of scoreState.decayIdle and must agree with it and
// with lua/score_update.lua.
func decayedOffset(offset int, elapsed int64, idleResetSeconds int64) int {
	if offset == 0 || idleResetSeconds <= 0 {
		return offset
	}
	periods := elapsed / idleResetSeconds
	if periods <= 0 {
		return offset
	}
	if offset > 0 {
		return offset - int(min64(periods, int64(offset)))
	}
	return offset + int(min64(periods, int64(-offset)))
}

// WarmFromShared repopulates the local mirror from Redis and reports how many
// records it restored.
//
// lookup reads only localStore, which starts empty in a fresh process. Redis held
// the authoritative state across the restart, but nothing put it back, so every
// channel routed at its admin baseline until live traffic repopulated the mirror
// one route at a time. For a demoted channel that is exactly backwards: the first
// requests after a restart go to the upstream that was demoted for failing them,
// and the operator sees the dead channel picked again for no visible reason.
//
// Idle decay is applied on read here rather than being written back: a record that
// went stale while the process was down should be as forgiven as it would have been
// had the process stayed up, but Redis remains the authority and the next Report
// recomputes it there anyway.
//
// Failures are logged and swallowed. A gateway that cannot reach Redis at startup
// must still serve traffic; it simply starts from the admin baseline, which is the
// same state it had before this function existed.
func WarmFromShared() int {
	if !redisActive() {
		return 0
	}
	setting := operation_setting.GetChannelDynamicScoreSetting()
	if !setting.Enabled {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	iter := common.RDB.Scan(ctx, 0, scoreKeyPrefix+"*", 200).Iterator()
	keys := make([]string, 0, 64)
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		common.SysError(fmt.Sprintf("channel score warm scan failed: err=%v", err))
		return 0
	}

	restored := 0
	for _, key := range keys {
		raw, err := common.RDB.HMGet(ctx, key, "off", "ct", "csu", "pt", "psu", "ua").Result()
		if err != nil {
			common.SysError(fmt.Sprintf("channel score warm read failed: key=%s, err=%v", key, err))
			continue
		}
		state := scoreState{
			tierOffset:  int(redisFieldInt(raw, 0)),
			curTotal:    redisFieldInt(raw, 1),
			curSuccess:  redisFieldInt(raw, 2),
			prevTotal:   redisFieldInt(raw, 3),
			prevSuccess: redisFieldInt(raw, 4),
			updatedAt:   redisFieldInt(raw, 5),
		}
		if state.updatedAt <= 0 {
			continue
		}
		// Keep the timestamp as Redis recorded it. lookup applies the same decay
		// when routing reads this mirror; pre-decaying here then retaining the old
		// timestamp would make lookup decay it a second time, while updating only the
		// local timestamp would make the next Redis Report disagree with the mirror.
		total, success := state.sampleTotals()
		if state.tierOffset == 0 && total == 0 {
			// Nothing to restore: this record would read identically to no record.
			continue
		}

		loaded, _ := localStore.LoadOrStore(key, &entry{})
		e := loaded.(*entry)
		e.mu.Lock()
		e.state = state
		e.published.Store(&snapshot{
			tierOffset: state.tierOffset,
			total:      total,
			success:    success,
		})
		e.mu.Unlock()
		restored++
	}
	return restored
}

// redisFieldInt reads one HMGET result position as an int64, treating a missing or
// malformed field as zero. HMGET returns a nil element for an absent field, so the
// type assertion has to tolerate it rather than indexing blindly.
func redisFieldInt(raw []any, index int) int64 {
	if index >= len(raw) || raw[index] == nil {
		return 0
	}
	str, ok := raw[index].(string)
	if !ok {
		return 0
	}
	value, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// Reset drops every score for one channel across all groups and models. Called
// from the model-layer mutation points, where an admin (or auto-ban) has just
// changed what the baseline means, so anything learned against the old baseline
// is no longer about the same channel.
//
// Redis is cleared before the local records, not after. A report that already
// ran the Lua script is holding a pre-reset result; clearing locally last means
// its generation is stale by the time it tries to publish, so it is dropped
// instead of writing the old tier offset back over the reset.
func Reset(channelId int) {
	if channelId <= 0 {
		return
	}
	prefix := scoreKeyPrefix + strconv.Itoa(channelId) + ":"
	resetSharedByPrefix(prefix, fmt.Sprintf("channel=%d", channelId))

	localStore.Range(func(k, v any) bool {
		key, _ := k.(string)
		if len(key) < len(prefix) || key[:len(prefix)] != prefix {
			return true
		}
		if e, ok := v.(*entry); ok {
			e.mu.Lock()
			e.resetLocked()
			e.mu.Unlock()
		}
		return true
	})
}

// resetSharedByPrefix deletes every shared key under prefix. label only
// identifies the caller in error messages.
func resetSharedByPrefix(prefix string, label string) {
	if !redisActive() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	iter := common.RDB.Scan(ctx, 0, prefix+"*", 200).Iterator()
	keys := make([]string, 0, 32)
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		common.SysError(fmt.Sprintf("channel score reset scan failed: %s, err=%v", label, err))
		return
	}
	if len(keys) == 0 {
		return
	}
	if err := common.RDB.Del(ctx, keys...).Err(); err != nil {
		common.SysError(fmt.Sprintf("channel score reset delete failed: %s, err=%v", label, err))
	}
}

// ResetMany resets a set of channels. Tag operations rewrite many channels in
// one statement, so the caller has a list rather than a single id.
func ResetMany(channelIds []int) {
	for _, channelId := range channelIds {
		Reset(channelId)
	}
}

// ResetAll drops every score. Reserved for explicit admin action and process
// start. It must never be wired to the periodic channel cache rebuild:
// SyncChannelCache calls InitChannelCache on a timer, so resetting there would
// erase every learned score once per sync interval and the feature would never
// accumulate anything.
func ResetAll() {
	resetSharedByPrefix(scoreKeyPrefix, "reset-all")

	localStore.Range(func(_, v any) bool {
		if e, ok := v.(*entry); ok {
			e.mu.Lock()
			e.resetLocked()
			e.mu.Unlock()
		}
		return true
	})
}
