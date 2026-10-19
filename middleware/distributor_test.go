package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newJSONTestContext(t *testing.T, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, w
}

func TestGetModelFromJSONBodyDuplicateKeys(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantModel string
		wantErr   string
	}{
		{
			name:      "single model key",
			body:      `{"model":"gpt-4o-mini","messages":[]}`,
			wantModel: "gpt-4o-mini",
		},
		{
			name:    "duplicate model keys",
			body:    `{"model":"gpt-4o-mini","messages":[],"model":"gpt-4o"}`,
			wantErr: "duplicate model field",
		},
		{
			name:    "duplicate model key via unicode escape",
			body:    `{"model":"gpt-4o-mini","mod\u0065l":"gpt-4o"}`,
			wantErr: "duplicate model field",
		},
		{
			name:    "duplicate group keys",
			body:    `{"model":"gpt-4o-mini","group":"default","group":"vip"}`,
			wantErr: "duplicate group field",
		},
		{
			name:      "nested model keys are not duplicates",
			body:      `{"model":"gpt-4o-mini","messages":[{"model":"x"}],"metadata":{"model":"y"}}`,
			wantModel: "gpt-4o-mini",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newJSONTestContext(t, http.MethodPost, "/v1/chat/completions", tt.body)
			modelRequest, err := getModelFromJSONBody(c)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, modelRequest)
			assert.Equal(t, tt.wantModel, modelRequest.Model)
		})
	}
}

func setupDistributorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	origDB, origLogDB := model.DB, model.LOG_DB
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		model.DB = origDB
		model.LOG_DB = origLogDB
	})
	return db
}

func TestDistributeAbortsWhenChannelHasNoAvailableKey(t *testing.T) {
	db := setupDistributorTestDB(t)
	channel := model.Channel{
		Id:     1201,
		Type:   1,
		Name:   "keyless-multikey",
		Status: common.ChannelStatusEnabled,
		Key:    "",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
		},
	}
	require.NoError(t, db.Create(&channel).Error)

	c, w := newJSONTestContext(t, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[]}`)
	common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "1201")

	Distribute()(c)

	require.True(t, c.IsAborted())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), string(types.ErrorCodeChannelNoAvailableKey))
	_, keySet := common.GetContextKey(c, constant.ContextKeyChannelKey)
	assert.False(t, keySet)
}

func TestDistributePassesWithEnabledSingleKeyChannel(t *testing.T) {
	db := setupDistributorTestDB(t)
	channel := model.Channel{
		Id:     1202,
		Type:   1,
		Name:   "single-key",
		Status: common.ChannelStatusEnabled,
		Key:    "sk-test",
	}
	require.NoError(t, db.Create(&channel).Error)

	c, w := newJSONTestContext(t, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[]}`)
	common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "1202")

	Distribute()(c)

	require.False(t, c.IsAborted())
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sk-test", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
}

// Requests that skip channel selection (task fetch style endpoints) reach
// SetupContextForSelectedChannel with a nil channel on purpose; its
// channel-is-nil error must not abort them.
func TestDistributeAllowsNilChannelOnTaskFetchPath(t *testing.T) {
	setupDistributorTestDB(t)

	c, w := newJSONTestContext(t, http.MethodGet, "/v1/videos/task_123", "")

	Distribute()(c)

	require.False(t, c.IsAborted())
	assert.Equal(t, http.StatusOK, w.Code)
}

// Tokens with a model allowlist must still be able to poll mj/suno task
// fetch endpoints: getModelRequest resolves no model for them (they consume
// none), so the allowlist has nothing to reject. #4834 covered only the
// video fetch variant.
func TestDistributeModelLimitAllowsTaskFetchEndpoints(t *testing.T) {
	require.NoError(t, i18n.Init())

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "mj task fetch", method: http.MethodGet, path: "/mj/task/123456/fetch"},
		{name: "mj task image seed", method: http.MethodGet, path: "/mj/task/123456/image-seed"},
		{name: "mj task list by condition", method: http.MethodPost, path: "/mj/task/list-by-condition", body: `{"ids":["123456"]}`},
		{name: "suno fetch", method: http.MethodPost, path: "/suno/fetch", body: `{"ids":["123456"]}`},
		{name: "suno fetch by id", method: http.MethodGet, path: "/suno/fetch/123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newJSONTestContext(t, tt.method, tt.path, tt.body)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"midjourney": true})

			Distribute()(c)

			require.False(t, c.IsAborted())
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}

// The distributor is the first reader of a relay body, so its answer is the one
// the client sees: an oversized body is a 413 the client can act on, not a 400,
// and every failure carries its cause once, without a doubled prefix.
func TestDistributeReportsWhyTheBodyCouldNotBeRead(t *testing.T) {
	require.NoError(t, i18n.Init())
	previous := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = previous })
	oversized := `{"model":"gpt-x","prompt":"` + strings.Repeat("a", 1<<20) + `"}`

	tests := []struct {
		name    string
		path    string
		body    string
		status  int
		message string
	}{
		{
			name:    "oversized JSON body",
			path:    "/v1/chat/completions",
			body:    oversized,
			status:  http.StatusRequestEntityTooLarge,
			message: "Invalid request: request body exceeds 1 MB: request body too large",
		},
		{
			name:    "oversized Midjourney body",
			path:    "/mj/submit/imagine",
			body:    oversized,
			status:  http.StatusRequestEntityTooLarge,
			message: "Invalid request: request body exceeds 1 MB: request body too large",
		},
		{
			name:    "invalid JSON body",
			path:    "/v1/chat/completions",
			body:    `{"model":"gpt-x",`,
			status:  http.StatusBadRequest,
			message: "Invalid request: invalid JSON request body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newJSONTestContext(t, http.MethodPost, tt.path, tt.body)

			Distribute()(c)

			require.True(t, c.IsAborted())
			assert.Equal(t, tt.status, w.Code)
			assert.Contains(t, w.Body.String(), `"message":"`+tt.message)
		})
	}
}

// A failed read consumes and closes the body, so a later reader must get the
// same cause instead of "http: invalid Read on closed Body".
func TestGetRequestBodyKeepsTheReadFailureForLaterReaders(t *testing.T) {
	previous := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = previous })
	c, _ := newJSONTestContext(t, http.MethodPost, "/v1/chat/completions", `{"prompt":"`+strings.Repeat("a", 1<<20)+`"}`)

	_, firstErr := common.GetBodyStorage(c)
	_, secondErr := common.GetBodyStorage(c)

	require.Error(t, firstErr)
	require.Error(t, secondErr)
	assert.True(t, common.IsRequestBodyTooLargeError(secondErr))
	assert.Equal(t, firstErr.Error(), secondErr.Error())
}

// The task-fetch exemption must stay scoped: submit endpoints and ordinary
// relay requests keep enforcing the allowlist, including when the request
// carries no model name at all.
func TestDistributeModelLimitStillRejectsNonFetchRequests(t *testing.T) {
	require.NoError(t, i18n.Init())

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "mj submit with disallowed model", method: http.MethodPost, path: "/mj/submit/imagine", body: `{"prompt":"a cat"}`},
		{name: "chat completion without model", method: http.MethodPost, path: "/v1/chat/completions", body: `{"messages":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newJSONTestContext(t, tt.method, tt.path, tt.body)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"midjourney": true})

			Distribute()(c)

			require.True(t, c.IsAborted())
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}
