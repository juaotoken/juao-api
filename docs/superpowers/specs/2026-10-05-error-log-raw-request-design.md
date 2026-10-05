# 错误日志记录原始请求信息 Design

> 分支：`juao_dev_base40`（基线 = 上游 `v1.0.0-rc.40`）
> 日期：2026-10-05

## 背景

线上排查上游报错时只能看到一句被 `MaskSensitiveError` 处理过的错误文本。以 2026-10-05 的
`202610050110115227392778268d9d6HgotlwGi` 为例（用户 `jiangsudianxin`，渠道 #150，
模型 `glm-5.3`），日志库里只有：

```
content: status_code=400, The request is invalid: the request was rejected by an
         internal MaaS component. Please check the request body, required fields,
         and request format.
other:   {"admin_info":{"request_policy":[...],"use_channel":["150"]},
          "error_code":"400001","error_type":"openai_error",
          "request_path":"/v1/chat/completions","status_code":400}
```

上游明确让人「check the request body」，但请求体在两处被丢弃：

1. **入站原始 body**：`common.BodyStorage` 由 `middleware.BodyStorageCleanup` 在请求结束时
   `Close()`，`diskStorage.Close()` 会 `os.Remove` 临时文件（`common/body_storage.go:214`）。
   生产未启磁盘缓存（`DiskCacheEnabled: false`），所以是纯内存持有，请求结束即随 GC 消失。
2. **HTTP 原始请求**：客户端请求头从头到尾没有任何地方落盘。`relayInfo.RequestHeaders`
   （`relay/common/relay_info.go:110`）虽然抓了全量头，但只喂给 param_override 上下文和
   计费表达式，不写日志。上游 URL 只在 `DEBUG=true` 时打，且经 `SanitizeURLForLog` 掩码。

上游 400 的原始响应体同样不可得：`service/error.go` 的 `RelayErrorHandler` 在 JSON 解析成功
且有 message 时直接丢弃整个 body，只把 message 塞进 `content`。

## 目标

上游返回错误时（不限于 400），把这两样东西落到错误日志里供管理员排查：

- **客户端发来的原始请求内容**（未经转换/模型映射的入站 body）
- **HTTP 原始请求**（method、URL、全部请求头，含 User-Agent）

范围与约束：

| 项 | 决定 |
|---|---|
| 记录哪一份请求 | 入站原始请求（客户端发来的），不是转换后发往上游的那份 |
| body 上限 | 前 1000 个字符；不足 1000 则全记 |
| 非 JSON body | 只记一行标记，不落二进制内容 |
| 请求头 | 全量记录，凭据类值替换为 `***` |
| 触发条件 | 仅上游/渠道错误（现有 `ProcessChannelError` 路径）；正常请求不记 |
| 客户端主动断开 | 不做任何额外处理 —— 该场景当前本来就不产生错误日志行（见下） |
| 多渠道重试 | 只在最终失败那条上记，并标注本次请求试过的渠道数 |
| 可见性 | 管理员及以上（挂 `other.admin_info`） |
| 开关 | 系统设置 → 运维 → 日志维护，新增「记录错误日志原始信息」，**默认开启** |

**客户端断开为什么不需要写代码**：非流式请求的上游调用用的是 `http.NewRequest`
（`relay/channel/api_request.go:319`），不带客户端 context，客户端断开不影响上游调用，
因此不会产生错误。流式请求被断开时，`stream_scanner.go:307` 把结束原因标成
`client_gone` 后 `OaiStreamHandler` 仍返回 `nil` error，写的是消费日志而非错误日志。
两条路径都不会走到 `ProcessChannelError`，所以「断开的请求不记录」自动成立。

## 非目标

- 不记录发往上游的那份（转换后）body 与上游请求头
- 不记录上游返回的原始响应体
- 不新增数据库表、不改 `logs` 表结构
- 不做按渠道/按用户的采样或限流
- 不改动现有错误日志行的写入条件（不因客户端断开而少写行）

## 存储设计

挂在 `logs.other` 的 `admin_info` 作用域下，不改表、不加迁移：

