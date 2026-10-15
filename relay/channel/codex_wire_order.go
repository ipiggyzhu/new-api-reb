package channel

import (
	"bytes"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// Captured Codex CLI 0.156.1 field order. Native metadata and user-defined
// objects retain their source order: even the capture uses different
// client_metadata order on successive turns.
// Unknown fields are retained, and array order and scalar values never change.
var codexRequestFieldOrder = []string{"model", "input", "tool_choice", "parallel_tool_calls", "reasoning", "store", "stream", "include", "service_tier", "prompt_cache_key", "text", "client_metadata"}
var codexTurnFieldOrder = []string{"installation_id", "session_id", "thread_id", "agent_name", "turn_id", "window_id", "window_number", "context_window_id", "request_kind", "root_turn_id", "thread_source", "turn_trigger", "sandbox", "sandbox_mode", "auto_review_enabled", "node_repl_auto_review_required", "node_repl_disabled", "turn_started_at_unix_ms", "analytics_enabled", "model", "reasoning_effort"}

func orderCodexJSON(current, reference gjson.Result, field string) []byte {
	if field == "x-codex-turn-metadata" && current.Type == gjson.String {
		ordered := orderCodexJSON(gjson.Parse(current.String()), gjson.Parse(reference.String()), "turn_metadata")
		encoded, _ := common.Marshal(string(ordered))
		return encoded
	}
	if current.IsArray() {
		items, originals := current.Array(), reference.Array()
		var out bytes.Buffer
		out.WriteByte('[')
		for i, item := range items {
			if i > 0 {
				out.WriteByte(',')
			}
			var original gjson.Result
			// Body shaping can prepend instructions/additional_tools. Match old
			// items by ID, or by their unchanged relative position after inserts.
			index := i
			if field == "input" && len(items) >= len(originals) {
				index -= len(items) - len(originals)
			}
			if index >= 0 && index < len(originals) {
				original = originals[index]
			}
			if field == "input" && item.Get("id").Exists() && original.Get("id").String() != item.Get("id").String() {
				for _, candidate := range originals {
					if candidate.Get("id").String() == item.Get("id").String() {
						original = candidate
						break
					}
				}
			}
			out.Write(orderCodexJSON(item, original, field))
		}
		out.WriteByte(']')
		return out.Bytes()
	}
	if !current.IsObject() {
		return []byte(current.Raw)
	}
	var order []string
	switch field {
	case "":
		order = codexRequestFieldOrder
	case "client_metadata":
		if !reference.Get("x-codex-installation-id").Exists() {
			order = []string{"x-codex-installation-id", "session_id", "x-codex-window-id", "thread_id", "turn_id", "root_turn_id", "x-codex-turn-metadata"}
		}
	case "turn_metadata":
		order = codexTurnFieldOrder
	case "reasoning":
		order = []string{"effort", "context", "summary"}
	case "input":
		if current.Get("type").Exists() {
			order = []string{"type", "id", "status", "role", "phase", "content", "tools", "summary", "encrypted_content", "call_id", "name", "arguments", "input", "output"}
		}
	case "content", "output":
		if current.Get("type").String() == "input_text" || current.Get("type").String() == "output_text" {
			order = []string{"type", "text"}
		}
	case "tools":
		order = []string{"type", "name", "description", "strict", "parameters", "format", "tools"}
	}
	reference.ForEach(func(key, _ gjson.Result) bool {
		order = append(order, key.String())
		return true
	})
	current.ForEach(func(key, _ gjson.Result) bool {
		order = append(order, key.String())
		return true
	})
	values, originals := current.Map(), reference.Map()
	var out bytes.Buffer
	out.WriteByte('{')
	for _, key := range order {
		value, exists := values[key]
		if !exists {
			continue
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		encodedKey, _ := common.Marshal(key)
		out.Write(encodedKey)
		out.WriteByte(':')
		out.Write(orderCodexJSON(value, originals[key], key))
		delete(values, key)
	}
	out.WriteByte('}')
	return out.Bytes()
}
