package model

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// resetBatchUpdateStores clears the package level pending maps so one test never
// inherits another test's deltas, and restores them afterwards.
func resetBatchUpdateStores(t *testing.T) {
	t.Helper()
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
	}
	t.Cleanup(func() {
		for i := 0; i < BatchUpdateTypeCount; i++ {
			batchUpdateLocks[i].Lock()
			batchUpdateStores[i] = make(map[int]int)
			batchUpdateLocks[i].Unlock()
		}
	})
}

func pendingBatchValue(updateType int, id int) (int, bool) {
	batchUpdateLocks[updateType].Lock()
	defer batchUpdateLocks[updateType].Unlock()
	value, ok := batchUpdateStores[updateType][id]
	return value, ok
}

// captureBatchUpdateConnPools records the connection each write in the flush ran
// on. Inside DB.Transaction GORM swaps Statement.ConnPool for the *sql.Tx, so
// identical pools across every statement is the observable proof that the flush
// used one transaction instead of one autocommit per update.
func captureBatchUpdateConnPools(t *testing.T) *[]gorm.ConnPool {
	t.Helper()
	pools := make([]gorm.ConnPool, 0, 8)
	record := func(tx *gorm.DB) {
		pools = append(pools, tx.Statement.ConnPool)
	}
	require.NoError(t, DB.Callback().Update().After("gorm:update").Register("test:capture_batch_pool", record))
	t.Cleanup(func() {
		DB.Callback().Update().Remove("test:capture_batch_pool")
	})
	return &pools
}

