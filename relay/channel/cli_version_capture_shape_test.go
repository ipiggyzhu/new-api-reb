package channel

import (
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

// Claude Code 2.1.306's main call (/tmp/capture/002.json, 005.json): the
// leading billing block, thinking, effort and output limit it sends.
const (
	claudeCode2306Billing  = `{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.306.813; cc_entrypoint=sdk-cli;"}`
	claudeCode2306Thinking = `{"type":"adaptive","display":"omitted"}`
	claudeCode2306Effort   = `{"effort":"medium"}`
)

// withOriginalRequest stores the caller's own request the way the relay does,
// so shaping can tell which fields the caller actually sent.
func withOriginalRequest(t *testing.T, c *gin.Context, original string) {
	t.Helper()
	storage, err := appcommon.CreateBodyStorage([]byte(original))
	require.NoError(t, err)
	c.Set(appcommon.KeyBodyStorage, storage)
	t.Cleanup(func() { appcommon.CleanupBodyStorage(c) })
}

// shapeVersionedClaudeRequest shapes a non-native request under a Claude
// version profile: OpenAI format goes through the real conversion first, any
// other format is an Anthropic SDK client's own Claude body.
func shapeVersionedClaudeRequest(t *testing.T, version string, format types.RelayFormat, request string) ([]byte, *gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := []byte(request)
	c, info := cliTestRequest("claude")
	if format == types.RelayFormatOpenAI {
		body, _, c, info = convertAndShapeOpenAIRequest(t, "claude", request)
	} else {
		c.Request.Header.Set("User-Agent", "Anthropic/Python 0.60.0")
		info.RelayFormat = types.RelayFormatClaude
		info.InitRequestConversionChain()
	}
	info.ChannelSetting.CLIVersionProfile = version
	withOriginalRequest(t, c, request)
	return shapeClaudeMessagesBody(t, c, info, body), c, info
}

// An OpenAI client's request under a 2.1.306 profile leaves as the captured
// main call: billing block first and bare, breakpoints on the caller's last two
// system blocks and last message block, the captured thinking in place of
// conversion's, the caller's effort as output_config.effort, the captured
// output limit since the caller set none, and the capture's top-level fields
// in its order. Only the TTL mode differs.
func TestClaudeVersionProfileSynthesizesCapturedMainCall(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	request := `{"model":"claude-opus-5-5","temperature":0.3,"top_p":0.9,"reasoning_effort":"low",
		"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}],
		"messages":[{"role":"system","content":[{"type":"text","text":"stable instructions"},{"type":"text","text":"dynamic instructions"}]},
			{"role":"user","content":"Say ok."}]}`
	cases := []struct {
		profile, cacheControl, beta string
	}{
		{"claude-2.1.306-linux-x64", `{"type":"ephemeral"}`, claudeCode2306Beta},
		{"claude-2.1.306-linux-x64-1h", `{"type":"ephemeral","ttl":"1h"}`, claudeCode2306Beta + ",extended-cache-ttl-2025-04-11"},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			converted, _, c, info := convertAndShapeOpenAIRequest(t, "claude", request)
			require.Equal(t, `{"type":"enabled","budget_tokens":1280}`, gjson.GetBytes(converted, "thinking").Raw, "conversion's own thinking")
			info.ChannelSetting.CLIVersionProfile = tc.profile
			withOriginalRequest(t, c, request)
			resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(string(converted)))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			got := <-captures

			var keys []string
			gjson.ParseBytes(got.body).ForEach(func(key, _ gjson.Result) bool {
				keys = append(keys, key.String())
				return true
			})
			assert.Equal(t, []string{"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking",
				"context_management", "output_config", "stream"}, keys)
			assert.Equal(t, claudeCode2306Billing, gjson.GetBytes(got.body, "system.0").Raw)
			assert.Equal(t, "stable instructions", gjson.GetBytes(got.body, "system.1.text").String())
			assert.Equal(t, map[string]string{
				"system.1.cache_control":             strings.TrimSpace("ephemeral " + gjson.Get(tc.cacheControl, "ttl").String()),
				"system.2.cache_control":             strings.TrimSpace("ephemeral " + gjson.Get(tc.cacheControl, "ttl").String()),
				"messages.0.content.0.cache_control": strings.TrimSpace("ephemeral " + gjson.Get(tc.cacheControl, "ttl").String()),
			}, claudeCacheBreakpoints(t, got.body))
			assert.Equal(t, tc.cacheControl, gjson.GetBytes(got.body, "system.2.cache_control").Raw)
			assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(got.body, "thinking").Raw)
			assert.Equal(t, `{"effort":"low"}`, gjson.GetBytes(got.body, "output_config").Raw)
			assert.Equal(t, "128000", gjson.GetBytes(got.body, "max_tokens").Raw)
			assert.Equal(t, tc.beta, got.header.Get("anthropic-beta"))
		})
	}
}

