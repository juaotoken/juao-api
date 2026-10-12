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

// withRawRequestBodyLimit 临时设置 body 保存长度。
func withRawRequestBodyLimit(t *testing.T, limit int) {
	t.Helper()
	previous := common.ErrorLogRawRequestBodyLimit
	common.ErrorLogRawRequestBodyLimit = limit
	t.Cleanup(func() { common.ErrorLogRawRequestBodyLimit = previous })
}

// 需求示例：Claude 多模态消息。超长时所有字段都在，系统字段的值原样保留，
// 只有用户输入 / base64 / 签名这类内容值被截短，且压缩后仍是合法 JSON。
func TestCollectRawRequestInfoCompressesContentButKeepsEveryField(t *testing.T) {
	withRawRequestBodyLimit(t, 1000)
	prompt := strings.Repeat("分析这张图", 400)
	body := `{"model":"claude-opus-4-1","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"` + prompt + `"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + strings.Repeat("A", 20000) + `"}},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},` +
		`{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}},` +
		`{"type":"tool_result","tool_use_id":"toolu_01","content":"` + strings.Repeat("晴", 3000) + `"},` +
		`{"type":"thinking","thinking":"` + strings.Repeat("t", 5000) + `","signature":"` + strings.Repeat("s", 2000) + `"}]}]}`
	c := newRawRequestContext(t, "application/json", []byte(body))

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.True(t, info.BodyTruncated)
	assert.Equal(t, int64(len(body)), info.BodyBytes)
	assert.LessOrEqual(t, len([]rune(info.Body)), 1000)
	var parsed map[string]any
	require.NoError(t, common.UnmarshalJsonStr(info.Body, &parsed), "压缩后的 body 必须仍是合法 JSON")
	assert.Equal(t, "claude-opus-4-1", parsed["model"])
	assert.Equal(t, float64(4096), parsed["max_tokens"])
	assert.Equal(t, true, parsed["stream"])

	message := parsed["messages"].([]any)[0].(map[string]any)
	assert.Equal(t, "user", message["role"])
	blocks := message["content"].([]any)
	require.Len(t, blocks, 6, "每个内容块都要保留")
	types := make([]string, 0, len(blocks))
	for _, block := range blocks {
		types = append(types, block.(map[string]any)["type"].(string))
	}
	assert.Equal(t, []string{"text", "image", "image", "tool_use", "tool_result", "thinking"}, types)

	text := blocks[0].(map[string]any)["text"].(string)
	assert.True(t, strings.HasPrefix(text, "分析这张图"), "内容值保留开头")
	assert.Contains(t, text, "…[+")
	source := blocks[1].(map[string]any)["source"].(map[string]any)
	assert.Equal(t, "base64", source["type"])
	assert.Equal(t, "image/jpeg", source["media_type"])
	assert.Contains(t, source["data"], "…[+")
	assert.Equal(t, "https://example.com/a.png", blocks[2].(map[string]any)["source"].(map[string]any)["url"], "短的值原样保留")
	toolUse := blocks[3].(map[string]any)
	assert.Equal(t, "toolu_01", toolUse["id"])
	assert.Equal(t, "get_weather", toolUse["name"])
	assert.Equal(t, map[string]any{}, toolUse["input"])
	assert.Equal(t, "toolu_01", blocks[4].(map[string]any)["tool_use_id"])
	thinking := blocks[5].(map[string]any)
	assert.Contains(t, thinking, "thinking")
	assert.Contains(t, thinking["signature"], "…[+")
}

