package relay

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setThinkingBlacklist points the global blacklist at entries for one test and
// restores it afterwards. No t.Parallel in this file: the setting is process
// global, so parallel cases would read each other's fixtures.
func setThinkingBlacklist(t *testing.T, entries ...string) {
	t.Helper()
	settings := model_setting.GetGlobalSettings()
	original := settings.ThinkingModelBlacklist
	t.Cleanup(func() { settings.ThinkingModelBlacklist = original })
	settings.ThinkingModelBlacklist = entries
}

// The bug this pins was at the call site, not inside ShouldPreserveThinkingSuffix:
// the blacklist was consulted with the client-facing name alone, so a
// model_mapping target of "claude-opus-5-thinking" got its suffix trimmed and the
// upstream received "claude-opus-5" — the mapping did nothing, which is exactly
// what a channel's logs showed. Testing the function alone would not catch a
// revert to a single argument here, so the decision is pinned where it is made.
func TestResolveClaudeThinkingModelName_MappedTargetSurvivesWhenBlacklisted(t *testing.T) {
	setThinkingBlacklist(t, "claude-opus-5-thinking")

	require.Equal(t, "claude-opus-5-thinking",
		resolveClaudeThinkingModelName("claude-opus-5", "claude-opus-5-thinking"),
		"the mapped name is what the upstream is meant to resolve; trimming it undoes the mapping")
}

// Blacklisting the client-facing name has to keep working: that is how the
// setting behaved before mapped names were consulted, and existing installs
// (kimi-k2-thinking) rely on it.
func TestResolveClaudeThinkingModelName_ClientFacingNameStillMatches(t *testing.T) {
	setThinkingBlacklist(t, "kimi-k2-thinking")

	assert.Equal(t, "kimi-k2-thinking",
		resolveClaudeThinkingModelName("kimi-k2-thinking", "kimi-k2-thinking"))
}

// The default path must not change. Nothing blacklisted means the suffix is a
// local instruction to the thinking adapter, which turns it into a thinking
// block and sends the base model upstream.
func TestResolveClaudeThinkingModelName_TrimsWhenNotBlacklisted(t *testing.T) {
	setThinkingBlacklist(t)

	assert.Equal(t, "claude-opus-5",
		resolveClaudeThinkingModelName("claude-opus-5", "claude-opus-5-thinking"),
		"an un-blacklisted suffix is consumed locally, not forwarded")
	assert.Equal(t, "claude-opus-5",
		resolveClaudeThinkingModelName("claude-opus-5-thinking", "claude-opus-5-thinking"),
		"a client asking for -thinking directly is the case the adapter was built for")
}

// A name without the suffix must come back untouched whatever the blacklist
// says, so an entry can never rewrite an unrelated model.
func TestResolveClaudeThinkingModelName_LeavesUnsuffixedNamesAlone(t *testing.T) {
	setThinkingBlacklist(t, "claude-opus-5-thinking", "claude-opus-5")

	assert.Equal(t, "claude-opus-5",
		resolveClaudeThinkingModelName("claude-opus-5", "claude-opus-5"))
	assert.Equal(t, "claude-opus-5-max",
		resolveClaudeThinkingModelName("claude-opus-5", "claude-opus-5-max"),
		"other mapped suffixes are the upstream's business and must pass through")
}

// Only the trailing suffix is a marker. A model whose name merely contains
// "-thinking" keeps it.
func TestResolveClaudeThinkingModelName_OnlyTrailingSuffixIsTrimmed(t *testing.T) {
	setThinkingBlacklist(t)

	assert.Equal(t, "claude-thinking-preview",
		resolveClaudeThinkingModelName("claude-thinking-preview", "claude-thinking-preview"))
}
