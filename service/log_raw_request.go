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

// credentialHeaderMarkers 是头名里出现即视为携带凭据的子串（小写）。
var credentialHeaderMarkers = [...]string{"token", "secret", "signature", "api-key"}

// isCredentialHeader 判定请求头是否携带凭据。
// 头名统一小写后匹配固定名单与前缀/后缀模式。
func isCredentialHeader(lowerName string) bool {
	if _, ok := credentialHeaderNames[lowerName]; ok {
		return true
	}
	if strings.HasSuffix(lowerName, "-key") || strings.HasSuffix(lowerName, "apikey") {
		return true
	}
	for _, marker := range credentialHeaderMarkers {
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

// boundHeaderValue 把请求头值截到 rawRequestHeaderValueRuneLimit 个字符，
// 被截断时追加省略号。所有写进日志的头值都走这里，避免异常客户端
// 用超长头把日志行撑爆。
func boundHeaderValue(value string) (string, bool) {
	shortened, truncated := truncateRunes(value, rawRequestHeaderValueRuneLimit)
	if truncated {
		shortened += "…"
	}
	return shortened, truncated
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
		shortened, wasTruncated := boundHeaderValue(value)
		if wasTruncated {
			truncated = true
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
// 整份内容读进内存。
//
// 注意本函数的读取并不完全无副作用：NewReader() 拿到的确实是独立游标、
// 不扰动正在被上游消费的那份，但 common.GetBodyStorage(c) 会经由
// GetRequestBody 在每个已有 storage 上做一次 Seek(0)；若上下文里还没有
// storage，它还会消费并关闭 c.Request.Body 并装入新的 storage。
// 因此调用方必须在上游 body 已经读完之后才调用本函数，不要在重试循环的
// 迭代中途调用。
func collectRawRequestBody(c *gin.Context) (body string, truncated bool, totalBytes int64, contentType string) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return "", false, 0, ""
	}
	totalBytes = storage.Size()
	// 与实际写入日志的其他头值一样限长：Content-Type 会进入 body_content_type
	// 与非 JSON 标记，不截断的话一个 1MB 的超长头就能把单条日志撑到约 2MB。
	// 只限长，不去掉 ";" 后的参数（charset/boundary 对排查有用）。
	contentType, _ = boundHeaderValue(c.Request.Header.Get("Content-Type"))
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
	// 兜底：读到上限时 truncateRunes 必然已报截断（4001 字节至少解出 1001 个 rune）。
	// 这一步只在 rawRequestBodyReadBytes 被调小到与 rune 上限不再匹配时才生效。
	if !truncated {
		truncated = totalBytes > int64(len(raw))
	}
	return body, truncated, totalBytes, contentType
}

// CollectRawRequestInfo 采集错误日志要记录的原始请求快照。
// 调用方负责判定「当前错误是否值得记录」，本函数只做采集，不读开关。
//
// 采集会经由 common.GetBodyStorage(c) 触碰请求体的共享游标（见
// collectRawRequestBody 的说明），所以必须在上游 body 已经发送完之后调用。
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
