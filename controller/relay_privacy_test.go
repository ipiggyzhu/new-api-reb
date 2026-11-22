package controller

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRelayPrivacyRealHTTPRetryPassthroughAndToolContinuation(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "converted", true: "passthrough"}[passthrough], func(t *testing.T) {
			original := *system_setting.GetPrivacySetting()
			*system_setting.GetPrivacySetting() = system_setting.PrivacySetting{ProtectIPs: true, RestoreToolArguments: true}
			t.Cleanup(func() { *system_setting.GetPrivacySetting() = original })
			var mu sync.Mutex
			var captures [][]byte
			server, db := setupRelayHTTPTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				captures = append(captures, body)
				attempt := len(captures)
				mu.Unlock()
				assert.NotContains(t, string(body), "fixture-ssh-pass")
				assert.NotContains(t, string(body), "203.0.113.41")
				assert.Empty(t, r.Header.Get("X-New-Api-Privacy-Session"))
				assert.Empty(t, r.Header.Get("X-New-Api-Privacy"))
				assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer cross-group-"), "provider authentication must survive privacy filtering")
				if attempt == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "fixture retry")
					return
				}
				content := gjson.GetBytes(body, "messages.0.content").String()
				arguments, err := common.Marshal(map[string]string{"command": content})
				if !assert.NoError(t, err) {
					return
				}
				response, err := common.Marshal(map[string]any{
					"id": "chatcmpl-privacy", "object": "chat.completion",
					"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_fixture", "type": "function", "function": map[string]any{"name": "run_command", "arguments": string(arguments)}}}}, "finish_reason": "tool_calls"}},
					"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
				})
				if !assert.NoError(t, err) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(response)
			}), 1, 0)
			settings, err := common.Marshal(dto.ChannelSettings{PassThroughBodyEnabled: passthrough})
			require.NoError(t, err)
			require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{81, 82}).Update("setting", string(settings)).Error)
			model.InitChannelCache()
			call := func(content, sessionID string) (http.Header, string) {
				body, err := common.Marshal(map[string]any{"model": taskRoutingModel, "messages": []any{map[string]string{"role": "user", "content": content}}, "max_tokens": 8})
				require.NoError(t, err)
				request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(string(body)))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer sk-gatewayhttptest")
				request.Header.Set("X-New-Api-Privacy", "on")
				request.Header.Set("X-New-Api-Privacy-Session", sessionID)
				response, err := server.Client().Do(request)
				require.NoError(t, err)
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode, string(data))
				return response.Header, string(data)
			}
			headers, result := call("连接 203.0.113.41，密码是 fixture-ssh-pass 检查磁盘", "")
			assert.Equal(t, "restore", headers.Get("X-New-Api-Privacy-Tools"))
			args := gjson.Get(result, "choices.0.message.tool_calls.0.function.arguments").String()
			require.True(t, gjson.Valid(args))
			assert.Contains(t, gjson.Get(args, "command").String(), "fixture-ssh-pass")
			assert.Contains(t, gjson.Get(args, "command").String(), "203.0.113.41")
			_, result = call("工具输出：fixture-ssh-pass", headers.Get("X-New-Api-Privacy-Session"))
			assert.Contains(t, result, "fixture-ssh-pass")
			mu.Lock()
			assert.Len(t, captures, 3, "first failure retries once, next turn succeeds")
			assert.JSONEq(t, string(captures[0]), string(captures[1]), "retry must reuse protected content")
			mu.Unlock()
			var user model.User
			require.NoError(t, db.First(&user, 991).Error)
			assert.Equal(t, relayHTTPInitialQuota-20, user.Quota, "two successes billed once each; failed attempt is not billed")
			var logs []model.Log
			require.NoError(t, db.Find(&logs).Error)
			encoded, err := common.Marshal(logs)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "fixture-ssh-pass")
			assert.NotContains(t, string(encoded), "203.0.113.41")
		})
	}
}
