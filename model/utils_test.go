package model

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedBoundedCountLogs(t *testing.T, userId int, rows int) {
	t.Helper()
	for i := 0; i < rows; i++ {
		require.NoError(t, DB.Create(&Log{
			UserId:    userId,
			Type:      LogTypeConsume,
			CreatedAt: int64(1000 + i),
			ModelName: "gpt-a",
			Username:  "alice",
		}).Error)
	}
}

// countUpTo must saturate at the limit instead of counting every matching row:
// `Limit(N).Count()` left the LIMIT on the aggregate, which caps the single
// output row and still scans the whole table.
func TestCountUpToSaturatesAtLimit(t *testing.T) {
	truncateTables(t)
	seedBoundedCountLogs(t, 1, 12)
	seedBoundedCountLogs(t, 2, 5)

	cases := []struct {
		name   string
		userId int
		limit  int
		want   int64
	}{
		{name: "limit above match count returns exact total", userId: 1, limit: 50, want: 12},
		{name: "limit equal to match count returns exact total", userId: 1, limit: 12, want: 12},
		{name: "limit below match count saturates at limit", userId: 1, limit: 8, want: 8},
		{name: "limit of one stops after one row", userId: 1, limit: 1, want: 1},
		{name: "filter still applies", userId: 2, limit: 8, want: 5},
		{name: "no match counts zero", userId: 99, limit: 8, want: 0},
		{name: "non positive limit means unbounded", userId: 1, limit: 0, want: 12},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var total int64
			require.NoError(t, countUpTo(DB.Model(&Log{}).Where("user_id = ?", tc.userId), tc.limit, &total))
			assert.Equal(t, tc.want, total)
		})
	}
}

// The bound has to live inside a derived table, otherwise the database still
// scans every matching row. The shape must also stay portable: PostgreSQL
// rejects an unaliased derived table.
func TestCountUpToPutsLimitInsideAliasedDerivedTable(t *testing.T) {
	truncateTables(t)
	seedBoundedCountLogs(t, 1, 3)

	var captured []string
	DB.Callback().Query().After("gorm:query").Register("test:capture_count_sql", func(tx *gorm.DB) {
		captured = append(captured, tx.Statement.SQL.String())
	})
	t.Cleanup(func() {
		DB.Callback().Query().Remove("test:capture_count_sql")
	})

	var total int64
	require.NoError(t, countUpTo(DB.Model(&Log{}).Where("user_id = ?", 1), 2, &total))

	var countSQL string
	for _, sql := range captured {
		if strings.Contains(sql, "count(*)") {
			countSQL = sql
		}
	}
	require.NotEmpty(t, countSQL, "no count query was captured: %v", captured)

	aliasAt := strings.Index(countSQL, ") as bounded")
	require.Greater(t, aliasAt, 0, "derived table must be aliased for PostgreSQL: %s", countSQL)
	limitAt := strings.Index(strings.ToUpper(countSQL), "LIMIT")
	require.Greater(t, limitAt, 0, "count must carry a LIMIT: %s", countSQL)
	assert.Less(t, limitAt, aliasAt, "LIMIT must sit inside the derived table: %s", countSQL)
}

// Call sites reuse the same query builder for the page fetch after counting, so
// the count must not leave a LIMIT or a derived table behind on it.
func TestBoundedCountCallSitesKeepPaginationSemantics(t *testing.T) {
	truncateTables(t)

	t.Run("GetUserLogs", func(t *testing.T) {
		seedBoundedCountLogs(t, 1, 12)

		logs, total, err := GetUserLogs(1, LogTypeUnknown, 0, 0, "", "", 0, 5, "", "", "")
		require.NoError(t, err)
		assert.Equal(t, int64(12), total)
		assert.Len(t, logs, 5)
	})

	t.Run("SearchUserTopUps", func(t *testing.T) {
		for i := 0; i < 7; i++ {
			require.NoError(t, DB.Create(&TopUp{
				UserId:     1,
				TradeNo:    "no-" + string(rune('a'+i)),
				Amount:     10,
				CreateTime: common.GetTimestamp(),
				Status:     common.TopUpStatusSuccess,
			}).Error)
		}

		// sanitizeLikePattern adds no wildcards, so a keyword without % is an exact
		// full match; "no-%" is what makes this a prefix search over all 7 rows.
		topups, total, err := SearchUserTopUps(1, "no-%", &common.PageInfo{Page: 1, PageSize: 3})
		require.NoError(t, err)
		assert.Equal(t, int64(7), total)
		assert.Len(t, topups, 3)
	})
}

