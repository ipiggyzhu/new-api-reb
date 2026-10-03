package channel

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// forceResponsesHighEffort runs after body shaping and header overrides on both
// transports. A nil result means the original reader was not consumed. Only the
// explicit channel opt-in takes precedence over a caller's effort/metadata;
// history, session identity and upstream responses are never rewritten here.
func forceResponsesHighEffort(info *relaycommon.RelayInfo, requestURL string, reader io.Reader, headers http.Header) ([]byte, error) {
	if info == nil || info.ChannelMeta == nil || !info.ChannelSetting.ResponsesForceHighEffort || reader == nil {
		return nil, nil
	}
	u, err := url.Parse(requestURL)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(u.Path, "/compact") ||
		(!strings.HasSuffix(u.Path, "/responses") && info.GetFinalRequestRelayFormat() != types.RelayFormatOpenAIResponses) {
		return nil, nil
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(data) || !gjson.ParseBytes(data).IsObject() {
		return nil, fmt.Errorf("fixed high effort requires a Responses JSON object")
	}
	if reasoning := gjson.GetBytes(data, "reasoning"); reasoning.Exists() && reasoning.Type != gjson.Null && !reasoning.IsObject() {
		return nil, fmt.Errorf("fixed high effort requires reasoning to be an object")
	}
	data, err = sjson.SetBytes(data, "reasoning.effort", "high")
	if err != nil {
		return nil, err
	}
	// GPT-6 configuration_update items override request-level effort. Keep
	// those explicit effort changes aligned without adding items or touching
	// messages, tool results or opaque reasoning history.
	if input := gjson.GetBytes(data, "input"); input.IsArray() {
		for i, item := range input.Array() {
			if item.Get("type").String() != "configuration_update" || !item.Get("reasoning.effort").Exists() {
				continue
			}
			data, err = sjson.SetBytes(data, fmt.Sprintf("input.%d.reasoning.effort", i), "high")
			if err != nil {
				return nil, err
			}
		}
	}
	const metadataKey = "x-codex-turn-metadata"
	const metadataPath = "client_metadata." + metadataKey
	if metadata := gjson.GetBytes(data, metadataPath); metadata.Exists() && metadata.Type != gjson.Null {
		if metadata.Type != gjson.String {
			return nil, fmt.Errorf("fixed high effort requires Codex turn metadata to be a JSON string")
		}
		value, err := highEffortTurnMetadata(metadata.String())
		if err != nil {
			return nil, err
		}
		data, err = sjson.SetBytes(data, metadataPath, value)
		if err != nil {
			return nil, err
		}
	}
	if metadata := headers.Get(metadataKey); metadata != "" {
		value, err := highEffortTurnMetadata(metadata)
		if err != nil {
			return nil, err
		}
		headers.Set(metadataKey, value)
	}
	info.ReasoningEffort = "high"
	info.UpstreamRequestBodySize = int64(len(data))
	return data, nil
}

func highEffortTurnMetadata(metadata string) (string, error) {
	// Validate the embedded JSON without float64 conversion or logging its data.
	if !gjson.Valid(metadata) || !gjson.Parse(metadata).IsObject() {
		return "", fmt.Errorf("fixed high effort requires valid Codex turn metadata")
	}
	return sjson.Set(metadata, "reasoning_effort", "high")
}
