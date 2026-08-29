package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedFlowQuotaData(t *testing.T, quotaData QuotaData) {
	t.Helper()
	require.NoError(t, DB.Create(&quotaData).Error)
}

func seedFlowLookupData(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Create(&Channel{Id: 1, Name: "east"}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 2, Name: "west"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 11, UserId: 1, Key: "sk-primary", Name: "primary"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 22, UserId: 2, Key: "sk-backup", Name: "backup"}).Error)
	require.NoError(t, DB.Delete(&Token{Id: 11}).Error)
}

func TestGetFlowQuotaDataUsesQuotaDataRoleSpecificDimensions(t *testing.T) {
	truncateTables(t)
	seedFlowLookupData(t)

	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1000,
		Count:     2,
		Quota:     100,
		TokenUsed: 40,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1100,
		Count:     1,
		Quota:     50,
		TokenUsed: 20,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 2,
		CreatedAt: 1200,
		Count:     1,
		Quota:     25,
		TokenUsed: 10,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    2,
		Username:  "bob",
		NodeName:  "node-b",
		TokenID:   22,
		UseGroup:  "default",
		ModelName: "gpt-b",
		ChannelID: 1,
		CreatedAt: 1300,
		Count:     3,
		Quota:     70,
		TokenUsed: 30,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		ModelName: "legacy",
		CreatedAt: 1400,
		Count:     99,
		Quota:     999,
		TokenUsed: 999,
	})

	rootRows, err := GetFlowQuotaData(900, 2000, "", 0, common.RoleRootUser)
	require.NoError(t, err)
	require.Len(t, rootRows, 3)
	// Token 11 was soft-deleted, so its name is intentionally left empty for the
	// frontend to render a localized "deleted (id)" label instead.
	require.Equal(t, FlowQuotaData{
		UserID:      1,
		Username:    "alice",
		NodeName:    "node-a",
		TokenID:     11,
		TokenName:   "",
		UseGroup:    "vip",
		ChannelID:   1,
		ChannelName: "east",
		ModelName:   "gpt-a",
		TokenUsed:   60,
		Count:       3,
		Quota:       150,
	}, *rootRows[0])
	// A token that still exists resolves to its current name.
	require.Equal(t, 22, rootRows[1].TokenID)
	require.Equal(t, "backup", rootRows[1].TokenName)

	adminRows, err := GetFlowQuotaData(900, 2000, "alice", 0, common.RoleAdminUser)
	require.NoError(t, err)
	require.Len(t, adminRows, 2)
	require.Equal(t, 0, adminRows[0].TokenID)
	require.Empty(t, adminRows[0].TokenName)
	require.Empty(t, adminRows[0].NodeName)
	require.Equal(t, "alice", adminRows[0].Username)
	require.Equal(t, "vip", adminRows[0].UseGroup)
	require.Equal(t, "east", adminRows[0].ChannelName)
	require.Equal(t, 150, adminRows[0].Quota)

	selfRows, err := GetFlowQuotaData(900, 2000, "", 1, common.RoleCommonUser)
	require.NoError(t, err)
	require.Len(t, selfRows, 1)
	require.Empty(t, selfRows[0].Username)
	require.Equal(t, 0, selfRows[0].ChannelID)
	require.Empty(t, selfRows[0].ChannelName)
	require.Empty(t, selfRows[0].TokenName)
	require.Equal(t, "vip", selfRows[0].UseGroup)
	require.Equal(t, 175, selfRows[0].Quota)
}

func TestLogQuotaDataSplitsRowsByUseGroupTokenChannelAndNode(t *testing.T) {
	truncateTables(t)
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()

	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3661,
		UseGroup:  "vip",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     100,
		TokenUsed: 40,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3700,
		UseGroup:  "vip",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     50,
		TokenUsed: 20,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3700,
		UseGroup:  "default",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     25,
		TokenUsed: 10,
	})

	SaveQuotaDataCache()

	var rows []QuotaData
	require.NoError(t, DB.Order("quota DESC").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.Equal(t, int64(3600), rows[0].CreatedAt)
	require.Equal(t, "vip", rows[0].UseGroup)
	require.Equal(t, 11, rows[0].TokenID)
	require.Equal(t, 1, rows[0].ChannelID)
	require.Equal(t, "node-a", rows[0].NodeName)
	require.Equal(t, 2, rows[0].Count)
	require.Equal(t, 150, rows[0].Quota)
	require.Equal(t, 60, rows[0].TokenUsed)
	require.Equal(t, "default", rows[1].UseGroup)
	require.Equal(t, 25, rows[1].Quota)
}

func resetQuotaDataCache(t *testing.T) {
	t.Helper()
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()
}

func flowLogParams(useGroup string, quota int, tokenUsed int) QuotaDataLogParams {
	return QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3600,
		UseGroup:  useGroup,
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     quota,
		TokenUsed: tokenUsed,
	}
}

