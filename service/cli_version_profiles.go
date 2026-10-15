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
}

var cliVersionProfiles = func() []CLIVersionProfile {
	var profiles []CLIVersionProfile
	if err := common.Unmarshal(cliVersionProfilesJSON, &profiles); err != nil {
		panic(fmt.Sprintf("invalid embedded CLI version profiles: %v", err))
	}
	return profiles
}()

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
