package channel

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// nativeClaudeCodeRequest is a request as the real Claude Code client sends it:
// Claude format in, never converted, with its own User-Agent.
func nativeClaudeCodeRequest() (*gin.Context, *relaycommon.RelayInfo) {
	c, info := cliTestRequest("claude")
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.282 (external, cli)")
	info.RelayFormat = types.RelayFormatClaude
	info.InitRequestConversionChain()
	return c, info
}

// claudeCacheBreakpoints maps the path of every cache_control in a Claude body
// to its "type ttl". It fails the test when one block carries two breakpoints
// or the body has more than Anthropic's limit of four.
func claudeCacheBreakpoints(t *testing.T, body []byte) map[string]string {
	t.Helper()
	found := make(map[string]string)
	collectClaudeCacheBreakpoints(t, gjson.ParseBytes(body), "", found)
	assert.LessOrEqual(t, len(found), 4, "Anthropic accepts at most four cache breakpoints")
	return found
}

func collectClaudeCacheBreakpoints(t *testing.T, value gjson.Result, path string, found map[string]string) {
	t.Helper()
	if !value.IsObject() && !value.IsArray() {
		return
	}
	breakpoints := 0
	value.ForEach(func(key, child gjson.Result) bool {
		childPath := key.String()
		if path != "" {
			childPath = path + "." + childPath
		}
		switch {
		case value.IsObject() && key.String() == "cache_control":
			breakpoints++
			found[childPath] = strings.TrimSpace(child.Get("type").String() + " " + child.Get("ttl").String())
		case key.String() != "input_schema" && key.String() != "input":
			// Schemas and tool inputs are caller data, not cache breakpoints.
			collectClaudeCacheBreakpoints(t, child, childPath, found)
		}
		return true
	})
	assert.LessOrEqual(t, breakpoints, 1, "%s carries duplicate cache breakpoints", path)
}

// convertAndShapeOpenAIRequest runs an OpenAI-format request through the real
// OpenAI -> Claude conversion and then the channel's CLI body shaping, as the
// relay does for a Claude channel.
func convertAndShapeOpenAIRequest(t *testing.T, profile, request string) (converted, final []byte, c *gin.Context, info *relaycommon.RelayInfo) {
	t.Helper()
	var openAIRequest dto.GeneralOpenAIRequest
	require.NoError(t, appcommon.UnmarshalJsonStr(request, &openAIRequest))
	c, info = cliTestRequest(profile)
	c.Request.Header.Set("User-Agent", "OpenAI/Python 1.99.0")
	info.RelayFormat = types.RelayFormatOpenAI
	info.InitRequestConversionChain()
	result, err := service.ConvertRequest(c, info, types.RelayFormatClaude, &openAIRequest)
	require.NoError(t, err)
	converted, err = appcommon.Marshal(result.Value)
	require.NoError(t, err)
	return converted, shapeClaudeMessagesBody(t, c, info, converted), c, info
}

// An OpenAI-format client marking odd places: a tool definition, both system
// parts, a mid-conversation part with ttl=1h and a tool result.
const oddlyCachedOpenAIRequest = `{"model":"claude-sonnet-4-5","max_tokens":512,
	"tools":[
		{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}@ttl1h},
		{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}
	],
	"messages":[
		{"role":"system","content":[{"type":"text","text":"stable instructions"@ttl1h},{"type":"text","text":"dynamic instructions"@ttl5m}]},
		{"role":"user","content":[{"type":"text","text":"first question"@ttl1h},{"type":"text","text":"details"}]},
		{"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"tool output"@ttl5m}]},
		{"role":"user","content":[{"type":"text","text":"follow-up"}]}
	]}`

var (
	withClientCache    = strings.NewReplacer("@ttl1h", `,"cache_control":{"type":"ephemeral","ttl":"1h"}`, "@ttl5m", `,"cache_control":{"type":"ephemeral"}`)
	withoutClientCache = strings.NewReplacer("@ttl1h", "", "@ttl5m", "")
)

// Conversion keeps an OpenAI client's cache_control, but under the Claude Code
// profile it must not decide the upstream body: the result is exactly the body
// shaped from the same request without any cache_control, i.e. the profile's
// own breakpoint placement, TTL and tool order.
func TestClaudeCLIOwnsCacheBreakpointsOfConvertedRequests(t *testing.T) {
	converted, shaped, c, info := convertAndShapeOpenAIRequest(t, "claude", withClientCache.Replace(oddlyCachedOpenAIRequest))
	assert.Equal(t, map[string]string{
		"system.0.cache_control":                       "ephemeral 1h",
		"system.1.cache_control":                       "ephemeral",
		"messages.0.content.0.cache_control":           "ephemeral 1h",
		"messages.2.content.0.content.0.cache_control": "ephemeral",
	}, claudeCacheBreakpoints(t, converted), "conversion passes the client's breakpoints to the shaping layer")

	_, profileShaped, _, _ := convertAndShapeOpenAIRequest(t, "claude", withoutClientCache.Replace(oddlyCachedOpenAIRequest))
	assert.JSONEq(t, string(profileShaped), string(shaped))
	assert.Equal(t, map[string]string{
		"system.0.cache_control":             "ephemeral",
		"system.1.cache_control":             "ephemeral",
		"messages.3.content.0.cache_control": "ephemeral",
	}, claudeCacheBreakpoints(t, shaped))
	assert.Equal(t, "Bash", gjson.GetBytes(shaped, "tools.0.name").String(), "client breakpoints must not pin the tool order")
	assert.Equal(t, "lookup", gjson.GetBytes(shaped, "tools.1.name").String())
	assert.JSONEq(t, string(shaped), string(shapeClaudeMessagesBody(t, c, info, shaped)), "a retry must not add or move breakpoints")
}

