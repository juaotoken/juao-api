// Package volcvideo implements the Go-native TaskAdaptor for channel type 500
// (Volcengine Video). The upstream (https://lm.sls.cn) speaks Volcengine's
// native video-generation protocol: the submit body carries the prompt and
// any reference media inside a "content" array (there is no top-level
// "prompt" field), so this adaptor reads and forwards the raw request body
// instead of converting through the host's generic TaskSubmitReq shape.
package volcvideo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

var (
	_ channel.TaskAdaptor          = (*Adaptor)(nil)
	_ channel.OpenAIVideoConverter = (*Adaptor)(nil)
)

// maxSubmitResponseBytes bounds how much of the upstream submit response this
// adaptor will buffer in memory.
const maxSubmitResponseBytes = 1 << 20 // 1 MiB

// Adaptor is the Go-native task adaptor for Volcengine Video.
type Adaptor struct {
	requestBody map[string]any
}

// Init is a no-op: every TaskAdaptor method below receives *relaycommon.RelayInfo
// directly as a parameter, so the adaptor never needs to cache it itself.
func (a *Adaptor) Init(*relaycommon.RelayInfo) {}

// ValidateRequestAndSetAction reads the raw submit body (Volcengine-native
// shape, not the host's generic TaskSubmitReq), validates the fields that
// also act as billing inputs, and determines the task action from the
// content array.
func (a *Adaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	raw, err := storage.Bytes()
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}

	var body map[string]any
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		body, err = multipartRequestBody(c)
		if err != nil {
			return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
		}
	} else if err := common.Unmarshal(raw, &body); err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_json", http.StatusBadRequest)
	}
	// POST /v1/videos clients send the OpenAI Videos shape; convert it so
	// both entry points share the native validation below.
	body, err = normalizeOpenAIVideoRequest(body)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}

	modelName, _ := body["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		return service.TaskErrorWrapperLocal(errors.New("model is required"), "missing_model", http.StatusBadRequest)
	}

	content, ok := body["content"].([]any)
	if !ok || len(content) == 0 {
		return service.TaskErrorWrapperLocal(errors.New("content or prompt is required"), "missing_content", http.StatusBadRequest)
	}

	if rawDuration, present := body["duration"]; present {
		duration, isNumber := rawDuration.(float64)
		if !isNumber || duration != float64(int(duration)) {
			return service.TaskErrorWrapperLocal(fmt.Errorf("duration must be an integer"), "invalid_duration", http.StatusBadRequest)
		}
		d := int(duration)
		if d != -1 && (d < 1 || d > relaycommon.MaxTaskDurationSeconds) {
			return service.TaskErrorWrapperLocal(fmt.Errorf("duration must be -1 or between 1 and %d", relaycommon.MaxTaskDurationSeconds), "invalid_duration", http.StatusBadRequest)
		}
	}

	if taskErr := checkVolcAssetReferences(body, info); taskErr != nil {
		return taskErr
	}

	a.requestBody = body

	info.Action = constant.TaskActionTextToVideo
	for _, item := range content {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := part["type"].(string); t {
		case "image_url", "video_url", "audio_url":
			info.Action = constant.TaskActionImageToVideo
		}
	}

	if info.OriginModelName == "" {
		info.OriginModelName = modelName
	}

	return nil
}

// checkVolcAssetReferences enforces asset:// ownership on every submit route
// and retry attempt, independent of the pin middleware: every referenced
// logical asset must belong to the caller and to the channel serving this
// attempt, and that channel must be single-key because a logical asset lives
// in exactly one upstream account.
func checkVolcAssetReferences(body map[string]any, info *relaycommon.RelayInfo) *dto.TaskError {
	logicalIDs := model.CollectVolcAssetReferences(body)
	if len(logicalIDs) == 0 {
		return nil
	}
	if len(logicalIDs) > model.MaxVolcAssetReferences {
		return service.TaskErrorWrapperLocal(errors.New("too many asset references"), "invalid_asset_reference", http.StatusBadRequest)
	}
	if info.HasChannelMeta() && info.ChannelIsMultiKey {
		return service.TaskErrorWrapperLocal(errors.New("Volcengine assets require a single-key channel"), "invalid_asset_reference", http.StatusBadRequest)
	}
	assets, err := model.GetVolcAssetsByLogicalIDs(info.UserId, logicalIDs)
	if err != nil {
		return service.TaskErrorWrapperLocal(fmt.Errorf("load volcengine assets failed: %w", err), "database_error", http.StatusInternalServerError)
	}
	owned := make(map[string]int, len(assets))
	for _, asset := range assets {
		owned[asset.LogicalID] = asset.ChannelID
	}
	channelID := info.GetChannelID()
	for _, logicalID := range logicalIDs {
		assetChannelID, ok := owned[logicalID]
		if !ok {
			return service.TaskErrorWrapperLocal(errors.New("asset reference not found: "+logicalID), "invalid_asset_reference", http.StatusBadRequest)
		}
		if assetChannelID != channelID {
			return service.TaskErrorWrapperLocal(errors.New("referenced assets belong to another channel"), "invalid_asset_reference", http.StatusBadRequest)
		}
	}
	return nil
}

