package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reservation path used to read the balance and then issue an unconditional
// decrement, so two requests could both see enough quota and both spend it.
// These tests pin the invariant that a reservation never drives a balance
// negative, which is what turns the overdraw into an admin-visible bug.

func TestPreConsumeUserQuotaNeverOverdraws(t *testing.T) {
	// AffCode is uniquely indexed, so it must not collide with other fixtures.
	user := &User{Username: "preconsume-user", Password: "placeholder", AffCode: "preconsume-aff", Quota: 100}
	require.NoError(t, DB.Create(user).Error)

	require.NoError(t, PreConsumeUserQuota(user.Id, 60))
	assert.Equal(t, 40, mustUserQuota(t, user.Id))

	err := PreConsumeUserQuota(user.Id, 60)
	require.ErrorIs(t, err, ErrInsufficientUserQuota)
	assert.Equal(t, 40, mustUserQuota(t, user.Id), "rejected reservation must not touch the balance")

	require.NoError(t, PreConsumeUserQuota(user.Id, 40))
	assert.Equal(t, 0, mustUserQuota(t, user.Id))

	require.ErrorIs(t, PreConsumeUserQuota(user.Id, 1), ErrInsufficientUserQuota)
	assert.Equal(t, 0, mustUserQuota(t, user.Id))
}

func TestPreConsumeTokenQuotaNeverOverdraws(t *testing.T) {
	token := &Token{UserId: 1, Key: "preconsume-token-key", Name: "preconsume", RemainQuota: 100}
	require.NoError(t, DB.Create(token).Error)

	require.NoError(t, PreConsumeTokenQuota(token.Id, token.Key, 60))
	remain, used := mustTokenQuota(t, token.Id)
	assert.Equal(t, 40, remain)
	assert.Equal(t, 60, used)

	err := PreConsumeTokenQuota(token.Id, token.Key, 60)
	require.ErrorIs(t, err, ErrInsufficientTokenQuota)
	remain, used = mustTokenQuota(t, token.Id)
	assert.Equal(t, 40, remain, "rejected reservation must not touch the balance")
	assert.Equal(t, 60, used, "rejected reservation must not book usage")

	require.NoError(t, PreConsumeTokenQuota(token.Id, token.Key, 40))
	remain, used = mustTokenQuota(t, token.Id)
	assert.Equal(t, 0, remain)
	assert.Equal(t, 100, used)
}

func mustUserQuota(t *testing.T, id int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.First(&user, id).Error)
	return user.Quota
}

func mustTokenQuota(t *testing.T, id int) (remain int, used int) {
	t.Helper()
	var token Token
	require.NoError(t, DB.First(&token, id).Error)
	return token.RemainQuota, token.UsedQuota
}
