package operation_setting

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/types"
)

type StatusCodeRange struct {
	Start int
	End   int
}

var AutomaticDisableStatusCodeRanges = []StatusCodeRange{{Start: 401, End: 401}}

// 默认重试区间：1xx、3xx、4xx（400/408 除外）、**整个 5xx**，2xx 不重试。
//
// ⚠️ 与上游 new-api 的差异（2026-09-26 钜敖改）：上游默认把 504/524 排除在
// 5xx 之外（写作 500-503、505-523、525-599），这里改成整段 500-599。
// 理由见下面 alwaysSkipRetryStatusCodes 的注释。
var AutomaticRetryStatusCodeRanges = []StatusCodeRange{
	{Start: 100, End: 199},
	{Start: 300, End: 399},
	{Start: 401, End: 407},
	{Start: 409, End: 499},
	{Start: 500, End: 599},
}

// alwaysSkipRetryStatusCodes 是「无论怎么配都不重试」的状态码。
//
// ⚠️ 2026-09-26 钜敖清空了它（上游原本是 504、524）。
//
// 为什么改：这张 map 在 ShouldRetryByStatusCode 的**第一行**就被检查，
// 优先于后台可配的 AutomaticRetryStatusCodes —— 生产把重试区间配成
// 409-599（已包含 504）却依然不重试，就是被它挡住的，而页面上完全看不出来。
// 实测案例：slsgufen 2026-09-26 08:27 用 claude-opus-5 打到渠道 109，
// 504（use_time=600 秒）后 use_channel 只有 ["109"] —— 一次重试都没发生，
// 而那个分组当时还有 3 个可用渠道。8 点那一小时同样的失败连着三次。
//
// 代价（已知并接受）：504/524 是网关超时，重试意味着再赌一轮长等待；
// 非流式请求上游可能已经算完并计了费，重试会再计一次。所以**必须配合**
// 把上游超时压到远小于客户端超时的量级，否则用户等待会成倍放大。
// 重试次数本身仍受 RetryTimes 限制（生产为 2）。
//
// 保留这张空 map 而不是删掉整套机制：将来要再拉黑某个状态码时，
// 只需往这里加一行，不必改 ShouldRetryByStatusCode 的结构。
var alwaysSkipRetryStatusCodes = map[int]struct{}{}

// alwaysSkipRetryCodes 是「无论怎么配都不重试」的**错误码**。
//
// ⚠️ 2026-09-26 钜敖清空了它（上游原本含 types.ErrorCodeBadResponseBody，
// 见上游 commit 329416d67 "fix(relay): skip retries for bad response body errors"）。
//
// 上游的设计意图本身是合理的：自己拿到 200 却解析不出合法响应时，重试大概率
// 同样失败，而非流式请求上游已经生成完并计了费。
//
// 但它在钜敖这里被**错误触发**：errorCode 并不总是我们自己判定的，
// RelayErrorHandler 在上游返回非 200 时会解析上游响应体，若 body 形如
// {"error":{...,"code":"..."}} 就走 types.WithOpenAIError，把上游 JSON 里的
// code 字段**原样强转**成 errorCode（relaykit/types/error.go，无白名单校验）。
// 于是上游只要回一个 "code":"bad_response_body"，哪怕 HTTP 状态是 502、
// 哪怕响应体其实解析得很成功，我们也会判成「解析失败」而跳过重试 ——
// 本该切换渠道的请求直接失败返回给客户。
//
// 清空它之后，这类错误回落到按**状态码**判定（502 → 在重试区间内 → 重试），
// 与「上游挂了就换一个渠道」这个本意一致。
var alwaysSkipRetryCodes = map[types.ErrorCode]struct{}{}

func AutomaticDisableStatusCodesToString() string {
	return statusCodeRangesToString(AutomaticDisableStatusCodeRanges)
}

func AutomaticDisableStatusCodesFromString(s string) error {
	ranges, err := ParseHTTPStatusCodeRanges(s)
	if err != nil {
		return err
	}
	AutomaticDisableStatusCodeRanges = ranges
	return nil
}

