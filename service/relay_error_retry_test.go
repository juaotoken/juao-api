package service

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// DecideRelayRetry 的实际判定（2026-09-26 钜敖改动的回归测试）。
//
// 生产两次「该切渠道却没切」都死在这个判定里，且都**不是配置问题**：
//
//	slsgufen 2026-09-26 08:27  claude-opus-5 打到渠道 109 返回 504
//	                           （use_time 600 秒）。use_channel 只有 ["109"]，
//	                           一次重试都没有，而该分组当时还有 3 个可用渠道。
//	                           死因：alwaysSkipRetryStatusCodes 硬编码含 504，
//	                           在 ShouldRetryByStatusCode 之前被检查，
//	                           优先于后台配的 AutomaticRetryStatusCodes=409-599。
//
//	另一例                      上游回 502、响应体里 code 是 "bad_response_body"。
//	                           WithOpenAIError 把上游 JSON 的 code 原样强转成
//	                           errorCode（无白名单），命中 alwaysSkipRetryCodes
//	                           → 同样不重试。
//
// 两张硬编码 map 现已清空。这些用例钉住「清空后真的会重试」，
// 以及那些**应当**继续不重试的分支没有被顺带放开。

func retryCtx(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

// upstreamErr 构造「上游非 200 且响应体带 code」的错误，与
// RelayErrorHandler 走 WithOpenAIError 那条路径同形。
func upstreamErr(statusCode int, code string) *types.NewAPIError {
	return types.WithOpenAIError(types.OpenAIError{
		Message: "bad response status code",
		Type:    "openai_error",
		Code:    code,
	}, statusCode)
}

func TestDecideRelayRetryStatusCodes(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		code       string
		retryTimes int
		wantRetry  bool
		wantReason string
	}{
		// —— 本次改动的三个核心诉求 ——
		{"504 网关超时要重试", http.StatusGatewayTimeout, "bad_response_status_code", 2, true, "retry_status_matched"},
		{"524 网关超时要重试", 524, "bad_response_status_code", 2, true, "retry_status_matched"},
		{"502 且错误码 bad_response_body 要重试", http.StatusBadGateway, "bad_response_body", 2, true, "retry_status_matched"},

		// —— 原有行为不能被顺带放开 ——
		{"2xx 不重试（哪怕错误码是 bad_response_body）", http.StatusOK, "bad_response_body", 2, false, "system_retry_exclusion"},
		{"400 不重试", http.StatusBadRequest, "invalid_request_error", 2, false, "status_not_retryable"},
		{"408 不重试", http.StatusRequestTimeout, "timeout", 2, false, "status_not_retryable"},
		{"重试预算用完不重试", http.StatusBadGateway, "bad_response_status_code", 0, false, "attempt_budget_exhausted"},

		// —— 其余 5xx / 429 照旧重试 ——
		{"500 重试", http.StatusInternalServerError, "bad_response_status_code", 2, true, "retry_status_matched"},
		{"503 重试", http.StatusServiceUnavailable, "bad_response_status_code", 2, true, "retry_status_matched"},
		{"429 重试", http.StatusTooManyRequests, "rate_limit_exceeded", 2, true, "retry_status_matched"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DecideRelayRetry(retryCtx(t), upstreamErr(c.statusCode, c.code), c.retryTimes)
			got := d.Action == "retry"
			require.Equalf(t, c.wantRetry, got,
				"状态码 %d / 错误码 %q：判定 %+v", c.statusCode, c.code, d)
			// 理由会落进日志的请求策略事件，排查时靠它区分「为什么没重试」
			require.Equal(t, c.wantReason, d.Reason)
		})
	}
}

