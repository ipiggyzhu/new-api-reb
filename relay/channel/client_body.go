package channel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/reasoning"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Structural defaults from Claude Code 2.1.282 and Codex CLI 0.156.1 captures
// (2026-09-27). No captured prompts, tool implementations, or identities are
// replayed. The existing channel profile switch also controls body shaping.
const claudeCodeBeta = "claude-code-20250219,context-1m-2025-08-07,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,advanced-tool-use-2025-11-20,effort-2025-11-24"

type cliObject = map[string]json.RawMessage

// Values here are JSON-derived objects or internally constructed JSON values;
// RawMessage keeps tool schemas and opaque payloads lossless, including integers
// larger than float64's exact range.
func cliField(value any) json.RawMessage {
	data, _ := common.Marshal(value)
	return data
}

type cliIdentity struct {
	device, installation, session, turn, window, contextWindow string
	startedAt                                                  int64
	// seed is the conversation the session ids derive from.
	seed string
}

// Keep stable, tenant-isolated pseudorandom IDs while matching the sample's
// UUID version/variant bits. UUIDv7 time comes from the source conversation's
// timestamp (when present), otherwise the channel's creation time; turn IDs
// use the current request time. No captured identifier is replayed.
func isolatedCLIUUID(key, scope string, version byte, timestampMillis int64) string {
	var id uuid.UUID
	_, _ = hex.Decode(id[:], []byte(common.GenerateHMACWithKey([]byte(key), scope)[:32]))
	if version == 7 {
		for i := 5; i >= 0; i-- {
			id[i] = byte(timestampMillis)
			timestampMillis >>= 8
		}
	}
	id[6] = (id[6] & 0x0f) | version<<4
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

// requestCLIIdentity derives the client identity a CLI-shaped request wears.
// It is scoped to the key's identity slot rather than its index: a single-key
// channel's slot is "0", the index it always had, and each other key of a
// multi-key channel has its own slot that follows the key across re-indexing.
func requestCLIIdentity(c *gin.Context, info *relaycommon.RelayInfo, family string, data []byte) cliIdentity {
	scope := fmt.Sprintf("cli:%s:%d:%s:%d:%d", family, info.ChannelId, info.ChannelKeySlot, info.UserId, info.TokenId)
	deviceSeed := strings.TrimSpace(info.ChannelSetting.ClientDeviceSeed)
	sessionSeed := strings.TrimSpace(info.ChannelSetting.ClientSessionSeed)
	cacheKey := scope + ":" + deviceSeed + ":" + sessionSeed
	if cached, ok := c.Get(cacheKey); ok {
		return cached.(cliIdentity)
	}
	deviceKey, sessionKey := common.CryptoSecret, common.CryptoSecret
	if deviceSeed != "" {
		deviceKey = deviceSeed
	}
	if sessionSeed != "" {
		sessionKey = sessionSeed
	}
	seed := info.CLISessionSeed
	if seed == "" {
		seed = service.CLIConversationSeed(c.Request.Header, data)
	}
	requestID := info.RequestId
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if seed == "" {
		seed = requestID
	}
	startedAt := info.StartTime
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	sessionVersion := byte(4)
	sessionTime := info.ChannelCreateTime * 1000
	if family == constant.ClientHeaderFamilyCodex {
		sessionVersion = 7
		if source, err := uuid.Parse(seed); err == nil && source.Version() == 7 {
			sessionTime = 0
			for _, b := range source[:6] {
				sessionTime = sessionTime<<8 | int64(b)
			}
		}
	}
	identity := cliIdentity{
		device:        common.GenerateHMACWithKey([]byte(deviceKey), scope+":device"),
		installation:  isolatedCLIUUID(deviceKey, scope+":installation", 4, 0),
		session:       isolatedCLIUUID(sessionKey, scope+":session:"+seed, sessionVersion, sessionTime),
		turn:          isolatedCLIUUID(sessionKey, scope+":turn:"+requestID, sessionVersion, startedAt.UnixMilli()),
		contextWindow: isolatedCLIUUID(sessionKey, scope+":context-window:"+seed, sessionVersion, sessionTime),
		startedAt:     startedAt.UnixMilli(),
		seed:          seed,
	}
	// Codex's window identifier is the thread UUID followed by the window index.
	identity.window = identity.session + ":0"
	c.Set(cacheKey, identity)
	return identity
}

// prepareCLIRequest runs after conversion/parameter overrides, including body
// passthrough, but before either HTTP or WebSocket sends anything. Profiles never
// change the upstream wire protocol: a Claude profile on /chat/completions still
// only changes headers. Custom routes use the resolved final relay format.
func prepareCLIRequest(c *gin.Context, info *relaycommon.RelayInfo, requestURL string, reader io.Reader) (string, io.Reader, http.Header, error) {
	if info == nil || info.ChannelMeta == nil || reader == nil {
		return requestURL, reader, nil, nil
	}
	settings := info.ChannelSetting
	settings.Normalize(info.ApiType)
	family := settings.SyntheticClientHeadersProfile
	version, err := service.ResolveCLIVersionProfile(settings.CLIVersionProfile, family, settings.TLSFingerprint, settings.WebsocketTransport)
	if err != nil {
		return requestURL, reader, nil, err
	}
	if family != constant.ClientHeaderFamilyClaude && family != constant.ClientHeaderFamilyCodex {
		return requestURL, reader, nil, nil
	}
	u, err := url.Parse(requestURL)
	if err != nil {
		return requestURL, reader, nil, err
	}
	format := info.GetFinalRequestRelayFormat()
	claude := family == constant.ClientHeaderFamilyClaude && (strings.HasSuffix(u.Path, "/messages") || format == types.RelayFormatClaude)
	codex := family == constant.ClientHeaderFamilyCodex && (strings.HasSuffix(u.Path, "/responses") || format == types.RelayFormatOpenAIResponses)
	if strings.HasSuffix(u.Path, "/compact") || (!claude && !codex) {
		return requestURL, reader, nil, nil
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return requestURL, reader, nil, fmt.Errorf("read CLI request body: %w", err)
	}
	reference := data
	if codex || version != nil {
		// Use the original request only as an ordering template, never as a
		// source of values: conversion, overrides and fallback remain effective.
		if cached, ok := c.Get(common.KeyBodyStorage); ok {
			if storage, ok := cached.(common.BodyStorage); ok {
				reference, err = storage.Bytes()
				if err != nil {
					return requestURL, reader, nil, err
				}
			}
		}
	}
	var body cliObject
	if err := common.Unmarshal(data, &body); err != nil || body == nil {
		return requestURL, reader, nil, fmt.Errorf("CLI request body must be a JSON object")
	}
	identity := requestCLIIdentity(c, info, family, data)
	// Recorded before shaping, so the reasoning is the caller's own.
	if !info.IsChannelTest {
		service.RecordChannelCLISession(info.ChannelId, info.ChannelKeySlot, service.ChannelCLISession{
			ModelName: info.OriginModelName, UserId: info.UserId, TokenId: info.TokenId,
			Seed: identity.seed, Reasoning: body["reasoning"],
		})
	}
	headers := make(http.Header)
	// Preserve the downstream streaming contract, including explicit false. CLI
	// defaults must not silently turn a synchronous request into an SSE response.
	body["stream"] = cliField(info.IsStream)
	headers.Set("Content-Type", "application/json")
	if info.IsStream && !claude {
		headers.Set("Accept", AcceptSSE)
	} else {
		headers.Set("Accept", AcceptJSON)
	}
	if claude {
		if u.Scheme == "https" || u.Scheme == "http" {
			headers.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			headers.Set("Connection", "keep-alive")
		}
		// Conversion always fills max_tokens; only the caller's own request
		// tells whether an output limit was actually asked for.
		callerMaxTokens := false
		for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "generationConfig.maxOutputTokens", "generationConfig.max_output_tokens"} {
			callerMaxTokens = callerMaxTokens || gjson.GetBytes(reference, path).Int() > 0
		}
		// Conversion turns an OpenAI caller's effort into a thinking budget that
		// the profile's thinking replaces; keep the effort itself as the caller's
		// output_config.effort. Levels Claude has no equivalent for are not sent.
		callerEffort := ""
		if info.RelayFormat == types.RelayFormatOpenAI || info.RelayFormat == types.RelayFormatOpenAIResponses {
			effort := gjson.GetBytes(reference, "reasoning_effort").String()
			if effort == "" {
				effort = gjson.GetBytes(reference, "reasoning.effort").String()
			}
			effort = reasoning.ClaudeSuffixEffort(gjson.GetBytes(data, "model").String(), effort)
			if slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, effort) {
				callerEffort = effort
			}
		}
		err = shapeClaudeCodeBody(body, identity, isNativeClaudeCodeRequest(c, info, body), callerMaxTokens, callerEffort, version)
		query := u.Query()
		query.Set("beta", "true")
		u.RawQuery = query.Encode()
		headers.Set("anthropic-beta", claudeCodeBeta)
		headers.Set("X-Claude-Code-Session-Id", identity.session)
		if version != nil {
			// Captured with the profile's TTL mode: a 1h profile's set includes
			// extended-cache-ttl-2025-04-11, a default profile's does not.
			headers.Set("anthropic-beta", version.Headers["anthropic-beta"])
			headers.Set("Accept-Encoding", version.Headers["accept-encoding"])
			if version.LegacyUserID {
				headers.Del("X-Claude-Code-Session-Id")
			}
		}
	} else {
		err = shapeCodexBody(body, identity, version)
		headers.Set("x-codex-beta-features", "remote_compaction_v2")
		headers.Set("x-openai-internal-codex-responses-lite", "true")
		headers.Set("session-id", identity.session)
		headers.Set("thread-id", identity.session)
		headers.Set("x-client-request-id", identity.turn)
		headers.Set("x-codex-window-id", identity.window)
		headers.Set("x-codex-turn-metadata", gjson.GetBytes(body["client_metadata"], "x-codex-turn-metadata").String())
		if version != nil {
			headers.Set("x-codex-beta-features", version.Headers["x-codex-beta-features"])
			if value := version.Headers["x-openai-internal-codex-responses-lite"]; value != "" {
				headers.Set("x-openai-internal-codex-responses-lite", value)
			} else {
				headers.Del("x-openai-internal-codex-responses-lite")
			}
			if version.RequestIDIsSession {
				headers.Set("x-client-request-id", identity.session)
			}
			var metadata cliObject
			_ = common.Unmarshal(body["client_metadata"], &metadata)
			selected := cliObject{}
			for _, key := range version.MetadataKeys {
				if value, ok := metadata[key]; ok {
					selected[key] = value
				}
			}
			body["client_metadata"] = cliField(selected)
		}
	}
	if err != nil {
		return requestURL, reader, nil, err
	}
	if codex {
		info.ReasoningEffort = gjson.GetBytes(body["reasoning"], "effort").String()
	}
	data, err = common.Marshal(body)
	if err != nil {
		return requestURL, reader, nil, fmt.Errorf("encode CLI request body: %w", err)
	}
	if version != nil {
		data = orderVersionedCLIJSON(gjson.ParseBytes(data), gjson.ParseBytes(reference), "", version)
		if claude {
			// Header layer only: the beta set follows the TTLs the final body
			// carries, wherever its breakpoints came from.
			headers.Set("anthropic-beta", service.CoherentAnthropicBeta(version.Headers["anthropic-beta"], claudeBodyHasHourTTL(data)))
		}
		if codex {
			turn := orderVersionedCLIJSON(gjson.Parse(headers.Get("x-codex-turn-metadata")), gjson.Result{}, "turn_metadata", version)
			headers.Set("x-codex-turn-metadata", string(turn))
		}
	} else if codex {
		data = orderCodexJSON(gjson.ParseBytes(data), gjson.ParseBytes(reference), "")
		headers.Set("x-codex-turn-metadata", gjson.GetBytes(data, "client_metadata.x-codex-turn-metadata").String())
	}
	info.UpstreamRequestBodySize = int64(len(data))
	return u.String(), bytes.NewReader(data), headers, nil
}

