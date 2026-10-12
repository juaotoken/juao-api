package service

import (
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// rawRequestBodyParseMaxBytes 是按字段压缩时允许整份解析的 body 上限。
	// 更大的 body（多为内联的大图/音频）退回到只读前缀截断，避免为一条错误日志
	// 把几十 MB 的请求整份读进内存再遍历。
	rawRequestBodyParseMaxBytes = 32 << 20
	// rawRequestProtectedValueRuneLimit 是系统字段（model/role/type 等）值的保留上限：
	// 不超过它就原样保留、不参与压缩；异常长的系统字段值仍按普通内容压缩。
	rawRequestProtectedValueRuneLimit = 256
	// rawRequestMinPreviewRunes 是内容字段压缩后至少保留的字符数。字段结构本身已超出
	// 配置长度时，按这个下限保留每个值的开头，让管理员仍能认出内容。
	rawRequestMinPreviewRunes = 8
	// rawRequestMaxNestingDepth 是按字段压缩允许的最大嵌套层数。压缩是递归遍历，
	// 更深的 body（正常请求不会出现）退回到前缀截断，避免恶意嵌套撑爆栈。
	rawRequestMaxNestingDepth = 256
	// rawRequestMaxRenderPasses 是收紧上限后重新渲染的最多次数。
	rawRequestMaxRenderPasses = 8
	// rawRequestHeaderValueRuneLimit 是单个请求头值的字符数上限，
	// 防止异常客户端塞超长头把日志撑爆。
	rawRequestHeaderValueRuneLimit = 512
	// rawRequestMaskedValue 是凭据类请求头的替换值。
	rawRequestMaskedValue = "***"
)

// rawRequestSystemKeys 是请求体里由客户端 SDK / 协议决定、而不是用户输入的字段。
// 它们的值通常很短且对排查上游拒绝最有用（模型名、角色、内容块类型、工具 id 等），
// 压缩时原样保留；数组值（如 stop、modalities）的元素同样受保护。
var rawRequestSystemKeys = map[string]struct{}{
	"model": {}, "role": {}, "type": {}, "name": {}, "id": {}, "object": {},
	"tool_call_id": {}, "tool_use_id": {}, "call_id": {}, "media_type": {}, "mime_type": {},
	"format": {}, "detail": {}, "stop": {}, "stop_sequences": {}, "modalities": {},
	"tool_choice": {}, "reasoning_effort": {}, "effort": {}, "service_tier": {}, "voice": {},
	"encoding_format": {}, "quality": {}, "size": {}, "style": {}, "response_format": {},
	"cache_control": {}, "status": {}, "finish_reason": {}, "previous_response_id": {},
}

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

// rawRequestTruncationMarker 是被压缩的字符串值末尾追加的标记，注明省略了多少字符。
func rawRequestTruncationMarker(omitted int) string {
	return "…[+" + strconv.Itoa(omitted) + " chars]"
}

// requestBodyCompressor 把 JSON 请求体压缩到目标长度：保留全部字段与结构、
// 系统字段的值，以及数字/布尔/null；只截短内容类字符串值（用户输入、
// base64 数据、签名等）。所有内容字符串共用一个截断上限，上限取满足目标
// 长度的最大值——短的值因此原样保留，长的值各留开头一段。
type requestBodyCompressor struct {
	// skeletonRunes 是不可压缩部分（括号、键、标点、系统字段与标量值）的字符数。
	skeletonRunes int
	// contentRunes 是每个可压缩字符串值（解码后）的字符数。
	contentRunes []int
	// contentRawRunes 是每个可压缩字符串值原样输出（含引号与转义）时的字符数。
	contentRawRunes []int
}

func isProtectedRequestValue(value gjson.Result, protected bool) bool {
	return protected && utf8.RuneCountInString(value.Str) <= rawRequestProtectedValueRuneLimit
}

func (b *requestBodyCompressor) measure(value gjson.Result, protected bool) {
	switch {
	case value.IsObject():
		b.skeletonRunes += 2
		first := true
		value.ForEach(func(key, child gjson.Result) bool {
			if !first {
				b.skeletonRunes++
			}
			first = false
			b.skeletonRunes += utf8.RuneCountInString(key.Raw) + 1
			_, isSystemKey := rawRequestSystemKeys[key.Str]
			b.measure(child, isSystemKey)
			return true
		})
	case value.IsArray():
		b.skeletonRunes += 2
		first := true
		value.ForEach(func(_, child gjson.Result) bool {
			if !first {
				b.skeletonRunes++
			}
			first = false
			b.measure(child, protected)
			return true
		})
	case value.Type == gjson.String && !isProtectedRequestValue(value, protected):
		b.contentRunes = append(b.contentRunes, utf8.RuneCountInString(value.Str))
		b.contentRawRunes = append(b.contentRawRunes, utf8.RuneCountInString(value.Raw))
	default:
		b.skeletonRunes += utf8.RuneCountInString(value.Raw)
	}
}

// estimatedRunes 估算以 limit 为截断上限时的输出字符数。被截断的值按「前缀 + 标记 + 引号」
// 计，前缀里的转义字符不计入——误差由 compressRequestBody 的收紧循环兜住。
func (b *requestBodyCompressor) estimatedRunes(limit int) int {
	total := b.skeletonRunes
	for i, runes := range b.contentRunes {
		if runes <= limit {
			total += b.contentRawRunes[i]
			continue
		}
		total += limit + utf8.RuneCountInString(rawRequestTruncationMarker(runes-limit)) + 2
	}
	return total
}

