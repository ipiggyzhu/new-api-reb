package model

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLogsForCleanup replaces the logs table with count rows at createdAt.
func seedLogsForCleanup(t *testing.T, oldCount int, oldCreatedAt int64, freshCount int, freshCreatedAt int64) {
	t.Helper()
	require.NoError(t, LOG_DB.Where("1 = 1").Delete(&Log{}).Error)

	logs := make([]*Log, 0, oldCount+freshCount)
	for i := 0; i < oldCount; i++ {
		logs = append(logs, &Log{CreatedAt: oldCreatedAt, Type: LogTypeConsume})
	}
	for i := 0; i < freshCount; i++ {
		logs = append(logs, &Log{CreatedAt: freshCreatedAt, Type: LogTypeConsume})
	}
	require.NoError(t, LOG_DB.CreateInBatches(logs, 100).Error)
}

func countLogsBefore(t *testing.T, targetTimestamp int64) int64 {
	t.Helper()
	var n int64
	require.NoError(t, LOG_DB.Model(&Log{}).Where("created_at < ?", targetTimestamp).Count(&n).Error)
	return n
}

// A purge batch must delete at most `limit` rows. GORM drops Limit() on
// Delete() for SQLite, so an unbounded DELETE would drain the table in one
// statement and hold the single writer for the whole purge.
func TestDeleteOldLogBatchIsBounded(t *testing.T) {
	seedLogsForCleanup(t, 25, 100, 5, 500)

	rowsAffected, err := DeleteOldLogBatch(context.Background(), 200, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 10, rowsAffected)
	assert.EqualValues(t, 15, countLogsBefore(t, 200))

	var total int64
	require.NoError(t, LOG_DB.Model(&Log{}).Count(&total).Error)
	assert.EqualValues(t, 20, total, "rows newer than the cutoff must survive")
}

func TestDeleteOldLogBatchClampsLimit(t *testing.T) {
	seedLogsForCleanup(t, maxLogDeleteBatch+50, 100, 0, 0)

	rowsAffected, err := DeleteOldLogBatch(context.Background(), 200, maxLogDeleteBatch+50)
	require.NoError(t, err)
	assert.EqualValues(t, maxLogDeleteBatch, rowsAffected)
	assert.EqualValues(t, 50, countLogsBefore(t, 200))
}

func TestDeleteOldLogBatchEmptyRange(t *testing.T) {
	seedLogsForCleanup(t, 0, 0, 5, 500)

	rowsAffected, err := DeleteOldLogBatch(context.Background(), 200, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 0, rowsAffected)

	var total int64
	require.NoError(t, LOG_DB.Model(&Log{}).Count(&total).Error)
	assert.EqualValues(t, 5, total)
}

func TestDeleteOldLogBatchHonoursCancelledContext(t *testing.T) {
	seedLogsForCleanup(t, 5, 100, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rowsAffected, err := DeleteOldLogBatch(ctx, 200, 10)
	require.Error(t, err)
	assert.EqualValues(t, 0, rowsAffected)
	assert.EqualValues(t, 5, countLogsBefore(t, 200), "a cancelled purge must not delete anything")
}

// The loop must drain every row older than the cutoff across multiple batches
// and stop, without touching newer rows.
func TestDeleteOldLogDrainsAcrossBatches(t *testing.T) {
	seedLogsForCleanup(t, 25, 100, 5, 500)

	total, err := DeleteOldLog(context.Background(), 200, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 25, total)
	assert.EqualValues(t, 0, countLogsBefore(t, 200))

	var remaining int64
	require.NoError(t, LOG_DB.Model(&Log{}).Count(&remaining).Error)
	assert.EqualValues(t, 5, remaining)
}