func ShouldDisableByStatusCode(code int) bool {
	return shouldMatchStatusCodeRanges(AutomaticDisableStatusCodeRanges, code)
}

func AutomaticRetryStatusCodesToString() string {
	return statusCodeRangesToString(AutomaticRetryStatusCodeRanges)
}

func AutomaticRetryStatusCodesFromString(s string) error {
	ranges, err := ParseHTTPStatusCodeRanges(s)
	if err != nil {
		return err
	}
	AutomaticRetryStatusCodeRanges = ranges
	return nil
}

func IsAlwaysSkipRetryStatusCode(code int) bool {
	_, exists := alwaysSkipRetryStatusCodes[code]
	return exists
}

func IsAlwaysSkipRetryCode(errorCode types.ErrorCode) bool {
	_, exists := alwaysSkipRetryCodes[errorCode]
	return exists
}

func ShouldRetryByStatusCode(code int) bool {
	if IsAlwaysSkipRetryStatusCode(code) {
		return false
	}
	return shouldMatchStatusCodeRanges(AutomaticRetryStatusCodeRanges, code)
}

func statusCodeRangesToString(ranges []StatusCodeRange) string {
	if len(ranges) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r.Start == r.End {
			parts = append(parts, strconv.Itoa(r.Start))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", r.Start, r.End))
	}
	return strings.Join(parts, ",")
}

func shouldMatchStatusCodeRanges(ranges []StatusCodeRange, code int) bool {
	if code < 100 || code > 599 {
		return false
	}
	for _, r := range ranges {
		if code < r.Start {
			return false
		}
		if code <= r.End {
			return true
		}
	}
	return false
}

func ParseHTTPStatusCodeRanges(input string) ([]StatusCodeRange, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	input = strings.NewReplacer("，", ",").Replace(input)
	segments := strings.Split(input, ",")

	var ranges []StatusCodeRange
	var invalid []string

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		r, err := parseHTTPStatusCodeToken(seg)
		if err != nil {
			invalid = append(invalid, seg)
			continue
		}
		ranges = append(ranges, r)
	}

	if len(invalid) > 0 {
		return nil, fmt.Errorf("invalid http status code rules: %s", strings.Join(invalid, ", "))
	}
	if len(ranges) == 0 {
		return nil, nil
	}

	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			return ranges[i].End < ranges[j].End
		}
		return ranges[i].Start < ranges[j].Start
	})

	merged := []StatusCodeRange{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}

	return merged, nil
}

func parseHTTPStatusCodeToken(token string) (StatusCodeRange, error) {
	token = strings.TrimSpace(token)
	token = strings.ReplaceAll(token, " ", "")
	if token == "" {
		return StatusCodeRange{}, fmt.Errorf("empty token")
	}

	if strings.Contains(token, "-") {
		parts := strings.Split(token, "-")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return StatusCodeRange{}, fmt.Errorf("invalid range token: %s", token)
		}
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return StatusCodeRange{}, fmt.Errorf("invalid range start: %s", token)
		}
		end, err := strconv.Atoi(parts[1])
		if err != nil {
			return StatusCodeRange{}, fmt.Errorf("invalid range end: %s", token)
		}
		if start > end {
			return StatusCodeRange{}, fmt.Errorf("range start > end: %s", token)
		}
		if start < 100 || end > 599 {
			return StatusCodeRange{}, fmt.Errorf("range out of bounds: %s", token)
		}
		return StatusCodeRange{Start: start, End: end}, nil
	}

	code, err := strconv.Atoi(token)
	if err != nil {
		return StatusCodeRange{}, fmt.Errorf("invalid status code: %s", token)
	}
	if code < 100 || code > 599 {
		return StatusCodeRange{}, fmt.Errorf("status code out of bounds: %s", token)
	}
	return StatusCodeRange{Start: code, End: code}, nil
}
