package service

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// DecideRelayRetry is the single retry decision for relay attempts. The reason
// is recorded in the request policy decision events of the log details.
func DecideRelayRetry(c *gin.Context, err *types.NewAPIError, retryTimes int) PolicyDecision {
	if err == nil {
		return PolicyDecision{Action: "stop", Reason: "request_completed", Source: "system"}
	}
	if ShouldSkipRetryAfterChannelAffinityFailure(c) {
		source := RequestPolicy(c).SessionModeSource
		if source == "" {
			source = "session_rule"
		}
		return PolicyDecision{Action: "stop", Reason: "strict_session", Source: source}
	}
	if GetChannelConstraints(c).SuppressesRetry() {
		return PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}
	}
	if types.IsChannelError(err) {
		return PolicyDecision{Action: "retry", Reason: "channel_error", Source: "system"}
	}
	if types.IsSkipRetryError(err) {
		return PolicyDecision{Action: "stop", Reason: "non_retryable_error", Source: "system"}
	}
	if retryTimes <= 0 {
		return PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"}
	}
	code := err.StatusCode
	if code >= 200 && code < 300 {
		return PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}
	}
	if code < 100 || code > 599 {
		return PolicyDecision{Action: "retry", Reason: "unrecognized_status", Source: "system"}
	}
	if operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) || operation_setting.IsAlwaysSkipRetryStatusCode(code) {
		return PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}
	}
	if operation_setting.ShouldRetryByStatusCode(code) {
		return PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"}
	}
	return PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"}
}

func ShouldRetryRelayError(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	return DecideRelayRetry(c, openaiErr, retryTimes).Action == "retry"
}

// IsFinalAttempt 判断本次尝试是否为整个请求的最后一次尝试。
// 不能只看 decision.Action：DecideRelayRetry 把 IsChannelError 判定排在
// 预算检查之前，所以 channel:* 类错误即使在预算耗尽时也返回 "retry"，
// 循环实际是靠自身 bound 退出的。只看 Action 会让这整类错误永远采不到原始请求。
//
// 另外，重试计数并非单调：cross_group_retry 在切换分组时会把计数器重置为 0
// （service/channel_select.go），一次请求因此可能多次满足 attempt >= RetryTimes。
// 需要「同一请求只采一次」的调用方请用 ClaimRawRequestSnapshot，不要直接用本函数。
func IsFinalAttempt(decision PolicyDecision, attempt int) bool {
	return decision.Action != "retry" || attempt >= common.RetryTimes
}

// ClaimRawRequestSnapshot 判断本次渠道失败是否应当写入原始请求快照，并**占用**
// 本次请求的唯一名额：返回 true 的同时把 RequestPolicyState 的闸门置位，
// 此后同一请求再调用一律返回 false。
// 因此它是一个「判定并占位」的操作，不要用它做纯判断——探询会白白消耗名额。
//
// 与 IsFinalAttempt 的区别：切换分组会把重试计数器重置为 0
// （service/channel_select.go 的 cross_group_retry 路径），一次请求因此可能
// 多次满足 attempt >= RetryTimes。用请求级的 RequestPolicyState 做一次性闸门，
// 保证同一请求只落一份 body。
func ClaimRawRequestSnapshot(c *gin.Context, decision PolicyDecision, attempt int) bool {
	if !IsFinalAttempt(decision, attempt) {
		return false
	}
	state := RequestPolicy(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.RawRequestLogged {
		return false
	}
	state.RawRequestLogged = true
	return true
}

// ProcessChannelError 记录一次渠道失败。shouldRecordRawRequest 为真时才会附上
// 原始请求快照——该值必须来自 ClaimRawRequestSnapshot，它同时保证「是最后一次尝试」
// 与「本次请求还没写过」两件事。直接用 IsFinalAttempt 或 decision.Action 会让
// cross_group_retry 场景重复记录（见 ClaimRawRequestSnapshot 的说明）。
func ProcessChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo, shouldRecordRawRequest bool) {
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
		if common.ErrorLogRawRequestEnabled && relayInfo != nil && shouldRecordRawRequest {
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
