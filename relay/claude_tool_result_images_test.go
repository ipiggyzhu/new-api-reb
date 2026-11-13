package relay

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const toolImageBody = `{"model":"m","messages":[` +
	`{"role":"user","content":"take two screenshots"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}},{"type":"tool_use","id":"t2","name":"shot","input":{}}]},` +
	`{"role":"user","content":[` +
	`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA<&>"}},{"type":"text","text":"ok 1"}]},` +
	`{"type":"tool_result","tool_use_id":"t2","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBB"}}],"cache_control":{"type":"ephemeral"}},` +
	`{"type":"text","text":"typed by the user"}]},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"shot","input":{}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"CCC"}}]}]},` +
	`{"role":"system","content":"a reminder appended after the user turn"}` +
	`],"max_tokens":10}`

func blockTypes(t *testing.T, body []byte, path string) []string {
	t.Helper()
	var types []string
	for _, block := range gjson.GetBytes(body, path).Array() {
		types = append(types, block.Get("type").String())
	}
	return types
}

func TestRewriteClaudeToolResultImagesHoist(t *testing.T) {
	out, hoisted, replaced := rewriteClaudeToolResultImages([]byte(toolImageBody), dto.ClaudeToolResultImagesHoist)
	require.True(t, gjson.ValidBytes(out))
	require.Equal(t, 3, hoisted)
	require.Equal(t, 0, replaced)

	// tool results first, then the images in order, then what the user typed
	require.Equal(t, []string{"tool_result", "tool_result", "image", "image", "text"}, blockTypes(t, out, "messages.2.content"))
	require.Equal(t, "AAA<&>", gjson.GetBytes(out, "messages.2.content.2.source.data").String())
	require.Equal(t, "BBB", gjson.GetBytes(out, "messages.2.content.3.source.data").String())
	require.Equal(t, "typed by the user", gjson.GetBytes(out, "messages.2.content.4.text").String())

	// each tool_result keeps a note and its own text, and is never left empty
	require.Equal(t, []string{"text", "text"}, blockTypes(t, out, "messages.2.content.0.content"))
	require.Contains(t, gjson.GetBytes(out, "messages.2.content.0.content.0.text").String(), "Image 1")
	require.Equal(t, "ok 1", gjson.GetBytes(out, "messages.2.content.0.content.1.text").String())
	require.Contains(t, gjson.GetBytes(out, "messages.2.content.1.content.0.text").String(), "Image 2")

	// the cache breakpoint moves from the last tool_result to the last image
	require.False(t, gjson.GetBytes(out, "messages.2.content.1.cache_control").Exists())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.2.content.3.cache_control.type").String())

	require.Equal(t, []string{"tool_result", "image"}, blockTypes(t, out, "messages.4.content"))

	// untouched parts are byte-identical, including characters json.Marshal would escape
	require.True(t, strings.HasPrefix(string(out), toolImageBody[:strings.Index(toolImageBody, `{"role":"user","content":[`)]))
	require.True(t, strings.HasSuffix(string(out), `],"max_tokens":10}`))
	require.Contains(t, string(out), `"data":"AAA<&>"`)

	// a second pass finds nothing left to do, so the cached prefix stays stable
	again, hoistedAgain, _ := rewriteClaudeToolResultImages(out, dto.ClaudeToolResultImagesHoist)
	require.Equal(t, 0, hoistedAgain)
	require.Equal(t, string(out), string(again))
}

func TestRewriteClaudeToolResultImagesPlaceholder(t *testing.T) {
	out, hoisted, replaced := rewriteClaudeToolResultImages([]byte(toolImageBody), dto.ClaudeToolResultImagesPlaceholder)
	require.True(t, gjson.ValidBytes(out))
	require.Equal(t, 1, hoisted)
	require.Equal(t, 2, replaced)

	// earlier turns lose their images to a note and keep everything else in place
	require.Equal(t, []string{"tool_result", "tool_result", "text"}, blockTypes(t, out, "messages.2.content"))
	require.Equal(t, claudeOmittedToolImageNote, gjson.GetBytes(out, "messages.2.content.0.content.0.text").String())
	require.Equal(t, "ok 1", gjson.GetBytes(out, "messages.2.content.0.content.1.text").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.2.content.1.cache_control.type").String())
	require.NotContains(t, string(out), "AAA")
	require.NotContains(t, string(out), "BBB")

	// the turn being answered still shows its image
	require.Equal(t, []string{"tool_result", "image"}, blockTypes(t, out, "messages.4.content"))
	require.Equal(t, "CCC", gjson.GetBytes(out, "messages.4.content.1.source.data").String())
}

func TestRewriteClaudeToolResultImagesLeavesOtherBodiesAlone(t *testing.T) {
	for name, body := range map[string]string{
		"off":                   toolImageBody,
		"top-level user image":  `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A"}},{"type":"text","text":"hi"}]}]}`,
		"string tool result":    `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"plain"}]}]}`,
		"text only tool result": `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"x"}]}]}]}`,
		"assistant image":       `{"messages":[{"role":"assistant","content":[{"type":"tool_result","content":[{"type":"image"}]}]}]}`,
		"openai shape":          `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`,
		"no messages":           `{"input":"x"}`,
		"not json":              `nope`,
	} {
		mode := dto.ClaudeToolResultImagesHoist
		if name == "off" {
			mode = ""
		}
		out, hoisted, replaced := rewriteClaudeToolResultImages([]byte(body), mode)
		require.Equal(t, body, string(out), name)
		require.Zero(t, hoisted+replaced, name)
	}
	out, _, _ := rewriteClaudeToolResultImages([]byte(toolImageBody), "unknown-mode")
	require.Equal(t, toolImageBody, string(out))
}
