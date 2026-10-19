package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// rectifyClaudeThinkingHistory retries a Messages request on the same channel
// after its replayed thinking was rejected (isClaudeThinkingHistoryRejected),
// first with the thinking rewritten as text, then with the tool blocks as well.
// Switching channels would not help: every channel behind another account
// rejects the same blocks. It only runs while nothing has reached the client,
// which holds in practice because these 400s return long before the first
// keepalive ping. Anything other than such a rejection is returned untouched.
func rectifyClaudeThinkingHistory(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor, jsonData []byte, usage any, newAPIError *types.NewAPIError) (any, *types.NewAPIError) {
	lastSent := jsonData
	for _, withTools := range []bool{false, true} {
		if !isClaudeThinkingHistoryRejected(newAPIError) || c.Writer.Written() || c.Request.Context().Err() != nil {
			break
		}
		downgraded := downgradeClaudeThinkingHistory(jsonData, withTools)
		if bytes.Equal(downgraded, lastSent) {
			continue
		}
		logger.LogWarn(c, fmt.Sprintf("claude thinking history rejected by channel #%d (%s); retrying with thinking as text, tool blocks as text: %t",
			info.ChannelId, newAPIError.Error(), withTools))
		body, closer, apiErr := buildUpstreamRequestBody(c, info, downgraded, true, "requestBody")
		if apiErr != nil {
			return nil, apiErr
		}
		info.ResetResponseState()
		usage, newAPIError = doUpstreamRoundTrip(c, info, adaptor, body, true)
		_ = closer.Close()
		lastSent = downgraded
	}
	return usage, newAPIError
}

// isClaudeThinkingHistoryRejected reports a 400 in which Anthropic refused the
// thinking blocks replayed from earlier turns. A thinking signature is bound to
// the account and conversation that produced it, so a conversation that moved
// to another channel — failover, affinity expiry, a key rotation — carries
// blocks the new upstream cannot verify. Every channel behind a different
// account rejects them the same way, which is why switching channels does not
// help and rewriting the history does.
//
// Matched on the documented wordings, all of which name thinking:
//   - "Invalid `signature` in `thinking` block" (also the block_binding variant)
//     and "thinking.signature: Field required"
//   - "thinking or redacted_thinking blocks ... cannot be modified"
//   - "Expected `thinking` or `redacted_thinking`, but found `tool_use`"
//   - "a final `assistant` message must start with a thinking block"
//   - "each thinking block must contain thinking"
//
// A bare "signature" is not enough (an AWS SigV4 failure says it too), and a
// bare "expected" would catch "thinking.budget_tokens: expected integer", a
// malformed request that must fail rather than quietly lose its thinking.
func isClaudeThinkingHistoryRejected(err *types.NewAPIError) bool {
	if err == nil || err.StatusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "thinking") {
		return false
	}
	return strings.Contains(message, "signature") ||
		strings.Contains(message, "cannot be modified") ||
		(strings.Contains(message, "expected") && strings.Contains(message, "redacted_thinking")) ||
		strings.Contains(message, "must start with a thinking block") ||
		strings.Contains(message, "thinking block must contain")
}

type claudeTextBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// downgradeClaudeThinkingHistory rewrites a Messages request whose replayed
// thinking was rejected so it can be sent again:
//   - each thinking block becomes a text block carrying the same reasoning, and
//     one with no visible reasoning (display "omitted") is dropped;
//   - redacted_thinking blocks are dropped, they are opaque by design;
//   - an enabled or adaptive top-level thinking is removed, because a thinking
//     turn requires the last assistant message to open with the thinking block
//     that was just converted. Models that cannot turn thinking off treat the
//     omitted field as adaptive. Context-management edits of type
//     clear_thinking_* go with it; they are rejected without thinking.
//
// withTools also converts tool_use and tool_result blocks to text. In a tool
// loop the upstream still demands that the final assistant turn open with a
// thinking block, and only removing the tool semantics satisfies that. It
// changes how the model sees its own earlier calls, so it is the second
// attempt, not the first. A converted block keeps its cache_control, so the
// prompt-cache breakpoint a client placed on the last tool_result survives.
//
// A message left with no blocks gets a placeholder, since empty content is
// rejected. The body is returned unchanged when nothing applies, or when it is
// not the JSON object it should be.
func downgradeClaudeThinkingHistory(body []byte, withTools bool) []byte {
	out := body
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	if thinkingType == "enabled" || thinkingType == "adaptive" {
		updated, err := sjson.DeleteBytes(out, "thinking")
		if err != nil {
			return body
		}
		out = updated
		edits := gjson.GetBytes(out, "context_management.edits")
		if edits.IsArray() {
			kept := make([]json.RawMessage, 0, len(edits.Array()))
			for _, edit := range edits.Array() {
				if !strings.HasPrefix(edit.Get("type").String(), "clear_thinking_") {
					kept = append(kept, json.RawMessage(edit.Raw))
				}
			}
			if len(kept) == 0 && len(gjson.GetBytes(out, "context_management").Map()) == 1 {
				out, err = sjson.DeleteBytes(out, "context_management")
			} else if len(kept) == 0 {
				out, err = sjson.DeleteBytes(out, "context_management.edits")
			} else if len(kept) != len(edits.Array()) {
				var keptJSON []byte
				if keptJSON, err = common.Marshal(kept); err == nil {
					out, err = sjson.SetRawBytes(out, "context_management.edits", keptJSON)
				}
			}
			if err != nil {
				return body
			}
		}
	}

	messages := gjson.GetBytes(out, "messages")
	if !messages.IsArray() {
		return out
	}
	rewritten := make([]json.RawMessage, 0, len(messages.Array()))
	messagesChanged := false
	for _, message := range messages.Array() {
		content := message.Get("content")
		if !content.IsArray() {
			rewritten = append(rewritten, json.RawMessage(message.Raw))
			continue
		}
		blocks := make([]any, 0, len(content.Array()))
		changed := false
		for _, block := range content.Array() {
			blockType := block.Get("type").String()
			if blockType == "thinking" || blockType == "redacted_thinking" {
				changed = true
				if reasoning := block.Get("thinking").String(); blockType == "thinking" && strings.TrimSpace(reasoning) != "" {
					blocks = append(blocks, claudeTextBlock{Type: "text", Text: reasoning})
				}
				continue
			}
			if !withTools || (blockType != "tool_use" && blockType != "tool_result") {
				blocks = append(blocks, json.RawMessage(block.Raw))
				continue
			}
			changed = true
			cacheControl := json.RawMessage(block.Get("cache_control").Raw)
			if blockType == "tool_use" {
				text := "(tool_use) name=" + block.Get("name").String() + " id=" + block.Get("id").String()
				if input := block.Get("input"); input.Exists() {
					text += " input=" + input.Raw
				}
				blocks = append(blocks, claudeTextBlock{Type: "text", Text: text, CacheControl: cacheControl})
				continue
			}
			// tool_result: its text joins one header block; an image or document
			// stays a block of its own instead of becoming base64 prose.
			text := "(tool_result) tool_use_id=" + block.Get("tool_use_id").String()
			if block.Get("is_error").Bool() {
				text += " is_error=true"
			}
			var media []any
			result := block.Get("content")
			var nestedBlocks []gjson.Result
			if result.IsArray() {
				nestedBlocks = result.Array()
			} else if strings.TrimSpace(result.String()) != "" {
				text += "\n" + result.String()
			}
			for _, nested := range nestedBlocks {
				switch nested.Get("type").String() {
				case "text":
					if strings.TrimSpace(nested.Get("text").String()) != "" {
						text += "\n" + nested.Get("text").String()
					}
				case "image", "document":
					media = append(media, json.RawMessage(nested.Raw))
				default:
					text += "\n" + nested.Raw
				}
			}
			blocks = append(blocks, claudeTextBlock{Type: "text", Text: text, CacheControl: cacheControl})
			blocks = append(blocks, media...)
		}
		if !changed {
			rewritten = append(rewritten, json.RawMessage(message.Raw))
			continue
		}
		if len(blocks) == 0 {
			placeholder := "(content removed)"
			if message.Get("role").String() == "assistant" {
				placeholder = "(assistant content removed)"
			}
			blocks = append(blocks, claudeTextBlock{Type: "text", Text: placeholder})
		}
		blocksJSON, err := common.Marshal(blocks)
		if err != nil {
			return body
		}
		updated, err := sjson.SetRawBytes([]byte(message.Raw), "content", blocksJSON)
		if err != nil {
			return body
		}
		rewritten = append(rewritten, updated)
		messagesChanged = true
	}
	if !messagesChanged {
		return out
	}
	messagesJSON, err := common.Marshal(rewritten)
	if err != nil {
		return body
	}
	if out, err = sjson.SetRawBytes(out, "messages", messagesJSON); err != nil {
		return body
	}
	return out
}
