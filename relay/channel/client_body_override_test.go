package channel

import (
	"io"
	"net/http"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// overrideAndShape runs an Anthropic SDK client's Claude body (or a Codex
// Responses body) through the channel's param override and then CLI shaping,
// in the relay's order.
func overrideAndShape(t *testing.T, profile, version, request string, paramOverride map[string]any) ([]byte, http.Header, *relaycommon.RelayInfo) {
	t.Helper()
	c, info := cliTestRequest(profile)
	path := "/v1/responses"
	if profile == "claude" {
		path = "/v1/messages"
		c.Request.Header.Set("User-Agent", "Anthropic/Python 0.60.0")
		info.RelayFormat = types.RelayFormatClaude
	} else {
		info.RelayFormat = types.RelayFormatOpenAIResponses
	}
	info.InitRequestConversionChain()
	info.ChannelSetting.CLIVersionProfile = version
	info.ParamOverride = paramOverride
	withOriginalRequest(t, c, request)
	overridden, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(request), info)
	require.NoError(t, err)
	_, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test"+path, strings.NewReader(string(overridden)))
	require.NoError(t, err)
	shaped, err := io.ReadAll(reader)
	require.NoError(t, err)
	return shaped, headers, info
}

func overrideOperations(operations ...map[string]any) map[string]any {
	list := make([]any, 0, len(operations))
	for _, operation := range operations {
		list = append(list, operation)
	}
	return map[string]any{"operations": list}
}

// A selected profile owns synthesized fields after parameter overrides. With
// no version selected the ordinary override output is preserved.
func TestClaudeProfileOwnsParamOverrideFields(t *testing.T) {
	const request = `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`
	for _, version := range []string{"claude-2.1.306-linux-x64", ""} {
		t.Run("version="+version, func(t *testing.T) {
			shaped, _, info := overrideAndShape(t, "claude", version, request, map[string]any{"max_tokens": 2048, "temperature": 0.3})
			assert.Equal(t, map[string]bool{"max_tokens": true, "temperature": true}, info.ParamOverrideWrites.Fields)
			if version == "" {
				assert.Equal(t, "2048", gjson.GetBytes(shaped, "max_tokens").Raw)
				assert.Equal(t, "0.3", gjson.GetBytes(shaped, "temperature").Raw)
				assert.False(t, gjson.GetBytes(shaped, "thinking").Exists())
				assert.False(t, gjson.GetBytes(shaped, "context_management").Exists())
			} else {
				assert.Equal(t, "128000", gjson.GetBytes(shaped, "max_tokens").Raw)
				assert.False(t, gjson.GetBytes(shaped, "temperature").Exists())
				assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
				assert.Equal(t, "all", gjson.GetBytes(shaped, "context_management.edits.0.keep").String())
			}

			shaped, _, _ = overrideAndShape(t, "claude", version, request, overrideOperations(
				map[string]any{"path": "thinking", "mode": "set", "value": map[string]any{"type": "enabled", "budget_tokens": 4096}}))
			if version == "" {
				assert.JSONEq(t, `{"type":"enabled","budget_tokens":4096}`, gjson.GetBytes(shaped, "thinking").Raw)
			} else {
				assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
			}
		})
	}
	t.Run("caller limit and effort survive overrides", func(t *testing.T) {
		shaped, _, _ := overrideAndShape(t, "claude", "claude-2.1.306-linux-x64",
			`{"model":"claude-opus-5-5","max_tokens":4096,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`,
			map[string]any{"max_tokens": 8192, "output_config": map[string]any{"effort": "max"}})
		assert.Equal(t, "4096", gjson.GetBytes(shaped, "max_tokens").Raw)
		assert.Equal(t, "low", gjson.GetBytes(shaped, "output_config.effort").String())
	})
	t.Run("operation whose condition did not match", func(t *testing.T) {
		shaped, _, info := overrideAndShape(t, "claude", "claude-2.1.306-linux-x64", request, overrideOperations(
			map[string]any{"path": "max_tokens", "mode": "set", "value": 2048,
				"conditions": []any{map[string]any{"path": "model", "mode": "full", "value": "another-model"}}}))
		assert.Empty(t, info.ParamOverrideWrites.Fields)
		assert.Equal(t, "128000", gjson.GetBytes(shaped, "max_tokens").Raw)
		assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
	})
}

