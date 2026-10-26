package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A binding is placed once and moved only from the value the mover saw, so two
// requests that both saw a dead key cannot move one conversation to two keys.
func TestClaimKeyEnvironmentBindingMovesOnlyFromWhatItSaw(t *testing.T) {
	originalRedis := common.RedisEnabled
	affinity := operation_setting.GetChannelAffinitySetting()
	originalAffinity := *affinity
	t.Cleanup(func() {
		common.RedisEnabled = originalRedis
		*affinity = originalAffinity
		ResetKeyEnvironmentBindingCacheForTest()
	})
	common.RedisEnabled = false
	*affinity = operation_setting.ChannelAffinitySetting{MaxEntries: 10, KeyEnvironmentBindingTTLSeconds: 60}
	ResetKeyEnvironmentBindingCacheForTest()

	const conversation = "kenv:token:1:default"
	a := KeyEnvironmentBinding{ChannelId: 5, KeyTag: "aaaa"}
	b := KeyEnvironmentBinding{ChannelId: 5, KeyTag: "bbbb"}
	c := KeyEnvironmentBinding{ChannelId: 6, KeyTag: "cccc"}
	stored := func() KeyEnvironmentBinding {
		binding, found, err := GetKeyEnvironmentBinding(conversation)
		require.NoError(t, err)
		require.True(t, found)
		return binding
	}

	claimed, err := ClaimKeyEnvironmentBinding(conversation, nil, a)
	require.NoError(t, err)
	assert.True(t, claimed, "the first request places the binding")
	claimed, err = ClaimKeyEnvironmentBinding(conversation, nil, b)
	require.NoError(t, err)
	assert.False(t, claimed, "a concurrent first request does not overwrite it")
	assert.Equal(t, a, stored())

	claimed, err = ClaimKeyEnvironmentBinding(conversation, &a, b)
	require.NoError(t, err)
	assert.True(t, claimed, "a migration from the stored binding goes through")
	claimed, err = ClaimKeyEnvironmentBinding(conversation, &a, c)
	require.NoError(t, err)
	assert.False(t, claimed, "a second migrator that saw the old binding loses")
	assert.Equal(t, b, stored())
}
