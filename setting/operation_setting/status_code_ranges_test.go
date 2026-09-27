package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/relaykit/types"
)

func TestParseHTTPStatusCodeRanges_CommaSeparated(t *testing.T) {
	ranges, err := ParseHTTPStatusCodeRanges("401,403,500-599")
	require.NoError(t, err)
	require.Equal(t, []StatusCodeRange{
		{Start: 401, End: 401},
		{Start: 403, End: 403},
		{Start: 500, End: 599},
	}, ranges)
}

func TestParseHTTPStatusCodeRanges_MergeAndNormalize(t *testing.T) {
	ranges, err := ParseHTTPStatusCodeRanges("500-505,504,401,403,402")
	require.NoError(t, err)
	require.Equal(t, []StatusCodeRange{
		{Start: 401, End: 403},
		{Start: 500, End: 505},
	}, ranges)
}

func TestParseHTTPStatusCodeRanges_Invalid(t *testing.T) {
	_, err := ParseHTTPStatusCodeRanges("99,600,foo,500-400,500-")
	require.Error(t, err)
}

func TestParseHTTPStatusCodeRanges_NoComma_IsInvalid(t *testing.T) {
	_, err := ParseHTTPStatusCodeRanges("401 403")
	require.Error(t, err)
}

func TestShouldDisableByStatusCode(t *testing.T) {
	orig := AutomaticDisableStatusCodeRanges
	t.Cleanup(func() { AutomaticDisableStatusCodeRanges = orig })

	AutomaticDisableStatusCodeRanges = []StatusCodeRange{
		{Start: 401, End: 403},
		{Start: 500, End: 599},
	}

	require.True(t, ShouldDisableByStatusCode(401))
	require.True(t, ShouldDisableByStatusCode(403))
	require.False(t, ShouldDisableByStatusCode(404))
	require.True(t, ShouldDisableByStatusCode(500))
	require.False(t, ShouldDisableByStatusCode(200))
}

func TestShouldRetryByStatusCode(t *testing.T) {
	orig := AutomaticRetryStatusCodeRanges
	t.Cleanup(func() { AutomaticRetryStatusCodeRanges = orig })

	AutomaticRetryStatusCodeRanges = []StatusCodeRange{
		{Start: 429, End: 429},
		{Start: 500, End: 599},
	}

	require.True(t, ShouldRetryByStatusCode(429))
	require.True(t, ShouldRetryByStatusCode(500))
	// 504/524 不再被硬编码拦下：配了 500-599 就该重试（2026-09-26 钜敖改）
	require.True(t, ShouldRetryByStatusCode(504))
	require.True(t, ShouldRetryByStatusCode(524))
	require.False(t, ShouldRetryByStatusCode(400))
	require.False(t, ShouldRetryByStatusCode(200))
}

// 默认区间的行为。与上游的差别只在 504/524（钜敖 2026-09-26 起要求重试），
// 400/408 仍然不重试。
func TestShouldRetryByStatusCode_JuaoDefaults(t *testing.T) {
	require.False(t, ShouldRetryByStatusCode(200))
	require.False(t, ShouldRetryByStatusCode(400))
	require.True(t, ShouldRetryByStatusCode(401))
	require.False(t, ShouldRetryByStatusCode(408))
	require.True(t, ShouldRetryByStatusCode(429))
	require.True(t, ShouldRetryByStatusCode(500))
	require.True(t, ShouldRetryByStatusCode(502))
	require.True(t, ShouldRetryByStatusCode(503))
	// ↓ 本次改动的核心：网关超时也要换渠道重试
	require.True(t, ShouldRetryByStatusCode(504))
	require.True(t, ShouldRetryByStatusCode(524))
	require.True(t, ShouldRetryByStatusCode(599))
}

// 「无论怎么配都不重试」的状态码清单已清空（2026-09-26 钜敖改）。
//
// ⚠️ 这张 map 在 ShouldRetryByStatusCode 的第一行就被检查，**优先于后台可配的
// AutomaticRetryStatusCodes** —— 生产把区间配成 409-599（已含 504）却依然
// 不重试，就是被它挡住的，且页面上看不出任何迹象。这条用例钉住它保持为空：
// 谁再往里加状态码，必须同时解释为什么它连配置都不该能覆盖。
func TestIsAlwaysSkipRetryStatusCode_EmptyAfterJuaoChange(t *testing.T) {
	require.False(t, IsAlwaysSkipRetryStatusCode(504))
	require.False(t, IsAlwaysSkipRetryStatusCode(524))
	require.False(t, IsAlwaysSkipRetryStatusCode(500))
	require.Empty(t, alwaysSkipRetryStatusCodes, "硬编码的「永不重试」状态码清单应为空")
}

// 错误码黑名单也已清空：bad_response_body 不再阻断重试（2026-09-26 钜敖改）。
//
// 触发链（上游行为）：上游返回非 200 时，RelayErrorHandler 会解析响应体，
// body 形如 {"error":{...,"code":"..."}} 时走 types.WithOpenAIError，
// 把上游 JSON 里的 code **原样强转**成 errorCode（无白名单校验）。
// 于是上游只要回 "code":"bad_response_body"，哪怕 HTTP 是 502、哪怕我们
// 其实解析成功，也会被判成「解析失败」而跳过重试 —— 本该换渠道的请求
// 直接失败返回给客户。清空后回落到按状态码判定。
func TestIsAlwaysSkipRetryCode_EmptyAfterJuaoChange(t *testing.T) {
	require.False(t, IsAlwaysSkipRetryCode(types.ErrorCodeBadResponseBody))
	require.Empty(t, alwaysSkipRetryCodes, "硬编码的「永不重试」错误码清单应为空")
}
