package privacy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flagSlashText builds the conversation text that used to poison a session:
// a command-line credential flag whose following token is a lone path
// separator. It is assembled by concatenation so the literal sequence never
// appears in this file; an agent reading the file back must not be able to
// trip the bug with its own next request.
func flagSlashText() string {
	return strings.Join([]string{"redis", "-cli -a ", "/"}, "")
}

func TestInferredShortValueIsIgnored(t *testing.T) {
	session := NewSession()
	protected, err := session.ProtectText("password", "abc", Options{})
	require.NoError(t, err)
	assert.Equal(t, "abc", protected)
	assert.Empty(t, session.snapshot())
}

func TestSessionAddRejectsImplausibleValues(t *testing.T) {
	for _, value := range []string{"", "a", "ab", "/", "---", "....", "////", "   "} {
		session := NewSession()
		require.NoError(t, session.Add(value))
		assert.Empty(t, session.snapshot(), "value %q must not be stored", value)
	}
	for _, value := range []string{"abcd", "fixture-pass", "fixture-pass-123"} {
		session := NewSession()
		require.NoError(t, session.Add(value))
		assert.Len(t, session.snapshot(), 1, "value %q must be stored", value)
	}
}

func TestCommandFlagSeparatorIsNotStoredAsSecret(t *testing.T) {
	session := NewSession()
	text := "run " + flagSlashText() + " to inspect the cache"
	protected, err := session.ProtectText("content", text, Options{})
	require.NoError(t, err)
	assert.Equal(t, text, protected, "a lone separator after a flag is not a credential")
	assert.Empty(t, session.snapshot())
	// Structural text the gateway itself relies on must survive a session
	// that has seen the trigger text.
	contentType, err := session.ProtectText("Content-Type", "application/json", Options{})
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)
	path, err := session.ProtectText("url", "/v1/messages", Options{})
	require.NoError(t, err)
	assert.Equal(t, "/v1/messages", path)
}

func TestCommandFlagRealPasswordStillProtected(t *testing.T) {
	session := NewSession()
	text := "run " + strings.Join([]string{"redis", "-cli -a ", "fixture-pass-123"}, "") + " to inspect the cache"
	protected, err := session.ProtectText("content", text, Options{})
	require.NoError(t, err)
	assert.NotContains(t, protected, "fixture-pass-123")
	assert.Contains(t, protected, Prefix)
	assert.Equal(t, text, session.Restore(protected))
	assert.NotEqual(t, text, session.Mask(text), "the real password must still be masked wherever it reappears")
}
