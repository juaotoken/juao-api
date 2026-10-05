# 错误日志记录原始请求信息 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 上游返回错误时，把客户端发来的原始请求内容（前 1000 字符）与 HTTP 原始请求（method / URL / 全量请求头，凭据打码）写进错误日志，供管理员排查；由系统设置里一个默认开启的开关控制。

**Architecture:** 新增 `service/log_raw_request.go` 作为采集单元，从 `common.GetBodyStorage(c)` 的独立游标限量读取入站 body。采集点放在既有的 `service.ProcessChannelError` 里，通过新增的 `decision` 参数判定「是否最终失败」；结果挂到 `other.admin_info.raw_request`，复用现有的三层可见性机制，不改表、不加迁移。

**Tech Stack:** Go 1.25.1 / Gin / GORM v2 / testify；React 19 / TypeScript / Vitest / react-i18next。

**设计文档：** `docs/superpowers/specs/2026-10-05-error-log-raw-request-design.md`

---

## 背景（实施者必读）

线上排查上游报错时只能看到一句被掩码过的错误文本。上游明确让人「check the request body」，
但请求体在请求结束时就随 `BodyStorage` 一起销毁，客户端请求头从头到尾没有任何地方落盘。
本计划要补的就是这两样。

**当前生产的 `.env` 里 `ERROR_LOG_ENABLED=true`，所以错误日志行本身是有的，缺的只是原始请求内容。**

## 需要注意的项目约定

- **JSON 一律走 `common.Marshal` / `common.Unmarshal`**，禁止直接 `encoding/json`（`AGENTS.md`）。
- **不为小功能散落测试文件**：后端只新建 `service/log_raw_request_test.go`，其余用例并入现有
  `service/relay_error_test.go`。前端只新建一个测试文件。
- **前端复用优先**：复制按钮用 `@/components/copy-button` 的 `CopyButton`，不要手写按钮 + hook。
- **前端 UI 文案必须走 i18n**，7 个 locale 全补。
- 每个任务结束都提交一次，`git add` 只加本任务涉及的文件。

---

## File Structure

| 文件 | 职责 | 动作 |
|---|---|---|
| `common/constants.go` | 新增开关变量 `ErrorLogRawRequestEnabled` | 修改 |
| `model/option.go` | 开关的注册与运行时更新；`IsOptionAvailable` 白名单 | 修改 |
| `service/log_raw_request.go` | **新单元**：从 gin 上下文采集原始请求（body / headers / method / url / attempts），是本次唯一的新增生产文件 | 新建 |
| `service/relay_error.go` | 采集触发点：`ProcessChannelError` 增加 `decision` 参数，在最终失败时挂上 `raw_request` | 修改 |
| `controller/relay.go` | 两个调用点传 `decision`；新增 `channelErrorDecision` 判定函数 | 修改 |
| `relay/responses_websocket.go` | WebSocket 调用点传 `decision` | 修改 |
| `controller/channel-test.go` | 渠道测试探针调用点传零值 decision | 修改 |
| `web/src/features/system-settings/types.ts` | `OperationsSettings` 加开关字段 | 修改 |
| `web/src/features/system-settings/operations/index.tsx` | 默认值 `true` | 修改 |
| `web/src/features/system-settings/operations/section-registry.tsx` | 给 `logs` 节传默认值 | 修改 |
| `web/src/features/system-settings/maintenance/log-settings-section.tsx` | 新增开关 UI | 修改 |
| `web/src/features/usage-logs/types.ts` | `LogOtherData['admin_info']` 加 `raw_request` 类型 | 修改 |
| `web/src/features/usage-logs/components/dialogs/details-dialog.tsx` | 新增「Raw Request」展示区块 | 修改 |
| `web/src/i18n/locales/*.json` | 5 个新 key × 7 个 locale | 修改 |
| `service/log_raw_request_test.go` | 采集单元的表驱动用例（**唯一新增后端测试**） | 新建 |
| `service/relay_error_test.go` | 门控用例（开关关 / 非最终失败 / relayInfo 为 nil） | 修改 |
| `web/src/features/usage-logs/components/__tests__/raw-request.test.tsx` | 前端展示用例 | 新建 |

---

## Task 1: 采集原始请求的纯函数

**Files:**
- Create: `service/log_raw_request.go`
- Test: `service/log_raw_request_test.go`

- [ ] **Step 1: 写失败的测试**

新建 `service/log_raw_request_test.go`：