// The captured output limit only fills an absent one: a caller's own limit
// bounds billing pre-deduction and the model. Thinking that Anthropic would
// reject (forced tool choice) is not synthesized, the caller's effort and
// response format survive, and a model the capture does not cover keeps
// conversion's shape: the capture says nothing about its limits.
func TestClaudeVersionProfileKeepsCallerLimitAndContract(t *testing.T) {
	const profile = "claude-2.1.306-linux-x64"
	t.Run("OpenAI caller max_tokens", func(t *testing.T) {
		shaped, _, _ := shapeVersionedClaudeRequest(t, profile, types.RelayFormatOpenAI,
			`{"model":"claude-opus-5-5","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`)
		assert.Equal(t, "4096", gjson.GetBytes(shaped, "max_tokens").Raw)
		assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
		assert.Equal(t, claudeCode2306Effort, gjson.GetBytes(shaped, "output_config").Raw)
	})
	t.Run("SDK caller thinking, sampling and format", func(t *testing.T) {
		shaped, _, _ := shapeVersionedClaudeRequest(t, profile, types.RelayFormatClaude,
			`{"model":"claude-opus-5-5","max_tokens":2048,"temperature":1,"top_k":5,"thinking":{"type":"enabled","budget_tokens":1024},
			"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`)
		assert.Equal(t, "2048", gjson.GetBytes(shaped, "max_tokens").Raw)
		assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
		assert.Equal(t, `{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}}`, gjson.GetBytes(shaped, "output_config").Raw)
		for _, field := range []string{"temperature", "top_p", "top_k"} {
			assert.False(t, gjson.GetBytes(shaped, field).Exists(), field)
		}
	})
	t.Run("SDK caller without max_tokens and forced tool", func(t *testing.T) {
		shaped, _, _ := shapeVersionedClaudeRequest(t, profile, types.RelayFormatClaude,
			`{"model":"claude-opus-5-5","temperature":0.2,"tool_choice":{"type":"tool","name":"Bash"},
			"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
		assert.Equal(t, "128000", gjson.GetBytes(shaped, "max_tokens").Raw)
		assert.False(t, gjson.GetBytes(shaped, "thinking").Exists())
		assert.Equal(t, "0.2", gjson.GetBytes(shaped, "temperature").Raw)
		assert.Equal(t, claudeCode2306Effort, gjson.GetBytes(shaped, "output_config").Raw)
	})
	t.Run("model outside the capture", func(t *testing.T) {
		request := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
		converted, _, _, _ := convertAndShapeOpenAIRequest(t, "claude", request)
		shaped, _, _ := shapeVersionedClaudeRequest(t, profile, types.RelayFormatOpenAI, request)
		assert.Equal(t, gjson.GetBytes(converted, "max_tokens").Raw, gjson.GetBytes(shaped, "max_tokens").Raw)
		assert.False(t, gjson.GetBytes(shaped, "thinking").Exists())
		assert.False(t, gjson.GetBytes(shaped, "output_config").Exists())
		assert.Equal(t, claudeCode2306Billing, gjson.GetBytes(shaped, "system.0").Raw, "the billing block is client identity, not model-dependent")
	})
}

// Effort is caller intent, not a shape constant of the client (Addendum C): a
// caller's effort, sent as output_config or as an OpenAI/Responses reasoning
// effort that conversion would only turn into a replaced thinking budget,
// always reaches upstream. The captured effort fills only an absent one, and
// caller-only keys (format) are kept.
func TestClaudeVersionProfileKeepsCallerEffort(t *testing.T) {
	const profile = "claude-2.1.306-linux-x64"
	const format = `"format":{"type":"json_schema","schema":{"type":"object"}}`
	sdk := []struct {
		name, outputConfig, effort string
	}{
		{"caller effort xhigh", `"output_config":{"effort":"xhigh"},`, "xhigh"},
		{"caller format without effort", `"output_config":{` + format + `},`, "medium"},
		{"no output_config", ``, "medium"},
	}
	for _, tc := range sdk {
		t.Run("SDK "+tc.name, func(t *testing.T) {
			shaped, _, _ := shapeVersionedClaudeRequest(t, profile, types.RelayFormatClaude,
				`{"model":"claude-opus-5-5","max_tokens":2048,`+tc.outputConfig+`"messages":[{"role":"user","content":"hi"}]}`)
			assert.Equal(t, tc.effort, gjson.GetBytes(shaped, "output_config.effort").String())
			assert.Equal(t, gjson.Get(`{`+tc.outputConfig+`}`, "output_config.format").Raw, gjson.GetBytes(shaped, "output_config.format").Raw)
			assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
		})
	}
	openAI := []struct {
		effort, want string
	}{
		{"xhigh", "xhigh"},
		{"minimal", "low"},
	}
	for _, tc := range openAI {
		t.Run("OpenAI reasoning_effort "+tc.effort, func(t *testing.T) {
			shaped, _, _ := shapeVersionedClaudeRequest(t, profile, types.RelayFormatOpenAI,
				`{"model":"claude-opus-5-5","reasoning_effort":"`+tc.effort+`","messages":[{"role":"user","content":"hi"}]}`)
			assert.Equal(t, `{"effort":"`+tc.want+`"}`, gjson.GetBytes(shaped, "output_config").Raw)
		})
	}
	t.Run("Responses reasoning.effort xhigh", func(t *testing.T) {
		request := `{"model":"claude-opus-5-5","reasoning":{"effort":"xhigh"},"input":[{"role":"user","content":"hi"}]}`
		var responsesRequest dto.OpenAIResponsesRequest
		require.NoError(t, appcommon.UnmarshalJsonStr(request, &responsesRequest))
		c, info := cliTestRequest("claude")
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.163.0")
		info.RelayFormat = types.RelayFormatOpenAIResponses
		info.InitRequestConversionChain()
		result, err := service.ConvertRequest(c, info, types.RelayFormatClaude, &responsesRequest)
		require.NoError(t, err)
		converted, err := appcommon.Marshal(result.Value)
		require.NoError(t, err)
		info.ChannelSetting.CLIVersionProfile = profile
		withOriginalRequest(t, c, request)
		shaped := shapeClaudeMessagesBody(t, c, info, converted)
		assert.Equal(t, `{"effort":"xhigh"}`, gjson.GetBytes(shaped, "output_config").Raw)
		assert.Equal(t, claudeCode2306Thinking, gjson.GetBytes(shaped, "thinking").Raw)
	})
}

// The billing block leads exactly once and never carries a breakpoint;
// breakpoints are counted over the caller's own system blocks. A leading
// billing block of the caller's own is replaced by the profile's, and a retry
// of the shaped body changes nothing.
func TestClaudeVersionProfileBillingBlockLeadsSystem(t *testing.T) {
	const profile = "claude-2.1.306-linux-x64"
	cases := []struct {
		name, system string
		texts        []string
		breakpoints  map[string]string
	}{
		{"string system", `"only instructions"`, []string{"only instructions"},
			map[string]string{"system.1.cache_control": "ephemeral", "messages.0.content.0.cache_control": "ephemeral"}},
		{"caller billing block", `[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.282.abc; cc_entrypoint=cli;","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"stable"},{"type":"text","text":"dynamic"}]`, []string{"stable", "dynamic"},
			map[string]string{"system.1.cache_control": "ephemeral", "system.2.cache_control": "ephemeral", "messages.0.content.0.cache_control": "ephemeral"}},
		{"no system", ``, nil, map[string]string{"messages.0.content.0.cache_control": "ephemeral"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := `{"model":"claude-sonnet-4-5","max_tokens":512,"messages":[{"role":"user","content":"hi"}]}`
			if tc.system != "" {
				request = strings.Replace(request, `"messages"`, `"system":`+tc.system+`,"messages"`, 1)
			}
			shaped, c, info := shapeVersionedClaudeRequest(t, profile, types.RelayFormatClaude, request)
			system := gjson.GetBytes(shaped, "system").Array()
			require.Len(t, system, len(tc.texts)+1)
			assert.Equal(t, claudeCode2306Billing, system[0].Raw)
			for i, text := range tc.texts {
				assert.Equal(t, text, system[i+1].Get("text").String())
			}
			assert.Equal(t, tc.breakpoints, claudeCacheBreakpoints(t, shaped))
			assert.JSONEq(t, string(shaped), string(shapeClaudeMessagesBody(t, c, info, shaped)), "a retry must not add a block or move breakpoints")
		})
	}
}

// The outgoing beta set agrees with the final body even where the real
// client's own breakpoints differ from the profile's TTL mode: header layer
// only, the native body's breakpoints reach upstream as sent. A shaped body's
// beta follows the profile's placement, not the caller's stripped 1h, and an
// admin beta override passes through the same rule.
func TestClaudeVersionProfileBetaFollowsFinalBodyTTL(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	withHour := withClientCache.Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1))
	onlyDefault := strings.NewReplacer("@ttl1h", `,"cache_control":{"type":"ephemeral"}`, "@ttl5m", `,"cache_control":{"type":"ephemeral"}`).
		Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1))
	uncached := `{"model":"claude-sonnet-4-5","max_tokens":32000,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`
	sdkHour := `{"model":"claude-sonnet-4-5","max_tokens":1024,"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name, profile, body, adminBeta string
		sdk, hourTTL                   bool
	}{
		{"default profile, client sends 1h", "claude-2.1.306-linux-x64", withHour, "", false, true},
		{"1h profile, client sends only 5m", "claude-2.1.306-linux-x64-1h", onlyDefault, "", false, false},
		{"1h profile, profile places 1h", "claude-2.1.306-linux-x64-1h", uncached, "", false, true},
		{"default profile, profile places 5m", "claude-2.1.306-linux-x64", uncached, "", false, false},
		{"shaped SDK 1h under default profile", "claude-2.1.306-linux-x64", sdkHour, "", true, false},
		{"shaped SDK under 1h profile", "claude-2.1.306-linux-x64-1h", sdkHour, "", true, true},
		{"admin beta with 1h token, 5m body", "claude-2.1.306-linux-x64", onlyDefault, claudeCode2306Beta + ",extended-cache-ttl-2025-04-11", false, false},
		{"admin beta without 1h token, 1h body", "claude-2.1.306-linux-x64-1h", withHour, claudeCode2306Beta, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info := nativeClaudeCodeRequest()
			if tc.sdk {
				c.Request.Header.Set("User-Agent", "Anthropic/Python 0.60.0")
			}
			if tc.adminBeta != "" {
				info.HeadersOverride = map[string]any{"anthropic-beta": tc.adminBeta}
			}
			info.ChannelSetting.CLIVersionProfile = tc.profile
			withOriginalRequest(t, c, tc.body)
			resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(tc.body))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			got := <-captures
			beta := claudeCode2306Beta
			if tc.hourTTL {
				beta += ",extended-cache-ttl-2025-04-11"
			}
			assert.Equal(t, beta, got.header.Get("anthropic-beta"))
			assert.Equal(t, tc.hourTTL, claudeBodyHasHourTTL(got.body), "the beta must describe the body that was sent")
			if !tc.sdk && tc.body != uncached {
				assert.JSONEq(t, gjson.Get(tc.body, "system").Raw, gjson.GetBytes(got.body, "system").Raw)
				assert.JSONEq(t, gjson.Get(tc.body, "messages").Raw, gjson.GetBytes(got.body, "messages").Raw)
			}
		})
	}
}

// A tool schema property or tool input named cache_control is caller data,
// not a breakpoint: the native client sent none, so the profile places them.
func TestClaudeVersionProfileIgnoresCacheControlNamedData(t *testing.T) {
	c, info := nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = "claude-2.1.306-linux-x64"
	shaped := shapeClaudeMessagesBody(t, c, info, []byte(withoutClientCache.Replace(strings.Replace(claudeCodeCachedRequest, "@extra", "", 1))))
	assert.Equal(t, map[string]string{
		"system.0.cache_control":             "ephemeral",
		"system.1.cache_control":             "ephemeral",
		"messages.4.content.0.cache_control": "ephemeral",
	}, claudeCacheBreakpoints(t, shaped))
	assert.Equal(t, "string", gjson.GetBytes(shaped, "tools.#(name==\"Write\").input_schema.properties.cache_control.type").String())
	assert.Equal(t, "You are Claude Code.", gjson.GetBytes(shaped, "system.0.text").String(), "native requests get no billing block")

	c, info = nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = "claude-2.1.306-linux-x64"
	shaped = shapeClaudeMessagesBody(t, c, info, []byte(`{"model":"claude-sonnet-4-5","max_tokens":32000,"messages":[
		{"role":"user","content":"set it"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Config","input":{"cache_control":{"type":"ephemeral"}}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`))
	assert.Equal(t, `{"type":"ephemeral"}`, gjson.GetBytes(shaped, "messages.2.content.0.cache_control").Raw)
	assert.Equal(t, `{"cache_control":{"type":"ephemeral"}}`, gjson.GetBytes(shaped, "messages.1.content.0.input").Raw)
}

// Codex 0.163.0 (/tmp/capture/004.json) carries instructions as a developer
// message and has no instructions key; neither shaping nor the Codex
// adaptor's empty default may add one. Profiles whose capture has the key, and
// unversioned shaping, keep it.
func TestCodexVersionProfileOmitsUncapturedInstructionsKey(t *testing.T) {
	cases := []struct {
		name, version, instructions, want string
	}{
		{"0.163 moves instructions", "codex-0.163.0-linux-x64", `"system instruction"`, ""},
		{"0.163 adaptor default", "codex-0.163.0-linux-x64", `""`, ""},
		{"0.150 captures the key", "codex-0.150.0-windows-x64", `"system instruction"`, `"system instruction"`},
		{"unversioned keeps the empty field", "", `"system instruction"`, `""`},
		{"unversioned adaptor default", "", `""`, `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info := cliTestRequest("codex")
			info.ChannelSetting.CLIVersionProfile = tc.version
			body := `{"model":"gpt-6-astra","instructions":` + tc.instructions + `,"input":[{"role":"user","content":"hello"}]}`
			_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			shaped, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, tc.want, gjson.GetBytes(shaped, "instructions").Raw)
			if tc.instructions != `""` && tc.want != `"system instruction"` {
				assert.Equal(t, "system instruction", gjson.GetBytes(shaped, `input.#(role=="developer").content.0.text`).String())
			}
		})
	}
}

