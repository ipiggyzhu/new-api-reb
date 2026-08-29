package channel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leakyClientHeaders are the headers a real capture showed reaching upstream
// through "send original request" on 2026-08-02. The deny-list
// (shouldSkipPassthroughHeader) does not name any of these, which is the point:
// a deny-list only strips what someone thought of, so credentials keep leaking
// as upstream ecosystems invent new header names.
var leakyClientHeaders = map[string]string{
	// Credentials the deny-list does not cover.
	"anthropic-api-key": "CLIENT-SECRET-ANTHROPIC",
	"openai-api-key":    "CLIENT-SECRET-OPENAI",
	"x-auth-token":      "CLIENT-SECRET-AUTHTOKEN",
	"x-access-token":    "CLIENT-SECRET-ACCESSTOKEN",
	"x-session-token":   "CLIENT-SECRET-SESSION",
	// The caller's real IP.
	"x-forwarded-for":  "203.0.113.77",
	"x-real-ip":        "203.0.113.77",
	"cf-connecting-ip": "203.0.113.77",
	// Caller identity / internal detail.
	"user-agent":      "MySecretInternalTool/9.9",
	"referer":         "https://internal.example.com/secret-path",
	"x-custom-tenant": "acme-corp-internal",
}

func newHeaderOverrideTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for name, value := range leakyClientHeaders {
		c.Request.Header.Set(name, value)
	}
	return c
}

func newHeaderOverrideRelayInfo(setting dto.ChannelSettings) *common.RelayInfo {
	setting.Normalize()
	return &common.RelayInfo{
		ChannelMeta: &common.ChannelMeta{
			ApiType:        constant.APITypeOpenAI,
			ChannelSetting: setting,
		},
	}
}

// wildcardPassthroughInfo builds a channel whose header override carries the "*"
// passthrough rule. That rule is the surviving way to forward caller headers now
// that "send original request" no longer does, so the leak tests exercise it.
func wildcardPassthroughInfo(setting dto.ChannelSettings) *common.RelayInfo {
	info := newHeaderOverrideRelayInfo(setting)
	info.ChannelMeta.HeadersOverride = map[string]any{"*": ""}
	return info
}

// TestWildcardPassthroughLeaksClientHeaders documents the behavior that motivated
// the synthetic-header setting. It is not aspirational: it pins what passthrough
// does today so a future change to the deny-list shows up here.
func TestWildcardPassthroughLeaksClientHeaders(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := wildcardPassthroughInfo(dto.ChannelSettings{})

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	for name, value := range leakyClientHeaders {
		assert.Equal(t, value, got[name],
			"passthrough is expected to forward %q today; if this changed, update the docs on SyntheticClientHeadersProfile", name)
	}
}

// TestStaleSendOriginalRequestKeyIsInert pins the removal contract at the relay
// layer. The setting is gone from ChannelSettings, but rows saved before the
// removal still carry the key in their settings JSON — parsing one must succeed,
// and the resulting settings must put nothing of the caller's on the upstream
// request.
func TestStaleSendOriginalRequestKeyIsInert(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	var setting dto.ChannelSettings
	require.NoError(t, json.Unmarshal([]byte(`{"send_original_request":true}`), &setting))
	info := newHeaderOverrideRelayInfo(setting)

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	assert.Empty(t, got, "a stale send_original_request key must not forward caller headers")
}

// TestSyntheticClientHeadersBlocksEveryLeak is the actual guarantee: with a
// profile selected, not one caller-supplied header value reaches upstream even
// when the wildcard passthrough rule is also configured.
func TestSyntheticClientHeadersBlocksEveryLeak(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := wildcardPassthroughInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
	})

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	for name, value := range leakyClientHeaders {
		assert.NotEqual(t, value, got[name],
			"caller header %q must not be forwarded when a synthetic profile is set", name)
	}
	// Asserting on values rather than names alone: user-agent IS sent, but it must
	// be the synthesized one, never the caller's.
	require.NotEmpty(t, got["user-agent"])
	assert.NotContains(t, got["user-agent"], "MySecretInternalTool")
	assert.Contains(t, got["user-agent"], "OpenAI/Python",
		"an OpenAI-type channel should look like the official SDK")
}

