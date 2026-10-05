# 客户端 IP 记录默认开启 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让消费日志与错误日志默认记录客户端 IP —— 用户从未设置过该项时视为开启，显式关掉的用户保持关闭。

**Architecture:** 把用户设置里的 `record_ip_log` 从「布尔值」升级为「可空布尔值」。后端用 `*bool` 配合 `omitempty`：字段缺失 = 用户从未表态 = 记录；字段存在 = 按用户选择。写设置接口改为读-改-写，避免前端每次都把它重置成 `false`。前端只改默认展示值与注释文案。

**Tech Stack:** Go 1.25.1 / Gin / GORM v2 / testify；React 19 / TypeScript / Vitest / react-i18next。

**背景（为什么不是简单改默认值）：** 当前链路有三处会把「没设置过」和「显式关闭」压成同一个布尔 `false`：

1. `relaykit/dto/user_settings.go:15` —— `RecordIpLog bool`，JSON 里缺失和 `false` 无法区分。
2. `web/src/features/profile/lib/user-settings.ts:46` —— `parsed.record_ip_log || false`，读取时把 `undefined` 归一成 `false`。
3. `controller/user.go:1384` —— 用请求体里的布尔直接覆盖，且其余入口（`use-profile.ts:100`、`notification-tab.tsx:81`）都会带上这个字段，于是任何一个入口保存一次，就把用户钉死成 `false`。

只改其中任意一处都不成立：改 ① 不改 ③，用户点一次「保存告警设置」就永久关闭记录；改 ③ 不改 ①，响应里永远带 `false`，前端读不出「没设置过」。

---

## File Structure

| 文件 | 职责 | 动作 |
|---|---|---|
| `relaykit/dto/user_settings.go` | 用户设置的 DTO 契约。`RecordIpLog` 改成可空，承载「未表态」语义 | 修改 |
| `model/log.go` | 消费/错误日志的写库决策。判定改为「指针为 nil 即记录」 | 修改 |
| `controller/user.go` | 用户设置写入口。改为保留未提交字段的已存值 | 修改 |
| `web/src/features/profile/lib/user-settings.ts` | 前端设置归一化。`record_ip_log` 缺省为 `true` | 修改 |
| `web/src/features/security/components/privacy-card.tsx` | 隐私开关的初始状态与提交值 | 修改 |
| `model/log_client_ip_test.go` | 后端回归测试（新建，本功能唯一新增测试文件） | 新建 |
| `web/src/features/security/components/__tests__/privacy-card.test.tsx` | 前端开关回归测试 | 修改 |
| `docs/superpowers/plans/2026-10-05-client-ip-default-on.md` | 本计划 | 新建 |

**明确不改（约定，不是延后）：** 本次改的「IP」特指**客户端 IP**，覆盖范围只到
消费日志（type 2）与错误日志（type 5）。充值、系统、退款、管理日志都不记录客户端 IP。

- `model/audit_log.go:79`、`controller/audit.go`、`controller/token.go` —— 审计/登录日志已经无条件记录 IP，无需改动。
- `controller/user_quota.go:81` 那条管理员调额度日志 —— `controller/user_manage_test.go:261` 明确断言「recipient logs must not disclose the administrator IP」，属于既有隐私约束，维持不记。
- `model.RecordLog` / `model.RecordTaskBillingLog` 的调用链（充值、系统、退款日志）—— **不引入 `gin.Context` 透传，不加客户端 IP 字段**。这些日志按设计不含客户端 IP。

> ⚠️ **一处容易混淆的既有行为，不要顺手删**：充值日志（`model/topup.go`）当前的
> `RecordTopupLog(..., callerIp, ...)` 里那个 `callerIp` 是**支付平台回调方的 IP**
> （来源 `controller/topup_stripe.go:191` 的 `c.ClientIP()`，写入 `log.Ip` 与
> `other.caller_ip`），用途是对账与支付取证，前端后台还会展示「Callback Caller IP」。
> 它跟本次的「用户客户端 IP 默认记录」是两回事 —— 保持原样，不在本次改动范围内。

---

## Task 1: DTO 契约改为可空布尔

**Files:**
- Modify: `relaykit/dto/user_settings.go:15`
- Test: `relaykit/dto/user_settings_test.go`（新建）

- [ ] **Step 1: 写失败的测试**

