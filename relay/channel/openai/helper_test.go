package openai

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// When the upstream reports no usage, completion tokens are estimated from the
// delivered text. The stream handler reads that text from the chunk it already
// parsed instead of decoding the chunk into the DTO again, so it must collect
// exactly what the DTO reading (ProcessStreamResponse, still used by xAI) does.
func TestProcessTokenDataMatchesDTOReading(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"hello "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think","reasoning":"shadowed"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"","reasoning":"shadowed by an empty value"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":null,"reasoning":"fallback"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\""}},{"index":1,"function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"Paris\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}

	var want, got strings.Builder
	wantTools, gotTools := 0, 0
	for _, chunk := range chunks {
		var response dto.ChatCompletionsStreamResponse
		require.NoError(t, common.UnmarshalJsonStr(chunk, &response))
		require.NoError(t, ProcessStreamResponse(response, &want, &wantTools))
		processTokenData(relayconstant.RelayModeChatCompletions, gjson.Parse(chunk), &got, &gotTools)
	}
	require.Equal(t, `hello thinkfallbackget_weather{"city"lookup:"Paris"}`, want.String(), "precondition: the DTO reading")
	assert.Equal(t, want.String(), got.String())
	assert.Equal(t, 2, wantTools)
	assert.Equal(t, wantTools, gotTools)

	var completion strings.Builder
	processTokenData(relayconstant.RelayModeCompletions,
		gjson.Parse(`{"choices":[{"index":0,"text":"legacy "},{"index":1,"text":"completion"}]}`), &completion, &gotTools)
	assert.Equal(t, "legacy completion", completion.String())
}