// 显式标记 SkipRetry 的错误仍然不重试（请求体过大、取渠道失败等内部判定）。
// 这是另一道闸，与那两张 map 无关，不该被本次改动影响。
func TestDecideRelayRetryRespectsExplicitSkipRetry(t *testing.T) {
	err := types.NewErrorWithStatusCode(
		errors.New("request body too large"),
		types.ErrorCodeReadRequestBodyFailed,
		http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
	d := DecideRelayRetry(retryCtx(t), err, 2)
	require.NotEqual(t, "retry", d.Action, "显式 SkipRetry 的错误不该重试：%+v", d)
	require.Equal(t, "non_retryable_error", d.Reason)
}

// nil 错误 = 请求成功，不重试。
func TestDecideRelayRetryNilError(t *testing.T) {
	d := DecideRelayRetry(retryCtx(t), nil, 2)
	require.Equal(t, "stop", d.Action)
	require.Equal(t, "request_completed", d.Reason)
}

// ShouldRetryRelayError 是 DecideRelayRetry 的布尔外壳，两者必须一致。
func TestShouldRetryRelayErrorMatchesDecision(t *testing.T) {
	for _, code := range []int{http.StatusGatewayTimeout, 524, http.StatusBadGateway,
		http.StatusBadRequest, http.StatusOK} {
		ctx := retryCtx(t)
		err := upstreamErr(code, "bad_response_status_code")
		require.Equal(t,
			DecideRelayRetry(ctx, err, 2).Action == "retry",
			ShouldRetryRelayError(ctx, err, 2),
			"状态码 %d 两个入口的结论不一致", code)
	}
}

// 分组独立重试设置：命中的分组用自己的重试次数与状态码，其余分组与 auto 分组走全局。
func TestGroupRetryOverrides(t *testing.T) {
	database := setupRawRequestLogTestDB(t)
	require.NoError(t, database.AutoMigrate(&model.Option{}))
	previousOptions := maps.Clone(model.CurrentRequestPolicy().Options)
	common.OptionMapRWMutex.Lock()
	previousOptionMap := common.OptionMap
	common.OptionMap = maps.Clone(previousOptions)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		require.NoError(t, model.UpdateRequestPolicyOptions(previousOptions))
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
	})
	require.NoError(t, model.UpdateRequestPolicyOptions(map[string]string{
		"RetryTimes":                "2",
		"AutomaticRetryStatusCodes": "500-599",
		"GroupRetryOverrides":       `{"cc-max":{"retry_times":5,"status_codes":"429"},"gemini":{"status_codes":"400"},"budget-only":{"retry_times":0}}`,
	}))

	withGroup := func(group string) *gin.Context {
		c := retryCtx(t)
		if group != "" {
			common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
		}
		return c
	}
	for _, tc := range []struct {
		group      string
		wantTimes  int
		status     int
		wantAction string
		wantSource string
	}{
		{group: "cc-max", wantTimes: 5, status: 429, wantAction: "retry", wantSource: "group"},
		{group: "cc-max", wantTimes: 5, status: 502, wantAction: "stop", wantSource: "group"},
		{group: "gemini", wantTimes: 2, status: 400, wantAction: "retry", wantSource: "group"},
		{group: "budget-only", wantTimes: 0, status: 502, wantAction: "retry", wantSource: "global"},
		{group: "default", wantTimes: 2, status: 429, wantAction: "stop", wantSource: "global"},
		{group: "default", wantTimes: 2, status: 502, wantAction: "retry", wantSource: "global"},
		{group: "auto", wantTimes: 2, status: 429, wantAction: "stop", wantSource: "global"},
		{group: "", wantTimes: 2, status: 502, wantAction: "retry", wantSource: "global"},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.group, tc.status), func(t *testing.T) {
			c := withGroup(tc.group)
			require.Equal(t, tc.wantTimes, RequestRetryTimes(c))
			decision := DecideRelayRetry(c, upstreamErr(tc.status, ""), 1)
			require.Equal(t, tc.wantAction, decision.Action)
			require.Equal(t, tc.wantSource, decision.Source)
		})
	}

	exhausted := DecideRelayRetry(withGroup("cc-max"), upstreamErr(429, ""), 0)
	require.Equal(t, "stop", exhausted.Action)
	require.Equal(t, "group", exhausted.Source, "预算耗尽的来源标注为分组配置")
	globalExhausted := DecideRelayRetry(withGroup("gemini"), upstreamErr(400, ""), 0)
	require.Equal(t, "global", globalExhausted.Source, "只覆盖状态码的分组，预算仍来自全局")
}