func cliContentBlocks(raw json.RawMessage, textType string) ([]cliObject, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if common.GetJsonType(raw) == "string" {
		if gjson.ParseBytes(raw).String() == "" {
			return nil, nil
		}
		return []cliObject{{"type": cliField(textType), "text": raw}}, nil
	}
	var blocks []cliObject
	if err := common.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("CLI content must be text or content blocks")
	}
	for _, block := range blocks {
		if block == nil {
			return nil, fmt.Errorf("CLI content blocks must be objects")
		}
	}
	return blocks, nil
}

// isNativeClaudeCodeRequest reports whether the Claude body is the real Claude
// Code client's own request: Claude format end to end, never converted, from a
// client identifying as Claude Code. Only such a request already carries the
// profile's cache layout. Conversion output (whose cache_control came from an
// OpenAI/Responses/Gemini client) and other Anthropic SDK clients do not.
func isNativeClaudeCodeRequest(c *gin.Context, info *relaycommon.RelayInfo, body cliObject) bool {
	if info.IsChannelTest || info.RelayFormat != types.RelayFormatClaude {
		return false
	}
	for _, format := range info.RequestConversionChain {
		if format != types.RelayFormatClaude {
			return false
		}
	}
	if strings.HasPrefix(c.Request.Header.Get("User-Agent"), "claude-cli/") {
		return true
	}
	// Proxies may rewrite User-Agent; Claude Code's metadata.user_id survives.
	userID := gjson.GetBytes(body["metadata"], "user_id").String()
	return gjson.Get(userID, "session_id").String() != "" ||
		(strings.HasPrefix(userID, "user_") && strings.Contains(userID, "_account_") && strings.Contains(userID, "_session_"))
}

