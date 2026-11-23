package privacy

import (
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Seed ciphertext directly: using Save here would let its filter hide a load bug.
func legacyPrivacyStore(t *testing.T) (*Store, string, map[string]string) {
	t.Helper()
	redisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = redisEnabled })
	store, err := NewStore(4, time.Minute)
	require.NoError(t, err)
	_, id, err := store.Open("fixture-owner", "")
	require.NoError(t, err)
	values := make(map[string]string)
	want := make(map[string]string)
	for i, tc := range []struct {
		value string
		valid bool
	}{
		{value: "/"},
		{value: "abc"},
		{value: strings.Repeat("!?", 6)},
		{value: "🔒"},
		{value: "opaque-legacy-fixture-72", valid: true},
		{value: "éé", valid: true},
		{value: "𠀀", valid: true},
	} {
		ref := fmt.Sprintf("%s%032x__", Prefix, i+1)
		values[ref] = tc.value
		if tc.valid {
			want[ref] = tc.value
		}
	}
	plain, err := common.Marshal(values)
	require.NoError(t, err)
	defer clear(plain)
	key := "fixture-owner:" + id
	nonce := make([]byte, store.aead.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	store.entries[key] = storedSession{
		data:    store.aead.Seal(nonce, nonce, plain, []byte(key)),
		expires: store.now().Add(store.ttl),
	}
	return store, id, want
}

func TestStoreLoadFiltersLegacyValues(t *testing.T) {
	store, id, want := legacyPrivacyStore(t)
	session, loadedID, err := store.Open("fixture-owner", id)
	require.NoError(t, err)
	assert.Equal(t, id, loadedID)
	assert.Equal(t, want, session.snapshot())
	for ref, value := range want {
		assert.Equal(t, ref, session.Mask(value))
		assert.Equal(t, value, session.Restore(ref))
	}
	for _, structural := range []string{"/v1/chat/completions", "application/json"} {
		assert.Equal(t, structural, session.Mask(structural))
	}
}

func TestStoreSaveFiltersMergedAndIncomingValues(t *testing.T) {
	store, id, want := legacyPrivacyStore(t)
	session := NewSession()
	require.NoError(t, session.Add("opaque-new-fixture-84"))
	require.Len(t, session.snapshot(), 1)
	for ref, value := range session.snapshot() {
		want[ref] = value
	}
	// An in-flight session from older code can also carry rejected entries.
	session.Values[Prefix+strings.Repeat("f", 32)+"__"] = "/"
	require.NoError(t, store.Save("fixture-owner", id, session))

	// Inspect saved ciphertext directly so load filtering cannot mask a Save bug.
	key := "fixture-owner:" + id
	entry := store.entries[key]
	n := store.aead.NonceSize()
	require.Greater(t, len(entry.data), n)
	plain, err := store.aead.Open(nil, entry.data[:n], entry.data[n:], []byte(key))
	require.NoError(t, err)
	defer clear(plain)
	var saved map[string]string
	require.NoError(t, common.Unmarshal(plain, &saved))
	assert.Equal(t, want, saved)

	loaded, _, err := store.Open("fixture-owner", id)
	require.NoError(t, err)
	for ref, value := range want {
		assert.Equal(t, ref, loaded.Mask(value))
		assert.Equal(t, value, loaded.Restore(ref))
	}
}