创建 `relaykit/dto/user_settings_test.go`：

```go
package dto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
)

// record_ip_log 必须是可空布尔：JSON 里缺失（用户从未表态）与显式 false
// （用户主动关闭）语义不同，前者要记录 IP，后者不记录。
func TestUserSettingRecordIpLogDistinguishesMissingFromFalse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantNil  bool
		wantBool bool
	}{
		{name: "字段缺失表示从未表态", raw: `{"language":"zh"}`, wantNil: true},
		{name: "显式 false 表示用户关闭", raw: `{"record_ip_log":false}`, wantNil: false, wantBool: false},
		{name: "显式 true 表示用户开启", raw: `{"record_ip_log":true}`, wantNil: false, wantBool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var setting UserSetting
			require.NoError(t, kitutil.UnmarshalJsonStr(tc.raw, &setting))
			if tc.wantNil {
				assert.Nil(t, setting.RecordIpLog)
				return
			}
			require.NotNil(t, setting.RecordIpLog)
			assert.Equal(t, tc.wantBool, *setting.RecordIpLog)
		})
	}
}

// 用户从未表态时不得把字段写进 JSON，否则读回来就变成「已关闭」。
func TestUserSettingRecordIpLogOmittedWhenNil(t *testing.T) {
	enabled := false
	raw, err := kitutil.Marshal(UserSetting{NotifyType: NotifyTypeEmail})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "record_ip_log")

	raw, err = kitutil.Marshal(UserSetting{NotifyType: NotifyTypeEmail, RecordIpLog: &enabled})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"record_ip_log":false`)
}
```

> `kitutil.Marshal` 与 `kitutil.UnmarshalJsonStr` 均为已导出的函数
> （`relaykit/relayconvert/kitutil/json.go:62,70`），无需另找替代。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd relaykit && GOWORK=off go test ./dto/ -run TestUserSettingRecordIpLog -v`
Expected: 编译失败，报 `cannot use &enabled (value of type *bool) as bool value in struct literal` / `setting.RecordIpLog is not a pointer`。

- [ ] **Step 3: 改 DTO 字段类型**

`relaykit/dto/user_settings.go:15`，把：

```go
	RecordIpLog                      bool    `json:"record_ip_log,omitempty"`                        // 是否记录请求和错误日志IP
```

改为：

```go
	// RecordIpLog 控制消费/错误日志是否落客户端 IP。
	// 指针语义：nil = 用户从未表态（按站点默认，记录）；
	// 非 nil = 用户显式选择，照办。不要退回成 bool ——
	// 那会把「没设置过」和「主动关闭」压成同一个值。
	RecordIpLog                      *bool   `json:"record_ip_log,omitempty"`                        // 是否记录请求和错误日志IP
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd relaykit && GOWORK=off go test ./dto/ -v`
Expected: PASS，`ok github.com/QuantumNous/new-api/relaykit/dto`

- [ ] **Step 5: 验证 relaykit 独立可构建**

Run: `cd relaykit && GOWORK=off go build ./...`
Expected: 无输出，退出码 0

- [ ] **Step 6: 提交**

```bash
git add relaykit/dto/user_settings.go relaykit/dto/user_settings_test.go
git commit -m "refactor(relaykit): record_ip_log 改为可空布尔以区分未设置与显式关闭"
```

---

## Task 2: 日志写库判定改为「未表态即记录」

**Files:**
- Modify: `model/log.go:285-291`（`RecordErrorLog`）
- Modify: `model/log.go:349-355`（`RecordConsumeLog`）
- Test: `model/log_client_ip_test.go`（新建）

> 此时 `model/log.go` 的两处 `settingMap.RecordIpLog` 是 `bool`，换成指针后无法直接编译。
> Task 1 与 Task 2 之间仓库是不可编译状态，属预期；Task 2 完成后恢复。

- [ ] **Step 1: 写失败的测试**

创建 `model/log_client_ip_test.go`：

```go
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
	user := User{Username: username, Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: username + "-aff"}
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./model/ -run 'TestRecord(Consume|Error)Log.*ClientIp' -v`
Expected: 编译失败或 FAIL —— `cannot use &disabled (value of type *bool) as bool value`。若 Task 1 未做则类型仍是 `bool`，测试仍会失败在 `RecordIpLog: &disabled`。

