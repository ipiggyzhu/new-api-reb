package model

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// openLogIndexTestDB builds a logs table the way AutoMigrate does, then adds the
// indexes an older schema left behind so repairLogIndexes has something to fix.
func openLogIndexTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	// A shared-cache in-memory database lives as long as one connection to it is
	// open, so the pool has to close or the schema leaks into the next run of this
	// test in the same process.
	t.Cleanup(func() {
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&Log{}))
	return db
}

func indexColumns(t *testing.T, db *gorm.DB, name string) []string {
	t.Helper()
	var columns []string
	require.NoError(t, db.Raw(
		fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", name),
	).Scan(&columns).Error)
	return columns
}

func logIndexNames(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var names []string
	require.NoError(t, db.Raw(
		"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'logs' AND name IS NOT NULL ORDER BY name",
	).Scan(&names).Error)
	return names
}

// TestRepairLogIndexesDropsIndexesNoQueryCanUse pins that the indexes older
// schemas left on the logs table are removed. logs takes one insert per relay
// request, so each of these is a B-tree update on the hottest write path in the
// system in exchange for nothing: idx_logs_ip indexes a column no query filters,
// sorts or groups by, and the other three are leading prefixes (or, for
// idx_token_created_ip, a superset differing only by created_at and the unused
// ip column) of indexes that remain.
//
// AutoMigrate never drops an index just because a struct tag stopped asking for
// one, so without this step they survive every restart. Production carried all
// four for months.
func TestRepairLogIndexesDropsIndexesNoQueryCanUse(t *testing.T) {
	db := openLogIndexTestDB(t)

	// Spelled out rather than derived from unusedLogIndexes: asserting against
	// the list under test would shrink the assertion along with the list.
	staleIndexes := map[string]string{
		"idx_logs_ip":          "CREATE INDEX `idx_logs_ip` ON `logs`(`ip`)",
		"idx_logs_model_name":  "CREATE INDEX `idx_logs_model_name` ON `logs`(`model_name`)",
		"idx_logs_user_id":     "CREATE INDEX `idx_logs_user_id` ON `logs`(`user_id`)",
		"idx_token_created_ip": "CREATE INDEX `idx_token_created_ip` ON `logs`(`token_id`,`created_at`,`ip`)",
	}
	for indexName, ddl := range staleIndexes {
		require.NoError(t, db.Exec(ddl).Error)
		require.True(t, db.Migrator().HasIndex(&Log{}, indexName),
			"fixture must start with %s present", indexName)
	}

	repairLogIndexes(db)

	for indexName := range staleIndexes {
		assert.False(t, db.Migrator().HasIndex(&Log{}, indexName),
			"%s costs a B-tree update per log insert and no query can use it", indexName)
	}
	// The indexes that cover those lookups must survive, or the drops trade a
	// write cost for a full table scan.
	for _, kept := range []string{
		"index_username_model_name",
		"idx_user_id_id",
		"idx_logs_token_id",
	} {
		assert.True(t, db.Migrator().HasIndex(&Log{}, kept),
			"%s is what makes dropping the redundant index safe", kept)
	}
}

// TestRepairLogIndexesRebuildsMisorderedTimeIndex pins that idx_created_at_id
// ends up led by created_at even when the database already has it built the
// other way round.
//
// The struct tag asks for (created_at, id) and the admin log listing ends in
// "ORDER BY created_at desc, id desc", but an older schema created the index as
// (id, created_at). AutoMigrate leaves an existing index alone once the name
// matches, whatever its columns, so the index stayed led by the integer primary
// key: useless for that ordering and for the "created_at < ?" range the cleanup
// job scans, while still costing a B-tree update on every insert. Production ran
// that way.
func TestRepairLogIndexesRebuildsMisorderedTimeIndex(t *testing.T) {
	db := openLogIndexTestDB(t)

	require.NoError(t, db.Migrator().DropIndex(&Log{}, "idx_created_at_id"))
	require.NoError(t, db.Exec("CREATE INDEX `idx_created_at_id` ON `logs`(`id`,`created_at`)").Error)
	require.Equal(t, []string{"id", "created_at"}, indexColumns(t, db, "idx_created_at_id"),
		"fixture must start from the reversed column order")

	repairLogIndexes(db)

	assert.Equal(t, []string{"created_at", "id"}, indexColumns(t, db, "idx_created_at_id"),
		"an index led by the primary key serves neither the log listing's ORDER BY nor the cleanup range scan")
}

// TestRepairLogIndexesIsIdempotent pins that the migration is a no-op once the
// schema is already correct: it runs on every startup, and a rebuild that fired
// unconditionally would drop and recreate an index on the largest table in the
// database each time the process restarts.
func TestRepairLogIndexesIsIdempotent(t *testing.T) {
	db := openLogIndexTestDB(t)

	before := logIndexNames(t, db)
	require.Contains(t, before, "idx_created_at_id")
	require.Equal(t, []string{"created_at", "id"}, indexColumns(t, db, "idx_created_at_id"),
		"AutoMigrate builds this index the right way round, so a fresh schema needs no repair")

	repairLogIndexes(db)
	repairLogIndexes(db)

	assert.Equal(t, before, logIndexNames(t, db),
		"a fresh schema must come out of the migration unchanged")
	assert.Equal(t, []string{"created_at", "id"}, indexColumns(t, db, "idx_created_at_id"))
}
