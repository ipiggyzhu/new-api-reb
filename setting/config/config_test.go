package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testConfigWithMap struct {
	Modes map[string]string `json:"modes"`
	Exprs map[string]string `json:"exprs"`
	Name  string            `json:"name"`
}

func TestUpdateConfigFromMap_MapReplacement(t *testing.T) {
	cfg := &testConfigWithMap{
		Modes: map[string]string{
			"model-a": "tiered_expr",
			"model-b": "tiered_expr",
		},
		Exprs: map[string]string{
			"model-a": "p * 5 + c * 25",
			"model-b": "p * 10 + c * 50",
		},
		Name: "billing",
	}

	// Simulate removing model-a: new value only has model-b
	err := UpdateConfigFromMap(cfg, map[string]string{
		"modes": `{"model-b": "tiered_expr"}`,
		"exprs": `{"model-b": "p * 10 + c * 50"}`,
	})
	if err != nil {
		t.Fatalf("UpdateConfigFromMap failed: %v", err)
	}

	if _, ok := cfg.Modes["model-a"]; ok {
		t.Errorf("Modes still contains model-a after it was removed from the update; got %v", cfg.Modes)
	}
	if _, ok := cfg.Exprs["model-a"]; ok {
		t.Errorf("Exprs still contains model-a after it was removed from the update; got %v", cfg.Exprs)
	}

	if cfg.Modes["model-b"] != "tiered_expr" {
		t.Errorf("Modes[model-b] = %q, want %q", cfg.Modes["model-b"], "tiered_expr")
	}
	if cfg.Exprs["model-b"] != "p * 10 + c * 50" {
		t.Errorf("Exprs[model-b] = %q, want %q", cfg.Exprs["model-b"], "p * 10 + c * 50")
	}
}

func TestUpdateConfigFromMap_EmptyMapClearsAll(t *testing.T) {
	cfg := &testConfigWithMap{
		Modes: map[string]string{
			"model-a": "tiered_expr",
		},
		Exprs: map[string]string{
			"model-a": "p * 5 + c * 25",
		},
	}

	err := UpdateConfigFromMap(cfg, map[string]string{
		"modes": `{}`,
		"exprs": `{}`,
	})
	if err != nil {
		t.Fatalf("UpdateConfigFromMap failed: %v", err)
	}

	if len(cfg.Modes) != 0 {
		t.Errorf("Modes should be empty after updating with {}, got %v", cfg.Modes)
	}
	if len(cfg.Exprs) != 0 {
		t.Errorf("Exprs should be empty after updating with {}, got %v", cfg.Exprs)
	}
}

func TestUpdateConfigFromMap_ScalarFieldsUnchanged(t *testing.T) {
	cfg := &testConfigWithMap{
		Modes: map[string]string{"m": "v"},
		Name:  "old",
	}

	err := UpdateConfigFromMap(cfg, map[string]string{
		"name": "new",
	})
	if err != nil {
		t.Fatalf("UpdateConfigFromMap failed: %v", err)
	}

	if cfg.Name != "new" {
		t.Errorf("Name = %q, want %q", cfg.Name, "new")
	}
	// modes was not in configMap, should remain unchanged
	if cfg.Modes["m"] != "v" {
		t.Errorf("Modes should be unchanged, got %v", cfg.Modes)
	}
}

// testRule mirrors the shape of a registered config slice element that carries
// compile-time defaults (see operation_setting.ChannelAffinityRule): the stored
// JSON usually specifies only a couple of fields, and every field the admin
// left out must come back as a zero value rather than inheriting the default
// element that happened to sit at the same index.
type testRule struct {
	Name       string   `json:"name"`
	ModelRegex []string `json:"model_regex"`
	PathRegex  []string `json:"path_regex"`
	SkipRetry  bool     `json:"skip_retry_on_failure"`
	TTLSeconds int      `json:"ttl_seconds"`
}

type testNestedStruct struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type testConfigWithSlice struct {
	Rules  []testRule       `json:"rules"`
	Target testNestedStruct `json:"target"`
}