- [ ] **Step 3: 抽出共用判定并改两处调用**

在 `model/log.go` 中 `RecordErrorLog` 上方（`RecordErrorLog` 函数定义之前，约 `model/log.go:278`）插入：

```go
// shouldRecordClientIp 决定消费/错误日志是否落客户端 IP。
// 站点默认是记录：用户从未表态（设置为 nil）时记，只有显式关闭才不记。
// 仅消费日志与错误日志走这条判定；充值/系统/退款日志按设计不记 IP。
func shouldRecordClientIp(userId int) bool {
	settingMap, err := GetUserSetting(userId, false)
	if err != nil {
		// 读不到设置时按站点默认处理，宁可多记也不要因为一次读失败就丢 IP。
		return true
	}
	if settingMap.RecordIpLog == nil {
		return true
	}
	return *settingMap.RecordIpLog
}
```

把 `RecordErrorLog` 里（`model/log.go:285-291`）的：

```go
	// 判断是否需要记录 IP
	needRecordIp := false
	if settingMap, err := GetUserSetting(userId, false); err == nil {
		if settingMap.RecordIpLog {
			needRecordIp = true
		}
	}
```

替换为：

```go
	needRecordIp := shouldRecordClientIp(userId)
```

把 `RecordConsumeLog` 里（`model/log.go:349-355`）完全相同的这段代码块替换为：

```go
	needRecordIp := shouldRecordClientIp(userId)
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./model/ -run 'TestRecord(Consume|Error)Log.*ClientIp' -v`
Expected: 4 个子测试全部 PASS

- [ ] **Step 5: 跑整个 model 包，确认没有连带破坏**

Run: `go test ./model/ 2>&1 | tail -20`
Expected: `ok github.com/QuantumNous/new-api/model`

- [ ] **Step 6: 提交**

```bash
git add model/log.go model/log_client_ip_test.go
git commit -m "feat(model): 消费与错误日志默认记录客户端 IP，显式关闭仍尊重用户选择"
```

---

## Task 3: 写设置接口保留未提交字段的已存值

**Files:**
- Modify: `controller/user.go:1282`
- Modify: `controller/user.go:1378-1385`
- Test: `controller/user_setting_test.go`（新建）

- [ ] **Step 1: 写失败的测试**

创建 `controller/user_setting_test.go`：

```go
package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
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
```

> `strings` 只在需要改写成 `strings.NewReader` 时保留，`bytes.Buffer` 不依赖它 —— 若 gofmt/vet
> 报未使用的 import，删掉 `strings` 即可。
> `setupManageUserTestDB` 会调 `i18n.Init()`，因此本文件不需要额外初始化 i18n。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./controller/ -run TestUpdateUserSetting -v`
Expected: `TestUpdateUserSettingPreservesStoredRecordIpLog` FAIL —— `setting.RecordIpLog` 为 nil（当前实现用 `req.RecordIpLog` 直接覆盖，而请求体没带该字段 → `false` 写入，或指针化后为 nil）。

- [ ] **Step 3: 改请求结构体为指针**

`controller/user.go:1282`，把：

```go
	RecordIpLog                      bool    `json:"record_ip_log"`
```

改为：

```go
	// 指针：nil 表示本次请求没有提交这个字段，应保留已存值而不是覆盖成 false。
	RecordIpLog                      *bool   `json:"record_ip_log"`
```

- [ ] **Step 4: 改为读-改-写**

`controller/user.go:1378-1385`，把：

```go
	// 构建设置
	settings := dto.UserSetting{
		NotifyType:                       req.QuotaWarningType,
		QuotaWarningThreshold:            req.QuotaWarningThreshold,
		UpstreamModelUpdateNotifyEnabled: upstreamModelUpdateNotifyEnabled,
		AcceptUnsetRatioModel:            req.AcceptUnsetModelRatioModel,
		RecordIpLog:                      req.RecordIpLog,
	}
