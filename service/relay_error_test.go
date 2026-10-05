package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestShouldRetryRelayErrorHonorsChannelPinOnChannelError(t *testing.T) {
	err := types.NewError(errors.New("channel failed"), types.ErrorCodeChannelNoAvailableKey)
	for _, test := range []struct {
		name      string
		pin       *dto.ChannelPin
		wantRetry bool
	}{
		{name: "unrestricted channel error", wantRetry: true},
		{
			name: "single attempt pin suppresses channel error retry",
			pin: &dto.ChannelPin{
				ChannelId: 1, Source: dto.PinSourceToken, Rank: dto.PinRankToken, RetryMode: dto.PinRetrySingleAttempt,
			},
			wantRetry: false,
		},
		{
			name: "origin task pin permits retry on the same channel",
			pin: &dto.ChannelPin{
				ChannelId: 1, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySameChannel,
			},
			wantRetry: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if test.pin != nil {
				GetChannelConstraints(c).AddPin(*test.pin)
			}
			assert.Equal(t, test.wantRetry, ShouldRetryRelayError(c, err, 1))
		})
	}
}

func TestProcessChannelErrorMasksDisableReasonAndNotification(t *testing.T) {
	previousDB, previousType := model.DB, common.MainDatabaseType()
	previousCache, previousRedis := common.MemoryCacheEnabled, common.RedisEnabled
	previousAutoDisable, previousErrorLog := common.AutomaticDisableChannelEnabled, constant.ErrorLogEnabled
	previousNotifyLimit := constant.NotifyLimitCount
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.MemoryCacheEnabled, common.RedisEnabled = previousCache, previousRedis
		common.AutomaticDisableChannelEnabled, constant.ErrorLogEnabled = previousAutoDisable, previousErrorLog
		constant.NotifyLimitCount = previousNotifyLimit
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		*fetch = previousFetch
	})
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}))
	model.DB = database
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	common.AutomaticDisableChannelEnabled, constant.ErrorLogEnabled = true, false
	constant.NotifyLimitCount = 10
	channel := &model.Channel{Name: "relay-review", Key: "fixture-key", Type: 1, Status: common.ChannelStatusEnabled, Group: "default", Models: "test-model"}
	require.NoError(t, channel.Insert())
	notifications := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		notifications <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	httpClient, system_setting.WorkerUrl = server.Client(), ""
	fetch.EnableSSRFProtection = false
	settings, err := common.Marshal(kitdto.UserSetting{NotifyType: kitdto.NotifyTypeWebhook, WebhookUrl: server.URL})
	require.NoError(t, err)
	root := &model.User{Username: "notification-test-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Setting: string(settings)}
	require.NoError(t, database.Create(root).Error)
	notifyKey := fmt.Sprintf("%d:%s:%s", root.Id, formatNotifyType(channel.Id, common.ChannelStatusAutoDisabled), time.Now().Format("2006010215"))
	notifyLimitStore.Delete(notifyKey)
	t.Cleanup(func() { notifyLimitStore.Delete(notifyKey) })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	apiErr := types.NewErrorWithStatusCode(errors.New("upstream https://private.example.com/path?token=review-token api_key:review-secret"), types.ErrorCodeChannelNoAvailableKey, http.StatusUnauthorized)
	ProcessChannelError(c, types.ChannelError{ChannelId: channel.Id, ChannelName: channel.Name, AutoBan: true}, apiErr, nil, true)
	var notification WebhookPayload
	select {
	case payload := <-notifications:
		require.NoError(t, common.Unmarshal(payload, &notification))
	case <-time.After(5 * time.Second):
		t.Fatal("automatic channel-disable notification was not delivered")
	}
	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, loaded.Status)
	wantReason := "status_code=401, upstream https://***.com/***?token=*** api_key:***"
	assert.Equal(t, wantReason, loaded.GetOtherInfo()["status_reason"])
	assert.Contains(t, notification.Content, wantReason)
	assert.NotContains(t, notification.Content, "review-token")
	assert.NotContains(t, notification.Content, "review-secret")
	assert.Equal(t, http.StatusUnauthorized, apiErr.StatusCode)
}

