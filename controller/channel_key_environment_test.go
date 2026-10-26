package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type keyEnvironmentResponse struct {
	Success bool           `json:"success"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func setupKeyEnvironmentControllerTest(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupErrorLogTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ChannelKeyEnvironment{}))
	originalMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = originalMemoryCache })
	return db
}

func callChannelHandler(t *testing.T, handler gin.HandlerFunc, method string, body any) (int, keyEnvironmentResponse) {
	t.Helper()
	payload, err := common.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, "/api/channel/", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 1)
	c.Set("role", common.RoleRootUser)
	handler(c)
	var response keyEnvironmentResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return recorder.Code, response
}

func addKeyEnvironmentChannel(name string, channelType int, setting string, keys string, allowUnassigned bool) AddChannelRequest {
	return AddChannelRequest{
		Mode:            "multi_to_single",
		AllowUnassigned: allowUnassigned,
		Channel: &model.Channel{
			Name: name, Type: channelType, Key: keys, Status: common.ChannelStatusEnabled,
			Models: "gpt-5", Group: "default", Setting: &setting,
		},
	}
}

func storedKeyEnvironmentCount(t *testing.T, db *gorm.DB, name string) (int64, *model.Channel) {
	t.Helper()
	var channel model.Channel
	if err := db.Where("name = ?", name).First(&channel).Error; err != nil {
		return 0, nil
	}
	var count int64
	require.NoError(t, db.Model(&model.ChannelKeyEnvironment{}).Where("channel_id = ?", channel.Id).Count(&count).Error)
	return count, &channel
}

func TestAddKeyEnvironmentChannelAnswersPlanRejections(t *testing.T) {
	const codex = `{"synthetic_client_headers_profile":"codex","key_environments":true}`
	const claude = `{"synthetic_client_headers_profile":"claude","key_environments":true}`
	for _, tc := range []struct {
		name     string
		request  AddChannelRequest
		status   int
		code     string
		rows     int64
		saved    bool
		dataKeys []string
	}{
		{name: "keys within the pool", request: addKeyEnvironmentChannel("fits", constant.ChannelTypeOpenAI, codex, "sk-1\nsk-2", false),
			status: http.StatusOK, rows: 2, saved: true},
		{name: "a key repeated with a carriage return", request: addKeyEnvironmentChannel("dup", constant.ChannelTypeOpenAI, codex, "sk-1\nsk-1\r", false),
			status: http.StatusBadRequest, code: "key_environment_duplicate", dataKeys: []string{"index", "first_index"}},
		{name: "more keys than versions", request: addKeyEnvironmentChannel("short", constant.ChannelTypeAnthropic, claude, "sk-1\nsk-2\nsk-3\nsk-4", false),
			status: http.StatusConflict, code: "key_environment_shortage", dataKeys: []string{"available", "keys", "unassigned_indexes", "versions"}},
		{name: "more keys than versions, confirmed", request: addKeyEnvironmentChannel("short-ok", constant.ChannelTypeAnthropic, claude, "sk-1\nsk-2\nsk-3\nsk-4", true),
			status: http.StatusOK, rows: 4, saved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupKeyEnvironmentControllerTest(t)
			status, response := callChannelHandler(t, AddChannel, http.MethodPost, tc.request)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.status == http.StatusOK, response.Success)
			assert.Equal(t, tc.code, response.Code)
			for _, key := range tc.dataKeys {
				assert.Contains(t, response.Data, key)
			}
			rows, channel := storedKeyEnvironmentCount(t, db, tc.request.Channel.Name)
			assert.Equal(t, tc.saved, channel != nil, "a rejected plan rolls the channel insert back")
			assert.Equal(t, tc.rows, rows)
			if channel != nil {
				setting := channel.GetSetting()
				assert.Len(t, setting.ClientDeviceSeed, 32, "saving the switch stores identity seeds")
				assert.Len(t, setting.ClientSessionSeed, 32)
			}
		})
	}
}

func TestUpdateKeyEnvironmentChannelRejectsAKeyAnotherChannelHolds(t *testing.T) {
	db := setupKeyEnvironmentControllerTest(t)
	const codex = `{"synthetic_client_headers_profile":"codex","key_environments":true}`
	status, _ := callChannelHandler(t, AddChannel, http.MethodPost, addKeyEnvironmentChannel("holder", constant.ChannelTypeOpenAI, codex, "sk-held", false))
	require.Equal(t, http.StatusOK, status)
	status, _ = callChannelHandler(t, AddChannel, http.MethodPost, addKeyEnvironmentChannel("editor", constant.ChannelTypeOpenAI, codex, "sk-own", false))
	require.Equal(t, http.StatusOK, status)
	_, holder := storedKeyEnvironmentCount(t, db, "holder")
	_, editor := storedKeyEnvironmentCount(t, db, "editor")

	update := map[string]any{"id": editor.Id, "key": "sk-held", "key_mode": "append"}
	status, response := callChannelHandler(t, UpdateChannel, http.MethodPut, update)
	assert.Equal(t, http.StatusConflict, status, response.Message)
	assert.Equal(t, "key_environment_conflict", response.Code)
	assert.EqualValues(t, holder.Id, response.Data["channel_id"])
	assert.Equal(t, "holder", response.Data["channel_name"])

	rows, stored := storedKeyEnvironmentCount(t, db, "editor")
	assert.Equal(t, int64(1), rows)
	assert.Equal(t, "sk-own", stored.Key, "the rejected key edit is rolled back")
}

func TestAddKeyEnvironmentChannelAcceptsTLSAndWebSocket(t *testing.T) {
	db := setupKeyEnvironmentControllerTest(t)
	const settings = `{"synthetic_client_headers_profile":"codex","key_environments":true,"cli_version_profile":"codex-0.163.0-linux-x64","tls_fingerprint":"claude-code","websocket_transport":true}`
	status, response := callChannelHandler(t, AddChannel, http.MethodPost,
		addKeyEnvironmentChannel("task21-transports", constant.ChannelTypeOpenAI, settings, "sk-task21-a\nsk-task21-b", false))
	require.Equal(t, http.StatusOK, status, response.Message)
	require.True(t, response.Success, response.Message)
	count, saved := storedKeyEnvironmentCount(t, db, "task21-transports")
	require.NotNil(t, saved)
	assert.Equal(t, int64(2), count)
	assert.True(t, saved.GetSetting().WebsocketTransport)
	assert.Equal(t, "claude-code", saved.GetSetting().TLSFingerprint, "the saved channel override is retained for use outside independent mode")
}

func TestValidateKeyEnvironmentSettings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		channelType int
		multiKey    bool
		setting     string
		header      string
		valid       bool
	}{
		{"codex on an OpenAI channel", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true}`, "", true},
		{"TLS and WebSocket together", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true,"tls_fingerprint":"claude-code","websocket_transport":true}`, "", true},
		{"claude 1h on an Anthropic channel", constant.ChannelTypeAnthropic, true, `{"synthetic_client_headers_profile":"claude","key_environments":true,"key_environment_cache_ttl":"1h"}`, "", true},
		{"single-key channel", constant.ChannelTypeOpenAI, false, `{"synthetic_client_headers_profile":"codex","key_environments":true}`, "", false},
		{"no client profile", constant.ChannelTypeOpenAI, true, `{"key_environments":true}`, "", false},
		{"codex on an Anthropic channel", constant.ChannelTypeAnthropic, true, `{"synthetic_client_headers_profile":"codex","key_environments":true}`, "", false},
		{"1h for codex", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true,"key_environment_cache_ttl":"1h"}`, "", false},
		{"separate TLS override", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true,"tls_fingerprint":"codex-cli"}`, "", true},
		{"WebSocket transport", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true,"websocket_transport":true}`, "", true},
		{"user-agent override", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true}`, `{"User-Agent":"x"}`, false},
		{"captured header override", constant.ChannelTypeAnthropic, true, `{"synthetic_client_headers_profile":"claude","key_environments":true}`, `{"x-app":"cli"}`, false},
		{"session header override", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true}`, `{"session_id":"fixed"}`, false},
		{"unrelated header override", constant.ChannelTypeOpenAI, true, `{"synthetic_client_headers_profile":"codex","key_environments":true}`, `{"x-tenant":"a"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := &model.Channel{Type: tc.channelType, Setting: &tc.setting, ChannelInfo: model.ChannelInfo{IsMultiKey: tc.multiKey}}
			if tc.header != "" {
				channel.HeaderOverride = &tc.header
			}
			err := validateKeyEnvironmentSettings(channel, tc.multiKey, nil)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