```

改为（`existingSettings` 已在上文 `controller/user.go:1372` 取到）：

```go
	// 构建设置。未在本次请求中提交的字段沿用已存值，
	// 避免通知页/隐私卡互相把对方的设置冲掉。
	settings := dto.UserSetting{
		NotifyType:                       req.QuotaWarningType,
		QuotaWarningThreshold:            req.QuotaWarningThreshold,
		UpstreamModelUpdateNotifyEnabled: upstreamModelUpdateNotifyEnabled,
		AcceptUnsetRatioModel:            req.AcceptUnsetModelRatioModel,
		RecordIpLog:                      existingSettings.RecordIpLog,
	}
	if req.RecordIpLog != nil {
		settings.RecordIpLog = req.RecordIpLog
	}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./controller/ -run TestUpdateUserSetting -v`
Expected: 3 个子测试全部 PASS

- [ ] **Step 6: 提交**

```bash
git add controller/user.go controller/user_setting_test.go
git commit -m "fix(controller): 写用户设置时保留未提交字段的已存值"
```

---

## Task 4: 前端默认值改为开启

**Files:**
- Modify: `web/src/features/profile/lib/user-settings.ts:46`
- Modify: `web/src/features/security/components/privacy-card.tsx:42-49`
- Test: `web/src/features/security/components/__tests__/privacy-card.test.tsx`

- [ ] **Step 1: 写失败的测试**

该文件当前的 `renderPrivacy()`（第 52 行）写死使用模块级常量 `profile`，无法按用例改
`setting`。先把它改成接受覆盖参数 —— 只改这一处，既有的 4 个用例调用 `renderPrivacy()`
时行为不变：

```tsx
function renderPrivacy(overrides: Partial<UserProfile> = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const onUpdate = vi.fn()
  const rendered = render(
    <QueryClientProvider client={client}>
      <PrivacyCard profile={{ ...profile, ...overrides }} onUpdate={onUpdate} />
      <Toaster />
    </QueryClientProvider>
  )
  return { ...rendered, onUpdate }
}
```

然后在 `describe('privacy settings', ...)` 内追加两个用例：

```tsx
  it('never-configured users see the IP switch on by default', () => {
    renderPrivacy({ setting: JSON.stringify({ notify_type: 'email' }) })
    expect(
      screen.getByRole('switch', { name: 'Record IP Address' })
    ).toBeChecked()
  })

  it('explicitly disabled users still see the IP switch off', () => {
    renderPrivacy({ setting: JSON.stringify({ record_ip_log: false }) })
    expect(
      screen.getByRole('switch', { name: 'Record IP Address' })
    ).not.toBeChecked()
  })
```

> 模块级常量 `profile`（第 30 行）本身不用改，它的 `setting` 是
> `{ record_ip_log: true }`，既有用例继续按「已开启」跑。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd web && bun run test -- privacy-card`
Expected: `never-configured users see the IP switch on by default` FAIL —— 开关未被选中

- [ ] **Step 3: 归一化默认值改为 true**

`web/src/features/profile/lib/user-settings.ts:46`，把：

```ts
    record_ip_log: parsed.record_ip_log || false,
```

改为：

```ts
    // 站点默认开启 IP 记录：只有用户显式关过才是 false，
    // 未表态（undefined）时按开启处理。参见 relaykit/dto/user_settings.go。
    record_ip_log: parsed.record_ip_log ?? true,
```

- [ ] **Step 4: 隐私开关初始状态对齐后端语义**

`web/src/features/security/components/privacy-card.tsx:42-49`，把：

```tsx
  const [recordIpLog, setRecordIpLog] = useState(() =>
    Boolean(parseUserSettings(props.profile.setting).record_ip_log)
  )
  useEffect(() => {
    setRecordIpLog(
      Boolean(parseUserSettings(props.profile.setting).record_ip_log)
    )
  }, [props.profile.setting])
```

改为：

```tsx
  // 后端未表态（字段缺失）= 站点默认开启。
  // 注意不能用 Boolean(...)：undefined 会被压成 false，界面就与后端不一致了。
  const [recordIpLog, setRecordIpLog] = useState(
    () => parseUserSettings(props.profile.setting).record_ip_log ?? true
  )
  useEffect(() => {
    setRecordIpLog(
      parseUserSettings(props.profile.setting).record_ip_log ?? true
    )
  }, [props.profile.setting])
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd web && bun run test -- privacy-card`
Expected: 该文件全部用例 PASS（含既有的 4 个开关交互用例）

- [ ] **Step 6: 类型检查与 lint**

Run: `cd web && bun run typecheck && bunx oxlint -c .oxlintrc.json src/features/security/components/privacy-card.tsx src/features/profile/lib/user-settings.ts`
Expected: typecheck 无输出（退出码 0）；oxlint 无 error