// stripCacheBreakpoints removes prompt-cache breakpoints from content blocks,
// including blocks nested in tool_result content, so that only the selected
// profile's cache policy reaches upstream.
func stripCacheBreakpoints(blocks []cliObject) {
	for _, block := range blocks {
		delete(block, "cache_control")
		if common.GetJsonType(block["content"]) != "array" {
			continue
		}
		var nested []cliObject
		if err := common.Unmarshal(block["content"], &nested); err != nil {
			continue
		}
		stripCacheBreakpoints(nested)
		block["content"] = cliField(nested)
	}
}

// claudePromptBreakpoints appends every cache_control Anthropic reads as a
// breakpoint in a system, tools or messages array: on its blocks, including
// content nested in tool results. Tool schemas and tool_use inputs are caller
// data, so a property named cache_control there is not a breakpoint.
func claudePromptBreakpoints(blocks gjson.Result, found []gjson.Result) []gjson.Result {
	for _, block := range blocks.Array() {
		if !block.IsObject() {
			continue
		}
		if breakpoint := block.Get("cache_control"); breakpoint.Exists() {
			found = append(found, breakpoint)
		}
		if content := block.Get("content"); content.IsArray() {
			found = claudePromptBreakpoints(content, found)
		}
	}
	return found
}

// claudeBodyHasHourTTL reports whether a final Claude body carries a 1h
// breakpoint, top-level or on a block, and so needs the extended-cache-ttl beta.
func claudeBodyHasHourTTL(data []byte) bool {
	final := gjson.ParseBytes(data)
	if final.Get("cache_control.ttl").String() == "1h" {
		return true
	}
	for _, field := range []string{"system", "tools", "messages"} {
		for _, breakpoint := range claudePromptBreakpoints(final.Get(field), nil) {
			if breakpoint.Get("ttl").String() == "1h" {
				return true
			}
		}
	}
	return false
}

