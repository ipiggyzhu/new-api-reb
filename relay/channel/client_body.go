package channel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
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
	device, installation, session, turn, window string
}

func requestCLIIdentity(c *gin.Context, info *relaycommon.RelayInfo, family string, body cliObject) cliIdentity {
	scope := fmt.Sprintf("cli:%s:%d:%d:%d:%d", family, info.ChannelId, info.ChannelMultiKeyIndex, info.UserId, info.TokenId)
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
	seed := ""
	for _, name := range []string{"session-id", "thread-id", "x-claude-code-session-id"} {
		if seed = c.Request.Header.Get(name); seed != "" {
			break
		}
	}
	if seed == "" {
		seed = gjson.ParseBytes(body["prompt_cache_key"]).String()
	}
	if seed == "" {
		seed = gjson.GetBytes(body["client_metadata"], "session_id").String()
	}
	if seed == "" {
		userID := gjson.GetBytes(body["metadata"], "user_id").String()
		seed = gjson.Get(userID, "session_id").String()
	}
	requestID := info.RequestId
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if seed == "" {
		seed = requestID
	}
	identity := cliIdentity{
		device:       common.GenerateHMACWithKey([]byte(deviceKey), scope+":device"),
		installation: uuid.NewSHA1(uuid.NameSpaceOID, []byte(common.GenerateHMACWithKey([]byte(deviceKey), scope+":installation"))).String(),
		session:      uuid.NewSHA1(uuid.NameSpaceOID, []byte(common.GenerateHMACWithKey([]byte(sessionKey), scope+":session:"+seed))).String(),
		turn:         uuid.NewSHA1(uuid.NameSpaceOID, []byte(common.GenerateHMACWithKey([]byte(sessionKey), scope+":turn:"+requestID))).String(),
		window:       uuid.NewSHA1(uuid.NameSpaceOID, []byte(common.GenerateHMACWithKey([]byte(sessionKey), scope+":window:"+seed))).String(),
	}
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
	var body cliObject
	if err := common.Unmarshal(data, &body); err != nil || body == nil {
		return requestURL, reader, nil, fmt.Errorf("CLI request body must be a JSON object")
	}
	identity := requestCLIIdentity(c, info, family, body)
	headers := make(http.Header)
	// Preserve the downstream streaming contract, including explicit false. CLI
	// defaults must not silently turn a synchronous request into an SSE response.
	body["stream"] = cliField(info.IsStream)
	headers.Set("Content-Type", "application/json")
	if info.IsStream {
		headers.Set("Accept", AcceptSSE)
	} else {
		headers.Set("Accept", AcceptJSON)
	}
	if claude {
		err = shapeClaudeCodeBody(body, identity)
		query := u.Query()
		query.Set("beta", "true")
		u.RawQuery = query.Encode()
		headers.Set("anthropic-beta", claudeCodeBeta)
		headers.Set("X-Claude-Code-Session-Id", identity.session)
	} else {
		err = shapeCodexBody(body, identity)
		headers.Set("x-codex-beta-features", "remote_compaction_v2")
		headers.Set("x-openai-internal-codex-responses-lite", "true")
		headers.Set("session-id", identity.session)
		headers.Set("thread-id", identity.session)
		headers.Set("x-client-request-id", identity.turn)
		headers.Set("x-codex-window-id", identity.window)
		headers.Set("x-codex-turn-metadata", gjson.GetBytes(body["client_metadata"], "x-codex-turn-metadata").String())
	}
	if err != nil {
		return requestURL, reader, nil, err
	}
	data, err = common.Marshal(body)
	if err != nil {
		return requestURL, reader, nil, fmt.Errorf("encode CLI request body: %w", err)
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

func shapeClaudeCodeBody(body cliObject, identity cliIdentity) error {
	system, err := cliContentBlocks(body["system"], "text")
	if err != nil {
		return err
	}
	// Preserve existing cache policy. Only add breakpoints when none were sent,
	// so the four-breakpoint provider limit and explicit TTLs stay intact.
	hasCache := false
	for _, key := range []string{"system", "tools", "messages", "cache_control"} {
		if bytes.Contains(body[key], []byte(`"cache_control"`)) || (key == "cache_control" && len(body[key]) > 0) {
			hasCache = true
		}
	}
	if !hasCache {
		for i := len(system) - 1; i >= 0 && i >= len(system)-2; i-- {
			if gjson.ParseBytes(system[i]["type"]).String() == "text" {
				system[i]["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
			}
		}
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
		if !hasCache && i == len(messages)-1 && len(blocks) > 0 {
			last := blocks[len(blocks)-1]
			switch gjson.ParseBytes(last["type"]).String() {
			case "text", "image", "document", "tool_result", "tool_use":
				last["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
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
	}
	// Adaptive thinking is model-dependent. Do not impose an Opus 5.5-only
	// setting on older models simply because they share the Claude wire format.
	if strings.HasPrefix(gjson.ParseBytes(body["model"]).String(), "claude-opus-5-5") {
		if _, ok := body["thinking"]; !ok {
			body["thinking"] = json.RawMessage(`{"type":"adaptive","display":"omitted"}`)
		}
		if _, ok := body["output_config"]; !ok {
			body["output_config"] = json.RawMessage(`{"effort":"medium"}`)
		}
	}
	thinkingType := gjson.GetBytes(body["thinking"], "type").String()
	if _, ok := body["context_management"]; !ok && (thinkingType == "enabled" || thinkingType == "adaptive") {
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
	body["metadata"] = cliField(metadata)
	return nil
}

func shapeCodexBody(body cliObject, identity cliIdentity) error {
	var input []cliObject
	if common.GetJsonType(body["input"]) == "string" {
		input = []cliObject{{"role": cliField("user"), "content": body["input"]}}
	} else if len(body["input"]) > 0 {
		if err := common.Unmarshal(body["input"], &input); err != nil {
			return fmt.Errorf("Codex CLI input must be text or an array")
		}
	}
	if instructions := body["instructions"]; common.GetJsonType(instructions) == "string" && gjson.ParseBytes(instructions).String() != "" {
		input = append([]cliObject{{"role": cliField("developer"), "content": instructions}}, input...)
		// Keep the empty field for Codex backends that require its presence.
		body["instructions"] = cliField("")
	}
	for i, item := range input {
		if item == nil {
			return fmt.Errorf("Codex CLI input items must be objects")
		}
		role := gjson.ParseBytes(item["role"]).String()
		kind := gjson.ParseBytes(item["type"]).String()
		if kind != "" && kind != "message" {
			continue // tool calls/results, reasoning, and additional_tools are opaque.
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
		if blocks != nil {
			item["content"] = cliField(blocks)
		}
		item["type"] = cliField("message")
		if _, ok := item["id"]; !ok {
			item["id"] = cliField("msg_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s:%d", identity.turn, i))).String())
		}
	}
	if raw, ok := body["tools"]; ok && string(raw) != "null" {
		var tools []cliObject
		if err := common.Unmarshal(raw, &tools); err != nil {
			return fmt.Errorf("Codex CLI tools must be an array")
		}
		if err := shapeCodexTools(tools); err != nil {
			return err
		}
		if len(tools) > 0 {
			input = append([]cliObject{{"type": cliField("additional_tools"), "role": cliField("developer"), "id": cliField("tools_" + identity.turn), "tools": cliField(tools)}}, input...)
		}
		delete(body, "tools")
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
	if strings.HasPrefix(gjson.ParseBytes(body["model"]).String(), "gpt-6-astra") {
		defaults["reasoning"] = json.RawMessage(`{"effort":"max","context":"all_turns"}`)
	}
	for key, value := range defaults {
		if _, ok := body[key]; !ok {
			body[key] = value
		}
	}
	// service_tier=priority is a paid routing choice, not a structural default.
	// Keep explicit tier, max_output_tokens, and reasoning settings unchanged.
	body["prompt_cache_key"] = cliField(identity.session)
	turnMetadata := map[string]string{
		"installation_id": identity.installation, "session_id": identity.session,
		"thread_id": identity.session, "turn_id": identity.turn, "root_turn_id": identity.turn,
		"window_id": identity.window, "request_kind": "turn",
		"model": gjson.ParseBytes(body["model"]).String(), "reasoning_effort": gjson.GetBytes(body["reasoning"], "effort").String(),
	}
	body["client_metadata"] = cliField(map[string]string{
		"x-codex-installation-id": identity.installation, "session_id": identity.session,
		"thread_id": identity.session, "turn_id": identity.turn, "root_turn_id": identity.turn,
		"x-codex-window-id": identity.window, "x-codex-turn-metadata": string(cliField(turnMetadata)),
	})
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
	}
	return nil
}
