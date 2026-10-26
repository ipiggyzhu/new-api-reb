package channel

import (
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// keyEnvironmentRequest is a Codex request on an independent key environment
// channel after channel setup selected key.
func keyEnvironmentRequest(key string, keyIndex, userId int) (*gin.Context, *relaycommon.RelayInfo) {
	c, info := cliTestRequest("codex")
	info.UserId = userId
	info.ChannelMultiKeyIndex = keyIndex
	info.ChannelIsMultiKey = true
	info.ChannelSetting.KeyEnvironments = true
	info.ChannelSetting.CLIVersionProfile = "codex-0.163.0-linux-x64"
	info.ChannelSetting.ClientDeviceSeed = "device-seed"
	info.ChannelSetting.ClientSessionSeed = "session-seed"
	appcommon.SetContextKey(c, constant.ContextKeyChannelKeyTag, model.ChannelKeyTag(key))
	return c, info
}

func TestKeyEnvironmentIdentityFollowsTheKey(t *testing.T) {
	identity := func(c *gin.Context, info *relaycommon.RelayInfo) cliIdentity {
		return requestCLIIdentity(c, info, constant.ClientHeaderFamilyCodex, cliObject{})
	}
	base := identity(keyEnvironmentRequest("sk-a", 0, 42))

	moved := identity(keyEnvironmentRequest("sk-a", 3, 42))
	assert.Equal(t, base.device, moved.device, "the key's position in the list does not change its device")
	assert.Equal(t, base.installation, moved.installation)
	assert.Equal(t, base.session, moved.session)

	otherUser := identity(keyEnvironmentRequest("sk-a", 0, 43))
	assert.Equal(t, base.device, otherUser.device, "every caller of one key shares the key's device")
	assert.Equal(t, base.installation, otherUser.installation)
	assert.NotEqual(t, base.session, otherUser.session, "sessions stay apart per user and token")

	otherKey := identity(keyEnvironmentRequest("sk-b", 0, 42))
	assert.NotEqual(t, base.device, otherKey.device, "another key is another device")
	assert.NotEqual(t, base.installation, otherKey.installation)

	// Without the switch the identity is the channel-level one, unchanged.
	c, info := keyEnvironmentRequest("sk-a", 3, 42)
	info.ChannelSetting.KeyEnvironments = false
	legacy := identity(c, info)
	scope := "cli:codex:9:3:42:7"
	assert.Equal(t, appcommon.GenerateHMACWithKey([]byte("device-seed"), scope+":device"), legacy.device)
	assert.Equal(t, isolatedCLIUUID("device-seed", scope+":installation", 4, 0), legacy.installation)
}

func TestKeyEnvironmentKeepsTheVersionHeadersFromGlobalOverrides(t *testing.T) {
	monitor := *operation_setting.GetMonitorSetting()
	monitor.ChannelTestClientHeaders = map[string]map[string]string{
		"*":     {"user-agent": "admin-ua", "x-tenant": "a"},
		"codex": {"originator": "admin-originator", "x-codex-beta-features": ""},
	}
	t.Cleanup(operation_setting.SetMonitorSettingForTest(monitor))

	c, info := keyEnvironmentRequest("sk-a", 0, 42)
	headers, err := processHeaderOverride(info, c)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(headers["user-agent"], "codex_exec/0.163.0"), headers["user-agent"])
	assert.Equal(t, "codex_exec", headers["originator"])
	assert.NotEmpty(t, headers["x-codex-beta-features"], "a global override cannot drop a version header either")
	assert.Equal(t, "a", headers["x-tenant"], "overrides of other headers still apply")

	info.ChannelSetting.KeyEnvironments = false
	headers, err = processHeaderOverride(info, c)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(headers["user-agent"], "codex_exec/0.163.0"))
	assert.Equal(t, "codex_exec", headers["originator"], "a channel-level version has the same protection")

	info.ChannelSetting.CLIVersionProfile = ""
	headers, err = processHeaderOverride(info, c)
	require.NoError(t, err)
	assert.Equal(t, "admin-ua", headers["user-agent"], "without a version profile the override behavior is unchanged")
	assert.Equal(t, "admin-originator", headers["originator"])
}

