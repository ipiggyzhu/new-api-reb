package controller

import (
	"net/http"

	relaychannel "github.com/QuantumNous/new-api/relay/channel"
)

// The client-header profiles moved to relay/channel so that live traffic can use
// them too: a channel with SyntheticClientHeaders replaces header passthrough
// with the same synthesized profile, which keeps the caller's own headers (and
// credentials) from reaching upstream. Duplicating the table here would let the
// two copies drift.
//
// These thin aliases keep the controller-side call sites and their tests
// unchanged.

const (
	clientHeaderFamilyClaude  = relaychannel.ClientHeaderFamilyClaude
	clientHeaderFamilyOpenAI  = relaychannel.ClientHeaderFamilyOpenAI
	clientHeaderFamilyCodex   = relaychannel.ClientHeaderFamilyCodex
	clientHeaderFamilyGemini  = relaychannel.ClientHeaderFamilyGemini
	clientHeaderFamilyGeneric = relaychannel.ClientHeaderFamilyGeneric
	clientHeaderFamilyAll     = relaychannel.ClientHeaderFamilyAll
)

type clientHeaderProfile = relaychannel.ClientHeaderProfile

// Accept values the profile falls back to; re-exported so the channel-test
// tests keep asserting against one definition rather than a second copy.
const (
	acceptJSON = relaychannel.AcceptJSON
	acceptSSE  = relaychannel.AcceptSSE
)

func clientHeaderFamilyForAPIType(apiType int) string {
	return relaychannel.ClientHeaderFamilyForAPIType(apiType)
}

func clientHeaderProfileForFamily(family string) clientHeaderProfile {
	return relaychannel.ClientHeaderProfileForFamily(family)
}

func clientHeaderProfileForAPIType(apiType int) clientHeaderProfile {
	return relaychannel.ClientHeaderProfileForAPIType(apiType)
}

// applyTestClientHeaders dresses the synthetic channel-test request as the
// client that channel type normally serves. Without it a test sends a shape no
// real caller ever sends, and upstreams that gate on the client reject it with a
// 4xx that says nothing about whether the model works.
func applyTestClientHeaders(header http.Header, apiType int, isStream bool) {
	relaychannel.ApplyClientHeaderProfile(header, apiType, isStream)
}