// Addendum A: without a selected version profile nothing of the captured main
// call is synthesized; conversion's thinking and limit reach upstream with no
// billing block, and the beta set is the unversioned one. (The unversioned
// Opus 5.5 effort default for an absent output_config predates profiles.)
func TestUnversionedClaudeShapingSynthesizesNothing(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	request := `{"model":"claude-opus-5-5","temperature":0.3,"reasoning_effort":"low",
		"messages":[{"role":"system","content":[{"type":"text","text":"stable"},{"type":"text","text":"dynamic"}]},{"role":"user","content":"Say ok."}]}`
	converted, _, c, info := convertAndShapeOpenAIRequest(t, "claude", request)
	withOriginalRequest(t, c, request)
	resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/messages"}, c, info, strings.NewReader(string(converted)))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	got := <-captures
	assert.Equal(t, "stable", gjson.GetBytes(got.body, "system.0.text").String())
	assert.NotContains(t, string(got.body), "x-anthropic-billing-header")
	for _, field := range []string{"thinking", "max_tokens"} {
		assert.Equal(t, gjson.GetBytes(converted, field).Raw, gjson.GetBytes(got.body, field).Raw, field)
	}
	assert.Equal(t, `{"type":"enabled","budget_tokens":1280}`, gjson.GetBytes(got.body, "thinking").Raw)
	assert.Equal(t, `{"effort":"medium"}`, gjson.GetBytes(got.body, "output_config").Raw)
	assert.Equal(t, claudeCodeBeta, got.header.Get("anthropic-beta"))
}
