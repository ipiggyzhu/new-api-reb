package controller

import (
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
				assert.Equal(t, model.ChannelKeyTag("sk-origin"), model.ChannelKeyTag(stored.GetKeys()[0]), "the original key remains first")
				assert.NotEqual(t, model.ChannelKeyTag("sk-origin"), model.ChannelKeyTag("sk-new-1"))
			}
		})
	}
}

// Per-key edits use the table and its unique version pool only while the
// switch is on. Nil preserves a field, empty proxy inherits, and empty profile
// leaves a row unassigned. Rejected edits and insufficient permissions save nothing.
func TestManageMultiKeysSetKeyEnv(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.ChannelKeyEnvironment{}))
	newChannel := func(id int, setting dto.ChannelSettings) *model.Channel {
		channel := &model.Channel{
			Id: id, Name: "multi-key", Type: constant.ChannelTypeOpenAI, Key: "sk-key-a\nsk-key-b",
			Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
			ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeRandom},
		}
		channel.SetSetting(setting)
		require.NoError(t, f.db.Create(channel).Error)
		if setting.KeyEnvironments {
			require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error { _, err := service.ReconcileKeyEnvironments(tx, channel, false); return err }))
		}
		return channel
	}
	codex := newChannel(50, dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex, KeyEnvironments: true})
	plain := newChannel(51, dto.ChannelSettings{})
	setEnv := func(role int, channel *model.Channel, index int, cliVersion, proxy *string) tokenAPIResponse {
		return callChannelAdminAPI(t, ManageMultiKeys, role, http.MethodPost, MultiKeyManageRequest{
			ChannelId: channel.Id, Action: "set_key_env", KeyIndex: &index, CLIProfile: cliVersion, Proxy: proxy,
		})
	}
	storedEnvs := func(channel *model.Channel) map[string]model.MultiKeyEnv {
		rows, err := model.LoadChannelKeyEnvironmentRows(f.db, channel.Id)
		require.NoError(t, err)
		var envs map[string]model.MultiKeyEnv
		for _, row := range rows {
			if row.KeyTag != model.ChannelKeyTag("sk-key-b") {
				continue
			}
			env := model.MultiKeyEnv{Proxy: row.Proxy}
			if row.ProfileId != nil {
				env.CLIVersionProfile = *row.ProfileId
			}
			if env != (model.MultiKeyEnv{}) {
				envs = map[string]model.MultiKeyEnv{row.KeyTag: env}
			}
		}
		return envs
	}

	before := storedEnvs(codex)
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
		{"a CLI version without client emulation", plain, 1, lo.ToPtr("codex-0.160.0-windows-x64"), nil, "该渠道未开启独立 key 环境"},
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
	assert.Equal(t, before, storedEnvs(codex), "rejected environments and denied permissions leave the assignment intact")

	duplicate := setEnv(common.RoleRootUser, codex, 1, lo.ToPtr("codex-0.163.0-linux-x64"), nil)
	assert.False(t, duplicate.Success, "versions cannot be shared by two keys")
	assert.Contains(t, duplicate.Message, "already assigned")
	assert.Equal(t, before, storedEnvs(codex))
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
		{"an empty profile leaves the key unassigned", lo.ToPtr(""), nil,
			map[string]model.MultiKeyEnv{keyB: {Proxy: "http://proxy-b:8080"}}},
		{"nothing of its own left", nil, lo.ToPtr(""), nil},
	} {
		response := setEnv(common.RoleRootUser, codex, 1, step.cliVersion, step.proxy)
		require.True(t, response.Success, "%s: %s", step.name, response.Message)
		assert.Equal(t, step.want, storedEnvs(codex), step.name)
	}

	response := setEnv(common.RoleRootUser, plain, 0, nil, lo.ToPtr("http://proxy-a:8080"))
	assert.False(t, response.Success, "proxy overrides also need the independent environment switch")
	assert.Equal(t, "该渠道未开启独立 key 环境", response.Message)
	assert.Empty(t, storedEnvs(plain))
	rows, err := model.LoadChannelKeyEnvironmentRows(f.db, codex.Id)
	require.NoError(t, err)
	require.Len(t, rows, 2, "clearing a profile keeps the key's ownership row")
	for _, row := range rows {
		if row.KeyTag == keyB {
			assert.Nil(t, row.ProfileId)
			assert.Empty(t, row.Proxy)
		}
	}
}