// Without CLI body shaping the gateway has no cache policy of its own: the
// converted request keeps the client's breakpoints, which is what lets an
// OpenAI-format client read the Anthropic prompt cache at all.
func TestConvertedCacheBreakpointsSurviveWithoutCLIShaping(t *testing.T) {
	for _, profile := range []string{"", "off", "openai", "codex"} {
		t.Run("profile="+profile, func(t *testing.T) {
			converted, final, _, _ := convertAndShapeOpenAIRequest(t, profile, withClientCache.Replace(oddlyCachedOpenAIRequest))
			assert.Equal(t, converted, final)
			assert.Equal(t, map[string]string{
				"system.0.cache_control":                       "ephemeral 1h",
				"system.1.cache_control":                       "ephemeral",
				"messages.0.content.0.cache_control":           "ephemeral 1h",
				"messages.2.content.0.content.0.cache_control": "ephemeral",
			}, claudeCacheBreakpoints(t, final))
			assert.Equal(t, "lookup", gjson.GetBytes(final, "tools.0.name").String())
		})
	}
}

// shapeClaudeMessagesBody sends a Claude body through the channel's CLI body
// shaping for a Messages upstream and returns the final body.
func shapeClaudeMessagesBody(t *testing.T, c *gin.Context, info *relaycommon.RelayInfo, body []byte) []byte {
	t.Helper()
	_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	final, err := io.ReadAll(reader)
	require.NoError(t, err)
	return final
}

// A Claude body with Claude Code's own cache layout: 1h tool and system
// breakpoints, 5m ones on the conversation, four in total. @extra stands for
// sender-specific top-level fields.
const claudeCodeCachedRequest = `{"model":"claude-sonnet-4-5","max_tokens":32000,@extra
	"tools":[
		{"name":"Write","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}@ttl1h},
		{"name":"Bash","input_schema":{"type":"object"}}
	],
	"system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"project instructions"@ttl1h}],
	"messages":[
		{"role":"user","content":[{"type":"text","text":"read the file"@ttl5m}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cat a"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"file body"@ttl5m}]}]},
		{"role":"assistant","content":[{"type":"text","text":"done"}]},
		{"role":"user","content":[{"type":"text","text":"thanks"}]}
	]}`

// Only the real Claude Code client's own breakpoints are passed through; the
// same layout from any other sender is replaced by the profile's placement.
func TestClaudeCLIKeepsOnlyNativeClaudeCodeBreakpoints(t *testing.T) {
	nativeBreakpoints := map[string]string{
		"tools.0.cache_control":                        "ephemeral 1h",
		"system.1.cache_control":                       "ephemeral 1h",
		"messages.0.content.0.cache_control":           "ephemeral",
		"messages.2.content.0.content.0.cache_control": "ephemeral",
	}
	profileBreakpoints := map[string]string{
		"system.0.cache_control":             "ephemeral",
		"system.1.cache_control":             "ephemeral",
		"messages.4.content.0.cache_control": "ephemeral",
	}
	cases := []struct {
		name      string
		userAgent string
		extra     string
		converted bool
		native    bool
	}{
		{name: "Claude Code client", userAgent: "claude-cli/2.1.282 (external, cli)", native: true},
		{name: "Claude Code behind a User-Agent rewriting proxy", userAgent: "corp-proxy/1.0",
			extra: `"metadata":{"user_id":"{\"device_id\":\"d1\",\"account_uuid\":\"\",\"session_id\":\"s1\"}"},`, native: true},
		{name: "legacy Claude Code behind a proxy", userAgent: "corp-proxy/1.0",
			extra: `"metadata":{"user_id":"user_d1_account__session_s1"},`, native: true},
		{name: "Anthropic SDK client with automatic caching", userAgent: "Anthropic/Python 0.60.0", extra: `"temperature":1@ttl5m,`},
		{name: "converted request claiming Claude Code", userAgent: "claude-cli/2.1.282 (external, cli)", converted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info := cliTestRequest("claude")
			c.Request.Header.Set("User-Agent", tc.userAgent)
			info.RelayFormat = types.RelayFormatClaude
			info.RequestConversionChain = []types.RelayFormat{types.RelayFormatClaude}
			if tc.converted {
				info.RelayFormat = types.RelayFormatOpenAIResponses
				info.RequestConversionChain = []types.RelayFormat{types.RelayFormatOpenAIResponses, types.RelayFormatClaude}
			}
			request := strings.Replace(claudeCodeCachedRequest, "@extra", tc.extra, 1)
			sent := []byte(withClientCache.Replace(request))
			shaped := shapeClaudeMessagesBody(t, c, info, sent)
			if !tc.native {
				assert.JSONEq(t, string(shapeClaudeMessagesBody(t, c, info, []byte(withoutClientCache.Replace(request)))), string(shaped))
				assert.Equal(t, profileBreakpoints, claudeCacheBreakpoints(t, shaped))
				assert.False(t, gjson.GetBytes(shaped, "cache_control").Exists(), "automatic caching is a cache policy too")
				assert.Equal(t, "Bash", gjson.GetBytes(shaped, "tools.0.name").String())
				return
			}
			assert.Equal(t, nativeBreakpoints, claudeCacheBreakpoints(t, shaped))
			assert.JSONEq(t, gjson.GetBytes(sent, "system").Raw, gjson.GetBytes(shaped, "system").Raw)
			assert.JSONEq(t, gjson.GetBytes(sent, "messages").Raw, gjson.GetBytes(shaped, "messages").Raw)
			assert.Equal(t, "Write", gjson.GetBytes(shaped, "tools.0.name").String(), "a native cache boundary must not move")
			assert.Equal(t, "string", gjson.GetBytes(shaped, "tools.0.input_schema.properties.cache_control.type").String())
			assert.JSONEq(t, string(shaped), string(shapeClaudeMessagesBody(t, c, info, shaped)), "a retry must not add or move breakpoints")
		})
	}
}

