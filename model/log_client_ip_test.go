package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newClientIpTestContext 构造带指定来源 IP 的 gin 上下文。
func newClientIpTestContext(t *testing.T, remoteAddr string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.RemoteAddr = remoteAddr
	return c
}

func newClientIpTestUser(t *testing.T, username string, setting dto.UserSetting) User {
	t.Helper()
	user := User{
		Username: username,
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		AffCode:  username + "-aff",
	}
	user.SetSetting(setting)
	require.NoError(t, DB.Create(&user).Error)
	return user
}

// 站点默认：用户从未表态（record_ip_log 字段缺失）→ 消费日志记 IP。
func TestRecordConsumeLogRecordsClientIpWhenSettingUnset(t *testing.T) {
	truncateTables(t)
	user := newClientIpTestUser(t, "ip-unset-consume", dto.UserSetting{})

	RecordConsumeLog(newClientIpTestContext(t, "203.0.113.9:54321"), user.Id, RecordConsumeLogParams{
		ModelName: "gpt-test",
		Content:   "consume",
	})

	var entry Log
	require.NoError(t, DB.Where("user_id = ? AND type = ?", user.Id, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, "203.0.113.9", entry.Ip)
}

// 站点默认：用户从未表态 → 错误日志记 IP。
func TestRecordErrorLogRecordsClientIpWhenSettingUnset(t *testing.T) {
	truncateTables(t)
	user := newClientIpTestUser(t, "ip-unset-error", dto.UserSetting{})

	RecordErrorLog(newClientIpTestContext(t, "203.0.113.10:54321"), user.Id, 0, "gpt-test", "", "boom", 0, 1, false, "default", NewLogOther())

	var entry Log
	require.NoError(t, DB.Where("user_id = ? AND type = ?", user.Id, LogTypeError).First(&entry).Error)
	assert.Equal(t, "203.0.113.10", entry.Ip)
}

// 用户显式关闭后必须尊重其选择 —— 这是本次改动的边界，不能被顺带放开。
func TestRecordConsumeLogSkipsClientIpWhenExplicitlyDisabled(t *testing.T) {
	truncateTables(t)
	disabled := false
	user := newClientIpTestUser(t, "ip-off", dto.UserSetting{RecordIpLog: &disabled})

	RecordConsumeLog(newClientIpTestContext(t, "203.0.113.11:54321"), user.Id, RecordConsumeLogParams{
		ModelName: "gpt-test",
		Content:   "consume",
	})

	var entry Log
	require.NoError(t, DB.Where("user_id = ? AND type = ?", user.Id, LogTypeConsume).First(&entry).Error)
	assert.Empty(t, entry.Ip)
}

// 用户显式开启时同样记录。
func TestRecordConsumeLogRecordsClientIpWhenExplicitlyEnabled(t *testing.T) {
	truncateTables(t)
	enabled := true
	user := newClientIpTestUser(t, "ip-on", dto.UserSetting{RecordIpLog: &enabled})

	RecordConsumeLog(newClientIpTestContext(t, "203.0.113.12:54321"), user.Id, RecordConsumeLogParams{
		ModelName: "gpt-test",
		Content:   "consume",
	})

	var entry Log
	require.NoError(t, DB.Where("user_id = ? AND type = ?", user.Id, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, "203.0.113.12", entry.Ip)
}

// 充值/系统/退款日志按设计不记客户端 IP：RecordLog 路径不经过本判定。
// 这条用例锁住「本次改动没有把 IP 扩散到消费/错误之外的日志类型」。
func TestRecordLogDoesNotAttachClientIpToTopupAndSystemLogs(t *testing.T) {
	truncateTables(t)
	user := newClientIpTestUser(t, "ip-non-billing", dto.UserSetting{})

	RecordLog(user.Id, LogTypeTopup, "topup")
	RecordLog(user.Id, LogTypeSystem, "system")

	var entries []Log
	require.NoError(t, DB.Where("user_id = ?", user.Id).Find(&entries).Error)
	require.Len(t, entries, 2)
	for _, entry := range entries {
		assert.Empty(t, entry.Ip, "type=%d 的日志不该带客户端 IP", entry.Type)
	}
}
