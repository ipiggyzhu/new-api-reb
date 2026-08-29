package model

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// swapRefundTestDB points the package-level DB at a file-backed SQLite database
// opened with the production _txlock=immediate option and a multi-connection
// pool. Under that setup a nested top-level DB.Transaction inside the refund
// path grabs a second connection and hits SQLITE_BUSY against the outer write
// lock, so the regression surfaces as a fast error instead of passing by
// accident on the shared single-connection in-memory database.
func swapRefundTestDB(t *testing.T) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "refund.db") + "?_pragma=busy_timeout(250)&_txlock=immediate"
	testDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, testDB.AutoMigrate(&UserSubscription{}, &SubscriptionPreConsumeRecord{}))
	prev := DB
	DB = testDB
	t.Cleanup(func() {
		DB = prev
		if sqlDB, err := testDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

func TestRefundSubscriptionPreConsumeRestoresQuotaInOneTransaction(t *testing.T) {
	swapRefundTestDB(t)

	require.NoError(t, DB.Create(&UserSubscription{Id: 7001, UserId: 501, PlanId: 1, AmountTotal: 1000, AmountUsed: 800, Status: "active"}).Error)
	require.NoError(t, DB.Create(&SubscriptionPreConsumeRecord{RequestId: "refund-req-1", UserId: 501, UserSubscriptionId: 7001, PreConsumed: 500, Status: "consumed"}).Error)

	require.NoError(t, RefundSubscriptionPreConsume("refund-req-1"))

	var sub UserSubscription
	require.NoError(t, DB.First(&sub, 7001).Error)
	assert.EqualValues(t, 300, sub.AmountUsed)

	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", "refund-req-1").First(&record).Error)
	assert.Equal(t, "refunded", record.Status)

	// Retrying the same requestId must be a no-op, never a second credit.
	require.NoError(t, RefundSubscriptionPreConsume("refund-req-1"))
	require.NoError(t, DB.First(&sub, 7001).Error)
	assert.EqualValues(t, 300, sub.AmountUsed)
}

func TestRefundSubscriptionPreConsumeFailedDeltaKeepsRecordConsumed(t *testing.T) {
	swapRefundTestDB(t)

	require.NoError(t, DB.Create(&SubscriptionPreConsumeRecord{RequestId: "refund-req-orphan", UserId: 502, UserSubscriptionId: 9999, PreConsumed: 100, Status: "consumed"}).Error)

	require.Error(t, RefundSubscriptionPreConsume("refund-req-orphan"))

	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", "refund-req-orphan").First(&record).Error)
	assert.Equal(t, "consumed", record.Status, "failed delta must roll back the status flip so a retry can still refund")
}

func TestPostConsumeUserSubscriptionDeltaBounds(t *testing.T) {
	truncateTables(t)

	require.NoError(t, DB.Create(&UserSubscription{Id: 7101, UserId: 503, PlanId: 1, AmountTotal: 100, AmountUsed: 60, Status: "active"}).Error)

	require.NoError(t, PostConsumeUserSubscriptionDelta(7101, 30))
	assert.EqualValues(t, 90, mustSubscriptionAmountUsed(t, 7101))

	err := PostConsumeUserSubscriptionDelta(7101, 20)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subscription used exceeds total")
	assert.EqualValues(t, 90, mustSubscriptionAmountUsed(t, 7101), "rejected delta must not touch the balance")

	require.NoError(t, PostConsumeUserSubscriptionDelta(7101, -200))
	assert.Zero(t, mustSubscriptionAmountUsed(t, 7101), "over-refund clamps at zero")

	require.NoError(t, PostConsumeUserSubscriptionDelta(7101, 0))
	assert.Zero(t, mustSubscriptionAmountUsed(t, 7101))

	require.Error(t, PostConsumeUserSubscriptionDelta(0, 10))
}

func mustSubscriptionAmountUsed(t *testing.T, id int) int64 {
	t.Helper()
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, id).Error)
	return sub.AmountUsed
}

func TestCalcSubscriptionBalanceQuotaRejectsNonFiniteAndOverflowingPrices(t *testing.T) {
	previousQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	t.Cleanup(func() { common.QuotaPerUnit = previousQuotaPerUnit })

	for _, price := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), float64(common.MaxQuota)} {
		quota, err := calcSubscriptionBalanceQuota(price)
		require.Error(t, err)
		assert.Zero(t, quota)
	}

	quota, err := calcSubscriptionBalanceQuota(0.01)
	require.NoError(t, err)
	assert.Equal(t, 5000, quota)
}
