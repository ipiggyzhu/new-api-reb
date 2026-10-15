package channel

import (
	"bytes"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/tidwall/gjson"
)

// Apply only observed protocol object orders. Caller schema properties, unknown
// objects, scalar values and array order remain intact. Metadata strings are
// encoded JSON, not ordinary objects on the wire.
func orderVersionedCLIJSON(current, reference gjson.Result, field string, profile *service.CLIVersionProfile) []byte {
	// Schemas and opaque payloads can themselves have keys named input/tools.
	// Restore their source order without interpreting those names as protocol.
	switch field {
	case "properties", "parameters", "$defs", "definitions", "patternProperties", "arguments", "output", "format":
		profile = nil
	}
	if profile != nil && current.Type == gjson.String && (field == "x-codex-turn-metadata" || field == "user_id") && gjson.Parse(current.String()).IsObject() {
		nested := field
		if field == "x-codex-turn-metadata" {
			nested = "turn_metadata"
		}
		ordered := orderVersionedCLIJSON(gjson.Parse(current.String()), gjson.Parse(reference.String()), nested, profile)
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
			index := i
			if field == "input" && len(items) >= len(originals) {
				index -= len(items) - len(originals)
			}
			if index >= 0 && index < len(originals) {
				original = originals[index]
			}
			matchKey := ""
			if field == "input" {
				matchKey = "id"
			} else if field == "tools" {
				matchKey = "name"
			}
			if matchKey != "" && item.Get(matchKey).Exists() {
				for _, candidate := range originals {
					if candidate.Get(matchKey).String() == item.Get(matchKey).String() {
						original = candidate
						break
					}
				}
			}
			out.Write(orderVersionedCLIJSON(item, original, field, profile))
		}
		out.WriteByte(']')
		return out.Bytes()
	}
	if !current.IsObject() {
		return []byte(current.Raw)
	}
	// Never append to a catalog slice: requests share the embedded profiles.
	var order []string
	if profile != nil {
		order = append(order, profile.BodyOrder[field]...)
		if typed, ok := profile.BodyOrder[field+"#"+current.Get("type").String()]; ok {
			order = append([]string(nil), typed...)
		}
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
		value, ok := values[key]
		if !ok {
			continue
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		encoded, _ := common.Marshal(key)
		out.Write(encoded)
		out.WriteByte(':')
		out.Write(orderVersionedCLIJSON(value, originals[key], key, profile))
		delete(values, key)
	}
	out.WriteByte('}')
	return out.Bytes()
}