```go
package service

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRawRequestContext 构造带指定 body 与 Content-Type 的 gin 上下文，
// 并把 body 装进 BodyStorage（与真实 relay 链路一致）。
func newRawRequestContext(t *testing.T, contentType string, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	if contentType != "" {
		c.Request.Header.Set("Content-Type", contentType)
	} else {
		c.Request.Header.Del("Content-Type")
	}
	_, err := common.GetRequestBody(c)
	require.NoError(t, err)
	return c
}

func TestCollectRawRequestInfoKeepsShortJsonBodyIntact(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	c := newRawRequestContext(t, "application/json", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Equal(t, "POST", info.Method)
	assert.Equal(t, "/v1/chat/completions", info.URL)
	assert.Equal(t, string(body), info.Body)
	assert.False(t, info.BodyTruncated)
	assert.Equal(t, int64(len(body)), info.BodyBytes)
	assert.Equal(t, "application/json", info.BodyContentType)
}

func TestCollectRawRequestInfoTruncatesLongBodyByRune(t *testing.T) {
	body := []byte(`{"pad":"` + strings.Repeat("a", 5000) + `"}`)
	c := newRawRequestContext(t, "application/json", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Len(t, []rune(info.Body), 1000)
	assert.True(t, info.BodyTruncated)
	assert.Equal(t, int64(len(body)), info.BodyBytes)
}

func TestCollectRawRequestInfoCountsChineseCharacters(t *testing.T) {
	// 1001 个汉字，UTF-8 下是 3003 字节。按 rune 截断应留下 1000 个汉字。
	body := []byte(`{"text":"` + strings.Repeat("汉", 1001) + `"}`)
	c := newRawRequestContext(t, "application/json", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Len(t, []rune(info.Body), 1000)
	// 截断点必须落在字符边界上：整个前缀应当能原样编码回去。
	assert.True(t, strings.HasPrefix(string(body), info.Body))
	assert.True(t, info.BodyTruncated)
}

func TestCollectRawRequestInfoMarksNonJsonBody(t *testing.T) {
	body := []byte("--boundary\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\n\x00\x01\x02")
	c := newRawRequestContext(t, "multipart/form-data; boundary=boundary", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Contains(t, info.Body, "[skip non-json body: multipart/form-data; boundary=boundary]")
	assert.Equal(t, int64(len(body)), info.BodyBytes)
	assert.Equal(t, "multipart/form-data; boundary=boundary", info.BodyContentType)
}

func TestCollectRawRequestInfoMarksMissingContentType(t *testing.T) {
	body := []byte(`{"model":"glm-5.3"}`)
	c := newRawRequestContext(t, "", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Contains(t, info.Body, "[skip non-json body")
	assert.Equal(t, int64(len(body)), info.BodyBytes)
	assert.Empty(t, info.BodyContentType)
}

func TestCollectRawRequestInfoAcceptsJsonSuffixContentType(t *testing.T) {
	body := []byte(`{"model":"glm-5.3"}`)
	c := newRawRequestContext(t, "application/vnd.api+json", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Equal(t, string(body), info.Body)
}

func TestCollectRawRequestInfoMasksCredentialHeaders(t *testing.T) {
	c := newRawRequestContext(t, "application/json", []byte(`{}`))
	c.Request.Header.Set("Authorization", "Bearer sk-real-token")
	c.Request.Header.Set("Cookie", "session=real-cookie")
	c.Request.Header.Set("x-api-key", "real-anthropic-key")
	c.Request.Header.Set("x-goog-api-key", "real-gemini-key")
	c.Request.Header.Set("mj-api-secret", "real-mj-secret")
	c.Request.Header.Set("Sec-WebSocket-Protocol", "realtime,openai-insecure-api-key.real-realtime-key")
	c.Request.Header.Set("User-Agent", "OpenAI/Python 1.52.0")
	c.Request.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key", "X-Goog-Api-Key", "Mj-Api-Secret", "Sec-Websocket-Protocol"} {
		assert.Equal(t, "***", info.Headers[name], "credential header %s must be masked", name)
	}
	assert.Equal(t, "OpenAI/Python 1.52.0", info.Headers["User-Agent"])
	assert.Equal(t, "prompt-caching-2024-07-31", info.Headers["Anthropic-Beta"])
}

func TestCollectRawRequestInfoTruncatesOversizedHeaderValue(t *testing.T) {
	c := newRawRequestContext(t, "application/json", []byte(`{}`))
	c.Request.Header.Set("X-Custom", strings.Repeat("x", 600))

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.True(t, info.HeadersTruncated)
	value := info.Headers["X-Custom"]
	assert.Equal(t, 512, len([]rune(strings.TrimSuffix(value, "…"))))
	assert.True(t, strings.HasSuffix(value, "…"))
}

func TestCollectRawRequestInfoMasksCredentialQueryKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?key=real-gemini-key&page=2", nil)
	_, err := common.GetRequestBody(c)
	require.NoError(t, err)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.NotContains(t, info.URL, "real-gemini-key")
	// SanitizeURLForLog 把命中的 query 值换成 "***masked***" 并做 URL 编码，
	// 所以断言要解析回 query 值，不要直接比对字面量。
	parsed, err := url.Parse(info.URL)
	require.NoError(t, err)
	assert.Equal(t, "***masked***", parsed.Query().Get("key"))
	assert.Equal(t, "2", parsed.Query().Get("page"))
}

func TestCollectRawRequestInfoReportsAttemptedChannels(t *testing.T) {
	c := newRawRequestContext(t, "application/json", []byte(`{}`))
	c.Set("use_channel", []string{"150", "7", "162"})

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Equal(t, 3, info.Attempts)
}

func TestCollectRawRequestInfoWithEmptyBodyReturnsMetadataOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Equal(t, "POST", info.Method)
	assert.Empty(t, info.Body)
	assert.Equal(t, int64(0), info.BodyBytes)
	assert.Equal(t, "application/json", info.BodyContentType)
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /Users/david/Documents/juao_ops/juao-api
go test ./service/ -run TestCollectRawRequestInfo -v
```

Expected: 编译失败，`undefined: CollectRawRequestInfo`。

- [ ] **Step 3: 写实现**

新建 `service/log_raw_request.go`：

```go
package service

import (
	"io"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

const (
	// rawRequestBodyRuneLimit 是写入日志的 body 字符数上限。按 rune 计数，
	// 中文 prompt 能看满 1000 个汉字；同时天然不会切出半个 UTF-8 字符。
	rawRequestBodyRuneLimit = 1000
	// rawRequestBodyReadBytes 是实际从 BodyStorage 读出的字节上限。
	// UTF-8 单字符最多 4 字节，1000 个字符最坏情况需要 4000 字节；
	// 多读 1 字节用于判断是否发生了截断。
	rawRequestBodyReadBytes = rawRequestBodyRuneLimit*4 + 1
	// rawRequestHeaderValueRuneLimit 是单个请求头值的字符数上限，
	// 防止异常客户端塞超长头把日志撑爆。
	rawRequestHeaderValueRuneLimit = 512
	// rawRequestMaskedValue 是凭据类请求头的替换值。
	rawRequestMaskedValue = "***"
)

// RawRequestInfo 是错误日志里记录的原始请求快照。
// 它挂载在 other.admin_info.raw_request 下，仅管理员及以上可见。
type RawRequestInfo struct {
	Method           string            `json:"method"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"`
	HeadersTruncated bool              `json:"headers_truncated,omitempty"`
	Body             string            `json:"body,omitempty"`
	BodyTruncated    bool              `json:"body_truncated,omitempty"`
	BodyBytes        int64             `json:"body_bytes"`
	BodyContentType  string            `json:"body_content_type"`
	Attempts         int               `json:"attempts,omitempty"`
}

// credentialHeaderNames 是值必须打码的请求头（小写）。
var credentialHeaderNames = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
	// TokenAuth 允许用 mj-api-secret 代替 Authorization 传 key。
	"mj-api-secret": {},
	// Realtime 客户端把 key 塞在子协议里，OpenAI 的官方约定就是
	// "openai-insecure-api-key.<key>"。
	"sec-websocket-protocol": {},
}

// isCredentialHeader 判定请求头是否携带凭据。
// 头名统一小写后匹配固定名单与前缀/后缀模式。
func isCredentialHeader(lowerName string) bool {
	if _, ok := credentialHeaderNames[lowerName]; ok {
		return true
	}
	if strings.HasSuffix(lowerName, "-key") || strings.HasSuffix(lowerName, "apikey") {
		return true
	}
	for _, marker := range []string{"token", "secret", "signature", "api-key"} {
		if strings.Contains(lowerName, marker) {
			return true
		}
	}
	return false
}

