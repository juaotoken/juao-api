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

// 通知页只提交告警设置时，用户已经表过态的 record_ip_log 必须保留。
func TestUpdateUserSettingPreservesStoredRecordIpLog(t *testing.T) {
	db := setupManageUserTestDB(t)

	disabled := false
	user := model.User{Username: "setting-keep-ip", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "keep-ip-aff"}
	user.SetSetting(dto.UserSetting{RecordIpLog: &disabled, NotifyType: dto.NotifyTypeEmail, QuotaWarningThreshold: 1})
	require.NoError(t, db.Create(&user).Error)

	body := `{"notify_type":"email","quota_warning_threshold":10}`
	recorder := postUserSetting(t, user.Id, body)
	require.Equal(t, http.StatusOK, recorder.Code)

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
	require.Equal(t, http.StatusOK, recorder.Code)

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
	require.Equal(t, http.StatusOK, recorder.Code)

	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	setting := stored.GetSetting()
	require.NotNil(t, setting.RecordIpLog)
	assert.False(t, *setting.RecordIpLog)
}
