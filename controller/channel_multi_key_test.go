package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callChannelAdminAPI runs one channel admin handler as user 1 with the given
// system role.
func callChannelAdminAPI(t *testing.T, handler gin.HandlerFunc, role int, method string, body any) tokenAPIResponse {
	t.Helper()
	c, recorder := newAuthenticatedContext(t, method, "/api/channel/", body, 1)
	c.Set("role", role)
	handler(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	return decodeAPIResponse(t, recorder)
}

// Appending keys to a single-key channel turns it multi-key: the key it had
// stays first and keeps the identity the channel always had, and each pasted
// key joins once. Channel types whose key is one JSON credential cannot be
// converted, whether or not the request names the type, and the conversion is
// a key change, so it takes the right to change keys.
func TestUpdateChannelAppendConvertsSingleKeyChannel(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	const codexKey = `{"access_token":"at-origin","account_id":"acct-origin"}`
	const vertexKey = `{"type":"service_account","project_id":"origin"}`
	const vertexRegion = `{"default":"us-central1"}`
	for i, tc := range []struct {
		name        string
		channelType int
		key         string
		other       string
		role        int
		patch       map[string]any
		rejected    string // the rejection message; empty when the update is applied
		wantKey     string
		wantMulti   bool
	}{
		{
			name: "pasted keys join after the channel's key", channelType: constant.ChannelTypeOpenAI, key: "sk-origin",
			role:      common.RoleRootUser,
			patch:     map[string]any{"type": constant.ChannelTypeOpenAI, "key": "sk-new-1\r\nsk-origin\n\n sk-new-2 \nsk-new-1"},
			wantKey:   "sk-origin\nsk-new-1\nsk-new-2",
			wantMulti: true,
		},
		{
			name: "nothing new to append", channelType: constant.ChannelTypeOpenAI, key: "sk-origin",
			role:    common.RoleRootUser,
			patch:   map[string]any{"type": constant.ChannelTypeOpenAI, "key": "sk-origin\n"},
			wantKey: "sk-origin",
		},
		{
			name: "codex OAuth credential", channelType: constant.ChannelTypeCodex, key: codexKey,
			role:     common.RoleRootUser,
			patch:    map[string]any{"type": constant.ChannelTypeCodex, "key": `{"access_token":"at-new","account_id":"acct-new"}`},
			rejected: "该渠道类型不支持追加密钥转为多密钥",
			wantKey:  codexKey,
		},
		{
			name: "codex, the request naming no type", channelType: constant.ChannelTypeCodex, key: codexKey,
			role:     common.RoleRootUser,
			patch:    map[string]any{"key": "sk-new-1\nsk-new-2"},
			rejected: "该渠道类型不支持追加密钥转为多密钥",
			wantKey:  codexKey,
		},
		{
			name: "vertex JSON credential", channelType: constant.ChannelTypeVertexAi, key: vertexKey, other: vertexRegion,
			role:     common.RoleRootUser,
			patch:    map[string]any{"type": constant.ChannelTypeVertexAi, "other": vertexRegion, "key": `{"type":"service_account","project_id":"new"}`},
			rejected: "该渠道类型不支持追加密钥转为多密钥",
			wantKey:  vertexKey,
		},
		{
			name: "an admin who may not change keys", channelType: constant.ChannelTypeOpenAI, key: "sk-origin",
			role:    common.RoleAdminUser,
			patch:   map[string]any{"type": constant.ChannelTypeOpenAI, "key": "sk-new-1"},
			wantKey: "sk-origin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := &model.Channel{
				Id: 40 + i, Name: "single-key", Type: tc.channelType, Key: tc.key, Other: tc.other,
				Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
			}
			require.NoError(t, f.db.Create(channel).Error)
			patch := map[string]any{"id": channel.Id, "key_mode": "append"}
			for field, value := range tc.patch {
				patch[field] = value
			}

			response := callChannelAdminAPI(t, UpdateChannel, tc.role, http.MethodPut, patch)

			applied := tc.rejected == "" && tc.role == common.RoleRootUser
			assert.Equal(t, applied, response.Success, response.Message)
			if tc.rejected != "" {
				assert.Equal(t, tc.rejected, response.Message)
			}
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantKey, stored.Key)
			assert.Equal(t, tc.wantMulti, stored.ChannelInfo.IsMultiKey)
			if tc.wantMulti {
				assert.Equal(t, 3, stored.ChannelInfo.MultiKeySize)
				assert.Equal(t, constant.MultiKeyModeRandom, stored.ChannelInfo.MultiKeyMode)
				assert.Equal(t, "0", stored.KeyIdentitySlot("sk-origin"), "the channel's key keeps the identity it had")
				assert.Equal(t, "k"+model.KeyFingerprint("sk-new-1"), stored.KeyIdentitySlot("sk-new-1"))
			}
		})
	}
}