// The key list shows each key's own environment with the proxy password
// masked, to everyone who may manage the keys.
func TestManageMultiKeysKeyStatusShowsTheKeysEnvironment(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.ChannelKeyEnvironment{}))
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
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex, KeyEnvironments: true})
	require.NoError(t, f.db.Create(channel).Error)
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error { _, err := service.ReconcileKeyEnvironments(tx, channel, false); return err }))

	response := callChannelAdminAPI(t, ManageMultiKeys, common.RoleAdminUser, http.MethodPost, MultiKeyManageRequest{
		ChannelId: channel.Id, Action: "get_key_status",
	})

	require.True(t, response.Success, response.Message)
	var status MultiKeyStatusResponse
	require.NoError(t, common.Unmarshal(response.Data, &status))
	require.Len(t, status.Keys, 2)
	assert.Equal(t, "codex-0.163.0-linux-x64", status.Keys[0].CLIProfile, "a key gets its assigned version from the table")
	assert.Empty(t, status.Keys[0].Proxy)
	assert.Equal(t, "codex-0.160.0-windows-x64", status.Keys[1].CLIProfile)
	assert.Equal(t, "socks5://user:xxxxx@proxy-b:1080", status.Keys[1].Proxy)
	assert.NotContains(t, string(response.Data), "secret")
}

// A key's own CLI version has to keep fitting the channel it is laid over, so
// a channel setting that would break it is refused until the key is changed,
// and one it still fits is saved. A save that also replaces the key is not held
// back by the environment it drops along with the key.
func TestUpdateChannelRefusesSettingsThatBreakAKeysEnvironment(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.ChannelKeyEnvironment{}))
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
	channel.SetSetting(dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyCodex, KeyEnvironments: true})
	require.NoError(t, f.db.Create(channel).Error)
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error { _, err := service.ReconcileKeyEnvironments(tx, channel, false); return err }))
	update := func(setting string) tokenAPIResponse {
		return callChannelAdminAPI(t, UpdateChannel, common.RoleRootUser, http.MethodPut, map[string]any{
			"id": channel.Id, "type": channel.Type, "setting": setting,
		})
	}

	response := update(`{"synthetic_client_headers_profile":"claude","key_environments":true}`)
	assert.False(t, response.Success)
	assert.Equal(t, "claude key environments need an Anthropic channel", response.Message)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, constant.ClientHeaderFamilyCodex, stored.GetSetting().SyntheticClientHeadersProfile)

	response = update(`{"synthetic_client_headers_profile":"codex","key_environments":true,"proxy":"http://channel-proxy:8080"}`)
	require.True(t, response.Success, response.Message)
	stored, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "http://channel-proxy:8080", stored.GetSetting().Proxy)
	profile, assigned := stored.KeyEnvironmentProfile("sk-key-b")
	assert.True(t, assigned)
	assert.Equal(t, "codex-0.160.0-windows-x64", profile)
	assert.Empty(t, stored.ChannelInfo.MultiKeyEnvs)

	response = callChannelAdminAPI(t, UpdateChannel, common.RoleRootUser, http.MethodPut, map[string]any{
		"id": channel.Id, "type": channel.Type, "setting": `{"synthetic_client_headers_profile":"claude"}`,
		"key_mode": "replace", "key": "sk-key-a\nsk-key-c",
	})
	require.True(t, response.Success, response.Message)
	stored, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, constant.ClientHeaderFamilyClaude, stored.GetSetting().SyntheticClientHeadersProfile)
	assert.Empty(t, stored.ChannelInfo.MultiKeyEnvs)
	assert.False(t, stored.GetSetting().KeyEnvironments)
	rows, err := model.LoadChannelKeyEnvironmentRows(f.db, stored.Id)
	require.NoError(t, err)
	for _, row := range rows {
		assert.NotEqual(t, model.ChannelKeyTag("sk-key-b"), row.KeyTag)
	}
}

