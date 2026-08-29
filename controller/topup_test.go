package controller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTopupControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.TopUp{}, &model.User{}))

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func TestGetPayMoneyTokensDisplayChargesWholeUnits(t *testing.T) {
	originalPrice := operation_setting.Price
	originalQuotaPerUnit := common.QuotaPerUnit
	originalDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	originalDiscounts := operation_setting.GetPaymentSetting().AmountDiscount
	originalRatio := common.TopupGroupRatio2JSONString()
	t.Cleanup(func() {
		operation_setting.Price = originalPrice
		common.QuotaPerUnit = originalQuotaPerUnit
		operation_setting.GetGeneralSetting().QuotaDisplayType = originalDisplayType
		operation_setting.GetPaymentSetting().AmountDiscount = originalDiscounts
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(originalRatio))
	})

	operation_setting.Price = 1
	common.QuotaPerUnit = 500000
	operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{}
	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"default":1}`))

	testCases := []struct {
		name             string
		amount           int64
		quotaDisplayType string
		expected         float64
	}{
		{
			name:             "usd display charges amount as-is",
			amount:           10,
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			expected:         10,
		},
		{
			name:             "tokens display exact multiple",
			amount:           1500000,
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			expected:         3,
		},
		{
			name:             "tokens display fractional request charges truncated whole units",
			amount:           750000,
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			expected:         1,
		},
		{
			name:             "tokens display below one unit charges nothing",
			amount:           250000,
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			expected:         0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().QuotaDisplayType = tc.quotaDisplayType
			assert.InDelta(t, tc.expected, getPayMoney(tc.amount, "default"), 1e-9)
		})
	}

	// The charge for a fractional token request must equal the charge for the
	// whole-unit amount that EpayNotify will actually credit.
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeTokens
	assert.InDelta(t, getPayMoney(500000, "default"), getPayMoney(750000, "default"), 1e-9)
}

func TestSettleEpayOrderCreditsOnceAndIsIdempotent(t *testing.T) {
	db := setupTopupControllerTestDB(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })
	common.QuotaPerUnit = 500000

	user := &model.User{Username: "epay-settle-user", Password: "test-password", Quota: 100}
	require.NoError(t, db.Create(user).Error)
	topUp := &model.TopUp{
		UserId:          user.Id,
		Amount:          5,
		TradeNo:         "epay-settle-1",
		PaymentMethod:   "alipay",
		PaymentProvider: model.PaymentProviderEpay,
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, db.Create(topUp).Error)

	quotaToAdd, err := settleEpayOrder(topUp)
	require.NoError(t, err)
	assert.Equal(t, 2500000, quotaToAdd)

	var storedUser model.User
	require.NoError(t, db.First(&storedUser, user.Id).Error)
	assert.Equal(t, 100+2500000, storedUser.Quota)

	var storedTopUp model.TopUp
	require.NoError(t, db.First(&storedTopUp, topUp.Id).Error)
	assert.Equal(t, common.TopUpStatusSuccess, storedTopUp.Status)
	assert.NotZero(t, storedTopUp.CompleteTime)

	// A repeated notify delivery must be a no-op with no double credit.
	quotaToAdd, err = settleEpayOrder(topUp)
	require.NoError(t, err)
	assert.Zero(t, quotaToAdd)
	require.NoError(t, db.First(&storedUser, user.Id).Error)
	assert.Equal(t, 100+2500000, storedUser.Quota)
}

func TestSettleEpayOrderFailedCreditLeavesOrderPending(t *testing.T) {
	db := setupTopupControllerTestDB(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })
	common.QuotaPerUnit = 500000

	topUp := &model.TopUp{
		UserId:          42,
		Amount:          2,
		TradeNo:         "epay-settle-rollback",
		PaymentMethod:   "alipay",
		PaymentProvider: model.PaymentProviderEpay,
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, db.Create(topUp).Error)

	// Force the quota credit to fail so the whole settlement must roll back.
	require.NoError(t, db.Migrator().DropTable(&model.User{}))

	quotaToAdd, err := settleEpayOrder(topUp)
	require.Error(t, err)
	assert.Zero(t, quotaToAdd)

	var storedTopUp model.TopUp
	require.NoError(t, db.First(&storedTopUp, topUp.Id).Error)
	assert.Equal(t, common.TopUpStatusPending, storedTopUp.Status)
}

func TestSettleEpayOrderRejectsInvalidQuota(t *testing.T) {
	db := setupTopupControllerTestDB(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })
	common.QuotaPerUnit = 500000

	user := &model.User{Username: "epay-invalid-user", Password: "test-password", Quota: 0}
	require.NoError(t, db.Create(user).Error)

	testCases := []struct {
		name   string
		amount int64
	}{
		{name: "saturated quota fails loudly", amount: 1000000},
		{name: "non-positive quota rejected", amount: 0},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			topUp := &model.TopUp{
				UserId:          user.Id,
				Amount:          tc.amount,
				TradeNo:         fmt.Sprintf("epay-settle-invalid-%d", i),
				PaymentMethod:   "alipay",
				PaymentProvider: model.PaymentProviderEpay,
				Status:          common.TopUpStatusPending,
			}
			require.NoError(t, db.Create(topUp).Error)

			quotaToAdd, err := settleEpayOrder(topUp)
			require.Error(t, err)
			assert.Zero(t, quotaToAdd)

			var storedTopUp model.TopUp
			require.NoError(t, db.First(&storedTopUp, topUp.Id).Error)
			assert.Equal(t, common.TopUpStatusPending, storedTopUp.Status)

			var storedUser model.User
			require.NoError(t, db.First(&storedUser, user.Id).Error)
			assert.Zero(t, storedUser.Quota)
		})
	}
}
