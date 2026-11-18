package channel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A profile that sends X-OpenAI-Internal-Codex-Responses-Lite must also send
// reasoning.context "all_turns": the upstream answers 400 without it, and a
// caller that is not the Codex CLI itself does not set it.
func TestCodexLiteProfileAlwaysSendsReasoningContext(t *testing.T) {
	cases := []struct{ name, profile, body, effort string }{
		{"effort only", "codex-0.160.0-windows-x64", `{"model":"gpt-6.2-astra","input":"hi","reasoning":{"effort":"max"}}`, "max"},
		{"no reasoning", "codex-0.163.0-linux-x64", `{"model":"gpt-6.2-astra","input":"hi"}`, ""},
		{"already set", "codex-0.163.0-linux-x64", `{"model":"gpt-6.2-astra","input":"hi","reasoning":{"effort":"low","context":"all_turns"}}`, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, captures := cliTLSUpstream(t)
			c, info := cliTestRequest("codex")
			info.ChannelSetting.CLIVersionProfile = tc.profile
			resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(tc.body))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			got := <-captures
			require.Equal(t, "true", got.header.Get("X-Openai-Internal-Codex-Responses-Lite"))
			assert.Equal(t, "all_turns", gjson.GetBytes(got.body, "reasoning.context").String())
			assert.Equal(t, tc.effort, gjson.GetBytes(got.body, "reasoning.effort").String())
		})
	}
}

// A version captured without that header keeps the caller's reasoning as is.
func TestCodexProfileWithoutLiteHeaderLeavesReasoningAlone(t *testing.T) {
	server, captures := cliTLSUpstream(t)
	c, info := cliTestRequest("codex")
	info.ChannelSetting.CLIVersionProfile = "codex-0.150.0-windows-x64"
	resp, err := DoApiRequest(cliTransportAdaptor{url: server.URL + "/v1/responses"}, c, info, strings.NewReader(`{"model":"gpt-6-astra","input":"hi","reasoning":{"effort":"high"}}`))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	got := <-captures
	assert.Empty(t, got.header.Get("X-Openai-Internal-Codex-Responses-Lite"))
	assert.False(t, gjson.GetBytes(got.body, "reasoning.context").Exists())
}