func TestDecideRelayRetryReasons(t *testing.T) {
	upstream := func(status int) *types.NewAPIError {
		return types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, status)
	}
	for _, tc := range []struct {
		name    string
		err     *types.NewAPIError
		retries int
		setup   func(*gin.Context)
		want    PolicyDecision
	}{
		{name: "retry status matched", err: upstream(http.StatusTooManyRequests), retries: 1, want: PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"}},
		{name: "status outside retry rules", err: upstream(http.StatusBadRequest), retries: 1, want: PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"}},
		{name: "attempt budget exhausted", err: upstream(http.StatusTooManyRequests), retries: 0, want: PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"}},
		// 504 自 2026-09-26 起要重试（钜敖改：清空 alwaysSkipRetryStatusCodes）。
		// 原因见 setting/operation_setting/status_code_ranges.go 的注释与
		// service/relay_error_retry_test.go 的事故复盘。
		{name: "gateway timeout now retries", err: upstream(http.StatusGatewayTimeout), retries: 1, want: PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"}},
		{name: "success status never retries", err: upstream(http.StatusOK), retries: 1, want: PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}},
		{name: "skip retry error", err: types.NewErrorWithStatusCode(errors.New("local"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry()), retries: 1, want: PolicyDecision{Action: "stop", Reason: "non_retryable_error", Source: "system"}},
		{name: "channel error retries without budget", err: types.NewError(errors.New("no key"), types.ErrorCodeChannelNoAvailableKey), retries: 0, want: PolicyDecision{Action: "retry", Reason: "channel_error", Source: "system"}},
		{name: "single attempt pin", err: upstream(http.StatusTooManyRequests), retries: 1, setup: func(c *gin.Context) {
			GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: 1, Source: dto.PinSourceToken, Rank: dto.PinRankToken, RetryMode: dto.PinRetrySingleAttempt})
		}, want: PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}},
		{name: "strict session", err: upstream(http.StatusTooManyRequests), retries: 1, setup: func(c *gin.Context) {
			c.Set(ginKeyChannelAffinitySkipRetry, true)
			RequestPolicy(c).SessionModeSource = "global"
		}, want: PolicyDecision{Action: "stop", Reason: "strict_session", Source: "global"}},
		{name: "nil error", retries: 1, want: PolicyDecision{Action: "stop", Reason: "request_completed", Source: "system"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.setup != nil {
				tc.setup(c)
			}
			decision := DecideRelayRetry(c, tc.err, tc.retries)
			assert.Equal(t, tc.want, decision)
			assert.Equal(t, tc.want.Action == "retry", ShouldRetryRelayError(c, tc.err, tc.retries))
		})
	}
}

func TestRequestPolicyEventsReachLogAdminInfo(t *testing.T) {
	previousAutoDisable := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previousAutoDisable })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("auto_ban", true)
	c.Set("channel_id", 7)
	state := RequestPolicy(c)
	state.BeginAttempt(&model.Channel{Id: 7}, "default")
	apiErr := types.NewOpenAIError(errors.New("invalid credential"), types.ErrorCodeBadResponseStatusCode, http.StatusUnauthorized)
	RecordPolicyFailure(c, 7, apiErr, DecideRelayRetry(c, apiErr, 0))

	failed := model.NewLogOther()
	AppendRelayLogAdminInfo(c, nil, failed)
	events, ok := failed.Snapshot()["admin_info"].(map[string]any)["request_policy"].([]PolicyEvent)
	require.True(t, ok, "a failed relay exposes its decision events to administrators")
	require.Len(t, events, 3)
	assert.Equal(t, PolicyDecision{Action: "attempt", Reason: "channel_selected", Source: "routing"}, events[0].Decision)
	assert.Equal(t, "default", events[0].Group)
	assert.Equal(t, PolicyDecision{Action: "failure", Reason: "upstream_failure", Source: "upstream"}, events[1].Decision)
	assert.Equal(t, http.StatusUnauthorized, events[1].Status)
	assert.Equal(t, PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"}, events[2].Decision)
	assert.Equal(t, "channel_disable_requested", events[2].Health, "the health entry follows the automatic disable rules")
	common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, true)
	RecordPolicyFailure(c, 7, apiErr, DecideRelayRetry(c, apiErr, 0))
	assert.Equal(t, "key_disable_requested", state.Events()[4].Health)

	state.BeginAttempt(&model.Channel{Id: 8}, "default")
	c.Set("channel_id", 8)
	MarkRequestPolicySuccess(c, nil)
	MarkRequestPolicySuccess(c, nil)
	succeeded := model.NewLogOther()
	AppendRelayLogAdminInfo(c, nil, succeeded)
	events, ok = succeeded.Snapshot()["admin_info"].(map[string]any)["request_policy"].([]PolicyEvent)
	require.True(t, ok, "a successful relay exposes its decision events to administrators")
	require.Len(t, events, 7, "the outcome is recorded once")
	assert.Equal(t, PolicyDecision{Action: "success", Reason: "request_completed", Source: "upstream"}, events[6].Decision)
	assert.Equal(t, 8, events[6].ChannelID)
	assert.Equal(t, 2, events[6].Attempt)
	assert.True(t, state.Successful)

	untouched, _ := gin.CreateTestContext(httptest.NewRecorder())
	other := model.NewLogOther()
	AppendRelayLogAdminInfo(untouched, nil, other)
	assert.NotContains(t, other.Snapshot()["admin_info"], "request_policy", "requests without decisions do not carry an empty record")
}

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
	adminInfo, ok := other["admin_info"].(map[string]any)
	require.True(t, ok, "the error log must carry an admin_info map")
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
		&relaycommon.RelayInfo{}, true)

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
		&relaycommon.RelayInfo{}, false)

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
		&relaycommon.RelayInfo{}, true)

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
	ProcessChannelError(c, types.ChannelError{ChannelId: 150, ChannelName: "glm"}, apiErr, nil, true)

	assert.NotContains(t, storedErrorLogAdminInfo(t, database), "raw_request",
		"a channel test probe is not a user request")
}