// EstimateBilling returns the token-based OtherRatio used to reserve quota at
// submit time. The host's ModelPriceHelperPerCall already reserved
// modelRatio/2 × QuotaPerUnit × groupRatio when the model has no fixed price
// (UsePrice == false); multiplying that base by token_estimate =
// estimatedTokens×2/QuotaPerUnit yields estimatedTokens × modelRatio ×
// groupRatio, matching the settlement formula in AdjustBillingOnComplete.
// Fixed-price models (UsePrice == true) skip token estimation entirely.
func (a *Adaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	if info.PriceData.UsePrice {
		return nil
	}
	resolution, _ := a.requestBody["resolution"].(string)
	seconds := requestSeconds(a.requestBody)
	tokens := estimateTokens(seconds, resolution)
	return map[string]float64{"token_estimate": tokens * 2 / common.QuotaPerUnit}
}

// AdjustBillingOnSubmit performs no submit-time adjustment: Volcengine does
// not echo billing-relevant parameters in the submit response.
func (a *Adaptor) AdjustBillingOnSubmit(info *relaycommon.RelayInfo, taskData []byte) map[string]float64 {
	return nil
}

// SettleOnComplete owns completion settlement for a successful token-billed
// task (service.TaskCompletionSettler): the final quota is the
// upstream-reported total_tokens × the model and group ratios frozen at submit
// time (task.PrivateData.BillingContext). Being authoritative matters: a zero
// result (e.g. a free group with GroupRatio 0) is final, and the host's
// RecalculateTaskQuotaByTokens fallback — which reads current ratios and
// reapplies the submit-time token_estimate — never runs. A success without a
// token count keeps the reservation. Failed tasks, per-call billing, and a
// missing billing context are not handled, so the host refunds or keeps the
// pre-charge as usual.
func (a *Adaptor) SettleOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) (int, *common.QuotaClamp, bool) {
	if task == nil || taskResult == nil || task.Status != model.TaskStatusSuccess {
		return 0, nil, false
	}
	billingContext := task.PrivateData.BillingContext
	if billingContext == nil || billingContext.PerCallBilling {
		return 0, nil, false
	}
	tokens := taskResult.TotalTokens
	if tokens <= 0 {
		return task.Quota, nil, true
	}
	if billingContext.ModelRatio <= 0 || billingContext.GroupRatio <= 0 {
		return 0, nil, true
	}
	quota, clamp := common.QuotaFromFloatChecked(float64(tokens) * billingContext.ModelRatio * billingContext.GroupRatio)
	return quota, clamp, true
}

// AdjustBillingOnComplete returns 0: settlement is owned by SettleOnComplete,
// which the host consults first, so this path never double settles.
func (a *Adaptor) AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int {
	return 0
}

// ConvertToOpenAIVideo renders a task for GET /v1/videos/:task_id. A
// successful task carries its result URL in metadata.url.
func (a *Adaptor) ConvertToOpenAIVideo(task *model.Task) ([]byte, error) {
	if task == nil {
		return nil, errors.New("task is required")
	}
	video := task.ToOpenAIVideo()
	// task.Data is the sanitized poll snapshot (see SanitizeTaskData).
	var snapshot struct {
		Duration   tokenCount `json:"duration"`
		Resolution string     `json:"resolution"`
		Ratio      string     `json:"ratio"`
	}
	if len(task.Data) > 0 {
		_ = common.Unmarshal(task.Data, &snapshot)
	}
	if snapshot.Duration > 0 {
		video.Seconds = strconv.Itoa(int(snapshot.Duration))
	}
	if snapshot.Resolution != "" {
		video.SetMetadata("resolution", snapshot.Resolution)
	}
	if snapshot.Ratio != "" {
		video.SetMetadata("ratio", snapshot.Ratio)
	}
	switch task.Status {
	case model.TaskStatusSuccess:
		if resultURL := task.GetResultURL(); resultURL != "" {
			video.SetMetadata("url", resultURL)
		}
	case model.TaskStatusFailure:
		message := task.FailReason
		if message == "" {
			message = "task failed"
		}
		video.Error = &kitdto.OpenAIVideoError{Message: message, Code: "task_failed"}
	}
	return common.Marshal(video)
}

// BuildRequestURL builds the submit URL: {base}/v1/video/generations.
func (a *Adaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.ChannelBaseUrl == "" {
		return "", errors.New("channel base url is empty")
	}
	return strings.TrimSuffix(info.ChannelBaseUrl, "/") + "/v1/video/generations", nil
}

func (a *Adaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}

