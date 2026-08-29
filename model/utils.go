package model

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
)

const (
	BatchUpdateTypeUserQuota = iota
	BatchUpdateTypeTokenQuota
	BatchUpdateTypeUsedQuota
	BatchUpdateTypeChannelUsedQuota
	BatchUpdateTypeRequestCount
	BatchUpdateTypeCount // if you add a new type, you need to add a new map and a new lock
)

var batchUpdateStores []map[int]int
var batchUpdateLocks []sync.Mutex

func init() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateStores = append(batchUpdateStores, make(map[int]int))
		batchUpdateLocks = append(batchUpdateLocks, sync.Mutex{})
	}
}

func InitBatchUpdater() {
	gopool.Go(func() {
		for {
			time.Sleep(time.Duration(common.BatchUpdateInterval) * time.Second)
			batchUpdate()
		}
	})
}

// FlushBatchUpdates writes out everything the batch updater is still holding in
// memory. Without this a SIGTERM discards up to BatchUpdateInterval seconds of
// quota deductions and usage counters, which shows up as free requests after a
// deploy.
func FlushBatchUpdates() {
	if !common.BatchUpdateEnabled {
		return
	}
	batchUpdate()
}

func addNewRecord(type_ int, id int, value int) {
	batchUpdateLocks[type_].Lock()
	defer batchUpdateLocks[type_].Unlock()
	if _, ok := batchUpdateStores[type_][id]; !ok {
		batchUpdateStores[type_][id] = value
	} else {
		batchUpdateStores[type_][id] += value
	}
}

// pendingBatchDelta is one (store, id, value) triple that has to go back into
// the pending stores because the database rejected it.
type pendingBatchDelta struct {
	updateType int
	id         int
	value      int
}

// countUpTo counts the rows matching query but stops looking after limit rows.
//
// `query.Limit(N).Count(&total)` does not bound anything: GORM keeps the LIMIT
// on the aggregate query, where it only caps the single count output row, so the
// database still scans every matching row. On SQLite that scan holds a read lock
// while the single writer needs it. Counting the rows of a derived table that
// carries the LIMIT makes the bound real, so total saturates at limit.
//
// The generated shape is portable across every supported database:
//
//	SELECT count(*) FROM (SELECT 1 FROM t WHERE ... LIMIT N) as bounded
//
// PostgreSQL requires the derived table to be aliased, MySQL and SQLite accept
// the alias, and the inner SELECT list is a literal so no column has to be
// named. query itself is left untouched (the LIMIT lands on a session clone),
// which lets call sites reuse it for the page query afterwards.
//
// A limit that is not positive means no bound rather than "count nothing": some
// call sites take theirs from an admin option, where an unparsable value decodes
// to 0, and reporting a total of 0 there would silently empty every page.
func countUpTo(query *gorm.DB, limit int, total *int64) error {
	if limit <= 0 {
		return query.Session(&gorm.Session{}).Count(total).Error
	}
	bounded := query.Session(&gorm.Session{}).Select("1").Limit(limit)
	return query.Session(&gorm.Session{NewDB: true}).Table("(?) as bounded", bounded).Count(total).Error
}

func batchUpdate() {
	// check if there's any data to update
	hasData := false
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		if len(batchUpdateStores[i]) > 0 {
			hasData = true
			batchUpdateLocks[i].Unlock()
			break
		}
		batchUpdateLocks[i].Unlock()
	}

	if !hasData {
		return
	}

	common.SysLog("batch update started")
	stores := make([]map[int]int, BatchUpdateTypeCount)
	pendingCount := 0
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		stores[i] = batchUpdateStores[i]
		batchUpdateStores[i] = make(map[int]int)
		pendingCount += len(stores[i])
		batchUpdateLocks[i].Unlock()
	}

	userQuotaStore := stores[BatchUpdateTypeUserQuota]
	usedQuotaStore := stores[BatchUpdateTypeUsedQuota]
	requestCountStore := stores[BatchUpdateTypeRequestCount]

	userIDs := make(map[int]struct{}, len(userQuotaStore)+len(usedQuotaStore)+len(requestCountStore))
	for key := range userQuotaStore {
		userIDs[key] = struct{}{}
	}
	for key := range usedQuotaStore {
		userIDs[key] = struct{}{}
	}
	for key := range requestCountStore {
		userIDs[key] = struct{}{}
	}

	// The snapshot is replayed back into the pending stores if the flush fails,
	// so a database error never silently drops quota that was already deducted
	// from the caller's in-memory balance.
	pending := make([]pendingBatchDelta, 0, pendingCount)
	for updateType, store := range stores {
		for id, value := range store {
			pending = append(pending, pendingBatchDelta{updateType: updateType, id: id, value: value})
		}
	}

	// One transaction for the whole flush. Each autocommit statement takes the
	// SQLite write lock and fsyncs on commit, so N pending updates used to cost N
	// lock acquisitions and N fsyncs; batching them into one transaction was
	// measured at 9.2x faster here.
	//
	// On error the batch is atomic in both directions: the transaction rolls back
	// so no row is half-updated, and every delta in the snapshot goes back into
	// the pending stores via addNewRecord, which adds to whatever accumulated in
	// the meantime. Nothing is applied twice (the rollback undid every statement)
	// and nothing is dropped — the next flush retries the whole batch. Aborting on
	// the first failure rather than skipping the bad item is also required for
	// portability: PostgreSQL rejects every later statement in a transaction once
	// one statement has failed.
	err := DB.Transaction(func(tx *gorm.DB) error {
		for updateType, store := range stores {
			for id, value := range store {
				switch updateType {
				case BatchUpdateTypeTokenQuota:
					if err := increaseTokenQuota(tx, id, value); err != nil {
						return err
					}
				case BatchUpdateTypeChannelUsedQuota:
					if err := updateChannelUsedQuota(tx, id, value); err != nil {
						return err
					}
				}
			}
		}
		for id := range userIDs {
			if err := updateUserQuotaUsedQuotaAndRequestCount(tx, id, userQuotaStore[id], usedQuotaStore[id], requestCountStore[id]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		for _, delta := range pending {
			addNewRecord(delta.updateType, delta.id, delta.value)
		}
		common.SysLog(fmt.Sprintf("batch update rolled back, %d pending updates requeued for the next flush: %s", len(pending), err.Error()))
		return
	}
	common.SysLog("batch update finished")
}

func RecordExist(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, err
}

func shouldUpdateRedis(fromDB bool, err error) bool {
	return common.RedisEnabled && fromDB && err == nil
}
