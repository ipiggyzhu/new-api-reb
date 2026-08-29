package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	goredis "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEmailBindWithoutSessionReturnsUnauthorized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	router.POST("/api/oauth/email/bind", EmailBind)

	const email = "bind-target@example.com"
	common.RegisterVerificationCodeWithKey(email, "123456", common.EmailVerificationPurpose)
	t.Cleanup(func() { common.DeleteKey(email, common.EmailVerificationPurpose) })

	body := strings.NewReader(`{"email":"` + email + `","code":"123456"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/oauth/email/bind", body)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
	assert.NotEmpty(t, resp.Message)
}

// 管理员 override 额度绕开了增减额度的缓存同步路径，必须使 Redis 用户缓存失效，
// 否则被清零的用户在 TTL 到期前仍按旧余额通过额度信任检查。
func TestManageUserQuotaOverrideInvalidatesUserCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	originalDB, originalLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Log{}))
	t.Cleanup(func() {
		model.DB, model.LOG_DB = originalDB, originalLogDB
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	originalRDB, originalEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		_ = client.Close()
		common.RDB, common.RedisEnabled = originalRDB, originalEnabled
	})

	target := &model.User{
		Username: "override-cache-target",
		Password: "placeholder",
		AffCode:  "override-cache-aff",
		Quota:    5000000,
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, model.DB.Create(target).Error)

	cacheKey := fmt.Sprintf("user:%d", target.Id)
	server.HSet(cacheKey,
		"Id", strconv.Itoa(target.Id),
		"Group", "default",
		"Email", "",
		"Quota", "5000000",
		"Status", strconv.Itoa(common.UserStatusEnabled),
		"Username", target.Username,
		"Setting", "",
	)
	server.SetTTL(cacheKey, 60*time.Second)

	cached, err := model.GetUserCache(target.Id)
	require.NoError(t, err)
	require.Equal(t, 5000000, cached.Quota, "fixture 必须先命中缓存")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := fmt.Sprintf(`{"id":%d,"action":"add_quota","mode":"override","value":0}`, target.Id)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/manage", strings.NewReader(body))
	c.Set("id", 999)
	c.Set("role", common.RoleRootUser)
	ManageUser(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &resp))
	require.True(t, resp.Success, resp.Message)

	assert.False(t, server.Exists(cacheKey), "override 后必须删除用户缓存")

	cached, err = model.GetUserCache(target.Id)
	require.NoError(t, err)
	assert.Equal(t, 0, cached.Quota, "缓存读取必须回源到覆盖后的余额")

	// 回源读取会异步回填缓存；等它落盘再让 t.Cleanup 还原全局的
	// RDB/RedisEnabled，否则后台 goroutine 会与清理代码竞争这两个全局变量。
	require.Eventually(t, func() bool {
		return server.Exists(cacheKey)
	}, 2*time.Second, 10*time.Millisecond, "异步缓存回填必须在测试结束前完成")
}