// truncateRunes 把字符串截到至多 limit 个字符。
func truncateRunes(value string, limit int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}
	return string(runes[:limit]), true
}

// collectRawRequestHeaders 复制请求头并打码凭据值。
func collectRawRequestHeaders(header map[string][]string) (map[string]string, bool) {
	if len(header) == 0 {
		return nil, false
	}
	headers := make(map[string]string, len(header))
	truncated := false
	for name := range header {
		value := strings.TrimSpace(strings.Join(header[name], ","))
		if value == "" {
			continue
		}
		if isCredentialHeader(strings.ToLower(name)) {
			headers[name] = rawRequestMaskedValue
			continue
		}
		shortened, wasTruncated := truncateRunes(value, rawRequestHeaderValueRuneLimit)
		if wasTruncated {
			truncated = true
			shortened += "…"
		}
		headers[name] = shortened
	}
	if len(headers) == 0 {
		return nil, false
	}
	return headers, truncated
}

// isJSONContentType 判定 Content-Type 是否承载 JSON 报文。
func isJSONContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// collectRawRequestBody 从 BodyStorage 的独立游标限量读出入站请求体。
// 读取失败或没有 body 时返回空串，由调用方继续用 Size() 记录总长度。
//
// 刻意不用 storage.Bytes()：它会 io.ReadFull 整个 body，遇到大请求会把
// 整份内容读进内存。NewReader() 拿的是独立游标，不扰动正在被上游消费的那份。
func collectRawRequestBody(c *gin.Context) (body string, truncated bool, totalBytes int64, contentType string) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return "", false, 0, ""
	}
	totalBytes = storage.Size()
	contentType = c.Request.Header.Get("Content-Type")
	if totalBytes == 0 {
		return "", false, totalBytes, contentType
	}
	if !isJSONContentType(contentType) {
		// 二进制/表单 body 写进日志既不可读又占体积，只留一行标记。
		return "[skip non-json body: " + contentType + "]", false, totalBytes, contentType
	}
	reader, err := storage.NewReader()
	if err != nil {
		return "", false, totalBytes, contentType
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, rawRequestBodyReadBytes))
	if err != nil {
		return "", false, totalBytes, contentType
	}
	body, truncated = truncateRunes(string(raw), rawRequestBodyRuneLimit)
	if !truncated {
		truncated = totalBytes > int64(len(raw))
	}
	return body, truncated, totalBytes, contentType
}

// CollectRawRequestInfo 采集错误日志要记录的原始请求快照。
// 调用方负责判定「当前错误是否值得记录」，本函数只做采集，不读开关。
func CollectRawRequestInfo(c *gin.Context) *RawRequestInfo {
	if c == nil || c.Request == nil {
		return nil
	}
	info := &RawRequestInfo{Method: c.Request.Method}
	if c.Request.URL != nil {
		info.URL = relaycommon.SanitizeURLForLog(c.Request.URL.RequestURI())
	}
	info.Headers, info.HeadersTruncated = collectRawRequestHeaders(c.Request.Header)
	info.Body, info.BodyTruncated, info.BodyBytes, info.BodyContentType = collectRawRequestBody(c)
	info.Attempts = len(c.GetStringSlice("use_channel"))
	return info
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./service/ -run TestCollectRawRequestInfo -v
```

Expected: 全部 PASS。

- [ ] **Step 5: 提交**

```bash
gofmt -w service/log_raw_request.go service/log_raw_request_test.go
git add service/log_raw_request.go service/log_raw_request_test.go
git commit -m "feat(service): 采集错误日志的原始请求快照

从 BodyStorage 的独立游标按 rune 限量读取入站 body（1000 字符），
复制全量请求头并把凭据类值打码，复用 SanitizeURLForLog 掩码 query。
非 JSON body 只留一行标记，不落二进制内容。"
```

---

## Task 2: 开关（后端）

**Files:**
- Modify: `common/constants.go:94`
- Modify: `model/option.go:53`（`InitOptionMap`）
- Modify: `model/option.go:401`（`updateOptionMap` 的 switch）

- [ ] **Step 1: 加开关变量**

`common/constants.go`，把第 94 行：

```go
var LogConsumeEnabled = true
```

改为：

```go
var LogConsumeEnabled = true

// ErrorLogRawRequestEnabled 控制错误日志是否附带原始请求快照
// （original body 前 1000 字符 + HTTP method / URL / 请求头）。
// 站点默认开启：这是排查上游报错的唯一线索。
var ErrorLogRawRequestEnabled = true
```

- [ ] **Step 2: 注册到 OptionMap**

`model/option.go`，在第 53 行 `common.OptionMap["LogConsumeEnabled"] = ...` 之后另起一行：

```go
	common.OptionMap["ErrorLogRawRequestEnabled"] = strconv.FormatBool(common.ErrorLogRawRequestEnabled)
```

- [ ] **Step 3: 接到运行时更新**

`model/option.go`，`updateOptionMap` 里的 switch，在第 401 行 `case "LogConsumeEnabled":` 那个分支之后插入：

```go
		case "ErrorLogRawRequestEnabled":
			common.ErrorLogRawRequestEnabled = boolValue
```

- [ ] **Step 4: 确认无需额外白名单**

后台是通过 `model.UpdateOption` → `updateOptionMap` 写入任意 key 的，没有 key 白名单需要登记。
先确认这一点：

```bash
grep -rn "IsOptionAvailable\|witchKeys" model/ controller/ | head
```

Expected: 无输出（这两个东西都不存在）。若确实有白名单，补上
`"ErrorLogRawRequestEnabled"` 再继续。

- [ ] **Step 5: 编译并跑 model 包测试**

```bash
go build ./... && go test ./model/ ./service/
```

Expected: 编译通过，测试全绿。

- [ ] **Step 6: 提交**

```bash
gofmt -w common/constants.go model/option.go
git add common/constants.go model/option.go
git commit -m "feat(model): 新增 ErrorLogRawRequestEnabled 开关，默认开启"
```

---

## Task 3: 接入 ProcessChannelError（只在最终失败时记录）

**Files:**
- Modify: `service/relay_error.go:64-99`
- Modify: `controller/relay.go:209`、`controller/relay.go:296`（包装函数）、`controller/relay.go:563`
- Modify: `relay/responses_websocket.go:321`
- Modify: `controller/channel-test.go:953`
- Test: `service/relay_error_test.go`

- [ ] **Step 1: 写失败的测试**

在 `service/relay_error_test.go` 末尾追加。该文件已 import `errors`、`io`、`net/http`、
`net/http/httptest`、`testing`、`time`、`common`、`constant`、`model`、`gin`、`sqlite`、
`assert`、`require`、`gorm`。需要新增三个 import：`strings`、
`relaycommon "github.com/QuantumNous/new-api/relay/common"`。

注意：`ProcessChannelError` 内部会调 `model.GetUserSetting(userId, false)` 读 `model.DB`，
所以每个用例都要准备 `model.DB`（与同文件已有的 `TestProcessChannelErrorMasksDisableReasonAndNotification`
一致的做法），只准备 `model.LOG_DB` 会在读用户设置时 panic。

```go
// setupRawRequestLogTestDB 准备错误日志测试用的双库上下文。
// ProcessChannelError 会读 model.DB 取用户设置（决定是否记 IP），写日志走 model.LOG_DB，
// 两者都要可用。返回的 database 同时被赋给 DB 和 LOG_DB。
func setupRawRequestLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Log{}))
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, model.LOG_DB = database, database
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		require.NoError(t, sqlDB.Close())
	})
	return database
}

