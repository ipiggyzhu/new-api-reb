package channel

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// Synthesized client profiles: the set of headers a real client of a given
// channel type sends.
//
// Two callers need these, which is why they live here rather than in
// controller:
//
//   - Channel tests. A test builds its own request carrying nothing but
//     Content-Type, and upstreams that gate on the client shape — the free relay
//     sites that only serve Claude Code, the ones checking for an official SDK —
//     answer that with a 4xx saying nothing about whether the model works.
//   - Channels with SyntheticClientHeaders. There, the profile *replaces*
//     header passthrough, so the caller's own headers (and credentials) never
//     reach upstream.
//
// Two properties matter and are covered by tests:
//   - nothing here overwrites an already-set header, so an explicit header
//     override still wins;
//   - unknown channel types get the generic set, never an empty map.

// ClientHeaderProfile is the set of headers one client family sends.
type ClientHeaderProfile map[string]string

const (
	// A stream request advertises SSE; a non-stream one asks for JSON. Left
	// unset, some upstreams reject an empty Accept outright.
	AcceptJSON = "application/json"
	AcceptSSE  = "text/event-stream"
)

// The version numbers below are the weak part of this file: a real client sends
// its own build's version and some upstreams gate on a minimum, so anything
// hardcoded here eventually goes stale — and on a gateway that would mean
// rebuilding an image to change one string. They are only defaults:
// operation_setting.MonitorSetting.ChannelTestClientHeaders overrides any of
// them from the admin UI. Values were taken from the package registries and the
// shipped clients on 2026-08-02, not invented.

// anthropicClientHeaders mirrors Claude Code CLI. The parenthesised suffix is a
// comma-separated list of run-context tags, which is how the CLI builds it.
var anthropicClientHeaders = ClientHeaderProfile{
	"user-agent":        "claude-cli/2.1.220 (external, cli)",
	"anthropic-version": "2023-06-01",
	"x-app":             "cli",
	"accept-language":   "*",
}

// openAIClientHeaders mirrors the official openai-python SDK, whose
// x-stainless-* headers are what "looks like the real SDK" means in practice.
var openAIClientHeaders = ClientHeaderProfile{
	"user-agent":                  "OpenAI/Python 2.52.0",
	"x-stainless-lang":            "python",
	"x-stainless-package-version": "2.52.0",
	"x-stainless-runtime":         "CPython",
	"x-stainless-runtime-version": "3.12.3",
	"x-stainless-os":              "Linux",
	"x-stainless-arch":            "x64",
	"accept-language":             "*",
}

// codexClientHeaders mirrors the Codex CLI, a distinct client from the Python
// SDK and the one that talks to /v1/responses.
var codexClientHeaders = ClientHeaderProfile{
	"user-agent":      "codex_cli_rs/0.146.0 (Linux 6.8.0; x86_64) terminal",
	"originator":      "codex_cli_rs",
	"accept-language": "*",
}

// geminiClientHeaders mirrors google-genai.
var geminiClientHeaders = ClientHeaderProfile{
	"user-agent":        "google-genai-sdk/2.16.0 gl-python/3.12.3",
	"x-goog-api-client": "google-genai-sdk/2.16.0 gl-python/3.12.3",
	"accept-language":   "*",
}

// genericClientHeaders is the fallback: still a plausible HTTP client rather
// than a bare request with no user-agent at all.
var genericClientHeaders = ClientHeaderProfile{
	"user-agent":      "new-api/1.0",
	"accept-language": "*",
}

// Client families. Defined in constant so dto can normalize a channel's
// synthetic_client_headers_profile against them; re-exported here so the call
// sites that reach for the profiles keep reading one name.
const (
	ClientHeaderFamilyClaude  = constant.ClientHeaderFamilyClaude
	ClientHeaderFamilyOpenAI  = constant.ClientHeaderFamilyOpenAI
	ClientHeaderFamilyCodex   = constant.ClientHeaderFamilyCodex
	ClientHeaderFamilyGemini  = constant.ClientHeaderFamilyGemini
	ClientHeaderFamilyGeneric = constant.ClientHeaderFamilyGeneric
	ClientHeaderFamilyAll     = constant.ClientHeaderFamilyAll
)

// ClientHeaderFamilyForAPIType names the client family a channel type serves.
// Dispatching on APIType rather than ChannelType means the Claude and Codex
// variants land on the right family without enumerating every vendor.
func ClientHeaderFamilyForAPIType(apiType int) string {
	switch apiType {
	case constant.APITypeAnthropic, constant.APITypeAws, constant.APITypeVertexAi:
		return ClientHeaderFamilyClaude
	case constant.APITypeCodex:
		// Codex is its own client and its own endpoint (/v1/responses); giving it
		// the Python SDK's headers would misrepresent both.
		return ClientHeaderFamilyCodex
	case constant.APITypeOpenAI:
		return ClientHeaderFamilyOpenAI
	case constant.APITypeGemini:
		return ClientHeaderFamilyGemini
	default:
		return ClientHeaderFamilyGeneric
	}
}

func ClientHeaderProfileForFamily(family string) ClientHeaderProfile {
	switch family {
	case ClientHeaderFamilyClaude:
		return anthropicClientHeaders
	case ClientHeaderFamilyCodex:
		return codexClientHeaders
	case ClientHeaderFamilyOpenAI:
		return openAIClientHeaders
	case ClientHeaderFamilyGemini:
		return geminiClientHeaders
	default:
		return genericClientHeaders
	}
}

func ClientHeaderProfileForAPIType(apiType int) ClientHeaderProfile {
	return ClientHeaderProfileForFamily(ClientHeaderFamilyForAPIType(apiType))
}

// EffectiveClientHeaders resolves the built-in profile for a family against the
// admin's overrides.
//
// "*" is applied before the family's own entries so a family-specific value
// beats a blanket one — the more specific rule wins, which is the only ordering
// that lets an admin set something globally and still special-case one family.
// An empty override value means "drop this built-in header" rather than "send an
// empty one", which is the only way to remove something the profile adds.
func EffectiveClientHeaders(family string) ClientHeaderProfile {
	profile := ClientHeaderProfileForFamily(family)
	overrides := operation_setting.GetMonitorSetting().ChannelTestClientHeaders

	effective := make(ClientHeaderProfile, len(profile)+len(overrides))
	for name, value := range profile {
		effective[name] = value
	}
	for _, scope := range []string{ClientHeaderFamilyAll, family} {
		for name, value := range overrides[scope] {
			normalizedName := strings.ToLower(strings.TrimSpace(name))
			if normalizedName == "" {
				continue
			}
			if strings.TrimSpace(value) == "" {
				delete(effective, normalizedName)
				continue
			}
			effective[normalizedName] = value
		}
	}
	return effective
}

// ApplyClientHeaderProfile fills header with the profile for apiType, then the
// admin's overrides. Existing headers are never overwritten, so callers may set
// anything they need first and it survives.
func ApplyClientHeaderProfile(header http.Header, apiType int, isStream bool) {
	if header == nil {
		return
	}

	for name, value := range EffectiveClientHeaders(ClientHeaderFamilyForAPIType(apiType)) {
		if header.Get(name) == "" {
			header.Set(name, value)
		}
	}

	if header.Get("accept") == "" {
		if isStream {
			header.Set("accept", AcceptSSE)
		} else {
			header.Set("accept", AcceptJSON)
		}
	}
}
