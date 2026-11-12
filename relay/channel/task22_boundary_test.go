package channel

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"strings"
	"testing"
)

func TestClaudeBillingCacheAndLengthBoundary(t *testing.T) {
	for _, count := range []int{0, 10} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("count=%d/native=%t", count, native), func(t *testing.T) {
				blocks := make([]map[string]any, 0, count+1)
				for i := 0; i < count; i++ {
					blocks = append(blocks, map[string]any{"type": "text", "text": "x-anthropic-billing-header: " + strings.Repeat("b", 65536), "cache_control": map[string]string{"type": "ephemeral", "ttl": "1h"}})
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": "system"})
				raw, err := common.Marshal(map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 512, "system": blocks, "messages": []map[string]any{{"role": "user", "content": "hello"}}})
				require.NoError(t, err)
				c, info := nativeClaudeCodeRequest()
				if !native {
					c.Request.Header.Set("User-Agent", "Anthropic/Python 0.60.0")
				}
				info.ChannelSetting.CLIVersionProfile = "claude-2.1.211-windows-x64"
				withOriginalRequest(t, c, string(raw))
				shaped := shapeClaudeMessagesBody(t, c, info, raw)
				actual := gjson.GetBytes(shaped, "system").Array()
				require.Len(t, actual, count+1)
				for i := 0; i < count; i++ {
					assert.Equal(t, blocks[i]["text"], actual[i].Get("text").String())
					assert.Equal(t, "1h", actual[i].Get("cache_control.ttl").String())
				}
				again := shapeClaudeMessagesBody(t, c, info, shaped)
				assert.Equal(t, string(shaped), string(again))
				if count == 10 {
					assert.Greater(t, len(gjson.GetBytes(shaped, "system.#(cache_control)#").Array()), 4, "caller-owned cache blocks exceed provider limit; shaping preserves them")
				}
			})
		}
	}
}

func TestProtectedHeaderValuesBoundary(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	for _, value := range []string{"", strings.Repeat("x", 65536)} {
		c, info := cliTestRequest("codex")
		info.ChannelSetting.CLIVersionProfile = "codex-0.163.0-linux-x64"
		info.HeadersOverride = map[string]any{" USER-AGENT ": value, "User-Agent": value, "uSeR-aGeNt": value}
		c.Request.Header["User-Agent"] = []string{"caller-first", "caller-second"}
		resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		got := <-captures
		profile := service.LookupCLIVersionProfile(info.ChannelSetting.CLIVersionProfile)
		require.NotNil(t, profile)
		assert.Equal(t, profile.Headers["user-agent"], got.header.Get("User-Agent"))
		assert.Len(t, got.header.Values("User-Agent"), 1)
	}
}

func TestBillingAndExistingCacheBreakpointsBoundary(t *testing.T) {
	blocks := []map[string]any{{"type": "text", "text": "x-anthropic-billing-header: caller;", "cache_control": map[string]string{"type": "ephemeral", "ttl": "1h"}}}
	for i := 0; i < 4; i++ {
		blocks = append(blocks, map[string]any{"type": "text", "text": fmt.Sprintf("system %d", i), "cache_control": map[string]string{"type": "ephemeral", "ttl": "1h"}})
	}
	raw, err := common.Marshal(map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 512, "system": blocks, "messages": []map[string]any{{"role": "user", "content": "hello"}}})
	require.NoError(t, err)
	c, info := nativeClaudeCodeRequest()
	info.ChannelSetting.CLIVersionProfile = "claude-2.1.211-windows-x64"
	withOriginalRequest(t, c, string(raw))
	shaped := shapeClaudeMessagesBody(t, c, info, raw)
	assert.Equal(t, map[string]string{"system.0.cache_control": "ephemeral 1h"}, claudeCacheBreakpoints(t, shaped), "only the opaque billing block preserves caller cache policy")
	assert.Equal(t, string(shaped), string(shapeClaudeMessagesBody(t, c, info, shaped)))
}

func TestEmptyProfileHeaderCollectionBoundary(t *testing.T) {
	profile := service.LookupCLIVersionProfile("codex-0.163.0-linux-x64")
	require.NotNil(t, profile)
	original := profile.Headers
	profile.Headers = nil
	t.Cleanup(func() { profile.Headers = original })
	server, captures := cliTLSUpstream(t)
	c, info := cliTestRequest("codex")
	info.ChannelSetting.CLIVersionProfile = profile.ID
	info.HeadersOverride = map[string]any{"User-Agent": "caller-attempt", "X-Tenant": "tenant"}
	resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	got := <-captures
	assert.NotEqual(t, "caller-attempt", got.header.Get("User-Agent"))
	assert.Equal(t, "tenant", got.header.Get("X-Tenant"))
}
