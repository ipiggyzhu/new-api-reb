package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetSettingIgnoresRemovedSendOriginalRequest pins the removal contract:
// send_original_request no longer exists, and a stored row that still carries
// the key must neither fail to parse nor influence any surviving setting. The
// rows that relied on its body half had pass_through_body_enabled written
// explicitly before the field was removed, so that value — not the legacy key —
// is what decides body passthrough.
func TestGetSettingIgnoresRemovedSendOriginalRequest(t *testing.T) {
	rawJSON := `{"send_original_request":true,"pass_through_body_enabled":false}`
	channel := &Channel{
		Type:    1,
		Setting: common.GetPointer(rawJSON),
	}

	settings := channel.GetSetting()

	assert.False(t, settings.PassThroughBodyEnabled,
		"a stale send_original_request key must not imply body passthrough after removal")
	assert.Empty(t, settings.SyntheticClientHeadersProfile)
}

func TestChannelSettingsNormalize(t *testing.T) {
	testCases := []struct {
		name     string
		input    dto.ChannelSettings
		expected dto.ChannelSettings
	}{
		{
			name: "body passthrough stays as stored",
			input: dto.ChannelSettings{
				PassThroughBodyEnabled: true,
			},
			expected: dto.ChannelSettings{
				PassThroughBodyEnabled: true,
			},
		},
		{
			name:     "everything off stays off",
			input:    dto.ChannelSettings{},
			expected: dto.ChannelSettings{},
		},
		{
			// Channels saved before the profile could be chosen carry only the bool.
			// Reading it as "off" would drop the protection they were saved with.
			name: "legacy synthetic bool reads as auto",
			input: dto.ChannelSettings{
				SyntheticClientHeaders: true,
			},
			expected: dto.ChannelSettings{
				SyntheticClientHeaders:        true,
				SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
			},
		},
		{
			name: "explicit family keeps the deprecated bool in step",
			input: dto.ChannelSettings{
				SyntheticClientHeadersProfile: constant.ClientHeaderFamilyClaude,
			},
			expected: dto.ChannelSettings{
				SyntheticClientHeaders:        true,
				SyntheticClientHeadersProfile: constant.ClientHeaderFamilyClaude,
			},
		},
		{
			// A typo must degrade to the wrong user-agent, never to forwarding the
			// caller's headers again — an unknown family would otherwise fall through
			// to the generic profile and look like it worked.
			name: "unknown family falls back to auto",
			input: dto.ChannelSettings{
				SyntheticClientHeadersProfile: "claud",
			},
			expected: dto.ChannelSettings{
				SyntheticClientHeaders:        true,
				SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
			},
		},
		{
			// Turning it off means clearing both fields. Writing an empty profile
			// while leaving the deprecated bool set would resurrect it as auto, which
			// is why buildSettingJSON on the frontend always writes the pair.
			name: "both synthetic fields cleared stays off",
			input: dto.ChannelSettings{
				SyntheticClientHeaders:        false,
				SyntheticClientHeadersProfile: "",
			},
			expected: dto.ChannelSettings{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			settings := testCase.input
			settings.Normalize()
			require.Equal(t, testCase.expected, settings)

			settings.Normalize()
			assert.Equal(t, testCase.expected, settings, "Normalize must be idempotent")
		})
	}
}
