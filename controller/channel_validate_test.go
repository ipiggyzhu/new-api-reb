package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

// TestValidateChannelRejectsNilWithoutPanic pins the nil guard in
// validateChannel.
//
// AddChannel binds into AddChannelRequest, whose Channel field is a *model.Channel
// under the "channel" JSON key. A request body that omits that key (or gets the
// shape wrong) leaves the pointer nil, and validateChannel used to call
// channel.ValidateSettings() before its nil check — so a malformed admin request
// panicked the handler instead of returning a validation error. Observed in
// production as:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	  model/channel.go:957 -> controller/channel.go:469
func TestValidateChannelRejectsNilWithoutPanic(t *testing.T) {
	for _, isAdd := range []bool{true, false} {
		require.NotPanics(t, func() {
			err := validateChannel(nil, isAdd)
			require.Error(t, err, "nil channel must be rejected, not accepted")
		}, "validateChannel(nil, isAdd=%v) must not panic", isAdd)
	}
}

// TestValidateChannelStillRejectsEmptyKeyOnAdd keeps the original add-path
// contract intact after the nil check was hoisted above it.
func TestValidateChannelStillRejectsEmptyKeyOnAdd(t *testing.T) {
	err := validateChannel(&model.Channel{Type: 1, Key: ""}, true)
	require.Error(t, err, "adding a channel with an empty key must still fail")
}

// Saving a channel is where an out-of-range keepalive interval is refused, so
// a channel cannot be configured to keepalive more often than every 5 minutes.
func TestValidateChannelUpstreamKeepaliveInterval(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		valid          bool
	}{
		{"keepalive off", `{}`, true},
		{"default interval", `{"upstream_keepalive_enabled":true}`, true},
		{"lower bound", `{"upstream_keepalive_enabled":true,"upstream_keepalive_interval_minutes":5}`, true},
		{"below lower bound", `{"upstream_keepalive_enabled":true,"upstream_keepalive_interval_minutes":4}`, false},
		{"negative", `{"upstream_keepalive_interval_minutes":-1}`, false},
		{"above upper bound", `{"upstream_keepalive_enabled":true,"upstream_keepalive_interval_minutes":10081}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChannel(&model.Channel{Type: 1, OtherSettings: tc.settings}, false)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "upstream_keepalive_interval_minutes")
			}
		})
	}
}

func TestValidateChannelCLIVersionConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		valid         bool
	}{
		{"matching version", `{"synthetic_client_headers_profile":"codex","cli_version_profile":"codex-0.160.0-windows-x64"}`, true},
		{"unknown version", `{"synthetic_client_headers_profile":"codex","cli_version_profile":"codex-made-up"}`, false},
		{"version used as TLS-only", `{"synthetic_client_headers_profile":"claude","tls_fingerprint":"codex-0.160.0-windows-x64"}`, false},
		{"wrong family", `{"synthetic_client_headers_profile":"claude","cli_version_profile":"codex-0.160.0-windows-x64"}`, false},
		{"mixed TLS", `{"synthetic_client_headers_profile":"codex","cli_version_profile":"codex-0.160.0-windows-x64","tls_fingerprint":"claude-code"}`, false},
		{"WebSocket", `{"synthetic_client_headers_profile":"codex","cli_version_profile":"codex-0.160.0-windows-x64","websocket_transport":true}`, false},
		{"unchanged legacy TLS", `{"synthetic_client_headers_profile":"codex","tls_fingerprint":"codex-cli"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChannel(&model.Channel{Type: 1, Setting: &tc.setting}, false)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
