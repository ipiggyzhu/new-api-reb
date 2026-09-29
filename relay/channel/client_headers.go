package channel

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// Synthesized client profiles: the set of headers a client family sends,
// selected independently of the channel's protocol type.
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

// CLI defaults match the user-supplied Claude Code 2.1.282 and Codex exec
// 0.156.1 JSON captures. Runtime declarations describe the captured client,
// not this gateway's operating system. Admin overrides remain authoritative.
var anthropicClientHeaders = ClientHeaderProfile{
	"user-agent":        "claude-cli/2.1.282 (external, sdk-cli)",
	"anthropic-version": "2023-06-01",
	"x-app":             "cli",
	"anthropic-dangerous-direct-browser-access": "true",
	"x-stainless-lang":                          "js",
	"x-stainless-package-version":               "0.112.1",
	"x-stainless-os":                            "Windows",
	"x-stainless-arch":                          "x64",
	"x-stainless-runtime":                       "node",
	"x-stainless-runtime-version":               "v26.3.0",
	"x-stainless-retry-count":                   "0",
	"x-stainless-timeout":                       "600",
}

// openAIClientHeaders mirrors the official openai-python SDK, whose
// x-stainless-* headers are what "looks like the real SDK" means in practice.
var openAIClientHeaders = ClientHeaderProfile{
	"user-agent":                  "OpenAI/Python 3.17.0",
	"x-stainless-lang":            "python",
	"x-stainless-package-version": "3.17.0",
	"x-stainless-runtime":         "CPython",
	"x-stainless-runtime-version": "3.12.3",
	"x-stainless-os":              "Linux",
	"x-stainless-arch":            "x64",
	"accept-language":             "*",
}

// Codex's sample has no Node/x-stainless runtime declarations.
var codexClientHeaders = ClientHeaderProfile{
	"user-agent": "codex_exec/0.156.1 (Windows 10.0.26100; x86_64) WindowsTerminal (codex_exec; 0.156.1)",
	"originator": "codex_exec",
}

// geminiClientHeaders mirrors google-genai.
var geminiClientHeaders = ClientHeaderProfile{
	"user-agent":        "google-genai-sdk/2.24.0 gl-python/3.12.3",
	"x-goog-api-client": "google-genai-sdk/2.24.0 gl-python/3.12.3",
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

// ClientHeaderFamilyForAPIType retains the defaults used by legacy settings and
// management requests without an explicit profile.
func ClientHeaderFamilyForAPIType(apiType int) string {
	return constant.ClientHeaderFamilyForAPIType(apiType)
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
func EffectiveClientHeaders(family string, fingerprint ...string) ClientHeaderProfile {
	profile := ClientHeaderProfileForFamily(family)
	overrides := operation_setting.GetMonitorSetting().ChannelTestClientHeaders

	effective := make(ClientHeaderProfile, len(profile)+len(overrides))
	for name, value := range profile {
		effective[name] = value
	}
	// Only declarations present in the Claude capture follow a Node preset.
	// Codex never gains x-stainless headers from a TLS selection.
	if family == ClientHeaderFamilyClaude && len(fingerprint) > 0 {
		for name, value := range service.TLSFingerprintRuntimeHeaders(fingerprint[0]) {
			if _, exists := effective[name]; exists {
				effective[name] = value
			}
		}
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

// ApplyClientHeaderProfile fills header with the selected profile, then the
// admin's overrides. Management requests always have a generated identity:
// auto, off and unrecognized profiles retain the API type's default family.
// Existing headers are never overwritten, so explicit values survive.
func ApplyClientHeaderProfile(header http.Header, apiType int, profile string, isStream bool) {
	if header == nil {
		return
	}

	family := profile
	if !constant.IsClientHeaderFamily(family) {
		family = ClientHeaderFamilyForAPIType(apiType)
	}
	for name, value := range EffectiveClientHeaders(family) {
		if header.Get(name) == "" {
			header.Set(name, value)
		}
	}

	if header.Get("accept") == "" {
		if isStream && family != ClientHeaderFamilyClaude {
			header.Set("accept", AcceptSSE)
		} else {
			header.Set("accept", AcceptJSON)
		}
	}
}