- [ ] **Step 7: 提交**

```bash
git add web/src/features/profile/lib/user-settings.ts web/src/features/security/components/privacy-card.tsx web/src/features/security/components/__tests__/privacy-card.test.tsx
git commit -m "feat(web): 隐私设置里的 IP 记录开关默认开启"
```

---

## Task 5: 全量回归与部署

**Files:**
- 无代码改动

- [ ] **Step 1: 根模块全量测试（需先构建前端，`main.go` 有 `//go:embed web/dist`）**

```bash
cd web && bun install --frozen-lockfile && bun run build
cd .. && go test ./... 2>&1 | grep -v "^ok\|no test files" | head -30
```

Expected: 无 FAIL 行。若报 `main.go:44:12: pattern web/dist: no matching files found`，说明前端没构建成功，先修前端。

- [ ] **Step 2: relaykit 独立构建与测试**

```bash
cd relaykit && GOWORK=off go build ./... && GOWORK=off go test ./...
```

Expected: 无输出；`ok` 行

- [ ] **Step 3: 前端全量测试与构建**

```bash
cd web && bun run test && bun run typecheck && bun run lint
```

Expected: 测试全绿；typecheck 退出 0；lint 无 error

- [ ] **Step 4: 提交本计划文档**

```bash
git add docs/superpowers/plans/2026-10-05-client-ip-default-on.md
git commit -m "docs: 客户端 IP 记录默认开启的实施计划"
```

（若该文档已在写作阶段提交，此步跳过即可 —— 检查 `git log --oneline -5` 有无对应提交。）

- [ ] **Step 5: 部署（node1 号池站 18082）**

版本串用 `juao_dev_base40-$(git rev-parse --short HEAD)`。构建必须在**干净 worktree** 里做，否则 `vcs.modified=true` 且 `builds/`、`juao-api/` 等未跟踪文件会混进产物。

```bash
# 1. 干净 worktree
git worktree add --detach /tmp/build-ip-default "$(git rev-parse HEAD)"
ln -s "$PWD/web/node_modules" /tmp/build-ip-default/web/node_modules

# 2. 前端
cd /tmp/build-ip-default/web
VITE_REACT_APP_VERSION=juao_dev_base40-$(git -C "$OLDPWD" rev-parse --short HEAD) bun run build

# 3. 后端交叉编译
cd /tmp/build-ip-default
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOPROXY=https://goproxy.cn,direct \
  go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=juao_dev_base40-$(git rev-parse --short HEAD)'" \
  -o /Users/david/Documents/juao_ops/builds/newapi-pool-linux-amd64-juao_dev_base40-$(git rev-parse --short HEAD) .

# 4. 校验产物
go version -m /Users/david/Documents/juao_ops/builds/newapi-pool-linux-amd64-juao_dev_base40-*. 2>/dev/null | grep -E "vcs|mod\s+github"
# 期望：build vcs.revision=<本提交> 且 build vcs.modified=false

# 5. 清理 worktree
rm -f /tmp/build-ip-default/web/node_modules
git worktree remove --force /tmp/build-ip-default
```

> ⚠️ 前端 `VITE_REACT_APP_VERSION` 在本仓库**不生效** —— `web/src/lib/build-metadata.ts`
> 那套 `import.meta.env?.VITE_REACT_APP_VERSION` 已被打包器摇树成死代码
> （产物里只剩 `if("string"==typeof e&&e.length>0)return e`，恒返回 `"0000"`）。
> 传这个变量无害但无作用，版本标识仍以后端 `X-New-Api-Version` 响应头为准。

- [ ] **Step 6: 备份并部署**

