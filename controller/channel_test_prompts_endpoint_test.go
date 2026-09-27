package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The settings page fills both its placeholder and its "built-in examples"
// button from this endpoint instead of a hardcoded copy, so the response shape
// is a contract: success plus a non-empty string array under data. The previous
// hardcoded frontend list silently drifted away from the backend pool, which is
// the bug this endpoint exists to prevent.
func TestListChannelTestPromptsResponseShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/option/channel_test_prompts", nil)

	ListChannelTestPrompts(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Success bool     `json:"success"`
		Data    []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.True(t, body.Success)
	require.NotEmpty(t, body.Data)

	for _, prompt := range body.Data {
		assert.Equal(t, strings.TrimSpace(prompt), prompt, "prompt should be pre-trimmed: %q", prompt)
		assert.NotEmpty(t, prompt)
	}
}

// Handing out the backing slice would let a caller mutate the pool the gateway
// draws from for every future channel test.
func TestListChannelTestPromptsDoesNotAliasThePool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	call := func() []string {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/option/channel_test_prompts", nil)
		ListChannelTestPrompts(ctx)

		var body struct {
			Data []string `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		return body.Data
	}

	first := call()
	require.NotEmpty(t, first)
	original := first[0]
	first[0] = "mutated by a caller"

	second := call()
	require.NotEmpty(t, second)
	assert.Equal(t, original, second[0])
}
