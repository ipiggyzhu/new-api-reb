package middleware

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/privacy"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrivacyDeclaredValuesFailClosed(t *testing.T) {
	for _, mode := range []string{"request_header", "site_switch"} {
		for _, tc := range []struct {
			name, value string
			valid       bool
		}{
			{name: "short", value: "abc"},
			{name: "symbols", value: strings.Repeat("!?", 6)},
			{name: "empty"},
			{name: "normal", value: "opaque-fixture-72", valid: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				calls := 0
				router := privacyRouter(t, func(c *gin.Context) {
					calls++
					if tc.valid {
						body, err := io.ReadAll(c.Request.Body)
						require.NoError(t, err)
						session := privacy.FromContext(c)
						require.NotNil(t, session)
						content := gjson.GetBytes(body, "messages.0.content").String()
						assert.NotEqual(t, tc.value, content)
						assert.Contains(t, content, privacy.Prefix)
						assert.Equal(t, tc.value, session.Restore(content))
						assert.False(t, gjson.GetBytes(body, "new_api_privacy").Exists())
					}
					c.JSON(http.StatusOK, gin.H{"ok": true})
				})
				body, err := common.Marshal(gin.H{
					"messages":        []gin.H{{"role": "user", "content": tc.value}},
					"new_api_privacy": gin.H{"sensitive_values": []string{tc.value}},
				})
				require.NoError(t, err)
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				if mode == "request_header" {
					request.Header.Set("X-New-Api-Privacy", "on")
				} else {
					require.NoError(t, config.UpdateConfigFromMap(system_setting.GetPrivacySetting(), map[string]string{"enabled": "true"}))
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if tc.valid {
					assert.Equal(t, http.StatusOK, response.Code)
					assert.Equal(t, 1, calls)
					assert.Equal(t, "on", response.Header().Get("X-New-Api-Privacy"))
					return
				}
				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.Zero(t, calls, "an unusable declaration must never reach the relay")
				assert.Equal(t, "privacy_invalid_declared_value", gjson.GetBytes(response.Body.Bytes(), "error.code").String())
				assert.Contains(t, gjson.GetBytes(response.Body.Bytes(), "error.message").String(), "declared sensitive value")
				assert.Empty(t, response.Header().Get("X-New-Api-Privacy"))
				assert.Empty(t, response.Header().Get("X-New-Api-Privacy-Session"))
				if tc.value != "" {
					assert.NotContains(t, response.Body.String(), tc.value, "errors must not echo declarations")
				}
			})
		}
	}
}

func TestPrivacyContentTypeParameters(t *testing.T) {
	secret := "opaque-fixture-72"
	for _, tc := range []struct {
		name, parameters, content string
		declared                  []string
		protected                 bool
	}{
		{name: "credential_only_in_parameter", parameters: "; password=" + secret, content: "hello", protected: true},
		{name: "same_reference_as_body", parameters: "; password=" + secret, content: secret, protected: true},
		{name: "declared_parameter", parameters: "; note=" + secret, content: "hello", declared: []string{secret}, protected: true},
		{name: "unchanged_parameters", parameters: ";  Charset=\"UTF-8\"; note=\"ordinary; text\" ", content: "hello"},
		{name: "media_type_collision", parameters: "; note=ordinary", content: "json", declared: []string{"json"}},
		{name: "media_type_without_parameters", content: "json", declared: []string{"json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType := "application/json" + tc.parameters
			calls := 0
			router := privacyRouter(t, func(c *gin.Context) {
				calls++
				session := privacy.FromContext(c)
				require.NotNil(t, session)
				got := c.GetHeader("Content-Type")
				mediaType, params, err := mime.ParseMediaType(got)
				require.NoError(t, err)
				assert.Equal(t, "application/json", mediaType)
				assert.Equal(t, "application/json", c.ContentType())
				var parsed struct {
					Model    string `json:"model"`
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				require.NoError(t, c.ShouldBind(&parsed))
				assert.Equal(t, "fixture-model", parsed.Model, "the downstream JSON parser must still run")
				require.Len(t, parsed.Messages, 1)
				assert.Equal(t, tc.content, session.Restore(parsed.Messages[0].Content))
				if tc.protected {
					assert.NotContains(t, got, secret)
					assert.Contains(t, got, privacy.Prefix)
					assert.Equal(t, contentType, session.Restore(got))
					if tc.content == secret {
						assert.Equal(t, params["password"], parsed.Messages[0].Content)
					}
				} else {
					assert.Equal(t, contentType, got, "ordinary parameter bytes must be preserved")
				}
				outgoing := http.Header{"Content-Type": []string{got, contentType}}
				privacy.ProtectHeaders(c, outgoing)
				assert.Equal(t, []string{got, got}, outgoing.Values("Content-Type"), "outbound masking must protect raw parameters and preserve issued references and the media type")
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
			body, err := common.Marshal(gin.H{
				"model":           "fixture-model",
				"messages":        []gin.H{{"role": "user", "content": tc.content}},
				"new_api_privacy": gin.H{"sensitive_values": tc.declared},
			})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("X-New-Api-Privacy", "on")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, 1, calls)
		})
	}
}