```bash
TS=$(date +%Y%m%d-%H%M%S)
ssh -p 52200 davidshan@121.41.73.50 "sudo cp -a /usr/local/bin/newapi-pool /usr/local/bin/newapi-pool.bak-$TS"
ssh -p 52200 davidshan@121.41.73.50 "sudo -u newapi sqlite3 /var/lib/newapi-pool/one-api.db \".backup /var/lib/newapi-pool/one-api.db.bak-$TS\""

cat /Users/david/Documents/juao_ops/builds/newapi-pool-linux-amd64-juao_dev_base40-*. | \
  ssh -p 52200 davidshan@121.41.73.50 "cat > /tmp/newapi-pool.new && sha256sum /tmp/newapi-pool.new"
# 与本机 shasum -a 256 比对，不一致就停下

ssh -p 52200 davidshan@121.41.73.50 "
  sudo systemctl stop newapi-pool &&
  sudo install -m 0755 -o root -g root /tmp/newapi-pool.new /usr/local/bin/newapi-pool &&
  sudo systemctl start newapi-pool && sleep 8 &&
  systemctl is-active newapi-pool &&
  curl -s -o /dev/null -w 'http=%{http_code}\n' http://127.0.0.1:18082/api/status &&
  curl -sI http://127.0.0.1:18082/ | grep -i x-new-api-version"
```

Expected: `active`、`http=200`、版本头为 `juao_dev_base40-<新 short sha>`

- [ ] **Step 7: 线上验收**

用一条测试账号发起一次消费请求，然后查该用户的消费日志 IP 是否非空：

```bash
ssh -p 52200 davidshan@121.41.73.50 \
  "sudo -u newapi sqlite3 /var/lib/newapi-pool/one-api.db \
   \"SELECT id, user_id, type, ip, created_at FROM logs WHERE type = 2 ORDER BY id DESC LIMIT 5;\""
```

Expected: 新产生的 `type=2` 行 `ip` 列非空。

- [ ] **Step 8: 推送**

```bash
git push origin juao_dev_base40
```

> push 前先确认远端分支状态；本分支没有 upstream 之外的协作者时用普通 push 即可。

---

## 回滚

```bash
# 二进制
ssh -p 52200 davidshan@121.41.73.50 \
  "sudo install -m 0755 /usr/local/bin/newapi-pool.bak-<TS> /usr/local/bin/newapi-pool && sudo systemctl restart newapi-pool"

# 数据库（仅在需要时；会丢弃备份点之后的数据）
ssh -p 52200 davidshan@121.41.73.50 \
  "sudo systemctl stop newapi-pool && sudo -u newapi cp /var/lib/newapi-pool/one-api.db.bak-<TS> /var/lib/newapi-pool/one-api.db && sudo systemctl start newapi-pool"
```

---

## Self-Review

**Spec coverage：**
- 「未设置即为开」→ Task 1（契约）+ Task 2（判定）+ Task 4（前端展示）✅
- IP 记录范围严格限定为消费日志（type 2）与错误日志（type 5）→ Task 2 只改这两处调用；
  充值/系统/退款/管理日志的调用链（`model.RecordLog`、`model.RecordTaskBillingLog`、
  `model/redemption.go`、`model/subscription.go`、`model/topup.go`、`controller/checkin.go`）
  一律不动，已在 File Structure 的「明确不改」里写明 ✅
- 「已显式关闭的用户保持关闭」→ Task 2 的 `TestRecordConsumeLogSkipsClientIpWhenExplicitlyDisabled`、Task 3 的 `TestUpdateUserSettingPreservesStoredRecordIpLog` ✅
- 「不改动读-改-写之外的路径」→ 保持 `model/user.go:796`、`:822` 与 `controller/subscription.go:93` 三处 `GetSetting()`→改字段→`UpdateUserSetting` 的既有模式不动 ✅

**Placeholder scan：** 无 TBD/TODO；Task 3 与 Task 4 各有一处「先看既有夹具再决定是否新建」的指令，都给出了具体文件与行号，不是占位符。

**Type consistency：**
- `RecordIpLog` 在 Task 1 定义为 `*bool`，Task 2 用 `settingMap.RecordIpLog == nil` 判空、`*settingMap.RecordIpLog` 解引用，Task 3 用 `req.RecordIpLog != nil` 判空 —— 均为 `*bool` ✅
- `shouldRecordClientIp(userId int) bool` 在 Task 2 定义并在同任务两处调用，签名一致 ✅
- 前端 `record_ip_log` 全程按 `boolean | undefined` 处理（`?? true`），未与后端的 `*bool` 混淆 ✅

**已知遗留（不在本计划范围）：** `web/src/features/profile/types.ts:120,163` 的 `record_ip_log?: boolean` 保持可选即可，无需改成三态 —— 前端只需要区分 `undefined` 与 `false`，`boolean | undefined` 已经够用。
