package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 各充值路径在事务里直接写库加额度；Redis 用户哈希必须同步这笔入账，
// 否则 Redis 部署会在 TTL 到期前一直按充值前的余额放行/拒绝请求。

func withTopupRedisCache(t *testing.T) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})

	originalRDB, originalEnabled := common.RDB, common.RedisEnabled
	originalSync := common.SyncFrequency
	common.RDB, common.RedisEnabled = client, true
	// populateUserCache 需要正的 TTL，否则 RedisHIncrBy 视 key 为不存在而跳过
	common.SyncFrequency = 60
	t.Cleanup(func() {
		_ = client.Close()
		common.RDB, common.RedisEnabled = originalRDB, originalEnabled
		common.SyncFrequency = originalSync
	})
}

func seedCachedTopupUser(t *testing.T, affCode string, quota int) *User {
	t.Helper()
	user := &User{Username: affCode, Password: "placeholder", AffCode: affCode, Quota: quota}
	require.NoError(t, DB.Create(user).Error)
	require.NoError(t, populateUserCache(*user))
	return user
}

func cachedUserQuota(t *testing.T, userId int) int {
	t.Helper()
	base, err := cacheGetUserBase(userId)
	require.NoError(t, err)
	return base.Quota
}

func TestRechargeStripeSyncsUserQuotaCache(t *testing.T) {
	truncateTables(t)
	withTopupRedisCache(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })

	user := seedCachedTopupUser(t, "stripe-cache-user", 1000)
	topUp := &TopUp{
		UserId:          user.Id,
		Amount:          2,
		Money:           2,
		TradeNo:         "stripe-cache-trade",
		PaymentMethod:   PaymentMethodStripe,
		PaymentProvider: PaymentProviderStripe,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, topUp.Insert())

	require.NoError(t, Recharge("stripe-cache-trade", "cus_test", "127.0.0.1"))

	assert.Equal(t, 1000+1000000, mustUserQuota(t, user.Id))
	assert.Equal(t, 1000+1000000, cachedUserQuota(t, user.Id), "Redis 余额必须与入账后的库内余额一致")
}

func TestRechargeCreemSyncsUserQuotaCache(t *testing.T) {
	truncateTables(t)
	withTopupRedisCache(t)

	user := seedCachedTopupUser(t, "creem-cache-user", 500)
	topUp := &TopUp{
		UserId:          user.Id,
		Amount:          250000,
		Money:           5,
		TradeNo:         "creem-cache-trade",
		PaymentMethod:   PaymentMethodCreem,
		PaymentProvider: PaymentProviderCreem,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, topUp.Insert())

	require.NoError(t, RechargeCreem("creem-cache-trade", "", "", "127.0.0.1"))

	assert.Equal(t, 500+250000, mustUserQuota(t, user.Id))
	assert.Equal(t, 500+250000, cachedUserQuota(t, user.Id))
}

func TestRechargeWaffoSyncsUserQuotaCacheIdempotently(t *testing.T) {
	truncateTables(t)
	withTopupRedisCache(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })

	user := seedCachedTopupUser(t, "waffo-cache-user", 0)
	topUp := &TopUp{
		UserId:          user.Id,
		Amount:          3,
		Money:           3,
		TradeNo:         "waffo-cache-trade",
		PaymentMethod:   PaymentMethodWaffo,
		PaymentProvider: PaymentProviderWaffo,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, topUp.Insert())

	require.NoError(t, RechargeWaffo("waffo-cache-trade", "127.0.0.1"))
	assert.Equal(t, 1500000, mustUserQuota(t, user.Id))
	assert.Equal(t, 1500000, cachedUserQuota(t, user.Id))

	// 回调重放：订单已成功，库和缓存都不得再次入账
	require.NoError(t, RechargeWaffo("waffo-cache-trade", "127.0.0.1"))
	assert.Equal(t, 1500000, mustUserQuota(t, user.Id))
	assert.Equal(t, 1500000, cachedUserQuota(t, user.Id))
}

func TestRechargeWaffoPancakeSyncsUserQuotaCache(t *testing.T) {
	truncateTables(t)
	withTopupRedisCache(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })

	user := seedCachedTopupUser(t, "waffo-pancake-cache-user", 100)
	topUp := &TopUp{
		UserId:          user.Id,
		Amount:          1,
		Money:           1,
		TradeNo:         "waffo-pancake-cache-trade",
		PaymentMethod:   PaymentMethodWaffoPancake,
		PaymentProvider: PaymentProviderWaffoPancake,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, topUp.Insert())

	require.NoError(t, RechargeWaffoPancake("waffo-pancake-cache-trade"))
	assert.Equal(t, 100+500000, mustUserQuota(t, user.Id))
	assert.Equal(t, 100+500000, cachedUserQuota(t, user.Id))
}

func TestManualCompleteTopUpSyncsUserQuotaCacheIdempotently(t *testing.T) {
	truncateTables(t)
	withTopupRedisCache(t)

	originalQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })

	user := seedCachedTopupUser(t, "manual-cache-user", 200)
	topUp := &TopUp{
		UserId:          user.Id,
		Amount:          2,
		Money:           2,
		TradeNo:         "manual-cache-trade",
		PaymentMethod:   "epay",
		PaymentProvider: PaymentProviderEpay,
		CreateTime:      common.GetTimestamp(),
		Status:          common.TopUpStatusPending,
	}
	require.NoError(t, topUp.Insert())

	require.NoError(t, ManualCompleteTopUp("manual-cache-trade", "127.0.0.1"))
	assert.Equal(t, 200+1000000, mustUserQuota(t, user.Id))
	assert.Equal(t, 200+1000000, cachedUserQuota(t, user.Id))

	// 重复补单：幂等路径不得再次推高缓存
	require.NoError(t, ManualCompleteTopUp("manual-cache-trade", "127.0.0.1"))
	assert.Equal(t, 200+1000000, mustUserQuota(t, user.Id))
	assert.Equal(t, 200+1000000, cachedUserQuota(t, user.Id))
}
