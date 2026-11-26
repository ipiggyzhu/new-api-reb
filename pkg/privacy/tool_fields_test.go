package privacy

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestToolDefinitionsAndCallsAcrossProtocols(t *testing.T) {
	type obj = map[string]any
	const runtimeValue = "fixture-runtime-value"
	const defaultValue = "fixture-default-value"
	const docValue = "fixture-doc-value"
	const description = "Read README.md before using this tool. Example password=" + docValue
	schema := obj{
		"type": "object", "required": []string{"password", "file"},
		"properties": obj{"password": obj{"type": "string", "default": defaultValue}, "file": obj{"type": "string"}},
	}
	args := obj{"password": runtimeValue, "name": runtimeValue, "file": runtimeValue}
	argsJSON, err := common.Marshal(args)
	require.NoError(t, err)
	function := obj{"name": "connect", "description": description, "parameters": schema}
	for _, tc := range []struct {
		name, schemaPath, namePath, outputPath, argsPath string
		body                                             obj
	}{
		{"chat", "tools.0.function.parameters", "tools.0.function.name", "messages.1.content", "messages.0.tool_calls.0.function.arguments",
			obj{"tools": []any{obj{"type": "function", "function": function}}, "messages": []any{
				obj{"role": "assistant", "tool_calls": []any{obj{"id": "call-1", "type": "function", "function": obj{"name": "connect", "arguments": string(argsJSON)}}}},
				obj{"role": "tool", "tool_call_id": "call-1", "content": string(argsJSON)},
			}}},
		{"responses", "tools.0.tools.0.parameters", "tools.0.tools.0.name", "input.1.output", "input.0.arguments",
			obj{"tools": []any{obj{"type": "namespace", "name": "catalog", "tools": []any{obj{"type": "function", "name": "connect", "description": description, "parameters": schema}}}}, "input": []any{
				obj{"type": "function_call", "call_id": "call-1", "name": "connect", "arguments": string(argsJSON)},
				obj{"type": "function_call_output", "call_id": "call-1", "output": string(argsJSON)},
			}}},
		{"messages", "tools.0.input_schema", "tools.0.name", "messages.1.content.0.content", "messages.0.content.0.input",
			obj{"tools": []any{obj{"name": "connect", "description": description, "input_schema": schema}}, "messages": []any{
				obj{"role": "assistant", "content": []any{obj{"type": "tool_use", "id": "call-1", "name": "connect", "input": args}}},
				obj{"role": "user", "content": []any{obj{"type": "tool_result", "tool_use_id": "call-1", "content": string(argsJSON)}}},
			}}},
		{"gemini", "tools.0.functionDeclarations.0.parameters", "tools.0.functionDeclarations.0.name", "contents.1.parts.0.functionResponse.response", "contents.0.parts.0.functionCall.args",
			obj{"tools": []any{obj{"functionDeclarations": []any{function}}}, "contents": []any{
				obj{"role": "model", "parts": []any{obj{"functionCall": obj{"name": "connect", "args": args}}}},
				obj{"role": "user", "parts": []any{obj{"functionResponse": obj{"name": "connect", "response": args}}}},
			}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := common.Marshal(tc.body)
			require.NoError(t, err)
			require.NoError(t, ValidateTextRequest(raw))
			s := NewSession()
			protected, err := s.ProtectJSON(raw, Options{})
			require.NoError(t, err)
			for _, v := range []string{runtimeValue, defaultValue, docValue} {
				assert.NotContains(t, string(protected), v)
			}
			assert.Equal(t, "connect", gjson.GetBytes(protected, tc.namePath).String())
			assert.Equal(t, "password", gjson.GetBytes(protected, tc.schemaPath+".required.0").String())
			assert.Equal(t, "string", gjson.GetBytes(protected, tc.schemaPath+".properties.file.type").String())
			assert.Len(t, s.Values, 3)
			for _, path := range []string{tc.argsPath, tc.outputPath} {
				data := gjson.Parse(gjson.GetBytes(protected, path).String())
				for _, key := range []string{"password", "file", "name"} {
					assert.Equal(t, s.Mask(runtimeValue), data.Get(key).String(), path)
				}
			}
			c := &gin.Context{}
			c.Set(ContextKey, s)
			out, err := ProtectOutboundJSON(c, protected)
			require.NoError(t, err)
			assert.JSONEq(t, string(protected), string(out))
			require.NoError(t, CheckOutboundJSON(c, out))
			assert.Error(t, CheckOutboundJSON(c, raw))
			restored, err := s.RestoreJSON(out, true)
			require.NoError(t, err)
			assert.JSONEq(t, string(raw), string(restored))
		})
	}
}