func (b *requestBodyCompressor) render(out *strings.Builder, value gjson.Result, protected bool, limit int) {
	switch {
	case value.IsObject():
		out.WriteByte('{')
		first := true
		value.ForEach(func(key, child gjson.Result) bool {
			if !first {
				out.WriteByte(',')
			}
			first = false
			out.WriteString(key.Raw)
			out.WriteByte(':')
			_, isSystemKey := rawRequestSystemKeys[key.Str]
			b.render(out, child, isSystemKey, limit)
			return true
		})
		out.WriteByte('}')
	case value.IsArray():
		out.WriteByte('[')
		first := true
		value.ForEach(func(_, child gjson.Result) bool {
			if !first {
				out.WriteByte(',')
			}
			first = false
			b.render(out, child, protected, limit)
			return true
		})
		out.WriteByte(']')
	case value.Type == gjson.String && !isProtectedRequestValue(value, protected):
		runes := []rune(value.Str)
		if len(runes) <= limit {
			out.WriteString(value.Raw)
			return
		}
		encoded, err := common.Marshal(string(runes[:limit]) + rawRequestTruncationMarker(len(runes)-limit))
		if err != nil {
			out.WriteString(`""`)
			return
		}
		out.Write(encoded)
	default:
		out.WriteString(value.Raw)
	}
}

// compressRequestBody 把 JSON body 压缩到约 targetRunes 个字符。
// 字段结构本身就超过目标时，按结构的实际长度保存（每个内容值保留
// rawRequestMinPreviewRunes 个字符），不会为了凑长度丢字段。
// 返回值 compressed 表示是否有字符串值被截短。
func compressRequestBody(body string, targetRunes int) (result string, compressed bool) {
	root := gjson.Parse(body)
	compressor := &requestBodyCompressor{}
	compressor.measure(root, false)

	longest := 0
	for _, runes := range compressor.contentRunes {
		longest = max(longest, runes)
	}
	limit := longest
	if compressor.estimatedRunes(longest) > targetRunes {
		// estimatedRunes 随 limit 单调不减：二分出满足目标的最大上限。
		low, high := rawRequestMinPreviewRunes, longest
		for low < high {
			mid := (low + high + 1) / 2
			if compressor.estimatedRunes(mid) <= targetRunes {
				low = mid
			} else {
				high = mid - 1
			}
		}
		limit = low
	}

	for pass := 1; ; pass++ {
		var out strings.Builder
		compressor.render(&out, root, false, limit)
		result = out.String()
		actual := utf8.RuneCountInString(result)
		if actual <= targetRunes || limit <= rawRequestMinPreviewRunes || pass >= rawRequestMaxRenderPasses {
			return result, limit < longest
		}
		// 转义字符让实际长度略超估算：按内容部分的超出比例收紧上限再渲染一次。
		contentBudget := max(1, targetRunes-compressor.skeletonRunes)
		contentActual := max(1, actual-compressor.skeletonRunes)
		limit = max(rawRequestMinPreviewRunes, min(limit-1, limit*contentBudget/contentActual))
	}
}

// jsonNestingDepth 返回 JSON 文本的最大括号嵌套层数（忽略字符串内的括号）。
// 迭代扫描，不受嵌套深度影响。
func jsonNestingDepth(data []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, ch := range data {
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
		case ch == '"':
			inString = true
		case ch == '{' || ch == '[':
			depth++
			deepest = max(deepest, depth)
		case ch == '}' || ch == ']':
			depth--
		}
	}
	return deepest
}

// collectRawRequestBody 从 BodyStorage 读出入站请求体并压缩到配置长度
// （common.ErrorLogRawRequestBodyLimit 个字符）。读取失败或没有 body 时返回空串，
// 由调用方继续用 Size() 记录总长度。
//
// JSON body 按字段压缩（见 compressRequestBody）：所有字段都保留，只截短内容值。
// 非法 JSON、嵌套过深或超过 rawRequestBodyParseMaxBytes 的 body 退回到前缀截断。
// 压缩结果无论如何不超过 common.ErrorLogRawRequestBodyLimitMax 个字符。
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
	targetRunes := min(max(common.ErrorLogRawRequestBodyLimit, 0), common.ErrorLogRawRequestBodyLimitMax)
	reader, err := storage.NewReader()
	if err != nil {
		return "", false, totalBytes, contentType
	}
	defer reader.Close()
	if totalBytes <= rawRequestBodyParseMaxBytes {
		raw, err := io.ReadAll(reader)
		if err != nil {
			return "", false, totalBytes, contentType
		}
		if jsonNestingDepth(raw) <= rawRequestMaxNestingDepth && gjson.ValidBytes(raw) {
			body, truncated = compressRequestBody(string(raw), targetRunes)
			if hardCapped, capped := truncateRunes(body, common.ErrorLogRawRequestBodyLimitMax); capped {
				return hardCapped, true, totalBytes, contentType
			}
			return body, truncated, totalBytes, contentType
		}
		body, truncated = truncateRunes(string(raw), targetRunes)
		return body, truncated, totalBytes, contentType
	}
	// UTF-8 单字符最多 4 字节，多读 1 字节用于判断是否发生了截断。
	raw, err := io.ReadAll(io.LimitReader(reader, int64(targetRunes)*4+1))
	if err != nil {
		return "", false, totalBytes, contentType
	}
	body, _ = truncateRunes(string(raw), targetRunes)
	return body, true, totalBytes, contentType
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