// The default codex affinity rule pass_headers the caller's User-Agent and
// session headers into the runtime header overrides. On an independent key
// environment channel the key's version and session still reach upstream.
func TestKeyEnvironmentKeepsTheKeyHeadersFromAffinityPassHeaders(t *testing.T) {
	var template map[string]any
	for _, rule := range operation_setting.GetChannelAffinitySetting().Rules {
		if rule.Name == "codex cli trace" {
			template = rule.ParamOverrideTemplate
		}
	}
	require.NotNil(t, template)

	server, captures := cliTLSUpstream(t)
	c, info := keyEnvironmentRequest("sk-a", 0, 42)
	caller := map[string]string{
		"User-Agent":        "codex_cli_rs/0.150.0 (Windows 10.0.26100; x86_64)",
		"Originator":        "codex_cli_rs",
		"Session_id":        "caller-session",
		"Thread-Id":         "caller-thread",
		"X-Codex-Window-Id": "caller-window",
		"X-OpenAI-Subagent": "review",
	}
	info.RequestHeaders = caller
	for name, value := range caller {
		c.Request.Header.Set(name, value)
	}
	info.ParamOverride = template
	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-5"}`), info)
	require.NoError(t, err)
	require.Equal(t, caller["User-Agent"], info.RuntimeHeadersOverride["user-agent"], "the template copies the caller's headers")

	resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	got := <-captures

	session := gjson.GetBytes(got.body, "client_metadata.session_id").String()
	require.NotEmpty(t, session)
	assert.True(t, strings.HasPrefix(got.header.Get("User-Agent"), "codex_exec/0.163.0"), got.header.Get("User-Agent"))
	assert.Equal(t, "codex_exec", got.header.Get("Originator"))
	assert.Equal(t, session, got.header.Get("Session-Id"))
	assert.Equal(t, session, got.header.Get("Thread-Id"))
	assert.Equal(t, session+":0", got.header.Get("X-Codex-Window-Id"))
	assert.Empty(t, got.header.Values("Session_id"), "the caller's session header does not travel next to the key's")
	assert.Equal(t, "review", got.header.Get("X-Openai-Subagent"), "headers the key does not own still pass")
}

func TestCLIProfileAuditRecordsVersionDrift(t *testing.T) {
	for _, tc := range []struct {
		name      string
		userAgent string
		keyEnv    bool
		bound     *service.KeyEnvironmentBinding
		want      map[string]any
	}{
		{"same version caller", "codex_exec/0.163.0 (Debian 13.0.0; x86_64)", true, &service.KeyEnvironmentBinding{ChannelId: 9, KeyTag: "t"},
			map[string]any{"cli_profile": "codex-0.163.0-linux-x64", "key_env_bound": true}},
		{"older native CLI", "codex_cli_rs/0.150.0 (Windows 10.0.26100; x86_64)", true, &service.KeyEnvironmentBinding{ChannelId: 9, KeyTag: "t"},
			map[string]any{"cli_profile": "codex-0.163.0-linux-x64", "key_env_bound": true,
				"cli_version_mismatch": map[string]string{"client": "0.150.0", "profile": "0.163.0"}}},
		{"served unbound after losing the claim", "codex_exec/0.163.0 (Debian 13.0.0; x86_64)", true, nil,
			map[string]any{"cli_profile": "codex-0.163.0-linux-x64", "key_env_bound": false}},
		{"bound to another channel", "codex_exec/0.163.0 (Debian 13.0.0; x86_64)", true, &service.KeyEnvironmentBinding{ChannelId: 10, KeyTag: "t"},
			map[string]any{"cli_profile": "codex-0.163.0-linux-x64", "key_env_bound": false}},
		{"SDK caller on an ordinary channel", "OpenAI/Python 2.1.0", false, nil,
			map[string]any{"cli_profile": "codex-0.163.0-linux-x64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, info := keyEnvironmentRequest("sk-a", 0, 42)
			info.ChannelSetting.KeyEnvironments = tc.keyEnv
			service.SetKeyEnvironmentBindingState(c, &service.KeyEnvironmentBindingState{Key: "kenv:test", Bound: tc.bound})
			c.Request.Header.Set("User-Agent", tc.userAgent)
			_, _, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
			require.NoError(t, err)
			audit, _ := c.Get(string(constant.ContextKeyCLIProfileAudit))
			assert.Equal(t, tc.want, audit)
		})
	}

	c, info := cliTestRequest("codex")
	c.Set(string(constant.ContextKeyCLIProfileAudit), map[string]any{"cli_profile": "stale"})
	_, _, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	require.NoError(t, err)
	audit, _ := c.Get(string(constant.ContextKeyCLIProfileAudit))
	assert.Nil(t, audit, "an attempt without a version profile clears the previous attempt's")
}