// newRawRequestChannelErrorContext 构造一个带 BodyStorage 的错误日志上下文。
func newRawRequestChannelErrorContext(t *testing.T, requestURL string, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, requestURL, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "OpenAI/Python 1.52.0")
	c.Request.Header.Set("Authorization", "Bearer sk-should-not-be-stored")
	c.Set("id", 1)
	c.Set("token_name", "token")
	c.Set("original_model", "glm-5.3")
	c.Set("group", "default")
	c.Set("use_channel", []string{"150"})
	_, err := common.GetRequestBody(c)
	require.NoError(t, err)
	return c
}

// storedErrorLogAdminInfo 取出唯一一条错误日志的 admin_info。
func storedErrorLogAdminInfo(t *testing.T, database *gorm.DB) map[string]any {
	t.Helper()
	var stored model.Log
	require.NoError(t, database.Where("type = ?", model.LogTypeError).First(&stored).Error)
	other, err := common.StrToMap(stored.Other)
	require.NoError(t, err)
	adminInfo, _ := other["admin_info"].(map[string]any)
	return adminInfo
}

func TestProcessChannelErrorRecordsRawRequestOnFinalFailure(t *testing.T) {
	previousErrorLog, previousRawRequest := constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled
	previousAutoDisable := common.AutomaticDisableChannelEnabled
	constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = true, true
	common.AutomaticDisableChannelEnabled = false
	t.Cleanup(func() {
		constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = previousErrorLog, previousRawRequest
		common.AutomaticDisableChannelEnabled = previousAutoDisable
	})
	database := setupRawRequestLogTestDB(t)

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"真实报错的请求"}]}`
	c := newRawRequestChannelErrorContext(t, "/v1/chat/completions", body)
	apiErr := types.NewOpenAIError(errors.New("bad request"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)

	ProcessChannelError(c, types.ChannelError{ChannelId: 150, ChannelName: "glm"}, apiErr,
		&relaycommon.RelayInfo{}, PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"})

	var stored model.Log
	require.NoError(t, database.Where("type = ?", model.LogTypeError).First(&stored).Error)
	adminInfo := storedErrorLogAdminInfo(t, database)
	rawRequest, ok := adminInfo["raw_request"].(map[string]any)
	require.True(t, ok, "final failure must carry the raw request snapshot")
	assert.Equal(t, "POST", rawRequest["method"])
	assert.Equal(t, "/v1/chat/completions", rawRequest["url"])
	assert.Equal(t, body, rawRequest["body"])
	assert.Equal(t, "application/json", rawRequest["body_content_type"])
	assert.Equal(t, float64(1), rawRequest["attempts"])
	headers, ok := rawRequest["headers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "OpenAI/Python 1.52.0", headers["User-Agent"])
	assert.Equal(t, "***", headers["Authorization"], "the client credential must never reach the log database")
	assert.NotContains(t, stored.Other, "sk-should-not-be-stored")
}

func TestProcessChannelErrorSkipsRawRequestWhileRetrying(t *testing.T) {
	previousErrorLog, previousRawRequest := constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled
	constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = true, true
	t.Cleanup(func() {
		constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = previousErrorLog, previousRawRequest
	})
	database := setupRawRequestLogTestDB(t)

	c := newRawRequestChannelErrorContext(t, "/v1/chat/completions", `{"model":"glm-5.3"}`)
	apiErr := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	ProcessChannelError(c, types.ChannelError{ChannelId: 150, ChannelName: "glm"}, apiErr,
		&relaycommon.RelayInfo{}, PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"})

	assert.NotContains(t, storedErrorLogAdminInfo(t, database), "raw_request",
		"an intermediate retry attempt must not carry the raw request")
}

func TestProcessChannelErrorSkipsRawRequestWhenSwitchDisabled(t *testing.T) {
	previousErrorLog, previousRawRequest := constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled
	constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = true, false
	t.Cleanup(func() {
		constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = previousErrorLog, previousRawRequest
	})
	database := setupRawRequestLogTestDB(t)

	c := newRawRequestChannelErrorContext(t, "/v1/chat/completions", `{"model":"glm-5.3"}`)
	apiErr := types.NewOpenAIError(errors.New("bad request"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)

	ProcessChannelError(c, types.ChannelError{ChannelId: 150, ChannelName: "glm"}, apiErr,
		&relaycommon.RelayInfo{}, PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"})

	assert.NotContains(t, storedErrorLogAdminInfo(t, database), "raw_request",
		"the switch must be able to turn the snapshot off")
}

func TestProcessChannelErrorSkipsRawRequestForChannelTestProbe(t *testing.T) {
	previousErrorLog, previousRawRequest := constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled
	constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = true, true
	t.Cleanup(func() {
		constant.ErrorLogEnabled, common.ErrorLogRawRequestEnabled = previousErrorLog, previousRawRequest
	})
	database := setupRawRequestLogTestDB(t)

	c := newRawRequestChannelErrorContext(t, "/v1/chat/completions", `{"model":"glm-5.3"}`)
	apiErr := types.NewOpenAIError(errors.New("bad request"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)

	// 渠道测试探针不传 relayInfo：body 是程序合成的，不是用户请求。
	ProcessChannelError(c, types.ChannelError{ChannelId: 150, ChannelName: "glm"}, apiErr, nil,
		PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"})

	assert.NotContains(t, storedErrorLogAdminInfo(t, database), "raw_request",
		"a channel test probe is not a user request")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./service/ -run TestProcessChannelError -v
```

Expected: 编译失败 —— `too many arguments in call to ProcessChannelError`。

- [ ] **Step 3: 改 `ProcessChannelError`**

