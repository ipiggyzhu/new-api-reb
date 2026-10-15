package channel

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexRequestOrderPreservesCapturedNestedObjects(t *testing.T) {
	const original = `{"model":"gpt-6-astra","input":[{"type":"additional_tools","id":"at_1","role":"developer","tools":[{"type":"function","name":"read","description":"keep","strict":false,"parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{"const":9007199254740993},"control\u0001":{"type":"string"}},"additionalProperties":false}}]},{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"keep <> input"}]},{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-ciphertext"}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"xhigh","context":"all_turns"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"original-session","text":{"verbosity":"low"},"client_metadata":{"x-codex-installation-id":"device","turn_id":"turn","thread_id":"thread","root_turn_id":"root","session_id":"session","x-codex-window-id":"window","x-codex-turn-metadata":"{\"installation_id\":\"device\",\"session_id\":\"session\",\"thread_id\":\"thread\",\"agent_name\":\"/root\",\"turn_id\":\"turn\"}"}}`
	for _, converted := range []bool{false, true} {
		c, info := cliTestRequest("codex")
		storage, err := common.CreateBodyStorage([]byte(original))
		require.NoError(t, err)
		c.Set(common.KeyBodyStorage, storage)
		t.Cleanup(func() { common.CleanupBodyStorage(c) })
		body := []byte(original)
		if converted {
			var request dto.OpenAIResponsesRequest
			require.NoError(t, common.Unmarshal(body, &request))
			body, err = common.Marshal(request)
			require.NoError(t, err)
		}
		_, reader, headers, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(string(body)))
		require.NoError(t, err)
		got, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.True(t, gjson.ValidBytes(got))
		var keys []string
		gjson.ParseBytes(got).ForEach(func(key, _ gjson.Result) bool {
			keys = append(keys, key.String())
			return true
		})
		assert.Equal(t, codexRequestFieldOrder, keys)
		// Compact both through the same JSON wrapper so this asserts nested
		// property order as well as values, without depending on HTML escaping.
		wantInput, err := common.Marshal(json.RawMessage(gjson.Get(original, "input").Raw))
		require.NoError(t, err)
		gotInput, err := common.Marshal(json.RawMessage(gjson.GetBytes(got, "input").Raw))
		require.NoError(t, err)
		assert.Equal(t, string(wantInput), string(gotInput))
		assert.Equal(t, "xhigh", gjson.GetBytes(got, "reasoning.effort").String())
		assert.Equal(t, gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String(), headers.Get("x-codex-turn-metadata"))
	}
}

func TestCodexOrderTemplateDoesNotRestoreRejectedReasoningOrOverrides(t *testing.T) {
	const reference = `{"model":"before","input":[{"type":"reasoning","id":"old-id","summary":[],"encrypted_content":"rejected"}],"reasoning":{"effort":"xhigh"},"removed":true}`
	const current = `{"new_field":{"z":1,"a":2},"reasoning":{"effort":"low"},"model":"after","input":[{"summary":[],"type":"reasoning"}]}`
	got := orderCodexJSON(gjson.Parse(current), gjson.Parse(reference), "")
	assert.JSONEq(t, current, string(got))
	assert.Equal(t, `{"model":"after","input":[{"type":"reasoning","summary":[]}],"reasoning":{"effort":"low"},"new_field":{"z":1,"a":2}}`, string(got))
}

func TestCodexGeneratedMessagesUseCLIFieldOrder(t *testing.T) {
	c, info := cliTestRequest("codex")
	const body = `{"model":"gpt-6-astra","input":[{"content":"keep","role":"user"}],"tools":[{"parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"number"}}},"name":"read","type":"function"}],"reasoning":{"context":"all_turns","effort":"xhigh"}}`
	_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	for path, want := range map[string][]string{
		"input.0":                               {"type", "id", "role", "tools"},
		"input.1":                               {"type", "id", "role", "content"},
		"input.0.tools.0":                       {"type", "name", "strict", "parameters"},
		"input.1.content.0":                     {"type", "text"},
		"reasoning":                             {"effort", "context"},
		"input.0.tools.0.parameters.properties": {"z", "a"},
	} {
		var keys []string
		gjson.GetBytes(got, path).ForEach(func(key, _ gjson.Result) bool {
			keys = append(keys, key.String())
			return true
		})
		assert.Equal(t, want, keys, path)
	}
}