func shapeClaudeCodeBody(body cliObject, identity cliIdentity, nativeCLI bool, callerMaxTokens bool, callerEffort string, versions ...*service.CLIVersionProfile) error {
	var version *service.CLIVersionProfile
	if len(versions) > 0 {
		version = versions[0]
	}
	system, err := cliContentBlocks(body["system"], "text")
	if err != nil {
		return err
	}
	// The real Claude Code client already sends the profile's cache layout:
	// keep its breakpoints and only add ours when it sent none. Every other
	// sender's cache_control (converted from another format, or another SDK's
	// own TTLs and positions) is replaced by the profile's placement below.
	hasCache := false
	if nativeCLI {
		for _, key := range []string{"system", "tools", "messages", "cache_control"} {
			if key == "cache_control" {
				hasCache = hasCache || len(body[key]) > 0
			} else if version != nil {
				hasCache = hasCache || len(claudePromptBreakpoints(gjson.ParseBytes(body[key]), nil)) > 0
			} else {
				hasCache = hasCache || bytes.Contains(body[key], []byte(`"cache_control"`))
			}
		}
	} else {
		delete(body, "cache_control")
		stripCacheBreakpoints(system)
	}
	// The profile client's leading billing block is never cached; breakpoints
	// are placed over the caller's own blocks, then the captured one leads.
	billing := version != nil && !nativeCLI && version.BillingHeader != ""
	if billing && len(system) > 0 && gjson.ParseBytes(system[0]["type"]).String() == "text" &&
		strings.HasPrefix(gjson.ParseBytes(system[0]["text"]).String(), "x-anthropic-billing-header:") {
		system = system[1:]
	}
	// Breakpoints we place carry the profile's TTL mode; its headers carry the
	// matching beta flag (see prepareCLIRequest).
	cacheControl := version.CacheControl()
	if !hasCache {
		for i := len(system) - 1; i >= 0 && i >= len(system)-2; i-- {
			if gjson.ParseBytes(system[i]["type"]).String() == "text" {
				system[i]["cache_control"] = cacheControl
			}
		}
	}
	if billing {
		system = append([]cliObject{{"type": cliField("text"), "text": cliField(version.BillingHeader)}}, system...)
	}
	if len(system) > 0 {
		body["system"] = cliField(system)
	}
	var messages []cliObject
	if err := common.Unmarshal(body["messages"], &messages); err != nil {
		return fmt.Errorf("Claude CLI messages must be an array")
	}
	for i, message := range messages {
		if message == nil {
			return fmt.Errorf("Claude CLI messages must be objects")
		}
		blocks, err := cliContentBlocks(message["content"], "text")
		if err != nil {
			return err
		}
		if !nativeCLI {
			stripCacheBreakpoints(blocks)
		}
		if !hasCache && i == len(messages)-1 && len(blocks) > 0 {
			last := blocks[len(blocks)-1]
			switch gjson.ParseBytes(last["type"]).String() {
			case "text", "image", "document", "tool_result", "tool_use":
				last["cache_control"] = cacheControl
			}
		}
		if blocks != nil {
			message["content"] = cliField(blocks)
		}
	}
	body["messages"] = cliField(messages)
	if raw, exists := body["tools"]; exists && string(raw) != "null" {
		var tools []cliObject
		if err := common.Unmarshal(raw, &tools); err != nil {
			return fmt.Errorf("Claude CLI tools must be an array")
		}
		for _, tool := range tools {
			if tool == nil {
				return fmt.Errorf("Claude CLI tools must be objects")
			}
			if !nativeCLI {
				delete(tool, "cache_control")
			}
			// The sample's deferred placeholder has only type/properties. It is
			// not a regular executable tool schema: preserve that minimal shape.
			if gjson.ParseBytes(tool["name"]).String() == "DeferredToolPlaceholder" && gjson.ParseBytes(tool["defer_loading"]).Bool() {
				continue
			}
			// Server tools do not have input_schema; do not turn them into functions.
			if schemaRaw, ok := tool["input_schema"]; ok {
				var schema cliObject
				if err := common.Unmarshal(schemaRaw, &schema); err != nil || schema == nil {
					return fmt.Errorf("Claude CLI tool input_schema must be an object")
				}
				if _, ok := schema["$schema"]; !ok {
					schema["$schema"] = cliField("https://json-schema.org/draft/2020-12/schema")
				}
				if gjson.ParseBytes(schema["type"]).String() == "object" {
					if _, ok := schema["properties"]; !ok {
						schema["properties"] = json.RawMessage(`{}`)
					}
					if _, ok := schema["additionalProperties"]; !ok {
						schema["additionalProperties"] = json.RawMessage(`false`)
					}
				}
				tool["input_schema"] = cliField(schema)
			}
		}
		// Match the captured CLI's built-in order without advertising tools the
		// caller cannot execute. Unknown/MCP tools retain their relative order.
		order := []string{"Agent", "Bash", "Edit", "Glob", "Grep", "ListAgents", "Read", "ReportFindings", "ScheduleWakeup", "Skill", "ToolSearch", "Workflow", "DeferredToolPlaceholder", "Write"}
		if version != nil {
			order = version.ToolOrder
		}
		rank := make(map[string]int, len(order))
		for i, name := range order {
			rank[name] = i - len(order)
		}
		if !hasCache {
			sort.SliceStable(tools, func(i, j int) bool {
				return rank[gjson.ParseBytes(tools[i]["name"]).String()] < rank[gjson.ParseBytes(tools[j]["name"]).String()]
			})
		}
		body["tools"] = cliField(tools)
	}
	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = json.RawMessage(`128000`)
		if version != nil {
			body["max_tokens"] = cliField(version.MaxTokens)
		}
	}
	// Under a versioned profile a non-native request carries the captured
	// client's thinking, effort and output limit, but only for the model it
	// was captured with: the capture says nothing about other models' limits.
	// A caller's own output limit and effort are kept (caller intent).
	if version != nil && !nativeCLI && version.CapturedModel != "" &&
		strings.HasPrefix(gjson.ParseBytes(body["model"]).String(), version.CapturedModel) {
		if !callerMaxTokens && version.MaxTokens > 0 {
			body["max_tokens"] = cliField(version.MaxTokens)
		}
		// Anthropic rejects thinking with a forced tool choice or changed
		// sampling; the captured client sends neither.
		toolChoice := gjson.GetBytes(body["tool_choice"], "type").String()
		if len(version.Thinking) > 0 && toolChoice != "any" && toolChoice != "tool" {
			body["thinking"] = version.Thinking
			delete(body, "temperature")
			delete(body, "top_p")
			delete(body, "top_k")
		}
		if len(version.OutputConfig) > 0 {
			// Output settings are caller intent (effort, format): the captured
			// values only fill keys the caller did not set.
			var outputConfig, captured cliObject
			_ = common.Unmarshal(body["output_config"], &outputConfig)
			if err := common.Unmarshal(version.OutputConfig, &captured); err != nil {
				return fmt.Errorf("CLI version profile output_config must be an object")
			}
			if outputConfig == nil {
				outputConfig = cliObject{}
			}
			if _, ok := outputConfig["effort"]; !ok && callerEffort != "" {
				outputConfig["effort"] = cliField(callerEffort)
			}
			for key, value := range captured {
				if _, ok := outputConfig[key]; !ok {
					outputConfig[key] = value
				}
			}
			body["output_config"] = cliField(outputConfig)
		}
	}
	// Adaptive thinking is model-dependent. Do not impose an Opus 5.5-only
	// setting on older models simply because they share the Claude wire format.
	if version == nil && strings.HasPrefix(gjson.ParseBytes(body["model"]).String(), "claude-opus-5-5") {
		if _, ok := body["thinking"]; !ok {
			body["thinking"] = json.RawMessage(`{"type":"adaptive","display":"omitted"}`)
		}
		if _, ok := body["output_config"]; !ok {
			body["output_config"] = json.RawMessage(`{"effort":"medium"}`)
		}
	}
	thinkingType := gjson.GetBytes(body["thinking"], "type").String()
	if _, ok := body["context_management"]; !ok && (version == nil || version.ContextManagement) && (thinkingType == "enabled" || thinkingType == "adaptive") {
		body["context_management"] = json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
	}
	var metadata cliObject
	if raw := body["metadata"]; len(raw) > 0 {
		if err := common.Unmarshal(raw, &metadata); err != nil {
			return fmt.Errorf("Claude CLI metadata must be an object")
		}
	}
	if metadata == nil {
		metadata = cliObject{}
	}
	// The capture's account_uuid is empty (API-key mode). Authentication stays
	// with the channel; never invent an upstream account or replay one from disk.
	metadata["user_id"] = cliField(string(cliField(map[string]string{
		"device_id": identity.device, "account_uuid": "", "session_id": identity.session,
	})))
	if version != nil && version.LegacyUserID {
		metadata["user_id"] = cliField("user_" + identity.device + "_account__session_" + identity.session)
	}
	body["metadata"] = cliField(metadata)
	return nil
}