// The task counters feed admin/user pagination. They must saturate at the hard
// limit instead of scanning the whole table, and they must still honour their
// WHERE clause — a count that quietly dropped its filters would report another
// user's rows and hand a user the wrong page count.
func TestTaskCountersAreBoundedAndFiltered(t *testing.T) {
	truncateTables(t)

	rows := taskCountHardLimit + 5
	tasks := make([]*Task, 0, rows+3)
	for i := 0; i < rows; i++ {
		tasks = append(tasks, &Task{
			TaskID:     "task-mine",
			Platform:   constant.TaskPlatformSuno,
			UserId:     1,
			Action:     "generate",
			Status:     TaskStatusSuccess,
			SubmitTime: int64(1000 + i),
		})
	}
	for i := 0; i < 3; i++ {
		tasks = append(tasks, &Task{
			TaskID:     "task-other",
			Platform:   constant.TaskPlatformSuno,
			UserId:     2,
			Action:     "generate",
			Status:     TaskStatusSuccess,
			SubmitTime: int64(1000 + i),
		})
	}
	require.NoError(t, DB.CreateInBatches(tasks, 500).Error)

	assert.Equal(t, int64(taskCountHardLimit), TaskCountAllTasks(SyncTaskQueryParams{}),
		"unfiltered admin count must saturate at the hard limit")
	assert.Equal(t, int64(3), TaskCountAllTasks(SyncTaskQueryParams{UserID: "2"}),
		"admin count must still apply its filters")
	assert.Equal(t, int64(taskCountHardLimit), TaskCountAllUserTask(1, SyncTaskQueryParams{}),
		"user count must saturate at the hard limit")
	assert.Equal(t, int64(3), TaskCountAllUserTask(2, SyncTaskQueryParams{}),
		"user count must only see its own user's tasks")
	assert.Equal(t, int64(0), TaskCountAllUserTask(2, SyncTaskQueryParams{Status: "NOT_A_STATUS"}),
		"a filter matching nothing must count zero, not fall back to every row")
}

// Same invariant for the midjourney table, which has its own counters and its
// own limit constant.
func TestMidjourneyCountersAreBoundedAndFiltered(t *testing.T) {
	truncateTables(t)

	rows := midjourneyCountHardLimit + 5
	tasks := make([]*Midjourney, 0, rows+3)
	for i := 0; i < rows; i++ {
		tasks = append(tasks, &Midjourney{
			UserId:     1,
			Action:     "IMAGINE",
			MjId:       "mj-mine",
			Status:     "SUCCESS",
			SubmitTime: int64(1000 + i),
		})
	}
	for i := 0; i < 3; i++ {
		tasks = append(tasks, &Midjourney{
			UserId:     2,
			Action:     "IMAGINE",
			MjId:       "mj-other",
			Status:     "SUCCESS",
			SubmitTime: int64(1000 + i),
		})
	}
	require.NoError(t, DB.CreateInBatches(tasks, 500).Error)

	assert.Equal(t, int64(midjourneyCountHardLimit), CountAllTasks(TaskQueryParams{}),
		"unfiltered admin count must saturate at the hard limit")
	assert.Equal(t, int64(3), CountAllTasks(TaskQueryParams{MjID: "mj-other"}),
		"admin count must still apply its filters")
	assert.Equal(t, int64(midjourneyCountHardLimit), CountAllUserTask(1, TaskQueryParams{}),
		"user count must saturate at the hard limit")
	assert.Equal(t, int64(3), CountAllUserTask(2, TaskQueryParams{}),
		"user count must only see its own user's tasks")
	assert.Equal(t, int64(0), CountAllUserTask(2, TaskQueryParams{MjID: "mj-mine"}),
		"a filter matching nothing must count zero, not fall back to every row")
}

// The index repair only helps if migration actually runs it, so assert the
// wiring and not just the helper. What each repair is for is covered in
// log_index_repair_test.go.
func TestMigrateLogDBRepairsLogIndexes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))
	require.NoError(t, db.Exec("CREATE INDEX IF NOT EXISTS `idx_logs_ip` ON `logs`(`ip`)").Error)
	require.NoError(t, db.Migrator().DropIndex(&Log{}, "idx_created_at_id"))
	require.NoError(t, db.Exec("CREATE INDEX `idx_created_at_id` ON `logs`(`id`,`created_at`)").Error)

	original := LOG_DB
	LOG_DB = db
	t.Cleanup(func() { LOG_DB = original })

	require.NoError(t, migrateLOGDB())
	assert.False(t, db.Migrator().HasIndex(&Log{}, "idx_logs_ip"),
		"migrateLOGDB must drop the unused ip index on an upgraded log database")
	assert.Equal(t, []string{"created_at", "id"}, indexColumns(t, db, "idx_created_at_id"),
		"migrateLOGDB must rebuild the misordered time index")
}
