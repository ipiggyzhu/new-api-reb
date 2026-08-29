package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupErrorLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	originalDB, originalLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.User{}))

	originalEnabled := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = true
	t.Cleanup(func() {
		constant.ErrorLogEnabled = originalEnabled
		model.DB, model.LOG_DB = originalDB, originalLogDB
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// errorLogTestContext builds a request context whose channel keys deliberately
// disagree with the channel an attempt actually ran against, which is what the
// retry loop produces once it has moved on to the next channel.
func errorLogTestContext(t *testing.T) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("id", 7)
	c.Set("original_model", "gpt-4o")
	c.Set("token_name", "tk")
	c.Set("group", "default")
	c.Set("channel_id", 999)
	c.Set("channel_name", "stale-channel")
	c.Set("channel_type", 1)
	return c
}

func readOnlyErrorLog(t *testing.T, db *gorm.DB) model.Log {
	t.Helper()
	var logs []model.Log
	require.NoError(t, db.Find(&logs).Error)
	require.Len(t, logs, 1, "expected exactly one error log row")
	return logs[0]
}

// A failed attempt must be attributed to the channel it actually ran against.
// The request context still names whichever channel was set up last, so reading
// the attribution from there mislabels the row.
func TestRecordRelayErrorLogAttributesFailureToTheAttemptedChannel(t *testing.T) {
	db := setupErrorLogTestDB(t)
	c := errorLogTestContext(t)

	attempted := types.NewChannelError(42, 8, "grok-cpa", false, "sk-x", true)
	apiErr := types.NewErrorWithStatusCode(fmt.Errorf("upstream refused"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	recordRelayErrorLog(c, attempted, apiErr)

	row := readOnlyErrorLog(t, db)
	assert.Equal(t, 42, row.ChannelId, "log row must point at the attempted channel, not the context")
	other, err := common.StrToMap(row.Other)
	require.NoError(t, err)
	assert.EqualValues(t, 42, other["channel_id"])
	assert.Equal(t, "grok-cpa", other["channel_name"])
	assert.EqualValues(t, 8, other["channel_type"])
}

// Failures raised before any channel was picked — an exhausted balance, no
// available channel for the group — used to return straight to the caller and
// leave nothing behind. They are the ones an operator most needs to see.
func TestRecordRelayErrorLogRecordsFailuresWithNoChannel(t *testing.T) {
	db := setupErrorLogTestDB(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("id", 7)
	c.Set("original_model", "gpt-4o")

	apiErr := types.NewError(fmt.Errorf("分组 default 下模型 gpt-4o 的可用渠道不存在"), types.ErrorCodeGetChannelFailed)

	recordRelayErrorLog(c, nil, apiErr)

	row := readOnlyErrorLog(t, db)
	assert.Equal(t, 0, row.ChannelId)
	assert.Contains(t, row.Content, "可用渠道不存在")
	other, err := common.StrToMap(row.Other)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrorCodeGetChannelFailed), other["error_code"])
}

// A block page leaves nothing in the message, so the raw upstream body is the
// only way to tell a Cloudflare challenge from a dead origin. It belongs under
// admin_info, which non-admin log views strip.
func TestRecordRelayErrorLogCarriesUpstreamBodyAsAdminInfo(t *testing.T) {
	db := setupErrorLogTestDB(t)
	c := errorLogTestContext(t)

	apiErr := types.NewErrorWithStatusCode(fmt.Errorf("bad response status code 403"), types.ErrorCodeBadResponseStatusCode, http.StatusForbidden)
	apiErr.SetUpstreamBody("<html><title>Attention Required! | Cloudflare</title>Ray ID: 8f2b1c3d</html>")

	recordRelayErrorLog(c, types.NewChannelError(42, 8, "grok-cpa", false, "sk-x", true), apiErr)

	other, err := common.StrToMap(readOnlyErrorLog(t, db).Other)
	require.NoError(t, err)
	adminInfo, ok := other["admin_info"].(map[string]any)
	require.True(t, ok, "admin_info must be a nested object so non-admin views can strip it")
	assert.Contains(t, adminInfo["upstream_body"], "Cloudflare")
}

// The existing suppression switches must keep working: quota failures are
// tagged no-record on purpose because they are user-caused and high volume.
func TestRecordRelayErrorLogHonoursSuppressionSwitches(t *testing.T) {
	t.Run("error tagged no-record writes nothing", func(t *testing.T) {
		db := setupErrorLogTestDB(t)
		apiErr := types.NewErrorWithStatusCode(fmt.Errorf("用户额度不足"), types.ErrorCodeInsufficientUserQuota,
			http.StatusForbidden, types.ErrOptionWithNoRecordErrorLog())

		recordRelayErrorLog(errorLogTestContext(t), nil, apiErr)

		var count int64
		require.NoError(t, db.Model(&model.Log{}).Count(&count).Error)
		assert.Zero(t, count)
	})

	t.Run("global switch off writes nothing", func(t *testing.T) {
		db := setupErrorLogTestDB(t)
		constant.ErrorLogEnabled = false

		recordRelayErrorLog(errorLogTestContext(t), nil,
			types.NewError(fmt.Errorf("boom"), types.ErrorCodeBadResponseStatusCode))

		var count int64
		require.NoError(t, db.Model(&model.Log{}).Count(&count).Error)
		assert.Zero(t, count)
	})
}

// End to end across the two seams that were changed: a Cloudflare-blocked
// upstream produces an error whose message is only a status code, and the log
// row still has to name the channel and carry enough of the block page for an
// operator to recognise it.
func TestCloudflareBlockedUpstreamIsFullyDiagnosableFromTheLogRow(t *testing.T) {
	db := setupErrorLogTestDB(t)
	c := errorLogTestContext(t)

	blocked := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"text/html"}, "Server": []string{"cloudflare"}},
		Body: io.NopCloser(strings.NewReader(
			"<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head>" +
				"<body>Sorry, you have been blocked. Ray ID: 8f2b1c3d4e5f6a7b</body></html>")),
	}
	apiErr := service.RelayErrorHandler(c.Request.Context(), blocked, false)
	require.Equal(t, "bad response status code 403", apiErr.Error(),
		"precondition: the message alone says nothing about Cloudflare")

	recordRelayErrorLog(c, types.NewChannelError(42, 8, "grok-cpa", false, "sk-x", true), apiErr)

	row := readOnlyErrorLog(t, db)
	assert.Equal(t, 42, row.ChannelId)
	other, err := common.StrToMap(row.Other)
	require.NoError(t, err)
	assert.EqualValues(t, http.StatusForbidden, other["status_code"])
	assert.Equal(t, "grok-cpa", other["channel_name"])
	adminInfo, ok := other["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, adminInfo["upstream_body"], "Cloudflare")
	assert.Contains(t, adminInfo["upstream_body"], "Ray ID")
}