func TestIsFinalAttempt(t *testing.T) {
	// RetryTimes 是包级变量（生产为 2，测试默认 0），此处固定成已知值，
	// 让「最后一次尝试 = attempt >= common.RetryTimes」这条语义可确定地断言。
	previousRetryTimes := common.RetryTimes
	common.RetryTimes = 2
	t.Cleanup(func() { common.RetryTimes = previousRetryTimes })

	upstream := func(status int) *types.NewAPIError {
		return types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, status)
	}
	for _, tc := range []struct {
		name    string
		err     *types.NewAPIError
		retries int
		attempt int
		want    bool
	}{
		{
			name:    "upstream 429 with budget remaining keeps retrying",
			err:     upstream(http.StatusTooManyRequests),
			retries: common.RetryTimes,
			attempt: 0,
			want:    false,
		},
		{
			name:    "upstream 400 is final even with budget remaining",
			err:     upstream(http.StatusBadRequest),
			retries: common.RetryTimes,
			attempt: 0,
			want:    true,
		},
		{
			// 回归：DecideRelayRetry 把 IsChannelError 排在预算检查之前，
			// channel:* 在预算耗尽时仍返回 retry，必须靠 attempt 判定为最终失败。
			name:    "channel no_available_key on the last attempt is final",
			err:     types.NewError(errors.New("no key"), types.ErrorCodeChannelNoAvailableKey),
			retries: 0,
			attempt: common.RetryTimes,
			want:    true,
		},
		{
			name:    "channel model_mapped_error on the last attempt is final",
			err:     types.NewError(errors.New("no mapping"), types.ErrorCodeChannelModelMappedError),
			retries: 0,
			attempt: common.RetryTimes,
			want:    true,
		},
		{
			name:    "channel error with budget remaining is not final",
			err:     types.NewError(errors.New("no key"), types.ErrorCodeChannelNoAvailableKey),
			retries: common.RetryTimes,
			attempt: 0,
			want:    false,
		},
		{
			name:    "nil error stops at the first attempt",
			attempt: 0,
			want:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			decision := DecideRelayRetry(c, tc.err, tc.retries)
			assert.Equal(t, tc.want, IsFinalAttempt(decision, tc.attempt))
		})
	}
}

func TestClaimRawRequestSnapshot(t *testing.T) {
	// 与 TestIsFinalAttempt 同理，固定 RetryTimes 才能确定地断言 attempt >= RetryTimes 的分支。
	previousRetryTimes := common.RetryTimes
	common.RetryTimes = 2
	t.Cleanup(func() { common.RetryTimes = previousRetryTimes })

	upstream := func(status int) *types.NewAPIError {
		return types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, status)
	}
	channelErr := func() *types.NewAPIError {
		return types.NewError(errors.New("no key"), types.ErrorCodeChannelNoAvailableKey)
	}
	// rawRequestStep 是在同一个 gin context 上的一次判定：err/retries 用来现场推导
	// decision，避免把重试判定硬编码成字符串而与 DecideRelayRetry 脱节。
	type rawRequestStep struct {
		err     *types.NewAPIError
		retries int
		attempt int
		want    bool
	}
	for _, tc := range []struct {
		name string
		// requests 中每个元素是一条独立请求（各自一个 gin context）上的判定序列。
		requests [][]rawRequestStep
	}{
		{
			name:     "第一次最终失败写入并占用闸门",
			requests: [][]rawRequestStep{{{err: upstream(http.StatusBadRequest), retries: common.RetryTimes, attempt: 0, want: true}}},
		},
		{
			// 回归：cross_group_retry 把计数器重置为 0，同一请求会多次满足
			// attempt >= RetryTimes，第二次必须被闸门挡住。
			name: "同一请求的第二次最终失败被抑制",
			requests: [][]rawRequestStep{{
				{err: channelErr(), retries: 0, attempt: common.RetryTimes, want: true},
				{err: channelErr(), retries: 0, attempt: common.RetryTimes, want: false},
			}},
		},
		{
			// 渠道测试探针在 controller/channel-test.go 里直接传 false（它另有
			// relayInfo == nil 兜底），等价于「非最终尝试」：不写入，也不占用闸门。
			name: "非最终尝试（含渠道测试探针）不写入且不占用闸门",
			requests: [][]rawRequestStep{{
				{err: upstream(http.StatusTooManyRequests), retries: common.RetryTimes, attempt: 0, want: false},
				{err: upstream(http.StatusTooManyRequests), retries: 0, attempt: common.RetryTimes, want: true},
			}},
		},
		{
			name: "不同请求各自独立占用闸门",
			requests: [][]rawRequestStep{
				{{err: channelErr(), retries: 0, attempt: common.RetryTimes, want: true}},
				{{err: channelErr(), retries: 0, attempt: common.RetryTimes, want: true}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for requestIndex, steps := range tc.requests {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				for stepIndex, step := range steps {
					decision := DecideRelayRetry(c, step.err, step.retries)
					assert.Equal(t, step.want, ClaimRawRequestSnapshot(c, decision, step.attempt),
						"request %d step %d", requestIndex, stepIndex)
				}
			}
		})
	}
}
