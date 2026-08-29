package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientHeaderProfileForAPIType(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		apiType    int
		wantClient string
	}{
		{"anthropic gets claude cli", constant.APITypeAnthropic, "claude-cli/"},
		{"aws gets claude cli", constant.APITypeAws, "claude-cli/"},
		{"vertex gets claude cli", constant.APITypeVertexAi, "claude-cli/"},
		{"openai gets sdk", constant.APITypeOpenAI, "OpenAI/Python"},
		// Codex is a separate client from the Python SDK and talks to
		// /v1/responses, so it must not inherit the SDK's identity.
		{"codex gets codex cli", constant.APITypeCodex, "codex_cli_rs/"},
		{"gemini gets genai", constant.APITypeGemini, "google-genai-sdk/"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			profile := clientHeaderProfileForAPIType(testCase.apiType)
			// The client family is the contract; the version is a default that
			// admins are expected to bump, so pinning it here would only make
			// this test fail on every legitimate change.
			assert.Contains(t, profile["user-agent"], testCase.wantClient)
		})
	}
}

// An unrecognised channel type must still look like some HTTP client. Returning
// an empty profile would put us back to sending a request with no user-agent,
// which is the shape upstreams reject.
func TestClientHeaderProfileForUnknownAPITypeIsNotEmpty(t *testing.T) {
	t.Parallel()

	profile := clientHeaderProfileForAPIType(-1)
	assert.NotEmpty(t, profile)
	assert.Equal(t, "new-api/1.0", profile["user-agent"])
}

func TestApplyTestClientHeadersSetsProfileAndAccept(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeAnthropic, false)

	assert.Contains(t, header.Get("user-agent"), "claude-cli/")
	assert.Equal(t, "2023-06-01", header.Get("anthropic-version"))
	assert.Equal(t, acceptJSON, header.Get("accept"))
}

func TestApplyTestClientHeadersUsesSSEForStream(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeOpenAI, true)

	assert.Equal(t, acceptSSE, header.Get("accept"))
	assert.Equal(t, "python", header.Get("x-stainless-lang"))
}

// The profile fills gaps only. A value already on the request was put there
// deliberately (a configured override, a caller-set anthropic-version) and
// silently replacing it would make the channel's own configuration unreachable.
func TestApplyTestClientHeadersNeverOverwritesExistingValues(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set("user-agent", "my-own-agent/9")
	header.Set("anthropic-version", "2099-01-01")
	header.Set("accept", "application/xml")

	applyTestClientHeaders(header, constant.APITypeAnthropic, false)

	assert.Equal(t, "my-own-agent/9", header.Get("user-agent"))
	assert.Equal(t, "2099-01-01", header.Get("anthropic-version"))
	assert.Equal(t, "application/xml", header.Get("accept"))
}

func TestApplyTestClientHeadersNilHeaderDoesNotPanic(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		applyTestClientHeaders(nil, constant.APITypeOpenAI, false)
	})
}

// withChannelTestClientHeaderOverrides sets the admin override and restores it,
// so these tests do not leak state into the rest of the package.
func withChannelTestClientHeaderOverrides(t *testing.T, overrides map[string]map[string]string) {
	t.Helper()
	setting := *operation_setting.GetMonitorSetting()
	setting.ChannelTestClientHeaders = overrides
	t.Cleanup(operation_setting.SetMonitorSettingForTest(setting))
}

// The whole point of the override is fixing a stale built-in client version
// without rebuilding the image.
func TestApplyTestClientHeadersOverrideReplacesBuiltinValue(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyClaude: {"user-agent": "claude-cli/9.9.9 (external, cli)"},
	})

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeAnthropic, false)

	assert.Equal(t, "claude-cli/9.9.9 (external, cli)", header.Get("user-agent"))
	// Untouched entries of the profile must survive a partial override.
	assert.Equal(t, "cli", header.Get("x-app"))
}

func TestApplyTestClientHeadersOverrideAddsNewHeader(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyOpenAI: {"x-custom-gate": "let-me-in"},
	})

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeOpenAI, false)

	assert.Equal(t, "let-me-in", header.Get("x-custom-gate"))
}

// An empty value is the only way to remove a header the built-in profile adds;
// sending it empty instead would be a different request than "not sending it".
func TestApplyTestClientHeadersEmptyOverrideRemovesBuiltinHeader(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyClaude: {"x-app": ""},
	})

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeAnthropic, false)

	// Get() returns "" both when the key is absent and when it is present with
	// an empty value, and those are not the same thing: an empty header is still
	// written to the wire as "X-App: ". Assert on the key itself.
	assert.Empty(t, header.Values("x-app"))
	assert.Contains(t, header.Get("user-agent"), "claude-cli/")
}