// Replacing a multi-key channel's keys keeps each key's state with the key: a
// key still on the list keeps its disabled state, reason and time at its new
// position, and a new key starts enabled whatever the key it took the position
// of had. A key listed twice is matched occurrence by occurrence, and a Vertex
// credential uploaded again is the same key whatever order its file or the
// stored key lists the fields in; the stored text is kept, so the key keeps its
// identity and environment too.
func TestUpdateChannelReplaceKeepsKeyStatusWithTheKey(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	const vertexA = `{"project_id":"proj-a","type":"service_account"}`
	const vertexB = `{"project_id":"proj-b","type":"service_account"}`
	const vertexBUploaded = `{"type":"service_account","project_id":"proj-b"}`
	const vertexRegion = `{"default":"us-central1"}`
	for i, tc := range []struct {
		name        string
		channelType int
		key         string
		other       string
		info        model.ChannelInfo
		patch       map[string]any
		wantKey     string
		wantStatus  map[int]int
		wantReason  map[int]string
		wantTime    map[int]int64
		wantEnvs    map[string]model.MultiKeyEnv
	}{
		{
			name: "new keys take positions of disabled ones", channelType: constant.ChannelTypeOpenAI, key: "sk-key-a\nsk-key-b\nsk-key-c",
			info: model.ChannelInfo{
				MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled, 2: common.ChannelStatusManuallyDisabled},
				MultiKeyDisabledReason: map[int]string{1: "401 unauthorized"},
				MultiKeyDisabledTime:   map[int]int64{1: 1761800000},
			},
			patch:      map[string]any{"key": "sk-key-d\nsk-key-e\nsk-key-b"},
			wantKey:    "sk-key-d\nsk-key-e\nsk-key-b",
			wantStatus: map[int]int{2: common.ChannelStatusAutoDisabled},
			wantReason: map[int]string{2: "401 unauthorized"},
			wantTime:   map[int]int64{2: 1761800000},
		},
		{
			name: "a key listed twice", channelType: constant.ChannelTypeOpenAI, key: "sk-key-a\nsk-key-a\nsk-key-b",
			info: model.ChannelInfo{
				MultiKeyStatusList:     map[int]int{1: common.ChannelStatusManuallyDisabled},
				MultiKeyDisabledReason: map[int]string{1: "manual"},
				MultiKeyDisabledTime:   map[int]int64{1: 1761800000},
			},
			patch:      map[string]any{"key": "sk-key-b\nsk-key-a\nsk-key-a"},
			wantKey:    "sk-key-b\nsk-key-a\nsk-key-a",
			wantStatus: map[int]int{2: common.ChannelStatusManuallyDisabled},
			wantReason: map[int]string{2: "manual"},
			wantTime:   map[int]int64{2: 1761800000},
		},
		{
			name: "vertex credential uploaded again", channelType: constant.ChannelTypeVertexAi, key: vertexA + "\n" + vertexB, other: vertexRegion,
			info: model.ChannelInfo{
				MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
				MultiKeyDisabledReason: map[int]string{1: "invalid_grant"},
				MultiKeyDisabledTime:   map[int]int64{1: 1761800000},
			},
			patch:      map[string]any{"other": vertexRegion, "key": vertexBUploaded},
			wantKey:    vertexB,
			wantStatus: map[int]int{0: common.ChannelStatusAutoDisabled},
			wantReason: map[int]string{0: "invalid_grant"},
			wantTime:   map[int]int64{0: 1761800000},
		},
		{
			name: "vertex credential stored as uploaded", channelType: constant.ChannelTypeVertexAi, key: vertexA + "\n" + vertexBUploaded, other: vertexRegion,
			info: model.ChannelInfo{
				MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
				MultiKeyDisabledReason: map[int]string{1: "invalid_grant"},
				MultiKeyDisabledTime:   map[int]int64{1: 1761800000},
				MultiKeyEnvs:           map[string]model.MultiKeyEnv{model.KeyFingerprint(vertexBUploaded): {Proxy: "http://proxy-b:8080"}},
			},
			patch:      map[string]any{"other": vertexRegion, "key": `[` + vertexB + `,{"project_id":"proj-c","type":"service_account"}]`},
			wantKey:    vertexBUploaded + "\n" + `{"project_id":"proj-c","type":"service_account"}`,
			wantStatus: map[int]int{0: common.ChannelStatusAutoDisabled},
			wantReason: map[int]string{0: "invalid_grant"},
			wantTime:   map[int]int64{0: 1761800000},
			wantEnvs:   map[string]model.MultiKeyEnv{model.KeyFingerprint(vertexBUploaded): {Proxy: "http://proxy-b:8080"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := tc.info
			info.IsMultiKey, info.MultiKeySize, info.MultiKeyMode = true, len(strings.Split(tc.key, "\n")), constant.MultiKeyModeRandom
			channel := &model.Channel{
				Id: 60 + i, Name: "multi-key", Type: tc.channelType, Key: tc.key, Other: tc.other,
				Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default", ChannelInfo: info,
			}
			require.NoError(t, f.db.Create(channel).Error)
			patch := map[string]any{"id": channel.Id, "type": channel.Type, "key_mode": "replace"}
			for field, value := range tc.patch {
				patch[field] = value
			}

			response := callChannelAdminAPI(t, UpdateChannel, common.RoleRootUser, http.MethodPut, patch)

			require.True(t, response.Success, response.Message)
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantKey, stored.Key)
			assert.Equal(t, tc.wantStatus, stored.ChannelInfo.MultiKeyStatusList)
			assert.Equal(t, tc.wantReason, stored.ChannelInfo.MultiKeyDisabledReason)
			assert.Equal(t, tc.wantTime, stored.ChannelInfo.MultiKeyDisabledTime)
			assert.Equal(t, tc.wantEnvs, stored.ChannelInfo.MultiKeyEnvs)
		})
	}
}

