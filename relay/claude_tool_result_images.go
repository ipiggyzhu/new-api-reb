package relay

import (
	"bytes"
	"fmt"

	"github.com/QuantumNous/new-api/dto"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeOmittedToolImageNote = "[An image returned here was omitted by the gateway to keep this long conversation deliverable. Run the tool again if you need to see it.]"

// rewriteClaudeToolResultImages applies dto.ChannelSettings.ClaudeToolResultImages
// to a Messages request body. It returns the body and how many images were
// moved and how many were replaced; the body is returned untouched when
// nothing applies or it is not the shape it should be.
//
// Only user messages are touched, and inside them only image blocks that sit
// directly in a tool_result's content array. A moved image is inserted right
// after the message's last tool_result block: tool results must stay first in
// the message, and the image keeps its place before whatever the user typed.
// The tool_result keeps a short text note where the image was, so it is never
// left empty and the model can still tell which call produced which image.
//
// Blocks are spliced as raw JSON rather than re-marshalled, so every byte the
// rewrite does not own is sent exactly as it arrived. The rewrite is a pure
// function of the message, which keeps the upstream prompt cache prefix stable
// from one turn to the next.
func rewriteClaudeToolResultImages(body []byte, mode string) ([]byte, int, int) {
	if mode != dto.ClaudeToolResultImagesHoist && mode != dto.ClaudeToolResultImagesPlaceholder {
		return body, 0, 0
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, 0, 0
	}
	list := messages.Array()
	// The turn being answered is the last user message. Clients may append
	// system-role messages after it, so it is not always the final entry.
	lastUser := -1
	for i, message := range list {
		if message.Get("role").String() == "user" {
			lastUser = i
		}
	}
	out := body
	hoisted, replaced := 0, 0
	for i, message := range list {
		content := message.Get("content")
		if message.Get("role").String() != "user" || !content.IsArray() {
			continue
		}
		// "placeholder" still hoists in the last user message: that is the turn
		// the model is answering, so its images must survive.
		hoist := mode == dto.ClaudeToolResultImagesHoist || i == lastUser

		blocks := content.Array()
		rewritten := make([]string, 0, len(blocks)+2)
		var moved []string
		lastToolResult := -1
		changed := false
		for _, block := range blocks {
			inner := block.Get("content")
			if block.Get("type").String() != "tool_result" || !inner.IsArray() {
				rewritten = append(rewritten, block.Raw)
				continue
			}
			lastToolResult = len(rewritten)
			items := make([]string, 0, len(inner.Array()))
			touched := false
			for _, item := range inner.Array() {
				if item.Get("type").String() != "image" {
					items = append(items, item.Raw)
					continue
				}
				touched = true
				if hoist {
					moved = append(moved, item.Raw)
					items = append(items, claudeTextBlockJSON(fmt.Sprintf("[Image %d returned by this tool follows the tool results in this message.]", len(moved))))
				} else {
					replaced++
					items = append(items, claudeTextBlockJSON(claudeOmittedToolImageNote))
				}
			}
			if !touched {
				rewritten = append(rewritten, block.Raw)
				continue
			}
			updated, err := sjson.SetRawBytes([]byte(block.Raw), "content", joinJSONArray(items))
			if err != nil {
				return body, 0, 0
			}
			rewritten = append(rewritten, string(updated))
			changed = true
		}
		if !changed {
			continue
		}
		if len(moved) > 0 {
			// A cache breakpoint on the last tool_result used to cover its
			// images; carry it to the last moved image so it still does.
			if cacheControl := gjson.Get(rewritten[lastToolResult], "cache_control"); cacheControl.Exists() &&
				!gjson.Get(moved[len(moved)-1], "cache_control").Exists() {
				stripped, err := sjson.Delete(rewritten[lastToolResult], "cache_control")
				if err != nil {
					return body, 0, 0
				}
				marked, err := sjson.SetRaw(moved[len(moved)-1], "cache_control", cacheControl.Raw)
				if err != nil {
					return body, 0, 0
				}
				rewritten[lastToolResult] = stripped
				moved[len(moved)-1] = marked
			}
			tail := append([]string{}, rewritten[lastToolResult+1:]...)
			rewritten = append(append(rewritten[:lastToolResult+1], moved...), tail...)
			hoisted += len(moved)
		}
		updated, err := sjson.SetRawBytes(out, fmt.Sprintf("messages.%d.content", i), joinJSONArray(rewritten))
		if err != nil {
			return body, 0, 0
		}
		out = updated
	}
	return out, hoisted, replaced
}

func claudeTextBlockJSON(text string) string {
	block, err := sjson.Set(`{"type":"text"}`, "text", text)
	if err != nil {
		return `{"type":"text","text":"[image]"}`
	}
	return block
}

func joinJSONArray(items []string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(item)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}