func TestLargeToolCatalogLeavesRoomForActualCredentials(t *testing.T) {
	var tools []map[string]any
	for i := 0; i < MaxSecrets+1; i++ {
		tools = append(tools, map[string]any{
			"type": "function", "name": fmt.Sprintf("operation_%d", i),
			"description": fmt.Sprintf("README.md section%d describes the arguments and return value.", i),
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"password": map[string]string{"type": "string"}}, "required": []string{"password"}},
		})
	}
	raw, err := common.Marshal(map[string]any{"tools": tools, "input": "password=fixture-real-value"})
	require.NoError(t, err)
	s := NewSession()
	out, err := s.ProtectJSON(raw, Options{})
	require.NoError(t, err)
	assert.Len(t, s.Values, 1)
	assert.NotContains(t, string(out), "fixture-real-value")
	assert.JSONEq(t, gjson.GetBytes(raw, "tools").Raw, gjson.GetBytes(out, "tools").Raw)
}

func TestToolResultMediaAndTextBoundaries(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}},{"type":"text","text":"password=fixture-value"}]}]}]}`)
	require.ErrorIs(t, ValidateTextRequest(raw), ErrUnsupported)
	s := NewSession()
	s.BestEffort = true
	protected, err := s.ProtectJSON(raw, Options{})
	require.NoError(t, err)
	assert.JSONEq(t, gjson.GetBytes(raw, "messages.0.content.0.content.0").Raw, gjson.GetBytes(protected, "messages.0.content.0.content.0").Raw)
	assert.NotContains(t, string(protected), "fixture-value")
}

func TestPrivacyMetadataKeepsNamesAndMasksEncodedValues(t *testing.T) {
	s := NewSession()
	require.NoError(t, s.AddDeclared("installation_id"))
	require.NoError(t, s.AddDeclared("fixture-secret"))
	c := &gin.Context{}
	c.Set(ContextKey, s)
	const metadata = `{ "installation_id":"fixture-secret", "note":"ok" }`
	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", metadata)
	require.NoError(t, ProtectHeaders(c, headers))
	got := headers.Get("X-Codex-Turn-Metadata")
	assert.Equal(t, s.Mask("fixture-secret"), gjson.Get(got, "installation_id").String())
	assert.NotContains(t, got, "fixture-secret")
	require.NoError(t, ProtectHeaders(c, headers))
	assert.Equal(t, got, headers.Get("X-Codex-Turn-Metadata"))
	const unchanged = `{"model":"model","input":"hi"}`
	src := []byte(unchanged)
	protected, err := s.ProtectJSON(src, Options{})
	require.NoError(t, err)
	clear(src)
	assert.Equal(t, unchanged, string(protected))
}

func TestToolResultListingAndAdjacentCredentials(t *testing.T) {
	type obj = map[string]any
	const listing = "access.log\r\npreview.PNG\r\nreport\r\nhost.example\r\nfixture-login\r\npassword=fixture-explicit"
	encoded, err := common.Marshal(obj{"stdout": listing})
	require.NoError(t, err)
	for name, body := range map[string]obj{
		"claude":    {"messages": []any{obj{"role": "user", "content": []any{obj{"type": "tool_result", "content": []any{obj{"type": "text", "text": listing}}}}}}},
		"chat":      {"messages": []any{obj{"role": "tool", "content": listing}}},
		"responses": {"input": []any{obj{"type": "function_call_output", "output": string(encoded)}}},
		"gemini":    {"contents": []any{obj{"parts": []any{obj{"functionResponse": obj{"response": obj{"stdout": listing}}}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := common.Marshal(body)
			require.NoError(t, err)
			s := NewSession()
			out, err := s.ProtectJSON(raw, Options{})
			require.NoError(t, err)
			assert.Contains(t, string(out), "report")
			assert.NotContains(t, string(out), "fixture-login")
			assert.NotContains(t, string(out), "fixture-explicit")
			c := &gin.Context{}
			c.Set(ContextKey, s)
			require.NoError(t, CheckOutboundJSON(c, []byte(`{"system":"report generation"}`)))
			require.Error(t, CheckOutboundJSON(c, []byte(`{"system":"fixture-login"}`)))
			require.NoError(t, s.AddDeclared("report"))
			require.Error(t, CheckOutboundJSON(c, []byte(`{"system":"report generation"}`)))
		})
	}
}

func TestToolDefinitionCompactLoginAndDeclaredArguments(t *testing.T) {
	const arguments = `{"mode":"production"}`
	raw, err := common.Marshal(map[string]any{
		"tools": []any{map[string]string{"type": "function", "name": "connect", "description": "Connect to service.example alice maple93 then read the report"}},
		"input": []any{map[string]string{"type": "function_call", "call_id": "call-1", "name": "connect", "arguments": arguments}},
	})
	require.NoError(t, err)
	s := NewSession()
	require.NoError(t, s.AddDeclared(arguments))
	out, err := s.ProtectJSON(raw, Options{})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "maple93")
	assert.Contains(t, gjson.GetBytes(out, "tools.0.description").String(), "then read the report")
	args := gjson.GetBytes(out, "input.0.arguments").String()
	assert.Equal(t, s.Mask(arguments), args)
	assert.Equal(t, arguments, s.Restore(args))
}