// A key's own environment is checked against the channel it is laid over and
// stored under the key itself. Each field is set on its own: an omitted field
// keeps its value, an empty one goes back to inheriting the channel's, and a
// key left with nothing of its own has no entry. Setting it takes the right to
// change keys.
func TestManageMultiKeysSetKeyEnv(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	newChannel := func(id int, setting dto.ChannelSettings) *model.Channel {
		channel := &model.Channel{
			Id: id, Name: "multi-key", Type: constant.ChannelTypeOpenAI, Key: "sk-key-a\nsk-key-b",
			Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
			ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeRandom},
		}
		channel.SetSetting(setting)
		require.NoError(t, f.db.Create(channel).Error)
		return channel
	}
	codex := newChannel(50, dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})
	plain := newChannel(51, dto.ChannelSettings{})
	setEnv := func(role int, channel *model.Channel, index int, cliVersion, proxy *string) tokenAPIResponse {
		return callChannelAdminAPI(t, ManageMultiKeys, role, http.MethodPost, MultiKeyManageRequest{
			ChannelId: channel.Id, Action: "set_key_env", KeyIndex: &index, CLIVersionProfile: cliVersion, Proxy: proxy,
		})
	}
	storedEnvs := func(channel *model.Channel) map[string]model.MultiKeyEnv {
		reloaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		return reloaded.ChannelInfo.MultiKeyEnvs
	}

	for _, tc := range []struct {
		name       string
		channel    *model.Channel
		index      int
		cliVersion *string
		proxy      *string
		message    string
	}{
		{"another family's CLI version", codex, 1, lo.ToPtr("claude-2.1.306-linux-x64"), nil, "CLI version profile claude-2.1.306-linux-x64 does not belong to codex"},
		{"an unknown CLI version", codex, 1, lo.ToPtr("codex-9.9.9-windows-x64"), nil, "unknown CLI version profile: codex-9.9.9-windows-x64"},
		{"a CLI version without client emulation", plain, 1, lo.ToPtr("codex-0.160.0-windows-x64"), nil, "渠道未启用客户端模拟，不能按密钥设置 CLI 版本"},
		{"a proxy the relay cannot dial", codex, 1, nil, lo.ToPtr("ftp://proxy-b:21"), "代理仅支持 http、https、socks5、socks5h 协议"},
		{"a proxy without a host", codex, 1, nil, lo.ToPtr("socks5://"), "代理地址缺少主机和端口"},
		{"an index past the keys", codex, 2, nil, lo.ToPtr("http://proxy-b:8080"), "密钥索引超出范围"},
	} {
		response := setEnv(common.RoleRootUser, tc.channel, tc.index, tc.cliVersion, tc.proxy)
		assert.False(t, response.Success, tc.name)
		assert.Equal(t, tc.message, response.Message, tc.name)
	}
	denied := setEnv(common.RoleAdminUser, codex, 1, nil, lo.ToPtr("http://proxy-b:8080"))
	assert.False(t, denied.Success, "an admin who may not change keys")
	assert.Empty(t, storedEnvs(codex), "a rejected environment is not stored")

	keyB := model.KeyFingerprint("sk-key-b")
	for _, step := range []struct {
		name       string
		cliVersion *string
		proxy      *string
		want       map[string]model.MultiKeyEnv
	}{
		{"both fields", lo.ToPtr(" Codex-0.160.0-Windows-x64 "), lo.ToPtr(" socks5://user:secret@proxy-b:1080 "),
			map[string]model.MultiKeyEnv{keyB: {CLIVersionProfile: "codex-0.160.0-windows-x64", Proxy: "socks5://user:secret@proxy-b:1080"}}},
		{"an omitted field keeps its value", nil, lo.ToPtr("http://proxy-b:8080"),
			map[string]model.MultiKeyEnv{keyB: {CLIVersionProfile: "codex-0.160.0-windows-x64", Proxy: "http://proxy-b:8080"}}},
		{"an empty field inherits the channel's again", lo.ToPtr(""), nil,
			map[string]model.MultiKeyEnv{keyB: {Proxy: "http://proxy-b:8080"}}},
		{"nothing of its own left", nil, lo.ToPtr(""), nil},
	} {
		response := setEnv(common.RoleRootUser, codex, 1, step.cliVersion, step.proxy)
		require.True(t, response.Success, "%s: %s", step.name, response.Message)
		assert.Equal(t, step.want, storedEnvs(codex), step.name)
	}

	response := setEnv(common.RoleRootUser, plain, 0, nil, lo.ToPtr("http://proxy-a:8080"))
	require.True(t, response.Success, response.Message)
	assert.Equal(t, map[string]model.MultiKeyEnv{model.KeyFingerprint("sk-key-a"): {Proxy: "http://proxy-a:8080"}}, storedEnvs(plain),
		"a proxy needs no client emulation")
}

