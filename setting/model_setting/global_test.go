package model_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A model_mapping target that carries the -thinking suffix is an explicit
// instruction to send that name upstream. The thinking adapters trim the suffix
// off the mapped name but used to consult the blacklist with the client-facing
// name only, so blacklisting the mapped target had no effect and the mapping was
// silently undone (upstream saw the client's model name instead).
func TestShouldPreserveThinkingSuffixMatchesMappedUpstreamName(t *testing.T) {
	original := globalSettings.ThinkingModelBlacklist
	t.Cleanup(func() { globalSettings.ThinkingModelBlacklist = original })
	globalSettings.ThinkingModelBlacklist = []string{"claude-opus-5-thinking"}

	require.True(t, ShouldPreserveThinkingSuffix("claude-opus-5", "claude-opus-5-thinking"),
		"blacklisting the mapping target must preserve the suffix it asked for")
	assert.False(t, ShouldPreserveThinkingSuffix("claude-opus-5", "claude-opus-5"),
		"an unmapped request to the client-facing name must stay unaffected")
	assert.False(t, ShouldPreserveThinkingSuffix("gemini-2.5-flash", "gemini-2.5-flash-thinking"),
		"a mapping target that was never blacklisted must still be adapted")
}

func TestShouldPreserveThinkingSuffixIgnoresEmptyCandidates(t *testing.T) {
	original := globalSettings.ThinkingModelBlacklist
	t.Cleanup(func() { globalSettings.ThinkingModelBlacklist = original })
	globalSettings.ThinkingModelBlacklist = []string{"", "  ", "kimi-k2-thinking"}

	assert.False(t, ShouldPreserveThinkingSuffix("", ""),
		"a blank blacklist entry must not match a request with no model name")
	assert.True(t, ShouldPreserveThinkingSuffix("", "kimi-k2-thinking"),
		"a blank first candidate must not stop later candidates from matching")
	assert.True(t, ShouldPreserveThinkingSuffix(" kimi-k2-thinking "),
		"surrounding whitespace must not defeat the match")
}
