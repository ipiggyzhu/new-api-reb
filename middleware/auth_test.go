package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestMain provisions a package-wide baseline DB: session auth resolves the
// user's current role/status from the database, so every test in this package
// that authenticates via a session cookie needs a matching users row. The
// seeded id-1 "tester" row backs the pre-existing header-nav session fixtures.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Unsetenv("LOG_SQL_DSN")
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false

	db, err := gorm.Open(sqlite.Open("file:middleware_baseline?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to open baseline test db: " + err.Error())
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		panic("failed to migrate baseline test db: " + err.Error())
	}
	model.DB = db
	// LOG_SQL_DSN is unset, so this only aliases LOG_DB to DB and initializes
	// the dialect-quoted column names needed by the token key lookup.
	if err := model.InitLogDB(); err != nil {
		panic("failed to init log db: " + err.Error())
	}
	tester := &model.User{Id: 1, Username: "tester", Password: "test-password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default"}
	if err := db.Create(tester).Error; err != nil {
		panic("failed to seed baseline user: " + err.Error())
	}

	os.Exit(m.Run())
}

func setupAuthTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))

	origDB, origLogDB := model.DB, model.LOG_DB
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
		model.DB, model.LOG_DB = origDB, origLogDB
	})
	return db
}

// newSessionAuthRouter builds a router whose /login handler bakes an
// enabled-admin snapshot into the session cookie, regardless of the user's
// current DB row — mimicking a session issued before a ban or demotion.
func newSessionAuthRouter(userId int) *gin.Engine {
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("auth-test-secret"))))
	r.POST("/login", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("id", userId)
		session.Set("username", "alice")
		session.Set("role", common.RoleAdminUser)
		session.Set("status", common.UserStatusEnabled)
		session.Set("group", "default")
		if err := session.Save(); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.String(http.StatusOK, "logged-in")
	})
	r.GET("/user", UserAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "handler-reached")
	})
	r.GET("/admin", AdminAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "handler-reached")
	})
	return r
}

func loginAndGetCookies(t *testing.T, r *gin.Engine) []*http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/login", nil))
	require.Equal(t, http.StatusOK, w.Code)
	cookies := w.Result().Cookies()
	require.NotEmpty(t, cookies)
	return cookies
}

func doSessionRequest(r *gin.Engine, path string, userId int, cookies []*http.Cookie) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("New-Api-User", strconv.Itoa(userId))
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestAuthHelperDemotionAppliesToExistingSession(t *testing.T) {
	db := setupAuthTestDB(t)
	user := &model.User{Username: "alice", Password: "test-password", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(user).Error)

	r := newSessionAuthRouter(user.Id)
	cookies := loginAndGetCookies(t, r)

	w := doSessionRequest(r, "/admin", user.Id, cookies)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "handler-reached", w.Body.String())

	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("role", common.RoleCommonUser).Error)

	// The cookie still claims admin, but the current DB role must win.
	w = doSessionRequest(r, "/admin", user.Id, cookies)
	assert.NotEqual(t, "handler-reached", w.Body.String())
	assert.Contains(t, w.Body.String(), `"success":false`)

	// The demoted user keeps ordinary user access.
	w = doSessionRequest(r, "/user", user.Id, cookies)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "handler-reached", w.Body.String())
}

func TestAuthHelperBanAppliesToExistingSession(t *testing.T) {
	db := setupAuthTestDB(t)
	user := &model.User{Username: "alice", Password: "test-password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(user).Error)

	r := newSessionAuthRouter(user.Id)
	cookies := loginAndGetCookies(t, r)

	w := doSessionRequest(r, "/user", user.Id, cookies)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "handler-reached", w.Body.String())

	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)

	// The cookie still claims an enabled user, but the ban must apply now.
	w = doSessionRequest(r, "/user", user.Id, cookies)
	assert.NotEqual(t, "handler-reached", w.Body.String())
	assert.Contains(t, w.Body.String(), `"success":false`)
}

func TestAuthHelperDeletedUserSessionRejected(t *testing.T) {
	db := setupAuthTestDB(t)
	user := &model.User{Username: "alice", Password: "test-password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(user).Error)

	r := newSessionAuthRouter(user.Id)
	cookies := loginAndGetCookies(t, r)

	require.NoError(t, db.Delete(&model.User{}, user.Id).Error)

	w := doSessionRequest(r, "/user", user.Id, cookies)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestTokenOrUserAuthBanAppliesToExistingSession(t *testing.T) {
	db := setupAuthTestDB(t)
	user := &model.User{Username: "alice", Password: "test-password", Role: common.RoleCommonUser, Status: common.UserStatusDisabled, Group: "default"}
	require.NoError(t, db.Create(user).Error)

	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("auth-test-secret"))))
	r.POST("/login", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("id", user.Id)
		session.Set("status", common.UserStatusEnabled) // stale pre-ban snapshot
		require.NoError(t, session.Save())
		c.String(http.StatusOK, "logged-in")
	})
	r.GET("/media", TokenOrUserAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "handler-reached")
	})
	cookies := loginAndGetCookies(t, r)

	// No API token supplied, so a banned user must not get through on the
	// stale session; the token-auth fallback rejects the request.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/media", nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.NotEqual(t, "handler-reached", w.Body.String())
}

func TestTokenAuthGeminiCredentialsOnModelsList(t *testing.T) {
	db := setupAuthTestDB(t)
	user := &model.User{Username: "alice", Password: "test-password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(user).Error)
	tokenKey := "authtestkey0123456789abcdefghijklmnopqrstuvwxyz0"
	token := &model.Token{
		UserId:         user.Id,
		Key:            tokenKey,
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, db.Create(token).Error)

	r := gin.New()
	r.GET("/v1/models", TokenAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "models-ok")
	})

	tests := []struct {
		name       string
		path       string
		headers    map[string]string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "query key credential accepted",
			path:       "/v1/models?key=sk-" + tokenKey,
			wantStatus: http.StatusOK,
			wantBody:   "models-ok",
		},
		{
			name:       "x-goog-api-key header credential accepted",
			path:       "/v1/models",
			headers:    map[string]string{"x-goog-api-key": tokenKey},
			wantStatus: http.StatusOK,
			wantBody:   "models-ok",
		},
		{
			name:       "no credential rejected",
			path:       "/v1/models",
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			r.ServeHTTP(w, req)
			assert.Equal(t, tt.wantStatus, w.Code)
			if tt.wantBody != "" {
				assert.Equal(t, tt.wantBody, w.Body.String())
			}
		})
	}
}