// TestLegacySyntheticClientHeadersBoolStillApplies covers channels saved before
// the profile could be chosen. Their stored setting is the bare bool, and reading
// it as "off" would quietly drop the protection they were saved with.
func TestLegacySyntheticClientHeadersBoolStillApplies(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := wildcardPassthroughInfo(dto.ChannelSettings{SyntheticClientHeaders: true})

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	for name, value := range leakyClientHeaders {
		assert.NotEqual(t, value, got[name], "legacy bool must still block %q", name)
	}
	assert.Contains(t, got["user-agent"], "OpenAI/Python")
}

// TestSyntheticClientHeadersForcedFamilyOverridesChannelType is the reason the
// setting is a choice rather than a switch: an OpenAI-compatible endpoint can be
// a Claude relay that gates on claude-cli's user-agent, and the channel type
// cannot say so.
func TestSyntheticClientHeadersForcedFamilyOverridesChannelType(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := newHeaderOverrideRelayInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: constant.ClientHeaderFamilyClaude,
	})
	require.Equal(t, constant.APITypeOpenAI, info.ChannelMeta.ApiType)

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	assert.Contains(t, got["user-agent"], "claude-cli/",
		"a forced family must beat the channel's own API type")
	assert.Equal(t, "2023-06-01", got["anthropic-version"])
}

// TestSyntheticClientHeadersUnknownFamilyFallsBackToAuto: a typo must not be a
// silent downgrade. Normalize rewrites it to auto, so the worst case is the
// wrong user-agent — never the caller's headers reaching upstream again.
func TestSyntheticClientHeadersUnknownFamilyFallsBackToAuto(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := wildcardPassthroughInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: "claud",
	})

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	for name, value := range leakyClientHeaders {
		assert.NotEqual(t, value, got[name], "a typo'd family must not reopen %q", name)
	}
	assert.Contains(t, got["user-agent"], "OpenAI/Python",
		"auto resolves from the channel's API type")
}

// TestSyntheticClientHeadersBlocksClientHeaderPlaceholder closes the way back in:
// {client_header:<name>} copies a caller header by design, so it has to be
// ignored when synthetic headers are on or an explicit override would reopen the
// exact leak the setting removes.
func TestSyntheticClientHeadersBlocksClientHeaderPlaceholder(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := newHeaderOverrideRelayInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
	})
	info.ChannelMeta.HeadersOverride = map[string]any{
		"x-smuggled": "{client_header:x-auth-token}",
	}

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	assert.NotContains(t, got, "x-smuggled",
		"a client_header placeholder must not smuggle a caller header past synthetic headers")
	for _, value := range got {
		assert.NotEqual(t, "CLIENT-SECRET-AUTHTOKEN", value)
	}
}

// TestSyntheticClientHeadersKeepsStaticOverrides makes sure the block above is
// narrow: a static override is still an admin's explicit intent and must apply.
func TestSyntheticClientHeadersKeepsStaticOverrides(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := newHeaderOverrideRelayInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
	})
	info.ChannelMeta.HeadersOverride = map[string]any{
		"x-upstream-tag": "my-static-value",
	}

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	assert.Equal(t, "my-static-value", got["x-upstream-tag"])
}

// TestSyntheticClientHeadersUsesChannelFamily checks auto follows the channel's
// API type, so an Anthropic channel is not dressed as a Python SDK.
func TestSyntheticClientHeadersUsesChannelFamily(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := newHeaderOverrideRelayInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
	})
	info.ChannelMeta.ApiType = constant.APITypeAnthropic

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	assert.Contains(t, got["user-agent"], "claude-cli/")
	assert.Equal(t, "2023-06-01", got["anthropic-version"])
}

// TestChannelTestIgnoresSyntheticClientHeaders keeps the channel-test path
// unchanged: its request is synthetic already and applyTestClientHeaders owns it.
func TestChannelTestIgnoresSyntheticClientHeaders(t *testing.T) {
	c := newHeaderOverrideTestContext(t)
	info := newHeaderOverrideRelayInfo(dto.ChannelSettings{
		SyntheticClientHeadersProfile: dto.SyntheticClientHeadersProfileAuto,
	})
	info.IsChannelTest = true

	got, err := processHeaderOverride(info, c)
	require.NoError(t, err)

	assert.Empty(t, got, "a channel test must not get profile headers injected here")
}