`service/relay_error.go`，把函数签名与 `other` 构造部分改成：

```go
// ProcessChannelError 记录一次渠道失败。decision 是本次尝试后 DecideRelayRetry /
// decideTaskRetry 给出的重试判定：只有最终失败（action != "retry"）才采集原始请求快照，
// 否则一次请求重试多个渠道会在每条日志上重复同一份 body。
func ProcessChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo, decision PolicyDecision) {
	if err == nil {
		return
	}
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, common.LocalLogPreview(err.MaskSensitiveErrorWithStatusCode())))
	if ShouldDisableChannel(err) && channelError.AutoBan {
		reason := err.MaskSensitiveErrorWithStatusCode()
		gopool.Go(func() {
			DisableChannel(channelError, reason)
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		other := model.NewLogOther()
		if c.Request != nil && c.Request.URL != nil {
			other.SetPublic("request_path", c.Request.URL.Path)
		}
		other.SetPublic("error_type", err.GetErrorType())
		other.SetPublic("error_code", err.GetErrorCode())
		other.SetPublic("status_code", err.StatusCode)
		AppendRelayLogAdminInfo(c, relayInfo, other)
		AppendResponseModelLogInfo(relayInfo, other)
		AppendTaskPluginContextAuditInfo(c, other)
		if common.ErrorLogRawRequestEnabled && relayInfo != nil && decision.Action != "retry" {
			if rawRequest := CollectRawRequestInfo(c); rawRequest != nil {
				other.SetAdmin("raw_request", rawRequest)
			}
		}
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(c, userId, channelError.ChannelId, modelName, tokenName, err.MaskSensitiveErrorWithStatusCode(), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}
}
```

- [ ] **Step 4: 更新三个真实调用点**

`controller/relay.go` 第 209 行，改为：

```go
		ProcessChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError, relayInfo, decision)
```

（该处通过包内包装 `processChannelError` 调用，保持包装函数不动，只给它加上同样的参数：

```go
func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo, decision service.PolicyDecision) {
	service.ProcessChannelError(c, channelError, err, relayInfo, decision)
}
```

）

`controller/relay.go` 第 563 行（任务提交路径），把已有的 `decision` 传进去：

```go
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				taskAPIError,
				relayInfo,
				decision)
```

`relay/responses_websocket.go` 第 321 行，改为：

```go
				service.ProcessChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, info.ApiKey, channel.GetAutoBan()), apiErr, info, decision)
```

- [ ] **Step 5: 更新渠道测试探针调用点**

`controller/channel-test.go` 第 953 行。该处没有重试判定（`ProcessChannelError` 只被用来触发自动禁用），
构造一个显式的不重试 decision：

```go
		processChannelError(result.context, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(result.context, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError, nil, service.PolicyDecision{Action: "stop", Reason: "channel_test_probe", Source: "system"})
```

注意这里 `relayInfo` 传的是 `nil`，因此原始请求快照不会写入 —— 这是刻意的，
探针的 body 是程序合成的，不是用户请求。

- [ ] **Step 6: 跑测试确认通过**

```bash
go build ./... && go test ./service/ ./controller/ -v -run "TestProcessChannelError|TestCollectRawRequestInfo"
```

Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
gofmt -w service/relay_error.go service/relay_error_test.go controller/relay.go controller/channel-test.go relay/responses_websocket.go
git add service/relay_error.go service/relay_error_test.go controller/relay.go controller/channel-test.go relay/responses_websocket.go
git commit -m "feat(relay): 错误日志在最终失败时附带原始请求快照

ProcessChannelError 增加 decision 参数：重试判定为 retry 时不采集，
避免一次请求重试多个渠道在每条日志上重复同一份 body。
渠道测试探针不传 relayInfo，因此不会写入。"
```

---

## Task 4: 前端开关

**Files:**
- Modify: `web/src/features/system-settings/types.ts:377`
- Modify: `web/src/features/system-settings/operations/index.tsx:46`
- Modify: `web/src/features/system-settings/operations/section-registry.tsx`（logs 节）
- Modify: `web/src/features/system-settings/maintenance/log-settings-section.tsx`

- [ ] **Step 1: 加类型**

`web/src/features/system-settings/types.ts`，`OperationsSettings` 里 `LogConsumeEnabled: boolean`
那一行下面加：

```ts
  ErrorLogRawRequestEnabled: boolean
```

- [ ] **Step 2: 加默认值**

`web/src/features/system-settings/operations/index.tsx`，`defaultOperationsSettings` 里
`LogConsumeEnabled: false,` 下面加：

```ts
  ErrorLogRawRequestEnabled: true,
```

- [ ] **Step 3: 给 logs 节传默认值**

`web/src/features/system-settings/operations/section-registry.tsx`，`id: 'logs'` 那一节改为：

```tsx
  {
    id: 'logs',
    titleKey: 'Log Maintenance',
    build: (settings: OperationsSettings) => (
      <LogSettingsSection
        defaultEnabled={Boolean(settings.LogConsumeEnabled)}
        rawRequestDefaultEnabled={
          settings.ErrorLogRawRequestEnabled ?? true
        }
      />
    ),
  },
