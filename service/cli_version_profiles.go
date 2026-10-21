package service

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// Captured from official Windows CLIs. These are versioned wire profiles, not
// random JA3 identities. Different releases can share the same TLS stack.
// Prompts, tool implementations, credentials and captured identities are absent.
//
//go:embed cli_version_profiles.json
var cliVersionProfilesJSON []byte

type CLIVersionProfile struct {
	ID                string              `json:"id"`
	Family            string              `json:"family"`
	Version           string              `json:"version"`
	Label             string              `json:"label"`
	JA3MD5            string              `json:"ja3_md5"`
	Headers           map[string]string   `json:"headers"`
	HeaderOrder       []string            `json:"header_order"`
	ClientHelloHex    string              `json:"client_hello_hex"`
	BodyOrder         map[string][]string `json:"body_order"`
	ToolOrder         []string            `json:"tool_order"`
	ToolsInInput      bool                `json:"tools_in_input"`
	MessageIDs        bool                `json:"message_ids"`
	MetadataKeys      []string            `json:"metadata_keys"`
	TurnDefaults      map[string]any      `json:"turn_defaults"`
	MaxTokens         int                 `json:"max_tokens"`
	LegacyUserID      bool                `json:"legacy_user_id"`
	ContextManagement bool                `json:"context_management"`
	ParallelToolCalls bool                `json:"parallel_tool_calls"`
	TextDefault       json.RawMessage     `json:"text_default"`
	// CacheTTL is the prompt-cache mode the client was captured in: empty for
	// its default breakpoints, "1h" when it ran with 1h caching. The beta flag
	// that mode needs is part of Headers, captured from the same run.
	CacheTTL string `json:"cache_ttl"`
	// RequestIDIsSession: x-client-request-id carries the session, not the turn.
	RequestIDIsSession bool `json:"request_id_is_session"`
	// The captured main call's model-dependent request shape: thinking,
	// output_config and max_tokens are only known for CapturedModel.
	CapturedModel string          `json:"captured_model"`
	Thinking      json.RawMessage `json:"thinking"`
	OutputConfig  json.RawMessage `json:"output_config"`
	// BillingHeader is the text of the client's leading, never-cached system
	// block, verbatim from the capture.
	BillingHeader string `json:"billing_header"`
}

// ExtendedCacheTTLBeta is the Anthropic beta flag a request needs once any of
// its cache breakpoints has ttl "1h".
const ExtendedCacheTTLBeta = "extended-cache-ttl-2025-04-11"

var cliVersionProfiles = func() []CLIVersionProfile {
	var profiles []CLIVersionProfile
	if err := common.Unmarshal(cliVersionProfilesJSON, &profiles); err != nil {
		panic(fmt.Sprintf("invalid embedded CLI version profiles: %v", err))
	}
	for _, p := range profiles {
		if p.CacheTTL != "" && p.CacheTTL != "1h" {
			panic(fmt.Sprintf("CLI version profile %s has uncaptured cache TTL mode %q", p.ID, p.CacheTTL))
		}
	}
	return profiles
}()

// CacheControl is the breakpoint the profile's client writes, key order
// included. Unversioned shaping (nil) keeps the plain default breakpoint.
func (p *CLIVersionProfile) CacheControl() json.RawMessage {
	if p == nil || p.CacheTTL == "" {
		return json.RawMessage(`{"type":"ephemeral"}`)
	}
	return json.RawMessage(`{"type":"ephemeral","ttl":"` + p.CacheTTL + `"}`)
}

// CoherentAnthropicBeta makes an anthropic-beta list agree with the final
// body: the extended-cache-ttl flag is present exactly when a breakpoint has
// ttl "1h", appended last as the 1h capture sends it. Other flags keep their
// order.
func CoherentAnthropicBeta(list string, hourTTL bool) string {
	var betas []string
	for _, beta := range strings.Split(list, ",") {
		if beta != "" && beta != ExtendedCacheTTLBeta {
			betas = append(betas, beta)
		}
	}
	if hourTTL {
		betas = append(betas, ExtendedCacheTTLBeta)
	}
	return strings.Join(betas, ",")
}

type CLIVersionProfileOption struct {
	ID      string `json:"id"`
	Family  string `json:"family"`
	Version string `json:"version"`
	Label   string `json:"label"`
	JA3MD5  string `json:"ja3_md5"`
}

func SupportedCLIVersionProfiles() []CLIVersionProfileOption {
	options := make([]CLIVersionProfileOption, 0, len(cliVersionProfiles))
	for _, p := range cliVersionProfiles {
		options = append(options, CLIVersionProfileOption{p.ID, p.Family, p.Version, p.Label, p.JA3MD5})
	}
	return options
}

// LookupCLIVersionProfile returns immutable capture data. Callers must copy
// maps/slices before changing them; requests never mutate the shared catalog.
func LookupCLIVersionProfile(id string) *CLIVersionProfile {
	id = strings.ToLower(strings.TrimSpace(id))
	for i := range cliVersionProfiles {
		if cliVersionProfiles[i].ID == id {
			return &cliVersionProfiles[i]
		}
	}
	return nil
}

// ResolveCLIVersionProfile does not change legacy channels. A version profile
// binds its whole family and cannot be mixed with an independent TLS override.
func ResolveCLIVersionProfile(id, family, tlsOverride string, websocket bool) (*CLIVersionProfile, error) {
	if strings.TrimSpace(id) == "" || family == "" || family == "off" {
		return nil, nil
	}
	p := LookupCLIVersionProfile(id)
	if p == nil {
		return nil, fmt.Errorf("unknown CLI version profile: %s", id)
	}
	if p.Family != family {
		return nil, fmt.Errorf("CLI version profile %s does not belong to %s", id, family)
	}
	if strings.TrimSpace(tlsOverride) != "" {
		return nil, fmt.Errorf("CLI version profile already binds TLS; clear the separate TLS override")
	}
	if websocket {
		return nil, fmt.Errorf("CLI version profiles require HTTPS HTTP/1.1; disable WebSocket transport")
	}
	return p, nil
}
