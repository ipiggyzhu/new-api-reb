package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedIpLogUser(t *testing.T, username, affCode string, recordIp bool) int {
	t.Helper()
	user := &User{Username: username, Password: "placeholder", AffCode: affCode}
	if recordIp {
		user.Setting = common.MapToJsonStr(map[string]any{"record_ip_log": true})
	}
	require.NoError(t, DB.Create(user).Error)
	return user.Id
}

func ipLogContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.RemoteAddr = "203.0.113.7:51234"
	return c
}

func withRecordIpForAll(t *testing.T, enabled bool) {
	t.Helper()
	general := operation_setting.GetGeneralSetting()
	original := general.RecordIpLogForAll
	general.RecordIpLogForAll = enabled
	t.Cleanup(func() { general.RecordIpLogForAll = original })
}

// record_ip_log is a preference each user controls on their own profile and it
// defaults to off, so an operator had no way to get IPs recorded for tracing
// abuse. The site-level switch has to override it, and it has to apply to
// consume logs and error logs alike.
func TestClientIpForLogSiteSwitchOverridesUserPreference(t *testing.T) {
	optedOut := seedIpLogUser(t, "ip-opted-out", "ip-aff-out", false)
	optedIn := seedIpLogUser(t, "ip-opted-in", "ip-aff-in", true)
	c := ipLogContext(t)

	t.Run("site switch off honours the per-user preference", func(t *testing.T) {
		withRecordIpForAll(t, false)

		assert.Empty(t, clientIpForLog(c, optedOut), "user did not opt in")
		assert.Equal(t, "203.0.113.7", clientIpForLog(c, optedIn), "user opted in")
	})

	t.Run("site switch on records every user", func(t *testing.T) {
		withRecordIpForAll(t, true)

		assert.Equal(t, "203.0.113.7", clientIpForLog(c, optedOut))
		assert.Equal(t, "203.0.113.7", clientIpForLog(c, optedIn))
	})

	t.Run("unknown user still resolves under the site switch", func(t *testing.T) {
		withRecordIpForAll(t, true)

		assert.Equal(t, "203.0.113.7", clientIpForLog(c, 0),
			"the site switch must not depend on a user setting lookup")
	})
}

// Both log types must agree; recording the IP on errors but not on ordinary
// traffic would make abuse impossible to correlate.
func TestConsumeAndErrorLogsBothRecordIpUnderSiteSwitch(t *testing.T) {
	userId := seedIpLogUser(t, "ip-both-logs", "ip-aff-both", false)
	withRecordIpForAll(t, true)
	c := ipLogContext(t)

	require.NoError(t, LOG_DB.Where("user_id = ?", userId).Delete(&Log{}).Error)

	RecordConsumeLog(c, userId, RecordConsumeLogParams{
		ModelName: "gpt-4o",
		Content:   "consume",
		Group:     "default",
	})
	RecordErrorLog(c, userId, 42, "gpt-4o", "tk", "upstream refused", 0, 1, false, "default", nil)

	var logs []Log
	require.NoError(t, LOG_DB.Where("user_id = ?", userId).Order("type asc").Find(&logs).Error)
	require.Len(t, logs, 2)
	for _, entry := range logs {
		assert.Equal(t, "203.0.113.7", entry.Ip, "log type %d must carry the client IP", entry.Type)
	}
}

// The admin toggle saves general_setting.record_ip_log_for_all through the
// generic option path, so the key has to actually bind to the setting struct.
// A typo in the json tag would silently leave the switch inert.
func TestRecordIpLogForAllBindsFromStoredOption(t *testing.T) {
	general := operation_setting.GetGeneralSetting()
	original := general.RecordIpLogForAll
	t.Cleanup(func() { general.RecordIpLogForAll = original })

	general.RecordIpLogForAll = false
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"general_setting.record_ip_log_for_all": "true",
	}))
	assert.True(t, operation_setting.ShouldRecordIpForAllUsers(),
		"the admin toggle must reach the setting struct")

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"general_setting.record_ip_log_for_all": "false",
	}))
	assert.False(t, operation_setting.ShouldRecordIpForAllUsers())
}
