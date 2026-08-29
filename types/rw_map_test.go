package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromJsonStringKeepsDataOnInvalidJSON(t *testing.T) {
	m := NewRWMap[string, float64]()
	require.NoError(t, LoadFromJsonString(m, `{"gpt-4o": 2.5, "claude-sonnet": 3}`))

	tests := []struct {
		name    string
		jsonStr string
	}{
		{name: "truncated object", jsonStr: `{"gpt-4o": `},
		{name: "not json", jsonStr: `oops`},
		{name: "value type mismatch after valid entry", jsonStr: `{"a": 1, "b": "not-a-number"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, LoadFromJsonString(m, tt.jsonStr))
			assert.Equal(t, map[string]float64{"gpt-4o": 2.5, "claude-sonnet": 3}, m.ReadAll())
		})
	}
}

func TestLoadFromJsonStringReplacesDataWholesale(t *testing.T) {
	m := NewRWMap[string, float64]()
	require.NoError(t, LoadFromJsonString(m, `{"old-model": 1}`))
	require.NoError(t, LoadFromJsonString(m, `{"new-model": 2}`))
	assert.Equal(t, map[string]float64{"new-model": 2}, m.ReadAll())
}

func TestLoadFromJsonStringWithCallbackFiresOnlyOnSuccess(t *testing.T) {
	m := NewRWMap[string, float64]()
	require.NoError(t, LoadFromJsonString(m, `{"gpt-4o": 2.5}`))

	calls := 0
	require.Error(t, LoadFromJsonStringWithCallback(m, `{bad`, func() { calls++ }))
	assert.Equal(t, 0, calls)
	assert.Equal(t, map[string]float64{"gpt-4o": 2.5}, m.ReadAll())

	require.NoError(t, LoadFromJsonStringWithCallback(m, `{"gpt-4o": 4}`, func() { calls++ }))
	assert.Equal(t, 1, calls)
	assert.Equal(t, map[string]float64{"gpt-4o": 4}, m.ReadAll())
}

func TestRWMapUnmarshalJSONKeepsDataOnInvalidJSON(t *testing.T) {
	m := NewRWMap[string, float64]()
	m.Set("gpt-4o", 2.5)

	require.Error(t, m.UnmarshalJSON([]byte(`{`)))
	assert.Equal(t, map[string]float64{"gpt-4o": 2.5}, m.ReadAll())

	require.NoError(t, m.UnmarshalJSON([]byte(`{"claude-sonnet": 3}`)))
	assert.Equal(t, map[string]float64{"claude-sonnet": 3}, m.ReadAll())
}
