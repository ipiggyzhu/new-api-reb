package relay

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestChatCompletionsUseResponsesOnPinnedCodexChannel(t *testing.T) {
	info := func(apiType int, setting dto.ChannelSettings) *relaycommon.RelayInfo {
		return &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ApiType: apiType, ChannelSetting: setting}}
	}
	codex := constant.ClientHeaderFamilyCodex
	require.True(t, chatCompletionsUseResponses(info(constant.APITypeOpenAI, dto.ChannelSettings{SyntheticClientHeadersProfile: codex, CLIVersionProfile: "codex-0.160.0-windows-x64"})))
	require.True(t, chatCompletionsUseResponses(info(constant.APITypeOpenAI, dto.ChannelSettings{SyntheticClientHeadersProfile: codex, KeyEnvironments: true})))
	require.False(t, chatCompletionsUseResponses(info(constant.APITypeOpenAI, dto.ChannelSettings{SyntheticClientHeadersProfile: codex})), "family alone only changes headers")
	require.False(t, chatCompletionsUseResponses(info(constant.APITypeOpenAI, dto.ChannelSettings{})))
	require.False(t, chatCompletionsUseResponses(info(constant.APITypeOpenAI, dto.ChannelSettings{SyntheticClientHeadersProfile: constant.ClientHeaderFamilyClaude, CLIVersionProfile: "claude-2.1.306-linux-x64"})))
	require.False(t, chatCompletionsUseResponses(info(constant.APITypeAnthropic, dto.ChannelSettings{SyntheticClientHeadersProfile: codex, CLIVersionProfile: "codex-0.160.0-windows-x64"})))
}
