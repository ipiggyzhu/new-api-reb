package channel_score

import (
	"context"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func useSharedScoreStore(t *testing.T) *redis.Client {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	originalClient, originalEnabled := common.RDB, common.RedisEnabled
	originalHealthy, originalCheckedAt := sharedStoreHealthy.Load(), sharedStoreCheckedAt.Load()
	common.RDB, common.RedisEnabled = client, true
	sharedStoreCheckedAt.Store(0)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		common.RDB, common.RedisEnabled = originalClient, originalEnabled
		sharedStoreHealthy.Store(originalHealthy)
		sharedStoreCheckedAt.Store(originalCheckedAt)
	})
	return client
}

// takeScoreMirror lets two simulated gateway instances use the same Redis while
// retaining separate in-process entries, without copying a live sync.Map.
func takeScoreMirror() map[string]*entry {
	saved := make(map[string]*entry)
	localStore.Range(func(key, value any) bool {
		saved[key.(string)] = value.(*entry)
		localStore.Delete(key)
		return true
	})
	return saved
}

func TestSharedResetRefreshesOtherInstance(t *testing.T) {
	for _, reset := range []struct {
		name              string
		run               func()
		remainingPriority int64
	}{
		{"one channel", func() { Reset(201) }, -1},
		{"channel batch", func() { ResetMany([]int{201}) }, -1},
		{"all channels", ResetAll, 0},
	} {
		t.Run(reset.name, func(t *testing.T) {
			useSharedScoreStore(t)
			now := useScoreSettingForTest(t, nil)
			candidates := []Candidate{{ChannelId: 201, Weight: 10}, {ChannelId: 202, Weight: 10}}
			Report(201, testGroup, testModel, OutcomeFault)
			Report(202, testGroup, testModel, OutcomeFault)
			before := ApplyToCandidates(testGroup, testModel, candidates)
			require.EqualValues(t, -1, before[0].Priority)

			firstInstance := takeScoreMirror()
			reset.run()
			takeScoreMirror()
			for key, value := range firstInstance {
				localStore.Store(key, value)
			}

			*now += 5
			after := ApplyToCandidates(testGroup, testModel, candidates)
			assert.EqualValues(t, 0, after[0].Priority, "another instance's reset must expire the stale mirror")
			assert.Equal(t, reset.remainingPriority, after[1].Priority)
			assert.Empty(t, Snapshot(ScoreFilter{ChannelID: 201}).Rows)
		})
	}
}

func TestSharedSnapshotObservesResetWithoutRouting(t *testing.T) {
	useSharedScoreStore(t)
	now := useScoreSettingForTest(t, nil)
	Report(211, testGroup, testModel, OutcomeFault)
	firstInstance := takeScoreMirror()
	Reset(211)
	for key, value := range firstInstance {
		localStore.Store(key, value)
	}
	*now += 5
	assert.Empty(t, Snapshot(ScoreFilter{ChannelID: 211}).Rows,
		"diagnostics and projection must not preserve another instance's discarded score")
}

func TestSharedCandidatesLoadOtherInstanceScores(t *testing.T) {
	useSharedScoreStore(t)
	useScoreSettingForTest(t, nil)
	Report(221, testGroup, testModel, OutcomeFault)
	takeScoreMirror()
	candidates := []Candidate{{ChannelId: 221, Weight: 10}, {ChannelId: 222, Weight: 10}}
	adjusted := ApplyToCandidates(testGroup, testModel, candidates)
	assert.EqualValues(t, -1, adjusted[0].Priority, "a cold instance must use the shared verdict")
	assert.EqualValues(t, 0, adjusted[1].Priority)
}

type scoreRedisHook struct {
	before        func(redis.Cmder)
	afterPipeline func([]redis.Cmder)
}

func (h scoreRedisHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if h.before != nil {
		h.before(cmd)
	}
	return ctx, nil
}
func (h scoreRedisHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (h scoreRedisHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	if h.before != nil {
		for _, cmd := range cmds {
			h.before(cmd)
		}
	}
	return ctx, nil
}
func (h scoreRedisHook) AfterProcessPipeline(_ context.Context, cmds []redis.Cmder) error {
	if h.afterPipeline != nil {
		h.afterPipeline(cmds)
	}
	return nil
}

func TestResetManyUsesOneSharedKeyspacePass(t *testing.T) {
	client := useSharedScoreStore(t)
	useScoreSettingForTest(t, nil)
	Report(231, testGroup, testModel, OutcomeFault)
	Report(231, "another-group", testModel, OutcomeFault)
	Report(232, testGroup, testModel, OutcomeFault)
	Report(233, testGroup, testModel, OutcomeFault)
	var scans int
	client.AddHook(scoreRedisHook{before: func(cmd redis.Cmder) {
		if cmd.Name() == "scan" && cmd.Args()[1] == uint64(0) {
			scans++
		}
	}})
	ResetMany([]int{231, 232, 231, 0, -1})
	assert.Equal(t, 1, scans, "one tag operation must not rescan the complete keyspace for each channel")
	assert.EqualValues(t, 0, offsetOf(t, 231))
	assert.EqualValues(t, 0, offsetOf(t, 232))
	assert.EqualValues(t, -1, offsetOf(t, 233))
	keys, err := client.Keys(context.Background(), scoreKeyPrefix+"*").Result()
	require.NoError(t, err)
	assert.Equal(t, []string{scoreKey(233, testGroup, testModel)}, keys)
}

func TestSharedRefreshCannotRestoreResetScore(t *testing.T) {
	client := useSharedScoreStore(t)
	now := useScoreSettingForTest(t, nil)
	Report(241, testGroup, testModel, OutcomeFault)
	*now += 5
	fetched, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	client.AddHook(scoreRedisHook{afterPipeline: func(cmds []redis.Cmder) {
		for _, cmd := range cmds {
			if cmd.Name() == "hmget" {
				once.Do(func() { close(fetched); <-release })
				return
			}
		}
	}})
	done := make(chan []Candidate, 1)
	go func() { done <- ApplyToCandidates(testGroup, testModel, []Candidate{{ChannelId: 241, Weight: 10}}) }()
	select {
	case <-fetched:
		Reset(241)
		close(release)
		adjusted := <-done
		assert.EqualValues(t, 0, adjusted[0].Priority)
		assert.Empty(t, Snapshot(ScoreFilter{ChannelID: 241}).Rows)
	case <-done:
		t.Fatal("expired mirror was used without consulting the shared store")
	}
}

func TestSharedRefreshFailureDoesNotProjectStaleScores(t *testing.T) {
	client := useSharedScoreStore(t)
	now := useScoreSettingForTest(t, nil)
	Report(251, testGroup, testModel, OutcomeFault)
	*now += 5
	// Redis is reachable, but the score read itself fails. A successful health
	// check must not turn a stale mirror into an apparently current projection.
	require.NoError(t, client.Set(context.Background(), scoreKey(251, testGroup, testModel), "invalid-score-record", 0).Err())
	candidates := []Candidate{{ChannelId: 251, Weight: 10}}
	assert.Equal(t, candidates, ApplyToCandidates(testGroup, testModel, candidates))
	projections, snapshot := ProjectByChannel()
	assert.False(t, snapshot.Usable)
	assert.Empty(t, projections)
}