// defaultSliceConfig returns a config pre-populated the way a registered module
// looks before LoadFromDB runs: two fully-specified default rules.
func defaultSliceConfig() *testConfigWithSlice {
	return &testConfigWithSlice{
		Rules: []testRule{
			{
				Name:       "codex cli trace",
				ModelRegex: []string{"^gpt-.*$"},
				PathRegex:  []string{"/v1/responses"},
				SkipRetry:  true,
				TTLSeconds: 60,
			},
			{
				Name:       "claude cli trace",
				ModelRegex: []string{"^claude-.*$"},
				PathRegex:  []string{"/v1/messages"},
				SkipRetry:  true,
				TTLSeconds: 120,
			},
		},
	}
}

// TestUpdateConfigFromMap_SliceReplacesDefaults pins the contract that the value
// stored in the options table fully replaces a slice field. Decoding in place
// merged element-wise, so a single stored rule silently inherited PathRegex and
// SkipRetry from the first compile-time default rule.
func TestUpdateConfigFromMap_SliceReplacesDefaults(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		want  []testRule
		about string
	}{
		{
			name: "shorter list with omitted fields keeps no default residue",
			json: `[{"name":"my rule","model_regex":["^claude-.*$"]}]`,
			want: []testRule{
				{Name: "my rule", ModelRegex: []string{"^claude-.*$"}},
			},
			about: "PathRegex/SkipRetry/TTLSeconds must be zero, not rule[0]'s values",
		},
		{
			name: "same length with omitted fields still clears each element",
			json: `[{"name":"first"},{"name":"second"}]`,
			want: []testRule{
				{Name: "first"},
				{Name: "second"},
			},
			about: "element-wise merge would keep both defaults' regexes",
		},
		{
			name:  "empty array clears every default rule",
			json:  `[]`,
			want:  []testRule{},
			about: "an admin who deletes all rules must not get the defaults back",
		},
		{
			name:  "json null clears the slice",
			json:  `null`,
			want:  nil,
			about: "configToMap marshals a nil slice as null; reloading must round-trip",
		},
		{
			name: "longer list than the defaults is taken verbatim",
			json: `[{"name":"a"},{"name":"b"},{"name":"c","ttl_seconds":5}]`,
			want: []testRule{
				{Name: "a"},
				{Name: "b"},
				{Name: "c", TTLSeconds: 5},
			},
			about: "growth past the default length must not reuse defaults for the overlap",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultSliceConfig()

			require.NoError(t, UpdateConfigFromMap(cfg, map[string]string{"rules": tc.json}))

			assert.Equal(t, tc.want, cfg.Rules, tc.about)
		})
	}
}

// TestUpdateConfigFromMap_InvalidSliceKeepsCurrentValue pins that a malformed
// stored value leaves the defaults intact instead of wiping the config.
func TestUpdateConfigFromMap_InvalidSliceKeepsCurrentValue(t *testing.T) {
	cfg := defaultSliceConfig()
	want := defaultSliceConfig().Rules

	require.NoError(t, UpdateConfigFromMap(cfg, map[string]string{"rules": `{not json`}))

	assert.Equal(t, want, cfg.Rules, "an unparsable value must not clear the slice")
}

// TestUpdateConfigFromMap_StructMergesInPlace locks the struct branch, which
// intentionally keeps in-place decoding: fields absent from the stored JSON
// must keep their current value rather than being reset to the zero value.
func TestUpdateConfigFromMap_StructMergesInPlace(t *testing.T) {
	cfg := &testConfigWithSlice{
		Target: testNestedStruct{Host: "upstream.internal", Port: 8080},
	}

	require.NoError(t, UpdateConfigFromMap(cfg, map[string]string{"target": `{"port":9090}`}))

	assert.Equal(t, testNestedStruct{Host: "upstream.internal", Port: 9090}, cfg.Target,
		"host was absent from the stored JSON and must survive the merge")
}