// Neither caller nor administrator breakpoints can replace the selected
// profile's placement or channel-wide TTL.
func TestClaudeProfileOwnsParamOverrideBreakpoints(t *testing.T) {
	const request = `{"model":"claude-sonnet-4-5","max_tokens":1024,
		"system":[{"type":"text","text":"stable"},{"type":"text","text":"dynamic"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`
	shaped, headers, info := overrideAndShape(t, "claude", "claude-2.1.306-linux-x64", request, overrideOperations(
		map[string]any{"path": "system.0.cache_control", "mode": "set", "value": map[string]any{"type": "ephemeral", "ttl": "1h"}}))
	assert.Len(t, info.ParamOverrideWrites.Breakpoints, 1)
	assert.Equal(t, map[string]string{
		"system.1.cache_control": "ephemeral", "system.2.cache_control": "ephemeral",
		"messages.2.content.0.cache_control": "ephemeral",
	}, claudeCacheBreakpoints(t, shaped))
	assert.Equal(t, "stable", gjson.GetBytes(shaped, "system.1.text").String())
	assert.NotContains(t, strings.Split(headers.Get("anthropic-beta"), ","), "extended-cache-ttl-2025-04-11")

	shaped, headers, _ = overrideAndShape(t, "claude", "claude-2.1.306-linux-x64", request, map[string]any{"max_tokens": 512})
	assert.Equal(t, map[string]string{
		"system.1.cache_control":             "ephemeral",
		"system.2.cache_control":             "ephemeral",
		"messages.2.content.0.cache_control": "ephemeral",
	}, claudeCacheBreakpoints(t, shaped), "without override breakpoints the profile places its own")
	assert.Equal(t, "1024", gjson.GetBytes(shaped, "max_tokens").Raw, "the original caller limit survives the override")
	assert.NotContains(t, strings.Split(headers.Get("anthropic-beta"), ","), "extended-cache-ttl-2025-04-11")
}

// Without a version the override result is untouched. With a version Codex
// sends only its isolated session cache key and its captured defaults.
func TestCodexProfileOwnsParamOverrideCacheFields(t *testing.T) {
	const request = `{"model":"gpt-6-astra","prompt_cache_retention":"in_memory","input":[{"role":"user","content":"hello"}]}`
	shaped, _, _ := overrideAndShape(t, "codex", "", request, map[string]any{"prompt_cache_retention": "24h", "prompt_cache_key": "channel-key"})
	assert.Equal(t, "24h", gjson.GetBytes(shaped, "prompt_cache_retention").String())
	assert.Equal(t, "channel-key", gjson.GetBytes(shaped, "prompt_cache_key").String())

	shaped, _, _ = overrideAndShape(t, "codex", "", request, overrideOperations(map[string]any{"path": "tool_choice", "mode": "delete"}))
	assert.Equal(t, "in_memory", gjson.GetBytes(shaped, "prompt_cache_retention").String())
	assert.False(t, gjson.GetBytes(shaped, "prompt_cache_key").Exists())
	assert.False(t, gjson.GetBytes(shaped, "tool_choice").Exists())

	shaped, headers, _ := overrideAndShape(t, "codex", "codex-0.163.0-linux-x64", request,
		map[string]any{"prompt_cache_retention": "24h", "prompt_cache_key": "channel-key"})
	assert.False(t, gjson.GetBytes(shaped, "prompt_cache_retention").Exists())
	assert.NotEqual(t, "channel-key", gjson.GetBytes(shaped, "prompt_cache_key").String())
	assert.Equal(t, headers.Get("session-id"), gjson.GetBytes(shaped, "prompt_cache_key").String())
}
