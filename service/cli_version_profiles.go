package service

import (
	"cmp"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/QuantumNous/new-api/common"
)

// Captured from official Windows and Linux CLIs. These are versioned wire profiles, not
// random JA3 identities. Different releases can share the same TLS stack.
// Prompts, tool implementations, credentials and captured identities are absent.
//
//go:embed cli_version_profiles.json
var cliVersionProfilesJSON []byte

type CLIVersionProfile struct {
	ID      string `json:"id"`
	Family  string `json:"family"`
	Version string `json:"version"`
	Label   string `json:"label"`
	// AutoAssign admits the profile to the independent key environment pool.
	// The capturer sets it once the capture is complete for that version.
	AutoAssign        bool                `json:"auto_assign"`
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
	// block, verbatim from the capture. Its cc_version suffix is derived from
	// the captured prompt; see BillingHeaderFor.
	BillingHeader string `json:"billing_header"`
}

// claudeBillingFingerprintSalt is the constant the Claude Code client hashes
// into the cc_version suffix of its billing block.
const claudeBillingFingerprintSalt = "59cf53e54c78"

// BillingHeaderFor is the billing block text the profile's client sends for a
// conversation whose first user prompt is prompt. The client appends to its
// version the first 3 hex digits of sha256(salt + prompt[4] + prompt[7] +
// prompt[20] + version), indexing UTF-16 code units and using "0" past the
// end, so the suffix is stable for a whole conversation. A header that does
// not carry "cc_version=<version>.<suffix>;" is returned unchanged.
func (p *CLIVersionProfile) BillingHeaderFor(prompt string) string {
	marker := "cc_version=" + p.Version + "."
	start := strings.Index(p.BillingHeader, marker)
	suffixAt := start + len(marker)
	if start < 0 || len(p.BillingHeader) < suffixAt+4 || p.BillingHeader[suffixAt+3] != ';' {
		return p.BillingHeader
	}
	units := utf16.Encode([]rune(prompt))
	var sampled strings.Builder
	for _, i := range []int{4, 7, 20} {
		if i < len(units) {
			sampled.WriteString(string(rune(units[i])))
		} else {
			sampled.WriteString("0")
		}
	}
	sum := sha256.Sum256([]byte(claudeBillingFingerprintSalt + sampled.String() + p.Version))
	return p.BillingHeader[:suffixAt] + hex.EncodeToString(sum[:])[:3] + p.BillingHeader[suffixAt+3:]
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
		if err := p.autoAssignIncomplete(); err != nil {
			panic(fmt.Sprintf("CLI version profile %s is marked auto_assign but %v", p.ID, err))
		}
	}
	return profiles
}()

// autoAssignIncomplete rejects a pool profile whose capture lacks what the
// shaping path needs to send that version as a whole: a claude profile without
// the captured main-call shape and billing block, a codex profile whose tools
// were not captured in input order.
func (p *CLIVersionProfile) autoAssignIncomplete() error {
	if !p.AutoAssign {
		return nil
	}
	switch p.Family {
	case "claude":
		if p.BillingHeader == "" || p.CapturedModel == "" {
			return fmt.Errorf("has no billing_header or captured_model")
		}
	case "codex":
		if !p.ToolsInInput {
			return fmt.Errorf("does not have tools_in_input")
		}
	default:
		return fmt.Errorf("has unknown family %q", p.Family)
	}
	return nil
}

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
// order; spaces around a flag (an admin's "a, b") and repeated flags are
// dropped.
func CoherentAnthropicBeta(list string, hourTTL bool) string {
	var betas []string
	for _, beta := range strings.Split(list, ",") {
		beta = strings.TrimSpace(beta)
		if beta != "" && beta != ExtendedCacheTTLBeta && !slices.Contains(betas, beta) {
			betas = append(betas, beta)
		}
	}
	if hourTTL {
		betas = append(betas, ExtendedCacheTTLBeta)
	}
	return strings.Join(betas, ",")
}

type CLIVersionProfileOption struct {
	ID         string `json:"id"`
	Family     string `json:"family"`
	Version    string `json:"version"`
	Label      string `json:"label"`
	JA3MD5     string `json:"ja3_md5"`
	CacheTTL   string `json:"cache_ttl"`
	AutoAssign bool   `json:"auto_assign"`
}

func SupportedCLIVersionProfiles() []CLIVersionProfileOption {
	options := make([]CLIVersionProfileOption, 0, len(cliVersionProfiles))
	for _, p := range cliVersionProfiles {
		options = append(options, CLIVersionProfileOption{p.ID, p.Family, p.Version, p.Label, p.JA3MD5, p.CacheTTL, p.AutoAssign})
	}
	return options
}

// KeyEnvironmentProfiles is the independent key environment pool for a family
// and cache mode, newest version first. Cache mode only selects between
// variants of a claude version; codex profiles have no cache mode and are
// listed whatever ttl says.
func KeyEnvironmentProfiles(family, ttl string) []*CLIVersionProfile {
	var pool []*CLIVersionProfile
	for i := range cliVersionProfiles {
		p := &cliVersionProfiles[i]
		if !p.AutoAssign || p.Family != family || (family == "claude" && p.CacheTTL != ttl) {
			continue
		}
		pool = append(pool, p)
	}
	slices.SortStableFunc(pool, func(a, b *CLIVersionProfile) int {
		return compareCLIVersions(b.Version, a.Version)
	})
	return pool
}

// compareCLIVersions orders dotted numeric versions ("2.1.306" after
// "2.1.288"); a non-numeric part compares as text.
func compareCLIVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		if xerr == nil && yerr == nil {
			if xn != yn {
				return cmp.Compare(xn, yn)
			}
			continue
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
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
func ResolveCLIVersionProfile(id, family, tlsOverride string, websocket, keyEnvironments bool) (*CLIVersionProfile, error) {
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
	// Independent environments always use the selected key's complete profile.
	// A saved channel TLS override only applies when that mode is turned off.
	if !keyEnvironments && strings.TrimSpace(tlsOverride) != "" {
		return nil, fmt.Errorf("CLI version profile already binds TLS; clear the separate TLS override")
	}
	if websocket && !keyEnvironments {
		return nil, fmt.Errorf("CLI version profiles require HTTPS HTTP/1.1; disable WebSocket transport")
	}
	return p, nil
}
