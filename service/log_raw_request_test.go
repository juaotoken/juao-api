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
