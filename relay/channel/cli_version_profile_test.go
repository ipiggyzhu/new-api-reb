package channel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCLIVersionRelayPreservesHistoryEffortAndBindsHeaders(t *testing.T) {
	old := appcommon.TLSInsecureSkipVerify
	appcommon.TLSInsecureSkipVerify = true
	service.InitHttpClient()
	service.ResetProxyClientCache()
	t.Cleanup(func() { appcommon.TLSInsecureSkipVerify = old; service.ResetProxyClientCache() })
	type capture struct {
		header http.Header
		body   []byte
	}
	captures := make(chan capture, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captures <- capture{r.Header.Clone(), body}
		_, _ = io.WriteString(w, `{"reasoning":{"effort":"low"}}`)
	}))
	defer server.Close()
	for _, option := range service.SupportedCLIVersionProfiles() {
		t.Run(option.ID, func(t *testing.T) {
			p := service.LookupCLIVersionProfile(option.ID)
			c, info := cliTestRequest(p.Family)
			info.IsStream = true
			info.ChannelSetting.CLIVersionProfile = p.ID
			path := "/v1/messages"
			input := `{"model":"claude-opus-4-6","messages":[{"role":"user","content":"keep context"}],"tools":[{"name":"custom","input_schema":{"type":"object","properties":{"z":{"const":9007199254740993},"input":{"type":"object","properties":{}}}}}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`
			if p.Family == "codex" {
				path = "/v1/responses"
				input = `{"model":"gpt-6-astra","input":[{"type":"message","id":"msg_keep","role":"user","content":[{"type":"input_text","text":"keep context"}]},{"type":"reasoning","id":"rs_keep","encrypted_content":"keep-ciphertext","summary":[]},{"type":"function_call","call_id":"call_keep","name":"custom","arguments":"{}"},{"type":"function_call_output","call_id":"call_keep","output":"keep result"}],"tools":[{"type":"function","name":"custom","parameters":{"type":"object","properties":{"z":{"const":9007199254740993},"input":{"type":"object","properties":{}}}}}],"reasoning":{"effort":"xhigh","context":"all_turns"}}`
			}
			response, err := DoApiRequest(cliTransportAdaptor{url: server.URL + path}, c, info, strings.NewReader(input))
			require.NoError(t, err)
			reply, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Contains(t, string(reply), `"effort":"low"`, "must not falsify the upstream effort")
			got := <-captures
			for name, value := range p.Headers {
				assert.Equal(t, value, got.header.Get(name), name)
			}
			assert.Equal(t, "Bearer channel-test-key", got.header.Get("Authorization"))
			var actualOrder, expectedOrder []string
			gjson.ParseBytes(got.body).ForEach(func(key, _ gjson.Result) bool {
				actualOrder = append(actualOrder, key.String())
				return true
			})
			for _, key := range p.BodyOrder[""] {
				if gjson.GetBytes(got.body, key).Exists() {
					expectedOrder = append(expectedOrder, key)
				}
			}
			assert.Equal(t, expectedOrder, actualOrder)
			assert.Contains(t, string(got.body), "9007199254740993")
			if p.Family == "claude" {
				assert.Equal(t, p.Headers["x-stainless-package-version"], got.header.Get("x-stainless-package-version"))
				assert.Equal(t, p.Headers["x-stainless-runtime-version"], got.header.Get("x-stainless-runtime-version"))
				assert.Equal(t, p.Headers["anthropic-beta"], got.header.Get("anthropic-beta"))
				assert.Equal(t, int64(p.MaxTokens), gjson.GetBytes(got.body, "max_tokens").Int())
				assert.Equal(t, "high", gjson.GetBytes(got.body, "output_config.effort").String())
				assert.Equal(t, "keep context", gjson.GetBytes(got.body, "messages.0.content.0.text").String())
				assert.Equal(t, p.LegacyUserID, strings.HasPrefix(gjson.GetBytes(got.body, "metadata.user_id").String(), "user_"))
			} else {
				assert.Equal(t, "xhigh", gjson.GetBytes(got.body, "reasoning.effort").String())
				items := gjson.GetBytes(got.body, "input").Array()
				if p.ToolsInInput {
					assert.Equal(t, "additional_tools", items[0].Get("type").String())
					assert.False(t, gjson.GetBytes(got.body, "tools").Exists())
					items = items[1:]
				} else {
					assert.True(t, gjson.GetBytes(got.body, "tools").IsArray())
				}
				originals := gjson.Get(input, "input").Array()
				require.Len(t, items, len(originals))
				for i, item := range items {
					assert.JSONEq(t, originals[i].Raw, item.Raw)
				}
				assert.Equal(t, p.Headers["x-openai-internal-codex-responses-lite"], got.header.Get("x-openai-internal-codex-responses-lite"))
				assert.Equal(t, p.Headers["x-codex-beta-features"], got.header.Get("x-codex-beta-features"))
				metadata := gjson.GetBytes(got.body, "client_metadata").Map()
				assert.Len(t, metadata, len(p.MetadataKeys))
			}
		})
	}
}

func TestVersionedCLIOrderingKeepsUserSchemaAndUltra(t *testing.T) {
	p := service.LookupCLIVersionProfile("codex-0.160.0-windows-x64")
	const reference = `{"reasoning":{"effort":"ultra"},"tools":[{"type":"function","parameters":{"properties":{"input":{"content":1,"type":2},"tools":{"parameters":1,"type":2}},"type":"object"},"name":"custom"}]}`
	var body cliObject
	require.NoError(t, appcommon.Unmarshal([]byte(reference), &body))
	current, err := appcommon.Marshal(body)
	require.NoError(t, err)
	out := orderVersionedCLIJSON(gjson.ParseBytes(current), gjson.Parse(reference), "", p)
	assert.Equal(t, gjson.Get(reference, "tools.0.parameters").Raw, gjson.GetBytes(out, "tools.0.parameters").Raw)
	assert.Equal(t, "ultra", gjson.GetBytes(out, "reasoning.effort").String())
}

