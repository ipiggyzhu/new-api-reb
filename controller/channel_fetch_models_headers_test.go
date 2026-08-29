package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 真 Anthropic API 只认 x-api-key，而 new-api 系网关的 /v1/models 只认
// Authorization: Bearer。拉取模型列表必须同时带两个头，缺任何一个都会让
// 其中一类上游返回 401，渠道从此"永远检测不到新模型"且不报显式错误。
func TestBuildFetchModelsHeadersAnthropicCarriesBothAuthSchemes(t *testing.T) {
	channel := &model.Channel{Type: constant.ChannelTypeAnthropic}
	headers, err := buildFetchModelsHeaders(channel, "sk-test")
	require.NoError(t, err)

	assert.Equal(t, "sk-test", headers.Get("x-api-key"))
	assert.Equal(t, "Bearer sk-test", headers.Get("Authorization"))
	assert.Equal(t, "2023-06-01", headers.Get("anthropic-version"))
	// agentrouter 等上游按客户端身份放行，裸 UA 会被 401 拒——拉取必须带画像。
	assert.NotEmpty(t, headers.Get("User-Agent"))
}

func TestBuildFetchModelsHeadersDefaultUsesBearerOnly(t *testing.T) {
	channel := &model.Channel{Type: constant.ChannelTypeOpenAI}
	headers, err := buildFetchModelsHeaders(channel, "sk-test")
	require.NoError(t, err)

	assert.Equal(t, "Bearer sk-test", headers.Get("Authorization"))
	assert.Empty(t, headers.Get("x-api-key"))
	assert.NotEmpty(t, headers.Get("User-Agent"))
}