```json
"admin_info": {
  "use_channel": ["150"],
  "request_policy": [ ... ],
  "raw_request": {
    "method": "POST",
    "url": "/v1/chat/completions",
    "body": "{\"model\":\"glm-5.3\",\"messages\":[...",
    "body_truncated": true,
    "body_bytes": 4213,
    "body_content_type": "application/json",
    "headers_truncated": false,
    "headers": {
      "User-Agent": "OpenAI/Python 1.52.0",
      "Content-Type": "application/json",
      "Authorization": "***"
    },
    "attempts": 3
  }
}
```

字段语义：

| 字段 | 说明 |
|---|---|
| `method` | 客户端 HTTP 方法 |
| `url` | `c.Request.URL.RequestURI()`，经 `SanitizeURLForLog` 掩码 query 里的凭据键 |
| `body` | 前 1000 个 rune；非 JSON 时为标记文本 |
| `body_truncated` | 是否发生截断，仅在为真时输出 |
| `body_bytes` | body 的原始字节长度（取自 `storage.Size()`），恒输出 |
| `body_content_type` | 客户端 Content-Type，空时输出空串（空本身是线索） |
| `headers` | 头名 → 值；凭据类值为 `***`；单值超 512 字符截断 |
| `headers_truncated` | 是否有头值被截断，仅在为真时输出 |
| `attempts` | 本次请求试过的渠道数 = `len(ctx.GetStringSlice("use_channel"))` |

`logs.other` 是 `longtext`，无需 DDL。三档可见性由 `model/log_other.go` 的
`formatLogOtherJSON` 在读取时执行：普通用户查自己的日志时整个 `admin_info` 被删除
（`logOtherVisibilityUser` 分支），管理员与 root 保留。**不需要改权限代码。**

node1 的 sys_monitor 已在整份同步 `other`（`internal/store/ratemon.go:508` 的 INSERT 列表
含 `other`），因此错误下钻页「展开单条完整信息（other 原始 JSON）」会自动带上这份数据，
无需改动 sys_monitor。

### 已知取舍

- **body 不做掩码**。用户 prompt 会以明文进 `newapi_log`。这是刻意的：对
  `MaskSensitiveInfo` 掩掉 URL/IP 会让「你的请求体有问题」这类上游报错失去排查价值。
  风险由 admin-only 可见性兜住。
- **`attempts` 与同层 `use_channel` 数据重复**。保留是为了排查时不必知道该去数哪个字段。

## 开关

沿用扁平 option 那套（与 `LogConsumeEnabled` 同构），不引入分层配置。

| 位置 | 改动 |
|---|---|
| `common/constants.go` | 新增 `var ErrorLogRawRequestEnabled = true` |
| `model/option.go` `InitOptionMap` | `common.OptionMap["ErrorLogRawRequestEnabled"] = strconv.FormatBool(...)` |
| `model/option.go` `updateOptionMap` | switch 加 `case "ErrorLogRawRequestEnabled"` 分支 |

`updateOptionMap` 的 `strings.HasSuffix(key, "Enabled")` 外层判定已覆盖该 key 的布尔解析，
只需在 switch 里落到具体变量。`controller/option.go` 无需新增校验分支——该 key 走默认路径。

`GetOptions` 的敏感 key 过滤（`HasSuffix("Token"/"Secret"/"Key")`）不会误伤。

前端四处：

| 文件 | 改动 |
|---|---|
| `web/src/features/system-settings/types.ts` | `OperationsSettings` 加 `ErrorLogRawRequestEnabled: boolean` |
| `.../operations/index.tsx` | `defaultOperationsSettings` 加 `ErrorLogRawRequestEnabled: true` |
| `.../operations/section-registry.tsx` | `logs` 节多传一个 prop |
| `.../maintenance/log-settings-section.tsx` | schema 加字段 + 新增 `SettingsSwitchItem` |

DB 里没有该 key 时，前端 `getOptionValue` 用 defaultSettings 的 `true` 兜底，后端变量初值也是
`true` —— 两侧默认一致，均为开。

## 采集实现