func TestCodexNamespaceDescriptionCompatibility(t *testing.T) {
	for _, profile := range []string{"", "codex-0.150.0-windows-x64", "codex-0.160.0-windows-x64"} {
		for _, tc := range []struct{ name, field, want string }{
			{"missing", "", "Tools in the probe namespace."},
			{"empty", `,"description":""`, "Tools in the probe namespace."},
			{"blank", `,"description":"  "`, "Tools in the probe namespace."},
			{"present", `,"description":"Caller description"`, "Caller description"},
		} {
			t.Run(profile+"/"+tc.name, func(t *testing.T) {
				c, info := cliTestRequest("codex")
				info.ChannelSetting.CLIVersionProfile = profile
				body := `{"model":"gpt-6-astra","reasoning":{"effort":"xhigh"},"input":[{"type":"reasoning","id":"rs_keep","encrypted_content":"keep-ciphertext","summary":[]}],"tools":[{"type":"namespace","name":"probe"` + tc.field + `,"tools":[{"type":"function","name":"ping","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}},"strict":false}]}]}`
				_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
				require.NoError(t, err)
				data, err := io.ReadAll(reader)
				require.NoError(t, err)
				path, history := "input.0.tools", "input.1"
				if p := service.LookupCLIVersionProfile(profile); p != nil && !p.ToolsInInput {
					path, history = "tools", "input.0"
				}
				assert.Equal(t, tc.want, gjson.GetBytes(data, path+".0.description").String())
				assert.Equal(t, "probe", gjson.GetBytes(data, path+".0.name").String())
				assert.False(t, gjson.GetBytes(data, path+".0.tools.0.description").Exists(), "function descriptions are optional")
				assert.Equal(t, "9007199254740993", gjson.GetBytes(data, path+".0.tools.0.parameters.properties.n.const").Raw)
				assert.Equal(t, "false", gjson.GetBytes(data, path+".0.tools.0.strict").Raw)
				assert.JSONEq(t, gjson.Get(body, "input.0").Raw, gjson.GetBytes(data, history).Raw)
				assert.Equal(t, "xhigh", gjson.GetBytes(data, "reasoning.effort").String())
			})
		}
	}
}

func TestCodexOlderProfileImportsNamespaceDescription(t *testing.T) {
	c, info := cliTestRequest("codex")
	info.ChannelSetting.CLIVersionProfile = "codex-0.150.0-windows-x64"
	body := `{"model":"gpt-6-astra","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"probe","description":"","tools":[{"type":"function","name":"ping","parameters":{"type":"object","properties":{}},"strict":false}]}]},{"type":"function_call_output","call_id":"call_keep","output":"previous result"}],"reasoning":{"effort":"xhigh"}}`
	_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "Tools in the probe namespace.", gjson.GetBytes(data, "tools.0.description").String())
	assert.JSONEq(t, gjson.Get(body, "input.0.tools.0.tools").Raw, gjson.GetBytes(data, "tools.0.tools").Raw)
	assert.JSONEq(t, gjson.Get(body, "input.1").Raw, gjson.GetBytes(data, "input.0").Raw)
	assert.Equal(t, "xhigh", gjson.GetBytes(data, "reasoning.effort").String())
}

func TestCodexNamespaceDescriptionInHistoricalToolSnapshots(t *testing.T) {
	for _, profile := range []string{"", "codex-0.156.1-windows-x64", "codex-0.160.0-windows-x64"} {
		t.Run(profile, func(t *testing.T) {
			c, info := cliTestRequest("codex")
			info.ChannelSetting.CLIVersionProfile = profile
			body := `{"model":"gpt-6-astra","input":[{"type":"additional_tools","id":"at_first","tools":[{"type":"namespace","name":"probe","description":"","tools":[{"type":"function","name":"ping","parameters":{"type":"object","properties":{}}}]}]},{"type":"function_call_output","call_id":"call_keep","output":"previous result"},{"type":"additional_tools","id":"at_later","tools":[{"type":"namespace","name":"later","description":"Keep this description","tools":[{"type":"custom","name":"patch","format":{"type":"text"}}]}]}],"reasoning":{"effort":"xhigh"}}`
			_, reader, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Len(t, gjson.GetBytes(data, "input").Array(), 3)
			assert.Equal(t, "at_first", gjson.GetBytes(data, "input.0.id").String())
			assert.Equal(t, "Tools in the probe namespace.", gjson.GetBytes(data, "input.0.tools.0.description").String())
			assert.JSONEq(t, gjson.Get(body, "input.0.tools.0.tools").Raw, gjson.GetBytes(data, "input.0.tools.0.tools").Raw)
			assert.JSONEq(t, gjson.Get(body, "input.1").Raw, gjson.GetBytes(data, "input.1").Raw)
			assert.JSONEq(t, gjson.Get(body, "input.2").Raw, gjson.GetBytes(data, "input.2").Raw)
			assert.False(t, gjson.GetBytes(data, "tools").Exists())
			assert.Equal(t, "xhigh", gjson.GetBytes(data, "reasoning.effort").String())
			_, again, _, err := prepareCLIRequest(c, info, "https://upstream.test/v1/responses", strings.NewReader(string(data)))
			require.NoError(t, err)
			second, err := io.ReadAll(again)
			require.NoError(t, err)
			assert.JSONEq(t, string(data), string(second), "retries must not change tool snapshots")
		})
	}
}
