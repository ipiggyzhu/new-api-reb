package privacy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrivacyStreamSplitReferencesAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "claude", "responses", "gemini", "arguments"} {
		t.Run(protocol, func(t *testing.T) {
			session := NewSession()
			secret := "fixture-中文-\"quote\\slash\nline"
			require.NoError(t, session.Add(secret))
			ref := session.Mask(secret)
			stream := &Stream{Session: session, RestoreTools: true}
			var wire, output strings.Builder
			chunks := []string{"before " + ref[:3], ref[3:19], ref[19:] + " after"}
			if protocol == "arguments" {
				chunks = []string{`{"password":"` + ref[:3], ref[3:19], ref[19:] + `"}`}
			}
			for _, chunk := range chunks {
				encoded, err := common.Marshal(chunk)
				require.NoError(t, err)
				var event string
				switch protocol {
				case "chat":
					event = fmt.Sprintf(`{"choices":[{"index":1,"delta":{"content":%s}}]}`, encoded)
				case "claude":
					event = fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, encoded)
				case "responses":
					event = fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":%s}`, encoded)
				case "gemini":
					event = fmt.Sprintf(`{"candidates":[{"index":0,"content":{"parts":[{"text":%s}]}}]}`, encoded)
				case "arguments":
					event = fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`, encoded)
				}
				wire.WriteString("data: " + event + "\r\n\r\n")
			}
			wire.WriteString("data: [DONE]\n\n")
			// A network write can end inside UTF-8, JSON escapes or an SSE delimiter.
			for _, b := range []byte(wire.String()) {
				out, err := stream.Write([]byte{b}, false)
				require.NoError(t, err)
				output.Write(out)
			}
			out, err := stream.Write(nil, true)
			require.NoError(t, err)
			output.Write(out)
			var content strings.Builder
			for _, event := range strings.Split(output.String(), "\n\n") {
				data := strings.TrimPrefix(event, "data: ")
				if !gjson.Valid(data) {
					continue
				}
				path := map[string]string{"chat": "choices.0.delta.content", "claude": "delta.text", "responses": "delta", "gemini": "candidates.0.content.parts.0.text", "arguments": "choices.0.delta.tool_calls.0.function.arguments"}[protocol]
				content.WriteString(gjson.Get(data, path).String())
			}
			if protocol == "arguments" {
				require.True(t, gjson.Valid(content.String()), content.String())
				assert.Equal(t, secret, gjson.Get(content.String(), "password").String())
			} else {
				assert.Equal(t, "before "+secret+" after", content.String())
			}
			assert.Contains(t, output.String(), "data: [DONE]")
		})
	}
}

func TestPrivacyStreamParallelToolsAndTerminalPartial(t *testing.T) {
	s := NewSession()
	require.NoError(t, s.Add("fixture-A"))
	require.NoError(t, s.Add("fixture-B"))
	a, b := s.Mask("fixture-A"), s.Mask("fixture-B")
	stream := &Stream{Session: s, RestoreTools: true}
	var output strings.Builder
	for _, item := range []struct {
		index int
		text  string
	}{{0, a[:20]}, {1, b[:20]}, {0, a[20:]}, {1, b[20:]}} {
		data, err := common.Marshal(map[string]any{"choices": []any{map[string]any{"index": item.index, "delta": map[string]any{"content": item.text}}}})
		require.NoError(t, err)
		out, err := stream.Write([]byte("data: "+string(data)+"\n\n"), false)
		require.NoError(t, err)
		output.Write(out)
	}
	assert.Contains(t, output.String(), "fixture-A")
	assert.Contains(t, output.String(), "fixture-B")
	out, err := stream.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"literal __NA_\"}}]}\n\ndata: [DONE]\n\n"), true)
	require.NoError(t, err)
	assert.Contains(t, string(out), "literal __NA_")
}