// The key list shows each key's own environment with the proxy password
// masked, to everyone who may manage the keys.
func TestManageMultiKeysKeyStatusShowsTheKeysEnvironment(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	channel := &model.Channel{
		Id: 52, Name: "multi-key", Type: constant.ChannelTypeOpenAI, Key: "sk-key-a\nsk-key-b",
		Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeRandom,
			MultiKeyEnvs: map[string]model.MultiKeyEnv{
				model.KeyFingerprint("sk-key-b"): {CLIVersionProfile: "codex-0.160.0-windows-x64", Proxy: "socks5://user:secret@proxy-b:1080"},
			},
		},
	}
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})
	require.NoError(t, f.db.Create(channel).Error)

	response := callChannelAdminAPI(t, ManageMultiKeys, common.RoleAdminUser, http.MethodPost, MultiKeyManageRequest{
		ChannelId: channel.Id, Action: "get_key_status",
	})

	require.True(t, response.Success, response.Message)
	var status MultiKeyStatusResponse
	require.NoError(t, common.Unmarshal(response.Data, &status))
	require.Len(t, status.Keys, 2)
	assert.Empty(t, status.Keys[0].CLIVersionProfile, "a key without its own environment shows none")
	assert.Empty(t, status.Keys[0].Proxy)
	assert.Equal(t, "codex-0.160.0-windows-x64", status.Keys[1].CLIVersionProfile)
	assert.Equal(t, "socks5://user:xxxxx@proxy-b:1080", status.Keys[1].Proxy)
	assert.NotContains(t, string(response.Data), "secret")
}

// A key's own CLI version has to keep fitting the channel it is laid over, so
// a channel setting that would break it is refused until the key is changed,
// and one it still fits is saved.
func TestUpdateChannelRefusesSettingsThatBreakAKeysEnvironment(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	channel := &model.Channel{
		Id: 53, Name: "multi-key", Type: constant.ChannelTypeOpenAI, Key: "sk-key-a\nsk-key-b",
		Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeRandom,
			MultiKeyEnvs: map[string]model.MultiKeyEnv{
				model.KeyFingerprint("sk-key-b"): {CLIVersionProfile: "codex-0.160.0-windows-x64"},
			},
		},
	}
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex})
	require.NoError(t, f.db.Create(channel).Error)
	update := func(setting string) tokenAPIResponse {
		return callChannelAdminAPI(t, UpdateChannel, common.RoleRootUser, http.MethodPut, map[string]any{
			"id": channel.Id, "type": channel.Type, "setting": setting,
		})
	}

	response := update(`{"synthetic_client_headers_profile":"claude"}`)
	assert.False(t, response.Success)
	assert.Equal(t, "有密钥的独立环境与新的渠道设置冲突，请先在多密钥管理中修改：CLI version profile codex-0.160.0-windows-x64 does not belong to claude", response.Message)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, constant.ClientHeaderFamilyCodex, stored.GetSetting().SyntheticClientHeadersProfile)

	response = update(`{"synthetic_client_headers_profile":"codex","proxy":"http://channel-proxy:8080"}`)
	require.True(t, response.Success, response.Message)
	stored, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "http://channel-proxy:8080", stored.GetSetting().Proxy)
	assert.Equal(t, channel.ChannelInfo.MultiKeyEnvs, stored.ChannelInfo.MultiKeyEnvs)
}