新增 `service/log_raw_request.go`，导出纯函数便于单测：

```go
type RawRequestInfo struct {
    Method          string            `json:"method"`
    URL             string            `json:"url"`
    Headers         map[string]string `json:"headers,omitempty"`
    HeadersTruncated bool             `json:"headers_truncated,omitempty"`
    Body            string            `json:"body,omitempty"`
    BodyTruncated   bool              `json:"body_truncated,omitempty"`
    BodyBytes       int64             `json:"body_bytes"`
    BodyContentType string            `json:"body_content_type,omitempty"`
    Attempts        int               `json:"attempts,omitempty"`
}

func collectRawRequestInfo(c *gin.Context) *RawRequestInfo
```

### Body 读取

1. **不用 `storage.Bytes()`** —— 它内部 `io.ReadFull` 读整个 body，遇大请求会把整份读进内存。
   改用 `storage.NewReader()` 拿独立游标（`diskStorage.NewReader` 另开 fd、
   `memoryStorage.NewReader` 新建 `bytes.Reader`），不扰动正在被上游消费的那份。
2. `io.ReadAll(io.LimitReader(reader, 4001))`，最多读 4001 字节。
3. 转 `[]rune` 取前 1000 —— 中文 prompt 能看满 1000 个汉字，英文能看满 1000 个字符，
   且天然不切出半个 UTF-8 字符。
4. `body_bytes` 取自 `storage.Size()`，与上面的限量读无关，因此「发了 4KB 只看得到 1KB」
   是可判断的。
5. **非 JSON 只记标记**：Content-Type 不是 `application/json` 或 `*+json` 时，
   `body` 写 `[skip non-json body: multipart/form-data; boundary=...]`，`body_bytes` 照记。
   Content-Type 为空也走这条路。

读取失败（storage 已关闭、Reader 出错）时 body 留空、`body_bytes` 取 `storage.Size()`，
不阻断错误日志写入。

### Headers

遍历 `c.Request.Header`，头名按 `strings.ToLower` 判定凭据，命中则值替换为 `***`：

- 显式名单：`authorization`、`cookie`、`set-cookie`、`proxy-authorization`
- 模式名单：头名含 `token` / `secret` / `signature` / `apikey` / `api-key`，或以 `-key` 结尾
  （覆盖 `x-api-key`、`x-goog-api-key`、realtime 客户端塞进 `sec-websocket-protocol` 的 key）

头名本身保留。单值超 512 字符截断并加 `…`，防止异常客户端塞超长头。

### URL

`relaycommon.SanitizeURLForLog(c.Request.URL.RequestURI())`，复用已有的 query 敏感键掩码
（`relay/common/relay_utils.go:70`）。`service` 包已 import `relaycommon`，无循环依赖。

## 触发时机

`ProcessChannelError` 目前在重试循环里**每次都调**（`controller/relay.go:209`），
一条请求重试 3 次会写 3 条错误日志。三个真实调用点都在它前面一行刚算完 decision：

```go
decision := service.DecideRelayRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry())
service.RecordPolicyFailure(c, channel.Id, newAPIError, decision)
processChannelError(c, ...)                          // controller/relay.go:209
if decision.Action != "retry" { break }
```

因此给 `service.ProcessChannelError` 增加 `decision service.PolicyDecision` 参数，
`action != "retry"` 时才采集原始请求。选显式传参而非 context flag，是因为三个调用点都刚
算完 decision，显式传参可测且不引入隐式状态。

改动面：

| 文件 | 改动 |
|---|---|
| `service/relay_error.go` | 签名加 `decision`；在 `if constant.ErrorLogEnabled` 分支内采集 |
| `controller/relay.go:209` | 传 `decision` |
| `controller/relay.go:563` | 传 `decision`（已由 `decideTaskRetry` 算出） |
| `relay/responses_websocket.go:321` | 传 `decision` |
| `controller/channel-test.go:953` | 传零值 decision + 依赖 `relayInfo == nil` 守卫 |
| `service/relay_error_test.go`、`controller/relay_error_log_test.go` | 适配新签名 |