func TestApplyTestClientHeadersOverrideNameIsCaseInsensitive(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyClaude: {"  User-Agent  ": "my-agent/1"},
	})

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeAnthropic, false)

	assert.Equal(t, "my-agent/1", header.Get("user-agent"))
}

// A single flat override table would put Claude Code's user-agent on OpenAI
// channels too, turning their tests into false failures. Scoping by family is
// the fix, so pin it: an override for one family must not leak into another.
func TestApplyTestClientHeadersOverrideDoesNotLeakAcrossFamilies(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyClaude: {"user-agent": "claude-cli/9.9.9 (external, cli)"},
	})

	openAIHeader := http.Header{}
	applyTestClientHeaders(openAIHeader, constant.APITypeOpenAI, false)
	assert.Contains(t, openAIHeader.Get("user-agent"), "OpenAI/Python")
	assert.NotContains(t, openAIHeader.Get("user-agent"), "claude-cli")
}

func TestApplyTestClientHeadersWildcardAppliesToEveryFamily(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyAll: {"x-gateway-probe": "yes"},
	})

	for _, apiType := range []int{constant.APITypeOpenAI, constant.APITypeAnthropic, constant.APITypeGemini} {
		header := http.Header{}
		applyTestClientHeaders(header, apiType, false)
		assert.Equal(t, "yes", header.Get("x-gateway-probe"))
	}
}

// The more specific rule has to win, otherwise an admin who sets a blanket value
// can never special-case one family.
func TestApplyTestClientHeadersFamilyBeatsWildcard(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyAll:    {"user-agent": "blanket/1"},
		clientHeaderFamilyClaude: {"user-agent": "specific/2"},
	})

	claudeHeader := http.Header{}
	applyTestClientHeaders(claudeHeader, constant.APITypeAnthropic, false)
	assert.Equal(t, "specific/2", claudeHeader.Get("user-agent"))

	geminiHeader := http.Header{}
	applyTestClientHeaders(geminiHeader, constant.APITypeGemini, false)
	assert.Equal(t, "blanket/1", geminiHeader.Get("user-agent"))
}

// Preset IDs are sent by the UI and persisted in admin config, so a duplicate or
// an empty one is a data bug. Every preset must also carry a user-agent, since a
// preset that sets no client identity defeats the point.
func TestBuiltinClientHeaderPresetsAreWellFormed(t *testing.T) {
	presets := GetChannelTestClientHeaderPresets()
	require.NotEmpty(t, presets)

	seen := make(map[string]bool, len(presets))
	for _, preset := range presets {
		assert.NotEmpty(t, preset.ID, "preset needs an ID: %+v", preset)
		assert.False(t, seen[preset.ID], "duplicate preset ID: %s", preset.ID)
		seen[preset.ID] = true

		assert.NotEmpty(t, preset.Label, "preset %s needs a label", preset.ID)
		assert.NotEmpty(t, preset.Headers["user-agent"], "preset %s needs a user-agent", preset.ID)

		// The family must be one the override map and profile lookup understand,
		// or choosing this preset would write config nothing ever reads.
		assert.Contains(t, []string{
			clientHeaderFamilyClaude, clientHeaderFamilyOpenAI,
			clientHeaderFamilyCodex, clientHeaderFamilyGemini,
			clientHeaderFamilyGeneric,
		}, preset.Family, "preset %s has an unknown family", preset.ID)
	}
}

// A preset is only useful if selecting it actually reaches the wire, so run one
// through the real apply path rather than asserting on the table alone.
func TestPresetHeadersApplyThroughOverride(t *testing.T) {
	var codexPresetHeaders map[string]string
	for _, preset := range GetChannelTestClientHeaderPresets() {
		if preset.Family == clientHeaderFamilyCodex {
			codexPresetHeaders = preset.Headers
			break
		}
	}
	require.NotNil(t, codexPresetHeaders, "expected at least one codex preset")

	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyCodex: codexPresetHeaders,
	})

	header := http.Header{}
	applyTestClientHeaders(header, constant.APITypeCodex, false)
	assert.Equal(t, codexPresetHeaders["user-agent"], header.Get("user-agent"))
	assert.Equal(t, "codex_cli_rs", header.Get("originator"))
}
