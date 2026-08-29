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