// A new channel takes no key environment or origin key from the request:
// environments are set one key at a time on the saved channel, where they are
// checked, and a channel created multi-key had no key before it.
func TestAddChannelIgnoresKeyEnvironmentsInTheRequest(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	response := callChannelAdminAPI(t, AddChannel, common.RoleRootUser, http.MethodPost, AddChannelRequest{
		Mode: "multi_to_single", MultiKeyMode: constant.MultiKeyModeRandom,
		Channel: &model.Channel{
			Name: "new-multi-key", Type: constant.ChannelTypeOpenAI, Key: "sk-key-a\nsk-key-b",
			Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
			ChannelInfo: model.ChannelInfo{
				MultiKeyEnvs: map[string]model.MultiKeyEnv{
					model.KeyFingerprint("sk-key-b"): {CLIVersionProfile: "codex-9.9.9-windows-x64", Proxy: "ftp://proxy-b:21"},
				},
				MultiKeyOriginFingerprint: model.KeyFingerprint("sk-key-b"),
			},
		},
	})

	require.True(t, response.Success, response.Message)
	var stored model.Channel
	require.NoError(t, f.db.Where("name = ?", "new-multi-key").First(&stored).Error)
	require.True(t, stored.ChannelInfo.IsMultiKey)
	assert.Empty(t, stored.ChannelInfo.MultiKeyEnvs)
	assert.Empty(t, stored.ChannelInfo.MultiKeyOriginFingerprint)
}

// The channel endpoints never return a key's own environment, whose proxy can
// carry a password; multi-key management shows it with the password masked.
func TestChannelEndpointsLeaveOutKeyEnvironments(t *testing.T) {
	f := newUpstreamValidationFixture(t)
	channel := &model.Channel{
		Id: 55, Name: "multi-key", Type: constant.ChannelTypeOpenAI, Key: "sk-key-a\nsk-key-b",
		Status: common.ChannelStatusEnabled, Models: "gpt-4o", Group: "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeRandom,
			MultiKeyEnvs: map[string]model.MultiKeyEnv{
				model.KeyFingerprint("sk-key-b"): {Proxy: "socks5://user:secret@proxy-b:1080"},
			},
		},
	}
	require.NoError(t, f.db.Create(channel).Error)

	for _, endpoint := range []struct {
		name    string
		handler gin.HandlerFunc
		target  string
		params  gin.Params
	}{
		{"channel detail", GetChannel, "/api/channel/55", gin.Params{{Key: "id", Value: "55"}}},
		{"channel list", GetAllChannels, "/api/channel/?p=1&page_size=10", nil},
	} {
		c, recorder := newAuthenticatedContext(t, http.MethodGet, endpoint.target, nil, 1)
		c.Params = endpoint.params
		endpoint.handler(c)
		require.Equal(t, http.StatusOK, recorder.Code, endpoint.name)
		assert.Contains(t, recorder.Body.String(), `"is_multi_key":true`, endpoint.name)
		assert.NotContains(t, recorder.Body.String(), "secret", endpoint.name)
		assert.NotContains(t, recorder.Body.String(), "multi_key_envs", endpoint.name)
	}
}
