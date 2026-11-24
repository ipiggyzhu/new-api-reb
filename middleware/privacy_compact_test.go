package middleware

import (
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/privacy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrivacyCompactCredentials(t *testing.T) {
	ip := strings.Join([]string{"192", "0", "2", "28"}, ".")
	domain := "host" + ".example" + ".com"
	short6 := ":" + ":" + "1"
	secret := "Pi" + "ne" + "@" + "!" + "#"
	user := "ali" + "ce"
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"ipv4_port", ip + ":22," + secret, []string{ip, secret}},
		{"domain_chain", domain + "," + user + "," + secret, []string{user, secret}},
		{"short_ipv6", short6 + "\n" + secret, []string{secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			router := privacyRouter(t, func(c *gin.Context) {
				calls++
				storage, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				body, err := storage.Bytes()
				require.NoError(t, err)
				content := gjson.GetBytes(body, "messages.0.content").String()
				session := privacy.FromContext(c)
				require.NotNil(t, session)
				assert.Equal(t, len(tc.want), len(session.Values), "exact registration count")
				for i, plain := range tc.want {
					found := false
					for ref, stored := range session.Values {
						if stored == plain {
							found = strings.Contains(content, ref)
						}
					}
					assert.True(t, found, "registered reference reaches upstream at index %d", i)
					assert.False(t, strings.Contains(content, plain), "plaintext absent at index %d", i)
				}
				require.NoError(t, privacy.CheckOutboundJSON(c, body))
				plainBody, err := common.Marshal(map[string]string{"content": secret})
				require.NoError(t, err)
				assert.Error(t, privacy.CheckOutboundJSON(c, plainBody), "final check still rejects a reintroduced credential")
				c.JSON(http.StatusOK, gin.H{"choices": []any{gin.H{"message": gin.H{"content": content}}}})
			})
			body, err := common.Marshal(map[string]any{
				"model": "fixture-model", "messages": []map[string]string{{"role": "user", "content": tc.input}},
			})
			require.NoError(t, err)
			response := privacyRequest(t, router, string(body), "")
			assert.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, 1, calls, "request reaches the upstream handler")
			assert.True(t, tc.input == gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String(), "response restores the entire conversation text")
		})
	}
}
