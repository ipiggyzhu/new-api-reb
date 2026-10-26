package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"sync"
	"testing"
)

func TestProfileHeaderNormalizationBoundary(t *testing.T) {
	for _, family := range []string{"codex", "claude", ""} {
		for _, name := range []string{"USER-AGENT", " user-Agent ", "\tX-CODEX-TEST\t", "Anthropic-Beta", " session_id "} {
			assert.True(t, IsProtectedProfileHeader(family, name), name)
		}
	}
	assert.False(t, IsProtectedProfileHeader("codex", ""))
}

func TestConcurrentBindingMigrationBoundary(t *testing.T) {
	redis := common.RedisEnabled
	common.RedisEnabled = false
	affinity := operation_setting.GetChannelAffinitySetting()
	original := *affinity
	*affinity = operation_setting.ChannelAffinitySetting{MaxEntries: 10, KeyEnvironmentBindingTTLSeconds: 60}
	ResetKeyEnvironmentBindingCacheForTest()
	t.Cleanup(func() { common.RedisEnabled = redis; *affinity = original; ResetKeyEnvironmentBindingCacheForTest() })
	const key = "task22-boundary-session"
	old := KeyEnvironmentBinding{ChannelId: 5, KeyTag: "old"}
	ok, err := ClaimKeyEnvironmentBinding(key, nil, old)
	require.NoError(t, err)
	require.True(t, ok)
	start := make(chan struct{})
	results := make(chan struct {
		won bool
		err error
	}, 3)
	var wg sync.WaitGroup
	for _, tag := range []string{"new-a", "new-b", "new-c"} {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			<-start
			won, err := ClaimKeyEnvironmentBinding(key, &old, KeyEnvironmentBinding{ChannelId: 5, KeyTag: tag})
			results <- struct {
				won bool
				err error
			}{won, err}
		}(tag)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for r := range results {
		require.NoError(t, r.err)
		if r.won {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
	binding, found, err := GetKeyEnvironmentBinding(key)
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEqual(t, old, binding)
	won, err := ClaimKeyEnvironmentBinding(key, nil, old)
	require.NoError(t, err)
	assert.False(t, won, "new connection cannot overwrite migration winner")
	final, found, err := GetKeyEnvironmentBinding(key)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, binding, final)
}

func TestCapturedTLSInvalidHelloBoundary(t *testing.T) {
	p := LookupCLIVersionProfile("codex-0.163.0-linux-x64")
	require.NotNil(t, p)
	original := p.ClientHelloHex
	t.Cleanup(func() { p.ClientHelloHex = original; resetFingerprintClientCache() })
	for _, tc := range []struct{ name, hex string }{{"empty", ""}, {"invalid hex", "zz"}, {"oversized record", strings.Repeat("00", 65541)}} {
		t.Run(tc.name, func(t *testing.T) {
			p.ClientHelloHex = tc.hex
			resetFingerprintClientCache()
			client, err := GetFingerprintHTTPClient("", p.ID)
			assert.Error(t, err)
			assert.Nil(t, client)
			dial, err := NewFingerprintTLSDialer("", p.ID)
			assert.Error(t, err)
			assert.Nil(t, dial)
		})
	}
}

func TestKeyVersionPoolShortageBoundary(t *testing.T) {
	pool := KeyEnvironmentPool("codex", "")
	require.NotEmpty(t, pool)
	plan, err := planKeyEnvironments([]string{"only"}, nil, pool, false)
	require.NoError(t, err)
	require.Len(t, plan, 1)
	assert.NotEmpty(t, plan[0].ProfileId)
	_, err = planKeyEnvironments([]string{"duplicate", "duplicate"}, nil, pool, false)
	require.Error(t, err)
	plan, err = planKeyEnvironments([]string{"a", "b"}, nil, nil, false)
	require.NoError(t, err)
	require.Len(t, plan, 2)
	for _, row := range plan {
		assert.Empty(t, row.ProfileId, "empty pool leaves every key unassigned")
	}
	setupKeyEnvironmentTable(t)
	original := cliVersionProfiles
	cliVersionProfiles = nil
	t.Cleanup(func() { cliVersionProfiles = original })
	channel := keyEnvironmentTestChannel(99201, "codex", "", true, "sk-boundary-a\nsk-boundary-b")
	var shortage *KeyEnvironmentShortageError
	require.ErrorAs(t, reconcileInTx(channel, false), &shortage)
	assert.Zero(t, shortage.Available)
	require.NoError(t, reconcileInTx(channel, true))
	assert.Equal(t, map[string]string{"sk-boundary-a": "", "sk-boundary-b": ""}, storedKeyEnvironments(t, 99201, "sk-boundary-a", "sk-boundary-b"))
}