// A second flush for the same hour bucket must add onto the row the first flush
// wrote instead of inserting a duplicate. The flush decides between UPDATE and
// INSERT from the UPDATE's RowsAffected now that the probing SELECT is gone, so
// this pins the branch that replaced it.
func TestSaveQuotaDataCacheUpsertsRepeatedFlushesIntoOneRow(t *testing.T) {
	cases := []struct {
		name          string
		flushes       [][]QuotaDataLogParams
		wantRows      int
		wantCount     int
		wantQuota     int
		wantTokenUsed int
	}{
		{
			name:          "first flush inserts the bucket",
			flushes:       [][]QuotaDataLogParams{{flowLogParams("vip", 100, 40)}},
			wantRows:      1,
			wantCount:     1,
			wantQuota:     100,
			wantTokenUsed: 40,
		},
		{
			name: "second flush updates the same bucket",
			flushes: [][]QuotaDataLogParams{
				{flowLogParams("vip", 100, 40)},
				{flowLogParams("vip", 25, 10)},
			},
			wantRows:      1,
			wantCount:     2,
			wantQuota:     125,
			wantTokenUsed: 50,
		},
		{
			name: "three flushes keep accumulating onto one row",
			flushes: [][]QuotaDataLogParams{
				{flowLogParams("vip", 100, 40)},
				{flowLogParams("vip", 25, 10)},
				{flowLogParams("vip", 5, 2)},
			},
			wantRows:      1,
			wantCount:     3,
			wantQuota:     130,
			wantTokenUsed: 52,
		},
		{
			name: "entries batched inside one flush collapse before the write",
			flushes: [][]QuotaDataLogParams{
				{flowLogParams("vip", 100, 40), flowLogParams("vip", 25, 10)},
			},
			wantRows:      1,
			wantCount:     2,
			wantQuota:     125,
			wantTokenUsed: 50,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			resetQuotaDataCache(t)

			for _, flush := range tc.flushes {
				for _, params := range flush {
					LogQuotaData(params)
				}
				SaveQuotaDataCache()
			}

			var rows []QuotaData
			require.NoError(t, DB.Find(&rows).Error)
			require.Len(t, rows, tc.wantRows)
			assert.Equal(t, tc.wantCount, rows[0].Count)
			assert.Equal(t, tc.wantQuota, rows[0].Quota)
			assert.Equal(t, tc.wantTokenUsed, rows[0].TokenUsed)
		})
	}
}

// The flush must not still hold CacheQuotaDataLock once it starts talking to the
// database. LogQuotaData takes that same mutex for every billed request, so a
// flush that held it across its round trips stalled the request path for the
// whole flush, every DataExportInterval minutes.
func TestSaveQuotaDataCacheReleasesLockBeforeDatabaseWork(t *testing.T) {
	cases := []struct {
		name string
		seed bool
	}{
		{name: "insert path", seed: false},
		{name: "update path", seed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			resetQuotaDataCache(t)

			if tc.seed {
				LogQuotaData(flowLogParams("vip", 10, 5))
				SaveQuotaDataCache()
			}

			// TryLock from inside the flush's own database callback. sync.Mutex is
			// not reentrant, so a flush still holding the lock fails this probe on
			// the very goroutine that holds it.
			probes := 0
			lockHeld := false
			probe := func(tx *gorm.DB) {
				if tx.Statement.Table != "quota_data" {
					return
				}
				probes++
				if CacheQuotaDataLock.TryLock() {
					CacheQuotaDataLock.Unlock()
					return
				}
				lockHeld = true
			}
			require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("test:quota_lock_probe", probe))
			require.NoError(t, DB.Callback().Create().Before("gorm:create").Register("test:quota_lock_probe", probe))
			t.Cleanup(func() {
				DB.Callback().Update().Remove("test:quota_lock_probe")
				DB.Callback().Create().Remove("test:quota_lock_probe")
			})

			LogQuotaData(flowLogParams("vip", 100, 40))
			SaveQuotaDataCache()

			require.Positive(t, probes, "flush did no quota_data write, probe proved nothing")
			assert.False(t, lockHeld, "CacheQuotaDataLock was still held during the flush's database work")
		})
	}
}

// A request that lands mid-flush must not have its delta swallowed: the flush
// detaches the pending map, so the new entry belongs to the next snapshot.
func TestSaveQuotaDataCacheKeepsDeltasLoggedDuringFlush(t *testing.T) {
	truncateTables(t)
	resetQuotaDataCache(t)

	logged := false
	lockHeld := false
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register("test:quota_concurrent_log", func(tx *gorm.DB) {
		if logged || tx.Statement.Table != "quota_data" {
			return
		}
		// TryLock rather than LogQuotaData directly: a flush that still held the
		// lock would deadlock this goroutine on the non-reentrant mutex instead
		// of reporting a failure, so probe first and record the miss.
		if !CacheQuotaDataLock.TryLock() {
			lockHeld = true
			return
		}
		CacheQuotaDataLock.Unlock()
		logged = true
		LogQuotaData(flowLogParams("vip", 7, 3))
	}))
	t.Cleanup(func() {
		DB.Callback().Create().Remove("test:quota_concurrent_log")
	})

	LogQuotaData(flowLogParams("vip", 100, 40))
	SaveQuotaDataCache()
	require.False(t, lockHeld, "CacheQuotaDataLock was still held during the flush's database work")
	require.True(t, logged, "probe never ran, the concurrent write was not exercised")

	var afterFirst []QuotaData
	require.NoError(t, DB.Find(&afterFirst).Error)
	require.Len(t, afterFirst, 1)
	assert.Equal(t, 100, afterFirst[0].Quota)

	// The delta logged during the flush is still pending, and the next flush
	// folds it onto the same row rather than dropping it or duplicating the row.
	SaveQuotaDataCache()

	var afterSecond []QuotaData
	require.NoError(t, DB.Find(&afterSecond).Error)
	require.Len(t, afterSecond, 1)
	assert.Equal(t, 107, afterSecond[0].Quota)
	assert.Equal(t, 43, afterSecond[0].TokenUsed)
	assert.Equal(t, 2, afterSecond[0].Count)
}