// BuildRequestBody forwards the validated request body unchanged, except for
// replacing "model" with the channel's mapped upstream model name.
func (a *Adaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	body := make(map[string]any, len(a.requestBody))
	maps.Copy(body, a.requestBody)

	upstreamModel := info.UpstreamModelName
	if upstreamModel == "" {
		upstreamModel = info.OriginModelName
	}
	body["model"] = upstreamModel

	data, err := common.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body failed: %w", err)
	}
	return bytes.NewReader(data), nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

// ParseResponse reads the submit response and extracts the upstream task ID
// (task_id, falling back to id for forward compatibility).
func (a *Adaptor) ParseResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*channel.TaskSubmitResponse, *dto.TaskError) {
	body, err := readLimited(resp.Body, maxSubmitResponseBytes)
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "read_response_failed", http.StatusBadGateway)
	}

	var parsed submitResponse
	if err := common.Unmarshal(body, &parsed); err != nil {
		return nil, service.TaskErrorWrapper(err, "invalid_response", http.StatusBadGateway)
	}

	taskID := parsed.TaskID
	if taskID == "" {
		taskID = parsed.ID
	}
	if taskID == "" {
		return nil, service.TaskErrorWrapper(errors.New("upstream response missing task id"), "missing_task_id", http.StatusBadGateway)
	}

	return &channel.TaskSubmitResponse{UpstreamTaskID: taskID, TaskData: body}, nil
}

// GetModelList returns a copy of ModelList so callers cannot mutate the
// package-level slice through the returned value.
func (a *Adaptor) GetModelList() []string {
	return append([]string(nil), ModelList...)
}

func (a *Adaptor) GetChannelName() string {
	return ChannelTypeName
}

// FetchTask polls GET {base}/v1/video/generations/{task_id}.
func (a *Adaptor) FetchTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	fullURL := strings.TrimSuffix(baseURL, "/") + "/v1/video/generations/" + url.PathEscape(task.GetUpstreamTaskID())

	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// ParseTaskResult maps the Volcengine-native poll envelope to the host's
// TaskInfo shape.
func (a *Adaptor) ParseTaskResult(task *model.Task, resp *http.Response, body []byte) (*relaycommon.TaskInfo, error) {
	var envelope pollEnvelope
	if err := common.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("invalid poll response: %w", err)
	}
	if envelope.Code != "success" {
		msg := envelope.Message
		if msg == "" {
			msg = envelope.Code
		}
		return nil, fmt.Errorf("upstream poll error: %s", msg)
	}

	info := &relaycommon.TaskInfo{
		Progress: envelope.Data.Progress,
	}

	switch envelope.Data.Status {
	case "NOT_START", "SUBMITTED", "QUEUED":
		info.Status = string(model.TaskStatusQueued)
	case "IN_PROGRESS":
		info.Status = string(model.TaskStatusInProgress)
	case "SUCCESS":
		info.Status = string(model.TaskStatusSuccess)
		info.Progress = "100%"
		info.Url = envelope.Data.ResultURL
		info.TotalTokens = envelope.Data.TotalTokens.Int()
	case "FAILURE":
		info.Status = string(model.TaskStatusFailure)
		reason := envelope.Data.FailReason
		if reason == "" {
			reason = "task failed"
		}
		info.Reason = reason
	default:
		info.Status = string(model.TaskStatusUnknown)
		info.Reason = "unrecognized status: " + envelope.Data.Status
	}

	return info, nil
}

// volcTaskDataAllowlist holds the only fields from the upstream poll
// envelope's "data" object that may reach the end user through
// TaskDto.Data. The upstream (lm.sls.cn) is itself a reseller gateway, and
// its "data" object additionally carries that gateway's own account and
// billing fields (quota, user_id, group, platform, upstream_task_id,
// action, submit_time, id, properties, ...), which must never be forwarded
// to our users.
var volcTaskDataAllowlist = []string{
	"status", "progress", "result_url", "last_frame_url", "fail_reason",
	"duration", "resolution", "ratio", "model", "description",
	"total_tokens", "video_created_at", "video_updated_at",
}

// SanitizeTaskData reduces an upstream poll body to the video fields our
// users may see, dropping the upstream gateway's own billing and account
// data. Returns nil if the body does not parse as JSON or its "data" field
// is not a JSON object.
func (a *Adaptor) SanitizeTaskData(body []byte) []byte {
	var envelope map[string]any
	if err := common.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil
	}

	sanitized := make(map[string]any, len(volcTaskDataAllowlist))
	for _, key := range volcTaskDataAllowlist {
		if value, present := data[key]; present {
			sanitized[key] = value
		}
	}

	out, err := common.Marshal(sanitized)
	if err != nil {
		return nil
	}
	return out
}

// readLimited reads at most limit bytes from r, returning an error if the
// body exceeds that bound.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return data, nil
}
