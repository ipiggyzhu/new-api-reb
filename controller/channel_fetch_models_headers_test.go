package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
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

func TestBuildFetchModelsHeadersHonorsChannelProfile(t *testing.T) {
	cases := []struct {
		name       string
		settings   dto.ChannelSettings
		wantClient string
		wantOrigin string
		wantLang   string
	}{
		{
			name:       "explicit codex replaces the API type default",
			settings:   dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex},
			wantClient: "codex_cli_rs/", wantOrigin: "codex_cli_rs",
		},
		{
			name:       "auto keeps the API type default",
			settings:   dto.ChannelSettings{SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto},
			wantClient: "OpenAI/Python", wantLang: "python",
		},
		{
			name:       "off keeps management requests synthesized",
			wantClient: "OpenAI/Python", wantLang: "python",
		},
		{
			name:       "legacy enabled keeps the API type default",
			settings:   dto.ChannelSettings{SyntheticClientHeaders: true},
			wantClient: "OpenAI/Python", wantLang: "python",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &model.Channel{Type: constant.ChannelTypeOpenAI}
			channel.SetSetting(tc.settings)

			headers, err := buildFetchModelsHeaders(channel, "sk-upstream")
			require.NoError(t, err)

			assert.Contains(t, headers.Get("User-Agent"), tc.wantClient)
			assert.Equal(t, tc.wantOrigin, headers.Get("Originator"))
			assert.Equal(t, tc.wantLang, headers.Get("X-Stainless-Lang"))
			assert.Equal(t, "Bearer sk-upstream", headers.Get("Authorization"))
			assert.Equal(t, acceptJSON, headers.Get("Accept"))
		})
	}
}

func TestBuildFetchModelsHeadersSelectedProfileKeepsOverridePriority(t *testing.T) {
	withChannelTestClientHeaderOverrides(t, map[string]map[string]string{
		clientHeaderFamilyAll:   {"user-agent": "global-client/1", "x-managed-probe": "enabled"},
		clientHeaderFamilyCodex: {"user-agent": "codex_cli_rs/9.9.9"},
	})
	channel := &model.Channel{Type: constant.ChannelTypeOpenAI}
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})

	headers, err := buildFetchModelsHeaders(channel, "sk-upstream")
	require.NoError(t, err)
	assert.Equal(t, "codex_cli_rs/9.9.9", headers.Get("User-Agent"))
	assert.Equal(t, "enabled", headers.Get("X-Managed-Probe"))

	overrides := `{"user-agent":"static-client/1","originator":"static-origin","x-upstream-key":"{api_key}","*":"","regex:^x-":""}`
	channel.HeaderOverride = &overrides
	headers, err = buildFetchModelsHeaders(channel, "sk-upstream")
	require.NoError(t, err)
	assert.Equal(t, "static-client/1", headers.Get("User-Agent"))
	assert.Equal(t, "static-origin", headers.Get("Originator"))
	assert.Equal(t, "sk-upstream", headers.Get("X-Upstream-Key"))
	assert.Equal(t, "Bearer sk-upstream", headers.Get("Authorization"))
	assert.NotContains(t, headers, "*")
	assert.NotContains(t, headers, "Regex:^x-")
}
