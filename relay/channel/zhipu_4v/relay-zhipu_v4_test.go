package zhipu_4v

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Content parts now carry the client's cache_control for providers that honor
// it; Zhipu has no such field, so its request body must stay without it.
func TestRequestOpenAI2ZhipuDropsCacheControl(t *testing.T) {
	body := `{
		"model": "glm-4v-plus",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "hello", "cache_control": {"type": "ephemeral"}},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}, "cache_control": {"type": "ephemeral"}}
			]}
		]
	}`
	var req dto.GeneralOpenAIRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &req))

	upstream, err := common.Marshal(requestOpenAI2Zhipu(req))
	require.NoError(t, err)

	content := gjson.GetBytes(upstream, "messages.0.content")
	require.Len(t, content.Array(), 2)
	assert.Equal(t, "hello", content.Get("0.text").String())
	assert.Equal(t, "aGVsbG8=", content.Get("1.image_url.url").String())
	assert.NotContains(t, string(upstream), "cache_control")
}