再加一道 `relayInfo != nil` 守卫，排除 `controller/channel-test.go:953` ——
那是管理员点「测试渠道」发的探针，body 是程序合成的，`relayInfo` 传的就是 `nil`。

### 与 `ERROR_LOG_ENABLED` 的关系

采集代码放在既有的 `if constant.ErrorLogEnabled && types.IsRecordErrorLog(err)` 分支内，
两个开关各管一段，互不影响：

| 开关 | 控制 | 生产现值 |
|---|---|---|
| `ERROR_LOG_ENABLED`（环境变量） | 是否写错误日志行 | `true` |
| `ErrorLogRawRequestEnabled`（系统设置） | 错误日志行里是否附 `raw_request` | 默认 `true` |

关掉 `ERROR_LOG_ENABLED` 时连日志行都不写，新开关自然无从生效 —— 这是既有语义，不额外处理。

### 时序确认

`middleware.BodyStorageCleanup` 是在 `router/relay-router.go:20` 用 `Use()` 注册的，它的
defer 在所有后续 handler 返回后才执行；而 `processChannelError` 是在 `Relay()` 内部、
重试循环里调用的。所以采集时 `BodyStorage` 必然还活着。

`api_request.go:587` 的 `_ = c.Request.Body.Close()` 关的是 `io.NopCloser` 包装
（`controller/relay.go:180` 设的），不会释放底层 storage，`storage.NewReader()` 仍然可用。

## 前端展示

`details-dialog.tsx` 新增 `{props.isAdmin && adminInfo?.raw_request && ...}` 的
`DetailSection`，标题 `Raw Request`，位置紧跟现有「Request policy decisions」区块（:793）。

- `Method` + `URL` 一行，等宽
- `Body`：`<pre>` 包 1000 字符，右上角复制按钮（复用该文件已有的 `copyToClipboard`）；
  `body_truncated` 为真时在标题旁挂「已截断，原始 {{bytes}} 字节」提示
- `Headers`：逐行 key/value，等宽

`LogOtherData['admin_info']`（`web/src/features/usage-logs/types.ts`）加
`raw_request?: {...}`。`admin_info` 的 admin-only 剥离已在后端完成，前端仍显式写
`props.isAdmin` 以与相邻区块一致。

i18n 新增 4 个 key × 7 个 locale（`en`/`zh`/`zh-TW`/`fr`/`ru`/`ja`/`vi`）：
`Raw Request`、`Request Body`、`Request Headers`、
`Body truncated, original size: {{bytes}} bytes`；
开关：`Record raw request info in error logs` /「记录错误日志原始信息」。

## 测试计划

| 文件 | 覆盖 |
|---|---|
| `service/log_raw_request_test.go`（新建） | 表驱动：JSON body 短于 / 等于 / 超过 1000 rune；中文 1001 字截到 1000 且无半个字符；multipart 只出标记；Content-Type 缺失只出标记；`body_bytes` 等于 `storage.Size()`；凭据头变 `***`；超长头值截断；URL query 的 `key=` 被掩码 |
| `service/relay_error_test.go`（改） | 开关关闭不写 `raw_request`；`decision.Action == "retry"` 不写；`relayInfo == nil` 不写；开启时字段完整；`ErrorLogRawRequestEnabled` 默认 `true` 且 `updateOptionMap` 能刷成 `false` |
| `web/.../__tests__/log-detail-raw-request.test.tsx`（新建） | 渲染、截断提示、非管理员不渲染 |

按 `AGENTS.md` 的「不为小改动散落测试」规则：后端只新建 1 个测试文件，其余用例并入现有文件；
不重复跨层建同一功能的测试文件。

## 上线影响

- **日志体积**：每条错误日志 `other` 增加至多约 1KB body + 请求头。按当前量级
  （近 5 天错误日志 63–2108 条/天）约增加 1–2MB/天，可忽略。
- **无 DDL、无迁移、无配置迁移**：老错误日志没有 `raw_request` 字段，前端按可选处理。
- **`ProcessChannelError` 签名变更**：仅影响包内与 controller，编译期即可发现遗漏。