```

- [ ] **Step 4: 加 UI 开关**

`web/src/features/system-settings/maintenance/log-settings-section.tsx`：

1) schema 加字段：

```ts
const logSettingsSchema = z.object({
  LogConsumeEnabled: z.boolean(),
  ErrorLogRawRequestEnabled: z.boolean(),
})
```

2) props 类型加参数：

```ts
type LogSettingsSectionProps = {
  defaultEnabled: boolean
  rawRequestDefaultEnabled: boolean
}
```

3) 组件签名与表单默认值改为：

```tsx
export function LogSettingsSection({
  defaultEnabled,
  rawRequestDefaultEnabled,
}: LogSettingsSectionProps) {
```

```tsx
  const form = useForm<LogSettingsFormValues>({
    resolver: zodResolver(logSettingsSchema),
    defaultValues: {
      LogConsumeEnabled: defaultEnabled,
      ErrorLogRawRequestEnabled: rawRequestDefaultEnabled,
    },
  })
```

4) 同步外部值的 `useEffect`：

```tsx
  useEffect(() => {
    form.reset({
      LogConsumeEnabled: defaultEnabled,
      ErrorLogRawRequestEnabled: rawRequestDefaultEnabled,
    })
  }, [defaultEnabled, rawRequestDefaultEnabled, form])
```

5) `onSubmit` 改为逐字段比较后逐个更新（沿用 `SystemBehaviorSection` 的写法）：

```tsx
  const onSubmit = async (values: LogSettingsFormValues) => {
    const defaults: LogSettingsFormValues = {
      LogConsumeEnabled: defaultEnabled,
      ErrorLogRawRequestEnabled: rawRequestDefaultEnabled,
    }
    const updates = Object.entries(values).filter(
      ([key, value]) => value !== defaults[key as keyof LogSettingsFormValues]
    )
    for (const [key, value] of updates) {
      await updateOption.mutateAsync({ key, value })
    }
  }
```

6) 在「Record quota usage」那个 `FormField` 之后、`SettingsControlGroup` 之前插入新开关：

```tsx
          <FormField
            control={form.control}
            name='ErrorLogRawRequestEnabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>
                    {t('Record raw request info in error logs')}
                  </FormLabel>
                  <FormDescription>
                    {t(
                      'Store the client request body (first 1000 characters) and HTTP headers on error logs so upstream rejections can be investigated. Visible to administrators only.'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                  />
                </FormControl>
                <FormMessage />
              </SettingsSwitchItem>
            )}
          />
```

- [ ] **Step 5: 类型检查与 lint**

```bash
cd web && bun run typecheck && bun run lint
```

Expected: 无 type error，无 lint error。

- [ ] **Step 6: 提交**

```bash
git add web/src/features/system-settings/types.ts \
  web/src/features/system-settings/operations/index.tsx \
  web/src/features/system-settings/operations/section-registry.tsx \
  web/src/features/system-settings/maintenance/log-settings-section.tsx
git commit -m "feat(web): 日志维护新增「记录错误日志原始信息」开关，默认开启"
```

---

## Task 5: 前端展示

**Files:**
- Modify: `web/src/features/usage-logs/types.ts`（`LogOtherData['admin_info']`）
- Modify: `web/src/features/usage-logs/components/dialogs/details-dialog.tsx:792` 前后
- Test: `web/src/features/usage-logs/components/__tests__/raw-request.test.tsx`

- [ ] **Step 1: 写失败的测试**

新建 `web/src/features/usage-logs/components/__tests__/raw-request.test.tsx`：

```tsx
/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import i18next from 'i18next'
import { beforeAll, describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { LogOtherData } from '../../types'
import { DetailsDialog } from '../dialogs/details-dialog'

const i18nKeys = {
  'Log Details': 'Log Details',
  Error: 'Error',
  'Raw Request': 'Raw Request',
  Method: 'Method',
  'Request Headers': 'Request Headers',
  'Request Body': 'Request Body',
  'Body truncated, original size: {{bytes}} bytes':
    'Body truncated, original size: {{bytes}} bytes',
}

beforeAll(() => {
  i18next.addResourceBundle('en', 'translation', i18nKeys)
})

function makeLog(other: LogOtherData): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 5,
    content: 'status_code=400, bad request',
    username: 'user',
    token_name: 'token',
    model_name: 'glm-5.3',
    quota: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 150,
    channel_name: '',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify(other),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function renderDetails(other: LogOtherData, isAdmin = true) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const freshAt = Date.now() + 60_000
  queryClient.setQueryData(['status'], {}, { updatedAt: freshAt })
  queryClient.setQueryData(
    ['pricing'],
    { data: [], vendors: [] },
    { updatedAt: freshAt }
  )
  render(
    <QueryClientProvider client={queryClient}>
      <DetailsDialog
        log={makeLog(other)}
        isAdmin={isAdmin}
        isRoot={false}
        open
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
  return queryClient
}

const rawRequest = {
  method: 'POST',
  url: '/v1/chat/completions',
  body: '{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}',
  body_bytes: 51,
  body_content_type: 'application/json',
  headers: {
    'User-Agent': 'OpenAI/Python 1.52.0',
    Authorization: '***',
  },
  attempts: 2,
}

describe('raw request section', () => {
  test('renders the captured request for administrators', () => {
    const queryClient = renderDetails({ admin_info: { raw_request: rawRequest } })
    expect(screen.getByText('Raw Request')).toBeInTheDocument()
    // method 与 url 合并在同一行展示
    expect(
      screen.getByText('POST /v1/chat/completions')
    ).toBeInTheDocument()
    // 请求头逐行渲染为 "Name: value"，用正则做子串匹配
    expect(screen.getByText(/OpenAI\/Python 1\.52\.0/)).toBeInTheDocument()
    expect(screen.getByText(/^Authorization: \*\*\*$/)).toBeInTheDocument()
    // body 在 <pre> 里渲染
    expect(screen.getByText(rawRequest.body)).toBeInTheDocument()
    queryClient.clear()
  })

  test('flags a truncated body with its original size', () => {
    const queryClient = renderDetails({
      admin_info: {
        raw_request: { ...rawRequest, body_truncated: true, body_bytes: 4213 },
      },
    })
    expect(
      screen.getByText('Body truncated, original size: 4213 bytes')
    ).toBeInTheDocument()
    queryClient.clear()
  })

  test('hides the section from non-administrators', () => {
    const queryClient = renderDetails({ admin_info: { raw_request: rawRequest } }, false)
    expect(screen.queryByText('Raw Request')).toBeNull()
    queryClient.clear()
  })

  test('renders nothing when the log has no captured request', () => {
    const queryClient = renderDetails({ admin_info: { use_channel: [150] } })
    expect(screen.queryByText('Raw Request')).toBeNull()
    queryClient.clear()
  })
})
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd web && bun run test -- src/features/usage-logs/components/__tests__/raw-request.test.tsx
```

Expected: 失败 —— 找不到 `Raw Request` 文案。

- [ ] **Step 3: 加类型**

`web/src/features/usage-logs/types.ts`，在 `LogOtherData` 的 `admin_info` 内部（`request_policy?` 附近）加：

```ts
    raw_request?: {
      method?: string
      url?: string
      headers?: Record<string, string>
      headers_truncated?: boolean
      body?: string
      body_truncated?: boolean
      body_bytes?: number
      body_content_type?: string
      attempts?: number
    }
```

- [ ] **Step 4: 加展示区块**

`web/src/features/usage-logs/components/dialogs/details-dialog.tsx`：

1) 顶部 import 区加：

```tsx
import { CopyButton } from '@/components/copy-button'
```

2) 在 `lucide-react` 的 import 列表里追加 `FileJson`（该列表当前是
`Copy, Check, Route, Settings2, AlertTriangle, Headphones, Monitor, Cloud, Globe, ShieldCheck, UserCog, Info, LogIn`，
是每行一个的无排序写法，直接加一行即可）。

3) 在「Request policy decisions」区块（约 792 行）**之前**插入：

```tsx
        {/* Raw request snapshot (admin only, error logs) */}
        {props.isAdmin && adminInfo?.raw_request ? (
          <DetailSection
            label={t('Raw Request')}
            icon={<FileJson className='size-4' />}
          >
            <DetailRow
              label={t('Method')}
              value={`${adminInfo.raw_request.method ?? ''} ${
                adminInfo.raw_request.url ?? ''
              }`.trim()}
              mono
            />
            {adminInfo.raw_request.headers &&
            Object.keys(adminInfo.raw_request.headers).length > 0 ? (
              <div className='min-w-0 space-y-1'>
                <div className='flex items-center justify-between gap-2'>
                  <span className='text-muted-foreground text-xs'>
                    {t('Request Headers')}
                  </span>
                  <CopyButton
                    size='sm'
                    value={Object.entries(adminInfo.raw_request.headers)
                      .map(([key, value]) => `${key}: ${value}`)
                      .join('\n')}
                    tooltip={t('Copy to clipboard')}
                  />
                </div>
                {Object.entries(adminInfo.raw_request.headers).map(
                  ([key, value]) => (
                    <div
                      key={key}
                      className='text-xs wrap-break-word font-mono break-all'
                    >
                      {key}: {value}
                    </div>
                  )
                )}
              </div>
            ) : null}
            {adminInfo.raw_request.body ? (
              <div className='min-w-0 space-y-1'>
                <div className='flex items-center justify-between gap-2'>
                  <span className='text-muted-foreground text-xs'>
                    {t('Request Body')}
                  </span>
                  <CopyButton
                    size='sm'
                    value={adminInfo.raw_request.body}
                    tooltip={t('Copy to clipboard')}
                  />
                </div>
                {adminInfo.raw_request.body_truncated ? (
                  <p className='text-muted-foreground text-xs'>
                    {t('Body truncated, original size: {{bytes}} bytes', {
                      bytes: adminInfo.raw_request.body_bytes ?? 0,
                    })}
                  </p>
                ) : null}
                <pre className='bg-muted/50 max-h-64 overflow-auto rounded-md border p-2 text-xs wrap-break-word whitespace-pre-wrap'>
                  {adminInfo.raw_request.body}
                </pre>
              </div>
            ) : null}
          </DetailSection>
        ) : null}
```

3) 确认 `lucide-react` 的 import 列表里已经加上 `FileJson`。

- [ ] **Step 5: 跑测试确认通过**

```bash
cd web && bun run test -- src/features/usage-logs/components/__tests__/raw-request.test.tsx
```

Expected: 4 个用例全部 PASS。

- [ ] **Step 6: 类型检查与 lint**

```bash
cd web && bun run typecheck && bun run lint
```

Expected: 无 type error，无 lint error。

- [ ] **Step 7: 提交**

```bash
git add web/src/features/usage-logs/types.ts \
  web/src/features/usage-logs/components/dialogs/details-dialog.tsx \
  web/src/features/usage-logs/components/__tests__/raw-request.test.tsx
git commit -m "feat(web): 日志详情展示错误请求的原始请求快照

仅管理员可见；body 截断时标注原始字节数，请求头与 body 各带复制按钮。"
```

---

## Task 6: i18n 与全量回归

**Files:**
- Modify: `web/src/i18n/locales/{en,zh,zh-TW,fr,ru,ja,vi}.json`

- [ ] **Step 1: 补 6 个 key（不是 5 个）**

> ⚠️ **本步骤标题原先写「5 个 key」，是错的。实际是 6 个**：展示区块 4 个
> （`Raw Request`、`Request Headers`、`Request Body`、
> `Body truncated, original size: {{bytes}} bytes`）加开关 2 个
> （`Record raw request info in error logs` 及其说明长句）。
> 下面脚本里的 `VALUES` 是权威清单。

> ⚠️ **不要用按键排序的脚本写这几个文件。** 这些 locale 文件的键顺序是
> **ICU 排序**（`Intl.Collator('en')`），不是码点排序——直观表现是
> `"360", "1000", "10000", "_copy", "，", ", and"` 这样的内容顺序。
> 用 Python `sorted()` 会把整个 `translation` 重排，实测每个文件产生约
> 2840 行改动（全文件 1.5 倍），完全不可 review。
>
> 正确做法：**只把新键插到它在 ICU 顺序里的位置，不移动任何已有键**，
> 写回时用 `JSON.stringify(doc, null, 2) + '\n'`（该写法与文件现有格式
> 逐字节一致），并注意保持 sync 脚本保护的那个混淆键
> `footer.newapi.projectAttributionSuffix` 不被反转义。
> 这样每个文件只有 6 行新增、0 行删除。

插入后的校验（`missing` 必须全为 `none`，且不得出现重复键）：

```bash
python3 - <<'PY'
import json
keys = ['Body truncated, original size: {{bytes}} bytes', 'Raw Request',
        'Record raw request info in error logs', 'Request Body', 'Request Headers',
        'Store the client request body (first 1000 characters) and HTTP headers on error logs so upstream rejections can be investigated. Visible to administrators only.']
for locale in ['en', 'zh', 'zh-TW', 'fr', 'ru', 'ja', 'vi']:
    t = json.load(open(f'web/src/i18n/locales/{locale}.json'))['translation']
    missing = [k for k in keys if k not in t]
    dupes = [k for k in keys if list(t).count(k) > 1]
    print(locale, 'missing:', missing or 'none', ' dupes:', dupes or 'none')
PY
```

`Method`、`Copy to clipboard`、`Error`、`Request policy decisions`、`Path` 这些
**已经存在**，不要重复添加。

下面是各语言取值（供插入时取用；`en` 值即 key 本身）：

```python
VALUES = {
    'Body truncated, original size: {{bytes}} bytes': {
        'en': 'Body truncated, original size: {{bytes}} bytes',
        'zh': '请求体已截断，原始大小 {{bytes}} 字节',
        'zh-TW': '請求體已截斷，原始大小 {{bytes}} 位元組',
        'fr': "Corps tronqué, taille d'origine : {{bytes}} octets",
        'ru': 'Тело обрезано, исходный размер: {{bytes}} байт',
        'ja': 'ボディは切り詰められました。元のサイズ: {{bytes}} バイト',
        'vi': 'Nội dung đã bị cắt, kích thước gốc: {{bytes}} byte',
    },
    'Raw Request': {
        'en': 'Raw Request', 'zh': '原始请求', 'zh-TW': '原始請求',
        'fr': 'Requête brute', 'ru': 'Исходный запрос',
        'ja': '生のリクエスト', 'vi': 'Yêu cầu gốc',
    },
    'Record raw request info in error logs': {
        'en': 'Record raw request info in error logs',
        'zh': '记录错误日志原始信息', 'zh-TW': '記錄錯誤日誌原始資訊',
        'fr': "Enregistrer la requête brute dans les journaux d'erreur",
        'ru': 'Записывать исходный запрос в журналы ошибок',
        'ja': 'エラーログに生のリクエスト情報を記録',
        'vi': 'Ghi thông tin yêu cầu gốc vào nhật ký lỗi',
    },
    'Request Body': {
        'en': 'Request Body', 'zh': '请求体', 'zh-TW': '請求體',
        'fr': 'Corps de la requête', 'ru': 'Тело запроса',
        'ja': 'リクエストボディ', 'vi': 'Nội dung yêu cầu',
    },
    'Request Headers': {
        'en': 'Request Headers', 'zh': '请求头', 'zh-TW': '請求標頭',
        'fr': 'En-têtes de la requête', 'ru': 'Заголовки запроса',
        'ja': 'リクエストヘッダー', 'vi': 'Tiêu đề yêu cầu',
    },
    'Store the client request body (first 1000 characters) and HTTP headers on error logs so upstream rejections can be investigated. Visible to administrators only.': {
        'en': 'Store the client request body (first 1000 characters) and HTTP headers on error logs so upstream rejections can be investigated. Visible to administrators only.',
        'zh': '在上游报错时，把客户端原始请求体（前 1000 字符）与 HTTP 请求头一并写入错误日志，便于排查上游拒绝原因。仅管理员可见。',
        'zh-TW': '在上游報錯時，把客戶端原始請求體（前 1000 字元）與 HTTP 標頭一併寫入錯誤日誌，便於排查上游拒絕原因。僅管理員可見。',
        'fr': "Enregistre le corps de la requête client (1 000 premiers caractères) et les en-têtes HTTP dans les journaux d'erreur afin de pouvoir analyser les rejets en amont. Visible uniquement par les administrateurs.",
        'ru': 'Записывает тело запроса клиента (первые 1000 символов) и HTTP-заголовки в журналы ошибок для разбора отказов вышестоящего сервиса. Видно только администраторам.',
        'ja': '上流の拒否を調査できるよう、クライアントのリクエストボディ（先頭 1000 文字）と HTTP ヘッダーをエラーログに記録します。管理者のみ閲覧できます。',
        'vi': 'Ghi nội dung yêu cầu của client (1000 ký tự đầu) và tiêu đề HTTP vào nhật ký lỗi để điều tra các lần bị từ chối từ upstream. Chỉ quản trị viên xem được.',
    },
}

# ⚠️ 不要直接照抄下面这段——它含 sorted() 重排，会把整个文件搅乱。
# 正确做法见本节开头的说明：按 ICU 顺序逐键插入，不移动已有键。
# for locale in [...]:
#     doc = json.load(...)
#     doc['translation'][key] = VALUES[key][locale]   # 只赋值，不重排
#     write(JSON.stringify(doc, null, 2) + '\n')
```

Expected: 每个 locale 文件只有 6 行新增、0 行删除。

- [ ] **Step 2: 跑 i18n 校验**

```bash
cd web && bun run i18n:sync
python3 -c "
import json
r = json.load(open('src/i18n/locales/_reports/_sync-report.json'))
print('base:', r['base'])
for loc, info in r['locales'].items():
    print(loc, 'missing=', info['missingCount'], 'untranslated=', info['untranslatedCount'], 'extras=', info['extrasCount'])
"
```

Expected: 6 个新 key 不出现在任何 locale 的 `missingCount` 里。
（`zh`/`ja`/`ru` 各有 3–4 条**既存**的 untranslated，与本次无关，不要去动。）

> ⚠️ 该脚本在这些文件上目前是幂等的（内容已是它认定的规范顺序），
> 所以正常情况下它不会改动文件。如果它突然重排了全部 7 个文件、
> 产生几百行 diff，**停下来**——那说明文件顺序与脚本预期已不一致，
> 别把它当成正常输出提交。

- [ ] **Step 3: 跑全量测试**

```bash
cd web && bun run test
cd /Users/david/Documents/juao_ops/juao-api && go test ./...
```

Expected: 全部通过。若有与本次无关的既存失败，记录下来并在提交信息里说明。

- [ ] **Step 4: 提交**

```bash
cd /Users/david/Documents/juao_ops/juao-api
git add web/src/i18n/locales/
git commit -m "chore(i18n): 补齐错误日志原始请求的 6 个文案 × 7 个语言"
```

提交前先 `git status --short` 确认：`i18n:sync` 会在 `web/src/i18n/locales/_reports/`
（已被 gitignore）与 `_extras/`（**未被 gitignore**）下写临时文件。
`_extras/` 目前是空的所以不显形，但若它出现未跟踪文件，**不要**加进提交，
改为逐个显式 `git add` 那 7 个 locale 的 `.json`。

---

## 验收清单

跑完所有任务后，逐条确认：

- [ ] `go build ./...` 与 `cd relaykit && GOWORK=off go build ./...` 都通过（`AGENTS.md` 要求）
- [ ] `go test ./...` 全绿
- [ ] `cd web && bun run typecheck && bun run lint && bun run test` 全绿
- [ ] `service/log_raw_request_test.go` 覆盖：短 body 原样保留、长 body 按 rune 截到 1000、
      中文按字符计数、非 JSON 只出标记、Content-Type 缺失只出标记、`+json` 被识别、
      凭据头打码、超长头值截断、query 凭据掩码、attempts 计数、无 storage 时降级
- [ ] `service/relay_error_test.go` 覆盖：最终失败写入、retry 中不写、开关关闭不写、
      探针（relayInfo 为 nil）不写；且断言日志里**不含**客户端的 `Bearer` 明文
- [ ] 前端测试覆盖：管理员可见、截断提示、非管理员不渲染、无数据不渲染
- [ ] 系统设置 → 运维 → 日志维护 页面能看到新开关，且默认是打开状态

## 上线注意事项（不在本次改代码范围，交付时告知用户）

- 本改动只到本地 commit，**不自动部署**。生产三台机是 `aliyun ecs RunCommand` 操作，
  按 `CLAUDE.md` 第六节的滚动升级步骤走，且升级前必须先给 RDS 做快照。
- 线上跑的是未修改的上游 `v1.0.0-rc.40`，本分支（`juao_dev_base40`）的改动**都不在生产**。
- 开关默认开启，部署后新产生的错误日志会立刻带上 `raw_request`。若要回退，
  把开关关掉即可，无需回滚二进制。