// 配置长度越大保留越多；未超出配置长度时 body 原样保存。
func TestCollectRawRequestInfoHonorsConfiguredBodyLimit(t *testing.T) {
	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"` + strings.Repeat("a", 4000) + `"}]}`

	withRawRequestBodyLimit(t, 5000)
	info := CollectRawRequestInfo(newRawRequestContext(t, "application/json", []byte(body)))
	require.NotNil(t, info)
	assert.Equal(t, body, info.Body)
	assert.False(t, info.BodyTruncated)

	withRawRequestBodyLimit(t, 1000)
	info = CollectRawRequestInfo(newRawRequestContext(t, "application/json", []byte(body)))
	require.NotNil(t, info)
	assert.True(t, info.BodyTruncated)
	assert.LessOrEqual(t, len([]rune(info.Body)), 1000)
	assert.Greater(t, len([]rune(info.Body)), 900, "压缩应尽量用满配置长度")
}

// 字段结构本身就超过配置长度时，按结构实际长度保存：不丢字段，每个内容值只留开头。
func TestCollectRawRequestInfoKeepsStructureWhenFieldsExceedLimit(t *testing.T) {
	withRawRequestBodyLimit(t, 50)
	messages := make([]string, 0, 20)
	for range 20 {
		messages = append(messages, `{"role":"user","content":"`+strings.Repeat("x", 100)+`"}`)
	}
	body := `{"model":"glm-5.3","messages":[` + strings.Join(messages, ",") + `]}`

	info := CollectRawRequestInfo(newRawRequestContext(t, "application/json", []byte(body)))

	require.NotNil(t, info)
	assert.True(t, info.BodyTruncated)
	assert.Greater(t, len([]rune(info.Body)), 50)
	var parsed map[string]any
	require.NoError(t, common.UnmarshalJsonStr(info.Body, &parsed))
	assert.Len(t, parsed["messages"].([]any), 20)
	for _, message := range parsed["messages"].([]any) {
		assert.Equal(t, "user", message.(map[string]any)["role"])
		assert.True(t, strings.HasPrefix(message.(map[string]any)["content"].(string), "xxxxxxxx…[+"))
	}
}

// 转义字符（换行、引号、控制字符）让输出比解码后的字符数更长，压缩结果仍须守住配置长度。
// 嵌套过深的 body 不做递归压缩，退回到前缀截断。
func TestCollectRawRequestInfoBoundsEscapedAndDeeplyNestedBodies(t *testing.T) {
	withRawRequestBodyLimit(t, 1000)
	escaped := `{"model":"glm-5.3","messages":[{"role":"user","content":"` + strings.Repeat(`a\n\"\u0001`, 2000) + `"},{"role":"assistant","content":"` + strings.Repeat(`\t`, 3000) + `"}]}`
	info := CollectRawRequestInfo(newRawRequestContext(t, "application/json", []byte(escaped)))
	require.NotNil(t, info)
	assert.LessOrEqual(t, len([]rune(info.Body)), 1000)
	var parsed map[string]any
	require.NoError(t, common.UnmarshalJsonStr(info.Body, &parsed))
	assert.Len(t, parsed["messages"].([]any), 2)

	deep := []byte(strings.Repeat("[", 100000) + strings.Repeat("]", 100000))
	info = CollectRawRequestInfo(newRawRequestContext(t, "application/json", deep))
	require.NotNil(t, info)
	assert.Len(t, info.Body, 1000)
	assert.True(t, info.BodyTruncated)
}

// 非法 JSON 无法按字段压缩，退回到按字符截断（中文按 rune 计数，不切半个字符）。
func TestCollectRawRequestInfoTruncatesInvalidJsonByRune(t *testing.T) {
	withRawRequestBodyLimit(t, 1000)
	body := []byte(`{"text":"` + strings.Repeat("汉", 1001))
	c := newRawRequestContext(t, "application/json", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Len(t, []rune(info.Body), 1000)
	assert.True(t, strings.HasPrefix(string(body), info.Body))
	assert.True(t, info.BodyTruncated)
	assert.Equal(t, int64(len(body)), info.BodyBytes)
}

func TestCollectRawRequestInfoMarksNonJsonBody(t *testing.T) {
	body := []byte("--boundary\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\n\x00\x01\x02")
	c := newRawRequestContext(t, "multipart/form-data; boundary=boundary", body)

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Contains(t, info.Body, "[skip non-json body: multipart/form-data; boundary=boundary]")
	assert.Equal(t, int64(len(body)), info.BodyBytes)
	assert.Equal(t, "multipart/form-data; boundary=boundary", info.BodyContentType)

	// 异常客户端可以塞一个近 1MB 的 Content-Type（http.Server 默认上限 1MB），
	// 它同时进入非 JSON 标记与 body_content_type，两处都必须限长。
	longContentType := "text/plain; " + strings.Repeat("p", 600)
	c2 := newRawRequestContext(t, longContentType, body)
	info2 := CollectRawRequestInfo(c2)

	require.NotNil(t, info2)
	assert.Len(t, []rune(strings.TrimSuffix(info2.BodyContentType, "…")), 512)
	assert.True(t, strings.HasSuffix(info2.BodyContentType, "…"))
	assert.Contains(t, info2.Body, "[skip non-json body: ")
	assert.True(t, strings.HasSuffix(info2.Body, "…]"))
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

func TestCollectRawRequestInfoAcceptsJsonContentTypeVariants(t *testing.T) {
	body := []byte(`{"model":"glm-5.3"}`)

	// +json 后缀（application/vnd.api+json）走 JSON 路径。
	c := newRawRequestContext(t, "application/vnd.api+json", body)
	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	assert.Equal(t, string(body), info.Body)

	// 带 charset 参数是极常见的形态，strings.Cut(contentType, ";") 之后
	// 仍须判定为 JSON，而不是落到非 JSON 标记上。
	c2 := newRawRequestContext(t, "application/json; charset=utf-8", body)
	info2 := CollectRawRequestInfo(c2)

	require.NotNil(t, info2)
	assert.Equal(t, string(body), info2.Body)
	assert.NotContains(t, info2.Body, "[skip non-json body")
}

func TestCollectRawRequestInfoMasksCredentialHeaders(t *testing.T) {
	c := newRawRequestContext(t, "application/json", []byte(`{}`))
	c.Request.Header.Set("Authorization", "Bearer sk-real-token")
	c.Request.Header.Set("Cookie", "session=real-cookie")
	c.Request.Header.Set("Set-Cookie", "session=real-set-cookie")
	c.Request.Header.Set("Proxy-Authorization", "Basic real-proxy-credential")
	c.Request.Header.Set("x-api-key", "real-anthropic-key")
	c.Request.Header.Set("x-goog-api-key", "real-gemini-key")
	c.Request.Header.Set("mj-api-secret", "real-mj-secret")
	c.Request.Header.Set("Xapikey", "real-apikey-suffix")
	c.Request.Header.Set("Sec-WebSocket-Protocol", "realtime,openai-insecure-api-key.real-realtime-key")
	c.Request.Header.Set("User-Agent", "OpenAI/Python 1.52.0")
	c.Request.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	info := CollectRawRequestInfo(c)

	require.NotNil(t, info)
	for _, name := range []string{
		"Authorization",
		"Cookie",
		"Set-Cookie",
		"Proxy-Authorization",
		"X-Api-Key",
		"X-Goog-Api-Key",
		"Mj-Api-Secret",
		// Xapikey 不吃 -key 后缀也不含 api-key 子串，只证明 apikey 后缀分支。
		"Xapikey",
		"Sec-Websocket-Protocol",
	} {
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
