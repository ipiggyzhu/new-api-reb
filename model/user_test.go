package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestInviteUserIncrementsOnlyAffColumns(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM users").Error)

	oldQuotaForInviter := common.QuotaForInviter
	common.QuotaForInviter = 250
	t.Cleanup(func() { common.QuotaForInviter = oldQuotaForInviter })

	inviter := User{
		Id:              21,
		Username:        "aff-inviter",
		Password:        "password",
		Status:          common.UserStatusEnabled,
		Quota:           1000,
		UsedQuota:       20,
		RequestCount:    3,
		AffCount:        1,
		AffQuota:        5,
		AffHistoryQuota: 7,
	}
	require.NoError(t, DB.Create(&inviter).Error)

	require.NoError(t, inviteUser(inviter.Id))
	require.NoError(t, inviteUser(inviter.Id))

	var got User
	require.NoError(t, DB.First(&got, inviter.Id).Error)
	assert.Equal(t, 3, got.AffCount)
	assert.Equal(t, 505, got.AffQuota)
	assert.Equal(t, 507, got.AffHistoryQuota)
	assert.Equal(t, 1000, got.Quota)
	assert.Equal(t, 20, got.UsedQuota)
	assert.Equal(t, 3, got.RequestCount)

	assert.ErrorIs(t, inviteUser(99999), gorm.ErrRecordNotFound)
}

func TestInviteUserDoesNotClobberConcurrentQuotaDeduction(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM users").Error)

	oldQuotaForInviter := common.QuotaForInviter
	common.QuotaForInviter = 100
	t.Cleanup(func() { common.QuotaForInviter = oldQuotaForInviter })

	inviter := User{
		Id:           22,
		Username:     "aff-race-inviter",
		Password:     "password",
		Status:       common.UserStatusEnabled,
		Quota:        1000,
		UsedQuota:    20,
		RequestCount: 3,
	}
	require.NoError(t, DB.Create(&inviter).Error)

	// Simulate a relay billing settle committing between a read of the inviter
	// row and the aff reward write: the deduction fires right after any SELECT
	// of the inviter row, i.e. inside a read-modify-write window if one exists.
	deducted := false
	const callbackName = "test:invite_user_concurrent_deduction"
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if deducted || db.Statement == nil || db.Statement.Table != "users" {
			return
		}
		u, ok := db.Statement.Dest.(*User)
		if !ok || u.Id != inviter.Id {
			return
		}
		deducted = true
		DB.Exec("UPDATE users SET quota = quota - ?, used_quota = used_quota + ? WHERE id = ?", 400, 400, inviter.Id)
	}))
	t.Cleanup(func() { DB.Callback().Query().Remove(callbackName) })

	require.NoError(t, inviteUser(inviter.Id))
	if !deducted {
		deducted = true
		require.NoError(t, DB.Exec("UPDATE users SET quota = quota - ?, used_quota = used_quota + ? WHERE id = ?", 400, 400, inviter.Id).Error)
	}

	var got User
	require.NoError(t, DB.First(&got, inviter.Id).Error)
	assert.Equal(t, 600, got.Quota)
	assert.Equal(t, 420, got.UsedQuota)
	assert.Equal(t, 3, got.RequestCount)
	assert.Equal(t, 1, got.AffCount)
	assert.Equal(t, 100, got.AffQuota)
	assert.Equal(t, 100, got.AffHistoryQuota)
}
