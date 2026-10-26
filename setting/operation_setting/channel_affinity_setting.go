package operation_setting

import (
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/setting/config"
)

type ChannelAffinityKeySource struct {
	Type string `json:"type"` // context_int, context_string, request_header, gjson
	Key  string `json:"key,omitempty"`
	Path string `json:"path,omitempty"`
}

type ChannelAffinityRule struct {
	Name             string                     `json:"name"`
	ModelRegex       []string                   `json:"model_regex"`
	PathRegex        []string                   `json:"path_regex"`
	UserAgentInclude []string                   `json:"user_agent_include,omitempty"`
	KeySources       []ChannelAffinityKeySource `json:"key_sources"`

	ValueRegex string `json:"value_regex"`
	TTLSeconds int    `json:"ttl_seconds"`

	ParamOverrideTemplate map[string]interface{} `json:"param_override_template,omitempty"`

	SkipRetryOnFailure bool `json:"skip_retry_on_failure"`

	IncludeUsingGroup bool `json:"include_using_group"`
	IncludeModelName  bool `json:"include_model_name"`
	IncludeRuleName   bool `json:"include_rule_name"`
}

type ChannelAffinitySetting struct {
	Enabled               bool `json:"enabled"`
	SwitchOnSuccess       bool `json:"switch_on_success"`
	KeepOnChannelDisabled bool `json:"keep_on_channel_disabled"`
	// StickyMultiKey pins an affinity-matched request to one key of a
	// multi-key channel, not just to the channel: the upstream prompt cache is
	// per account, so rotating keys within a conversation misses it.
	StickyMultiKey bool `json:"sticky_multi_key"`
	// SaturatedWaitSeconds is how long a request whose pinned channel is at its
	// concurrency limit waits for a slot there before falling back to ordinary
	// selection. Falling back re-sends the whole prompt to a cold upstream cache,
	// so a short wait is usually the cheaper outcome. 0 disables waiting.
	SaturatedWaitSeconds int `json:"saturated_wait_seconds"`
	// SaturatedMaxWaiters caps how many requests may wait on one channel at
	// once; the rest fall back immediately. 0 disables waiting.
	SaturatedMaxWaiters int                   `json:"saturated_max_waiters"`
	MaxEntries          int                   `json:"max_entries"`
	DefaultTTLSeconds   int                   `json:"default_ttl_seconds"`
	Rules               []ChannelAffinityRule `json:"rules"`
	// KeyEnvironmentBindingTTLSeconds is how long a conversation stays bound to
	// one key of an independent key environment channel after its last request.
	// 0 or less uses the 24h default.
	KeyEnvironmentBindingTTLSeconds int `json:"key_environment_binding_ttl_seconds"`
}

// KeyEnvironmentBindingTTL returns the binding lifetime, 24h unless configured.
func (s *ChannelAffinitySetting) KeyEnvironmentBindingTTL() time.Duration {
	if s.KeyEnvironmentBindingTTLSeconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(s.KeyEnvironmentBindingTTLSeconds) * time.Second
}

// MaxChannelAffinitySaturatedWait bounds SaturatedWaitSeconds. Cloudflare drops
// an origin that sends nothing for ~100s, and a non-streaming request cannot
// ping while it waits, so the wait must leave the upstream most of that window.
const MaxChannelAffinitySaturatedWait = 60 * time.Second

// SaturatedWait returns the configured wait, clamped to
// [0, MaxChannelAffinitySaturatedWait].
func (s *ChannelAffinitySetting) SaturatedWait() time.Duration {
	if s.SaturatedWaitSeconds <= 0 {
		return 0
	}
	wait := time.Duration(s.SaturatedWaitSeconds) * time.Second
	if wait > MaxChannelAffinitySaturatedWait {
		return MaxChannelAffinitySaturatedWait
	}
	return wait
}

// Keep Codex CLI passthrough aligned with upstream. Codex uses lower-case
// header names, while HTTP matching here is case-insensitive.
// Request session/thread headers:
// https://github.com/openai/codex/commit/7c7b4861d88960f7e3bd5b7f30f8351be666dd84
// Responses metadata headers/client_metadata:
// https://github.com/openai/codex/commit/14df0e8833aad0d6d78287954b61ffac67af936c
// x-codex-turn-state response/request round trip:
// https://github.com/openai/codex/commit/ebdd8795e924a8149b616e46ca2ed7848c207a4b
var codexCliPassThroughHeaders = []string{
	"Originator",
	"Session_id",
	"Thread_id",
	"Session-Id",
	"Thread-Id",
	"X-Client-Request-Id",
	"User-Agent",
	"X-Codex-Beta-Features",
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
	"X-Codex-Window-Id",
	"X-Codex-Parent-Thread-Id",
	//"X-Codex-Installation-Id",
	"X-OpenAI-Subagent",
	"X-OpenAI-Memgen-Request",
	//"X-OAI-Attestation",
	"X-ResponsesAPI-Include-Timing-Metrics",
	"X-OpenAI-Internal-Codex-Responses-Lite",
}

