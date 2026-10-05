package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postUserSetting 直接调用 UpdateUserSetting handler，绕开完整路由与鉴权，
// 只验证「请求体字段如何合并进已存设置」这一条契约。
// 数据库夹具复用同包内的 setupManageUserTestDB（controller/user_manage_test.go:30），
// 它已迁移 User 与 Log 表并把 model.DB/model.LOG_DB 指向临时库。
func postUserSetting(t *testing.T, userId int, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/user/setting", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", userId)
	UpdateUserSetting(c)
	return recorder
}

// 这个 API 的约定是 HTTP 恒 200，成败看 body 里的 success 字段
// （common/gin.go ApiErrorI18n 也是 200）。
func requireAPISuccess(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code)
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &resp))
	require.True(t, resp.Success, "期望 success=true，实际响应：%s", recorder.Body.String())
}

func requireAPIFailure(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code)
	var resp struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &resp))
	require.False(t, resp.Success, "期望 success=false，实际响应：%s", recorder.Body.String())
}

// 通知页只提交告警设置时，用户已经表过态的 record_ip_log 必须保留。
func TestUpdateUserSettingPreservesStoredRecordIpLog(t *testing.T) {
	db := setupManageUserTestDB(t)

	disabled := false
	user := model.User{Username: "setting-keep-ip", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "keep-ip-aff"}
	user.SetSetting(dto.UserSetting{RecordIpLog: &disabled, NotifyType: dto.NotifyTypeEmail, QuotaWarningThreshold: 1})
	require.NoError(t, db.Create(&user).Error)

	body := `{"notify_type":"email","quota_warning_threshold":10}`
	recorder := postUserSetting(t, user.Id, body)
	requireAPISuccess(t, recorder)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	setting := stored.GetSetting()
	require.NotNil(t, setting.RecordIpLog, "用户已表态的 record_ip_log 不该被清空")
	assert.False(t, *setting.RecordIpLog, "用户显式关闭的选择必须保留")
	assert.EqualValues(t, 10, setting.QuotaWarningThreshold)
}

// 用户从未表态时，提交告警设置不应把他钉死成「已关闭」。
func TestUpdateUserSettingLeavesRecordIpLogUnsetWhenNeverConfigured(t *testing.T) {
	db := setupManageUserTestDB(t)

	user := model.User{Username: "setting-fresh-ip", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "fresh-ip-aff"}
	require.NoError(t, db.Create(&user).Error)

	body := `{"notify_type":"email","quota_warning_threshold":10}`
	recorder := postUserSetting(t, user.Id, body)
	requireAPISuccess(t, recorder)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	assert.Nil(t, stored.GetSetting().RecordIpLog, "从未表态的用户保存别的设置后仍应保持未表态")
}

// 隐私卡显式提交 false 时必须落库。
func TestUpdateUserSettingStoresExplicitRecordIpLog(t *testing.T) {
	db := setupManageUserTestDB(t)

	user := model.User{Username: "setting-explicit-ip", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "explicit-ip-aff"}
	require.NoError(t, db.Create(&user).Error)

	body := `{"notify_type":"email","quota_warning_threshold":10,"record_ip_log":false}`
	recorder := postUserSetting(t, user.Id, body)
	requireAPISuccess(t, recorder)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	setting := stored.GetSetting()
	require.NotNil(t, setting.RecordIpLog)
	assert.False(t, *setting.RecordIpLog)
}

// 隐私卡的真实请求体只带 record_ip_log，不带任何告警字段（2026-10-05 复现：
// 旧代码先无条件校验 notify_type/threshold，这个请求永远被 MsgSettingInvalidType
// 挡下，安全页的「记录 IP」开关实际保存不了）。
// 未提交的字段必须沿用已存值，而不是被零值冲掉。
func TestUpdateUserSettingAcceptsRecordIpLogOnlyPayload(t *testing.T) {
	db := setupManageUserTestDB(t)

	user := model.User{Username: "setting-privacy-only", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "privacy-only-aff"}
	user.SetSetting(dto.UserSetting{
		NotifyType:            dto.NotifyTypeWebhook,
		QuotaWarningThreshold: 50,
		WebhookUrl:            "https://example.com/hook",
		WebhookSecret:         "s3cret",
	})
	require.NoError(t, db.Create(&user).Error)

	recorder := postUserSetting(t, user.Id, `{"record_ip_log":false}`)
	requireAPISuccess(t, recorder)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	setting := stored.GetSetting()
	require.NotNil(t, setting.RecordIpLog)
	assert.False(t, *setting.RecordIpLog, "显式关闭必须落库")
	assert.Equal(t, dto.NotifyTypeWebhook, setting.NotifyType, "未提交的告警类型必须沿用已存值")
	assert.EqualValues(t, 50, setting.QuotaWarningThreshold, "未提交的告警阈值必须沿用已存值")
	assert.Equal(t, "https://example.com/hook", setting.WebhookUrl, "未提交的 webhook 地址必须沿用已存值")
	assert.Equal(t, "s3cret", setting.WebhookSecret, "未提交的 webhook 密钥必须沿用已存值")
}

// 提交了告警字段就必须过校验——部分更新不能变成校验旁路。
// 非法的 notify_type 即使和合法的 record_ip_log 同包提交，也必须整体拒绝。
func TestUpdateUserSettingRejectsInvalidNotifyTypeEvenWithRecordIpLog(t *testing.T) {
	db := setupManageUserTestDB(t)

	user := model.User{Username: "setting-bad-type", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "bad-type-aff"}
	user.SetSetting(dto.UserSetting{NotifyType: dto.NotifyTypeEmail, QuotaWarningThreshold: 10})
	require.NoError(t, db.Create(&user).Error)

	recorder := postUserSetting(t, user.Id, `{"notify_type":"sms","record_ip_log":false}`)
	requireAPIFailure(t, recorder)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	setting := stored.GetSetting()
	assert.Nil(t, setting.RecordIpLog, "请求被拒后任何字段都不该落库")
	assert.Equal(t, dto.NotifyTypeEmail, setting.NotifyType)
}

// 提交了告警类型就必须带正阈值——「沿用已存阈值」只发生在根本没提交
// 告警字段的时候。
func TestUpdateUserSettingRejectsNonPositiveThresholdWhenTypeSubmitted(t *testing.T) {
	db := setupManageUserTestDB(t)

	user := model.User{Username: "setting-bad-threshold", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "bad-threshold-aff"}
	user.SetSetting(dto.UserSetting{NotifyType: dto.NotifyTypeEmail, QuotaWarningThreshold: 10})
	require.NoError(t, db.Create(&user).Error)

	recorder := postUserSetting(t, user.Id, `{"notify_type":"email","quota_warning_threshold":0}`)
	requireAPIFailure(t, recorder)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	assert.EqualValues(t, 10, stored.GetSetting().QuotaWarningThreshold, "请求被拒后已存阈值不该被改")
}