// Codex has no cache breakpoints and its prompt cache identity is the CLI
// session: caller cache fields, and Anthropic cache_control from a conversion
// or passthrough, must never reach a Codex-shaped upstream.
func TestCodexCLIOwnsPromptCacheFields(t *testing.T) {
	body := `{"model":"gpt-6-astra","instructions":"system","prompt_cache_key":"caller-cache-key","prompt_cache_retention":"24h",
		"prompt_cache_options":{"mode":"explicit"},"cache_control":{"type":"ephemeral"},
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}],"cache_control":{"type":"ephemeral"}},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"result","cache_control":{"type":"ephemeral"}}]}
		],
		"tools":[
			{"type":"function","name":"lookup","parameters":{"type":"object"},"cache_control":{"type":"ephemeral"}},
			{"type":"function","function":{"name":"wrapped","parameters":{"type":"object"},"cache_control":{"type":"ephemeral"}}}
		]}`
	cases := []struct {
		name          string
		version       string
		callerSession bool
	}{
		{name: "unversioned", callerSession: true},
		{name: "versioned with top-level tools", version: "codex-0.150.0-windows-x64", callerSession: true},
		{name: "caller cache key as the only session hint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info := cliTestRequest("codex")
			info.ChannelSetting.CLIVersionProfile = tc.version
			if !tc.callerSession {
				c.Request.Header.Del("session-id")
			}
			_, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, headers.Get("thread-id"), gjson.GetBytes(data, "prompt_cache_key").String())
			assert.NotEqual(t, "caller-cache-key", gjson.GetBytes(data, "prompt_cache_key").String())
			for _, field := range []string{"cache_control", "prompt_cache_retention", "prompt_cache_options"} {
				assert.NotContains(t, string(data), fmt.Sprintf("%q", field))
			}
			assert.Equal(t, "result", gjson.GetBytes(data, `input.#(type=="function_call_output").output.0.text`).String())
			assert.Contains(t, string(data), `"name":"wrapped"`)
		})
	}
}

// The reverse conversion (Claude -> Chat -> Responses) is the way a Claude
// client reaches a Codex-shaped channel; its cache_control must end there.
func TestClaudeRequestToCodexCarriesNoCacheControl(t *testing.T) {
	var claudeRequest dto.ClaudeRequest
	require.NoError(t, appcommon.UnmarshalJsonStr(withClientCache.Replace(`{"model":"gpt-6-astra","max_tokens":256,
		"system":[{"type":"text","text":"stable"@ttl1h}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"@ttl5m},{"type":"text","text":"more"}]},
			{"role":"assistant","content":[{"type":"text","text":"ok"@ttl5m}]},
			{"role":"user","content":"again"}
		]}`), &claudeRequest))
	c, info := cliTestRequest("codex")
	info.RelayFormat = types.RelayFormatClaude
	info.InitRequestConversionChain()
	result, err := service.ConvertRequest(c, info, types.RelayFormatOpenAIResponses, &claudeRequest)
	require.NoError(t, err)
	converted, err := appcommon.Marshal(result.Value)
	require.NoError(t, err)
	assert.NotContains(t, string(converted), `"cache_control"`)
	assert.Equal(t, "stable", gjson.GetBytes(converted, "instructions").String())

	_, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", bytes.NewReader(converted))
	require.NoError(t, err)
	final, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.NotContains(t, string(final), `"cache_control"`)
	assert.Equal(t, headers.Get("thread-id"), gjson.GetBytes(final, "prompt_cache_key").String())
}