var claudeCliPassThroughHeaders = []string{
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-Os",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"User-Agent",
	"X-App",
	"Anthropic-Beta",
	"Anthropic-Dangerous-Direct-Browser-Access",
	"Anthropic-Version",
}

func buildPassHeaderTemplate(headers []string) map[string]interface{} {
	clonedHeaders := make([]string, 0, len(headers))
	clonedHeaders = append(clonedHeaders, headers...)
	return map[string]interface{}{
		"operations": []map[string]interface{}{
			{
				"mode":        "pass_headers",
				"value":       clonedHeaders,
				"keep_origin": true,
			},
		},
	}
}

func buildCodexPassHeaderTemplate() map[string]interface{} {
	requestHeaders := make([]string, 0, len(codexCliPassThroughHeaders))
	requestHeaders = append(requestHeaders, codexCliPassThroughHeaders...)
	return map[string]interface{}{
		"operations": []map[string]interface{}{
			{
				"mode":        "pass_headers",
				"value":       requestHeaders,
				"keep_origin": true,
			},
		},
	}
}

var channelAffinitySetting = ChannelAffinitySetting{
	Enabled:                         true,
	SwitchOnSuccess:                 true,
	KeepOnChannelDisabled:           false,
	StickyMultiKey:                  true,
	SaturatedWaitSeconds:            20,
	SaturatedMaxWaiters:             3,
	MaxEntries:                      100_000,
	DefaultTTLSeconds:               3600,
	KeyEnvironmentBindingTTLSeconds: 86400,
	Rules: []ChannelAffinityRule{
		{
			Name:       "codex cli trace",
			ModelRegex: []string{"^gpt-.*$"},
			PathRegex:  []string{"/v1/responses"},
			KeySources: []ChannelAffinityKeySource{
				{Type: "gjson", Path: "prompt_cache_key"},
			},
			ValueRegex:            "",
			TTLSeconds:            0,
			ParamOverrideTemplate: buildCodexPassHeaderTemplate(),
			// Ordinary channels fail over after a pinned attempt fails. Independent
			// key environments apply their own same-key retry matrix in the relay.
			SkipRetryOnFailure: false,
			IncludeUsingGroup:  true,
			IncludeRuleName:    true,
			UserAgentInclude:   nil,
		},
		{
			Name:       "claude cli trace",
			ModelRegex: []string{"^claude-.*$"},
			PathRegex:  []string{"/v1/messages"},
			KeySources: []ChannelAffinityKeySource{
				{Type: "gjson", Path: "metadata.user_id"},
			},
			ValueRegex:            "",
			TTLSeconds:            0,
			ParamOverrideTemplate: buildPassHeaderTemplate(claudeCliPassThroughHeaders),
			// Messages requests carry no server-side conversation state, so a pinned
			// channel that fails can hand the request to another one. Refusing to
			// retry sent every such failure straight back to the client.
			SkipRetryOnFailure: false,
			IncludeUsingGroup:  true,
			IncludeRuleName:    true,
			UserAgentInclude:   nil,
		},
		{
			// Catches the chat traffic the client-specific rules above cannot key:
			// clients that send no session id, and every model outside their
			// model_regex. The API token is the one stable per-caller identity such a
			// request carries. The model is part of the key so a token calling several
			// models keeps a separate pin for each.
			Name:       "token fallback",
			ModelRegex: []string{".*"},
			PathRegex:  []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"},
			KeySources: []ChannelAffinityKeySource{
				{Type: "context_int", Key: "token_id"},
			},
			ValueRegex:         "",
			TTLSeconds:         0,
			SkipRetryOnFailure: false,
			IncludeUsingGroup:  true,
			IncludeModelName:   true,
			IncludeRuleName:    true,
			UserAgentInclude:   nil,
		},
	},
}

// publishedAffinitySetting holds the snapshot the request path reads.
//
// The config layer replaces Rules with reflect.Value.Set, which writes a slice
// header (pointer, len, cap) non-atomically. Handing the request path a bare
// *ChannelAffinitySetting let it range over that header mid-write and pick up the
// new pointer with the stale length, indexing past the new backing array. Publish
// an immutable snapshot through an atomic pointer instead, the same way
// currentIndex does for tool prices.
var publishedAffinitySetting atomic.Pointer[ChannelAffinitySetting]

func init() {
	config.GlobalConfig.Register("channel_affinity_setting", &channelAffinitySetting)
	RepublishChannelAffinitySetting()
}

// RepublishChannelAffinitySetting snapshots the mutable config into the atomic
// pointer. Called on init and after an admin save, never on the request path.
func RepublishChannelAffinitySetting() {
	snapshot := channelAffinitySetting
	snapshot.Rules = append([]ChannelAffinityRule(nil), channelAffinitySetting.Rules...)
	publishedAffinitySetting.Store(&snapshot)
}

// GetChannelAffinitySetting returns the current snapshot. The returned value is
// shared and must be treated as read-only; callers that mutate a rule's
// ParamOverrideTemplate must clone it first (see cloneStringAnyMap).
func GetChannelAffinitySetting() *ChannelAffinitySetting {
	if snapshot := publishedAffinitySetting.Load(); snapshot != nil {
		return snapshot
	}
	return &channelAffinitySetting
}