func shapeCodexBody(body cliObject, identity cliIdentity, versions ...*service.CLIVersionProfile) error {
	var version *service.CLIVersionProfile
	if len(versions) > 0 {
		version = versions[0]
	}
	toolsInInput := version == nil || version.ToolsInInput
	var input []cliObject
	if common.GetJsonType(body["input"]) == "string" {
		input = []cliObject{{"role": cliField("user"), "content": body["input"]}}
	} else if len(body["input"]) > 0 {
		if err := common.Unmarshal(body["input"], &input); err != nil {
			return fmt.Errorf("Codex CLI input must be text or an array")
		}
	}
	if !toolsInInput {
		for i, item := range input {
			if gjson.ParseBytes(item["type"]).String() != "additional_tools" {
				continue
			}
			if i != 0 || len(body["tools"]) > 0 {
				return fmt.Errorf("selected CLI version cannot represent historical additional_tools changes; select a newer CLI profile")
			}
			body["tools"] = item["tools"]
		}
		if len(input) > 0 && gjson.ParseBytes(input[0]["type"]).String() == "additional_tools" {
			input = input[1:]
		}
	}
	if instructions := body["instructions"]; toolsInInput && common.GetJsonType(instructions) == "string" && gjson.ParseBytes(instructions).String() != "" {
		input = append([]cliObject{{"role": cliField("developer"), "content": instructions,
			"id": cliField("msg_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity.turn+":instructions")).String())}}, input...)
		// Keep the empty field for Codex backends that require its presence.
		body["instructions"] = cliField("")
	}
	// A versioned client that carries instructions as a developer message has
	// no instructions key at all; neither the empty field above nor the Codex
	// adaptor's default may add one its capture does not have.
	if version != nil && toolsInInput && !slices.Contains(version.BodyOrder[""], "instructions") &&
		common.GetJsonType(body["instructions"]) == "string" && gjson.ParseBytes(body["instructions"]).String() == "" {
		delete(body, "instructions")
	}
	for i, item := range input {
		if item == nil {
			return fmt.Errorf("Codex CLI input items must be objects")
		}
		// Codex has no cache breakpoints; its cache identity is prompt_cache_key.
		delete(item, "cache_control")
		role := gjson.ParseBytes(item["role"]).String()
		kind := gjson.ParseBytes(item["type"]).String()
		if kind == "additional_tools" {
			var tools []cliObject
			if err := common.Unmarshal(item["tools"], &tools); err != nil {
				return fmt.Errorf("Codex CLI tools must be an array")
			}
			if err := normalizeCodexNamespaceDescriptions(tools); err != nil {
				return err
			}
			item["tools"] = cliField(tools)
		}
		if common.GetJsonType(item["output"]) == "array" {
			var output []cliObject
			if err := common.Unmarshal(item["output"], &output); err == nil {
				stripCacheBreakpoints(output)
				item["output"] = cliField(output)
			}
		}
		if kind != "" && kind != "message" {
			continue // Keep tool calls/results, reasoning, and tool snapshot positions.
		}
		if role == "" {
			continue
		}
		if role == "system" {
			item["role"] = cliField("developer")
		}
		textType := "input_text"
		if role == "assistant" {
			textType = "output_text"
		}
		blocks, err := cliContentBlocks(item["content"], textType)
		if err != nil {
			return err
		}
		stripCacheBreakpoints(blocks)
		if blocks != nil {
			item["content"] = cliField(blocks)
		}
		item["type"] = cliField("message")
		if _, ok := item["id"]; !ok && (version == nil || version.MessageIDs) {
			item["id"] = cliField("msg_" + isolatedCLIUUID(identity.turn, fmt.Sprintf("message:%d", i), 7, identity.startedAt))
		}
	}
	if raw, ok := body["tools"]; ok && string(raw) != "null" {
		var tools []cliObject
		if err := common.Unmarshal(raw, &tools); err != nil {
			return fmt.Errorf("Codex CLI tools must be an array")
		}
		if err := normalizeCodexNamespaceDescriptions(tools); err != nil {
			return err
		}
		if err := shapeCodexTools(tools); err != nil {
			return err
		}
		if len(tools) > 0 && toolsInInput {
			input = append([]cliObject{{"type": cliField("additional_tools"), "role": cliField("developer"),
				"id": cliField("at_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity.turn+":tools")).String()), "tools": cliField(tools)}}, input...)
		}
		if toolsInInput {
			delete(body, "tools")
		} else {
			body["tools"] = cliField(tools)
		}
	}
	if input == nil {
		input = []cliObject{}
	}
	body["input"] = cliField(input)
	defaults := cliObject{
		"tool_choice": json.RawMessage(`"auto"`), "parallel_tool_calls": json.RawMessage(`false`),
		"store": json.RawMessage(`false`), "include": json.RawMessage(`["reasoning.encrypted_content"]`),
		"text": json.RawMessage(`{"verbosity":"low"}`),
	}
	if version == nil && strings.HasPrefix(gjson.ParseBytes(body["model"]).String(), "gpt-6-astra") {
		defaults["reasoning"] = json.RawMessage(`{"effort":"max","context":"all_turns"}`)
	}
	if version != nil {
		defaults["parallel_tool_calls"] = cliField(version.ParallelToolCalls)
		if len(version.TextDefault) > 0 && string(version.TextDefault) != "null" {
			defaults["text"] = version.TextDefault
		} else {
			delete(defaults, "text")
		}
	}
	for key, value := range defaults {
		if _, ok := body[key]; !ok {
			body[key] = value
		}
	}
	// service_tier=priority is a paid routing choice, not a structural default.
	// Keep explicit tier, max_output_tokens, and reasoning settings unchanged.
	// Cache policy belongs to the profile: Codex sends only the session's
	// prompt_cache_key, never a caller's retention, options or breakpoints.
	body["prompt_cache_key"] = cliField(identity.session)
	delete(body, "prompt_cache_retention")
	delete(body, "prompt_cache_options")
	delete(body, "cache_control")
	// Preserve the capture's field set and JSON scalar types, including the
	// nested JSON string. These are client metadata, not gateway permissions.
	// Identifiers and the turn timestamp always come from this isolated request.
	turnMetadata := map[string]any{
		"installation_id": identity.installation, "session_id": identity.session,
		"thread_id": identity.session, "turn_id": identity.turn, "root_turn_id": identity.turn,
		"window_id": identity.window, "request_kind": "turn",
		"context_window_id": identity.contextWindow, "turn_started_at_unix_ms": identity.startedAt,
		"agent_name": "/root", "window_number": 0, "thread_source": "user", "turn_trigger": "exec",
		"sandbox": "windows_elevated", "sandbox_mode": "workspace-write",
		"auto_review_enabled": false, "node_repl_auto_review_required": true,
		"node_repl_disabled": false, "analytics_enabled": true,
		"model": gjson.ParseBytes(body["model"]).String(), "reasoning_effort": gjson.GetBytes(body["reasoning"], "effort").String(),
	}
	if version != nil {
		selected := make(map[string]any)
		for _, key := range version.BodyOrder["turn_metadata"] {
			if value, ok := version.TurnDefaults[key]; ok {
				selected[key] = value
			} else if value, ok := turnMetadata[key]; ok {
				selected[key] = value
			}
		}
		turnMetadata = selected
	}
	body["client_metadata"] = cliField(map[string]string{
		"x-codex-installation-id": identity.installation, "session_id": identity.session,
		"thread_id": identity.session, "turn_id": identity.turn, "root_turn_id": identity.turn,
		"x-codex-window-id": identity.window, "x-codex-turn-metadata": string(cliField(turnMetadata)),
	})
	return nil
}

// Normalize only namespace labels, including historical additional_tools.
// Do not apply function defaults or flatten wrappers in historical snapshots.
func normalizeCodexNamespaceDescriptions(tools []cliObject) error {
	for _, tool := range tools {
		if tool == nil {
			return fmt.Errorf("Codex CLI tools must be objects")
		}
		if gjson.ParseBytes(tool["type"]).String() != "namespace" {
			continue
		}
		// Native Codex can send an empty namespace description, which some
		// Responses upstreams reject as missing. Keep nonempty caller text.
		description, exists := tool["description"]
		parsed := gjson.ParseBytes(description)
		if !exists || (parsed.Type == gjson.String && strings.TrimSpace(parsed.String()) == "") {
			tool["description"] = cliField("Tools in the " + gjson.ParseBytes(tool["name"]).String() + " namespace.")
		}
		var nested []cliObject
		if err := common.Unmarshal(tool["tools"], &nested); err != nil {
			return fmt.Errorf("Codex CLI namespace tools must be an array")
		}
		if err := normalizeCodexNamespaceDescriptions(nested); err != nil {
			return err
		}
		tool["tools"] = cliField(nested)
	}
	return nil
}

func shapeCodexTools(tools []cliObject) error {
	for _, tool := range tools {
		if tool == nil {
			return fmt.Errorf("Codex CLI tools must be objects")
		}
		switch gjson.ParseBytes(tool["type"]).String() {
		case "namespace":
			var nested []cliObject
			if err := common.Unmarshal(tool["tools"], &nested); err != nil {
				return fmt.Errorf("Codex CLI namespace tools must be an array")
			}
			if err := shapeCodexTools(nested); err != nil {
				return err
			}
			tool["tools"] = cliField(nested)
		case "function":
			// Also accept Chat-style wrappers after a custom passthrough route.
			if raw, ok := tool["function"]; ok {
				var function cliObject
				if err := common.Unmarshal(raw, &function); err != nil {
					return fmt.Errorf("Codex CLI function must be an object")
				}
				for key, value := range function {
					tool[key] = value
				}
				delete(tool, "function")
			}
			if _, ok := tool["strict"]; !ok {
				tool["strict"] = json.RawMessage(`false`)
			}
		}
		// After flattening, so a Chat-style wrapper cannot carry one through.
		delete(tool, "cache_control")
	}
	return nil
}