// batchUpdate used to flush every pending delta as its own autocommit
// transaction. On SQLite each one takes the single write lock and fsyncs
// separately, so a flush cost one lock acquisition and one fsync per pending id.
func TestBatchUpdateFlushesInOneTransaction(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateStores(t)

	user := &User{Username: "batch-tx-user", Password: "placeholder", AffCode: "batch-tx-aff", Quota: 500}
	require.NoError(t, DB.Create(user).Error)
	token := &Token{UserId: user.Id, Key: "batch-tx-token-key", Name: "batch-tx", RemainQuota: 200}
	require.NoError(t, DB.Create(token).Error)
	channel := &Channel{Id: 4100, Name: "batch-tx-channel"}
	require.NoError(t, DB.Create(channel).Error)

	addNewRecord(BatchUpdateTypeUserQuota, user.Id, 30)
	addNewRecord(BatchUpdateTypeUsedQuota, user.Id, 12)
	addNewRecord(BatchUpdateTypeRequestCount, user.Id, 3)
	addNewRecord(BatchUpdateTypeTokenQuota, token.Id, 40)
	addNewRecord(BatchUpdateTypeChannelUsedQuota, channel.Id, 55)

	pools := captureBatchUpdateConnPools(t)
	batchUpdate()

	require.NotEmpty(t, *pools, "flush issued no UPDATE at all")
	for _, pool := range *pools {
		_, isTx := pool.(*sql.Tx)
		assert.True(t, isTx, "every write in the flush must run inside a transaction")
	}
	for i, pool := range *pools {
		assert.Same(t, (*pools)[0], pool, "write %d ran on a different connection, so the flush was not one transaction", i)
	}

	var got User
	require.NoError(t, DB.First(&got, user.Id).Error)
	var gotToken Token
	require.NoError(t, DB.First(&gotToken, token.Id).Error)
	var gotChannel Channel
	require.NoError(t, DB.First(&gotChannel, channel.Id).Error)

	cases := []struct {
		name string
		want int
		got  int
	}{
		{name: "user quota increased", want: 530, got: got.Quota},
		{name: "user used quota increased", want: 12, got: got.UsedQuota},
		{name: "user request count increased", want: 3, got: got.RequestCount},
		{name: "token remain quota increased", want: 240, got: gotToken.RemainQuota},
		{name: "token used quota decreased", want: -40, got: gotToken.UsedQuota},
		{name: "channel used quota increased", want: 55, got: int(gotChannel.UsedQuota)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}

	for updateType := 0; updateType < BatchUpdateTypeCount; updateType++ {
		_, still := pendingBatchValue(updateType, user.Id)
		assert.False(t, still, "a successful flush must drain store %d", updateType)
	}
}

// A failing item must not silently drop the rest of the batch. The flush aborts
// so no row is left half-updated, then replays the whole snapshot into the
// pending stores, so the next flush retries it instead of losing quota that the
// caller already spent in memory.
func TestBatchUpdateRequeuesEveryDeltaWhenFlushFails(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateStores(t)

	user := &User{Username: "batch-fail-user", Password: "placeholder", AffCode: "batch-fail-aff", Quota: 500}
	require.NoError(t, DB.Create(user).Error)
	token := &Token{UserId: user.Id, Key: "batch-fail-token-key", Name: "batch-fail", RemainQuota: 200}
	require.NoError(t, DB.Create(token).Error)
	channel := &Channel{Id: 4200, Name: "batch-fail-channel"}
	require.NoError(t, DB.Create(channel).Error)

	addNewRecord(BatchUpdateTypeUserQuota, user.Id, 30)
	addNewRecord(BatchUpdateTypeTokenQuota, token.Id, 40)
	addNewRecord(BatchUpdateTypeChannelUsedQuota, channel.Id, 55)

	// The channel write is the last one the flush issues, so failing it proves the
	// earlier token and user writes were rolled back rather than merely skipped.
	injected := errors.New("injected channel failure")
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("test:fail_channel_update", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() {
		DB.Callback().Update().Remove("test:fail_channel_update")
	})

	batchUpdate()

	var got User
	require.NoError(t, DB.First(&got, user.Id).Error)
	var gotToken Token
	require.NoError(t, DB.First(&gotToken, token.Id).Error)
	var gotChannel Channel
	require.NoError(t, DB.First(&gotChannel, channel.Id).Error)

	rolledBack := []struct {
		name string
		want int
		got  int
	}{
		{name: "user quota untouched", want: 500, got: got.Quota},
		{name: "user used quota untouched", want: 0, got: got.UsedQuota},
		{name: "token remain quota untouched", want: 200, got: gotToken.RemainQuota},
		{name: "channel used quota untouched", want: 0, got: int(gotChannel.UsedQuota)},
	}
	for _, tc := range rolledBack {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got, "failed flush must roll back every write")
		})
	}

	requeued := []struct {
		name       string
		updateType int
		id         int
		want       int
	}{
		{name: "user quota requeued", updateType: BatchUpdateTypeUserQuota, id: user.Id, want: 30},
		{name: "token quota requeued", updateType: BatchUpdateTypeTokenQuota, id: token.Id, want: 40},
		{name: "channel used quota requeued", updateType: BatchUpdateTypeChannelUsedQuota, id: channel.Id, want: 55},
	}
	for _, tc := range requeued {
		t.Run(tc.name, func(t *testing.T) {
			value, ok := pendingBatchValue(tc.updateType, tc.id)
			require.True(t, ok, "delta was dropped instead of requeued")
			assert.Equal(t, tc.want, value)
		})
	}
}

// A delta that arrives while the flush is mid-transaction belongs to the next
// snapshot: the flush detaches the pending maps before it touches the database,
// so the new delta must survive the flush it raced.
func TestBatchUpdateKeepsDeltasAddedDuringFlush(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateStores(t)

	user := &User{Username: "batch-race-user", Password: "placeholder", AffCode: "batch-race-aff", Quota: 500}
	require.NoError(t, DB.Create(user).Error)

	addNewRecord(BatchUpdateTypeUserQuota, user.Id, 30)

	var once bool
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("test:add_during_flush", func(tx *gorm.DB) {
		if once {
			return
		}
		once = true
		addNewRecord(BatchUpdateTypeUserQuota, user.Id, 7)
	}))
	t.Cleanup(func() {
		DB.Callback().Update().Remove("test:add_during_flush")
	})

	batchUpdate()

	var got User
	require.NoError(t, DB.First(&got, user.Id).Error)
	assert.Equal(t, 530, got.Quota, "the snapshot taken before the flush must be applied")

	value, ok := pendingBatchValue(BatchUpdateTypeUserQuota, user.Id)
	require.True(t, ok, "a delta logged during the flush must not be swallowed")
	assert.Equal(t, 7, value)
}
