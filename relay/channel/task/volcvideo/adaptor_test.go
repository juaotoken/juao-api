package volcvideo

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/taskusage"
	"github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newTestContext(t *testing.T, body map[string]any) *gin.Context {
	t.Helper()
	raw, err := common.Marshal(body)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func TestValidateRequestAndSetAction(t *testing.T) {
	textContent := []any{map[string]any{"type": "text", "text": "a cat playing piano"}}
	imageContent := []any{
		map[string]any{"type": "text", "text": "a cat playing piano"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/cat.png"}},
	}

	cases := []struct {
		name        string
		body        map[string]any
		wantErrCode string
		wantAction  string
	}{
		{
			name:       "ok text to video",
			body:       map[string]any{"model": "doubao-seedance-2-0", "content": textContent},
			wantAction: constant.TaskActionTextToVideo,
		},
		{
			name:       "ok image to video",
			body:       map[string]any{"model": "doubao-seedance-2-0", "content": imageContent},
			wantAction: constant.TaskActionImageToVideo,
		},
		{
			name:        "missing model",
			body:        map[string]any{"content": textContent},
			wantErrCode: "missing_model",
		},
		{
			name:        "missing content",
			body:        map[string]any{"model": "doubao-seedance-2-0"},
			wantErrCode: "missing_content",
		},
		{
			name:        "empty content",
			body:        map[string]any{"model": "doubao-seedance-2-0", "content": []any{}},
			wantErrCode: "missing_content",
		},
		{
			name:        "duration out of range rejected",
			body:        map[string]any{"model": "doubao-seedance-2-0", "content": textContent, "duration": 4000},
			wantErrCode: "invalid_duration",
		},
		{
			name:       "duration -1 lets model choose",
			body:       map[string]any{"model": "doubao-seedance-2-0", "content": textContent, "duration": -1},
			wantAction: constant.TaskActionTextToVideo,
		},
		{
			name:        "duration string rejected",
			body:        map[string]any{"model": "doubao-seedance-2-0", "content": textContent, "duration": "abc"},
			wantErrCode: "invalid_duration",
		},
		{
			name:        "duration fractional rejected",
			body:        map[string]any{"model": "doubao-seedance-2-0", "content": textContent, "duration": 2.5},
			wantErrCode: "invalid_duration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestContext(t, tc.body)
			info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
			a := &Adaptor{}

			taskErr := a.ValidateRequestAndSetAction(c, info)
			if tc.wantErrCode != "" {
				require.NotNil(t, taskErr)
				assert.Equal(t, tc.wantErrCode, taskErr.Code)
				return
			}
			require.Nil(t, taskErr)
			assert.Equal(t, tc.wantAction, info.Action)
		})
	}
}

func TestValidateRequestAssetOwnership(t *testing.T) {
	previousDB := model.DB
	t.Cleanup(func() { model.DB = previousDB })
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.VolcAsset{}))
	model.DB = database
	for _, asset := range []*model.VolcAsset{
		{LogicalID: "la_own", UserID: 7, ChannelID: 1},
		{LogicalID: "la_other_channel", UserID: 7, ChannelID: 2},
		{LogicalID: "la_foreign", UserID: 8, ChannelID: 1},
	} {
		require.NoError(t, asset.Insert())
	}

	withImage := func(image map[string]any) map[string]any {
		return map[string]any{"model": "doubao-seedance-2-0", "content": []any{
			map[string]any{"type": "text", "text": "a cat"},
			image,
		}}
	}
	cases := []struct {
		name      string
		body      map[string]any
		multiKey  bool
		wantError string
	}{
		{name: "owned asset on the serving channel", body: withImage(map[string]any{"type": "image_url", "image_url": map[string]any{"url": "asset://la_own"}})},
		{name: "owned asset on another channel", body: withImage(map[string]any{"type": "image_url", "image_url": map[string]any{"url": "asset://la_other_channel"}}), wantError: "referenced assets belong to another channel"},
		{name: "another user's asset", body: withImage(map[string]any{"type": "image_url", "image_url": map[string]any{"url": "asset://la_foreign"}}), wantError: "asset reference not found: la_foreign"},
		{name: "uppercase key is still checked", body: withImage(map[string]any{"type": "image_url", "IMAGE_URL": map[string]any{"URL": "Asset://la_foreign"}}), wantError: "asset reference not found: la_foreign"},
		{name: "nested object is still checked", body: withImage(map[string]any{"type": "image_url", "extra": map[string]any{"refs": []any{"asset://la_other_channel"}}}), wantError: "referenced assets belong to another channel"},
		{name: "multi-key channel rejects asset references", body: withImage(map[string]any{"type": "image_url", "image_url": map[string]any{"url": "asset://la_own"}}), multiKey: true, wantError: "single-key channel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestContext(t, tc.body)
			info := &relaycommon.RelayInfo{
				UserId:        7,
				ChannelMeta:   &relaycommon.ChannelMeta{ChannelId: 1, ChannelIsMultiKey: tc.multiKey},
				TaskRelayInfo: &relaycommon.TaskRelayInfo{},
			}
			taskErr := (&Adaptor{}).ValidateRequestAndSetAction(c, info)
			if tc.wantError == "" {
				require.Nil(t, taskErr)
				assert.Equal(t, constant.TaskActionImageToVideo, info.Action)
				return
			}
			require.NotNil(t, taskErr)
			assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
			assert.True(t, taskErr.LocalError, "ownership rejections must not be retried or penalize the channel")
			assert.Contains(t, taskErr.Message, tc.wantError)
		})
	}
}

func TestEstimateBilling(t *testing.T) {
	cases := []struct {
		name           string
		body           map[string]any
		wantSeconds    int
		wantResolution string
	}{
		{name: "720p explicit 5s", body: map[string]any{"resolution": "720p"}, wantSeconds: 5, wantResolution: "720p"},
		{name: "duration -1 means model chooses, 15s @1080p default", body: map[string]any{"duration": float64(-1)}, wantSeconds: 15, wantResolution: ""},
		{name: "absent duration and resolution default to 5s @1080p", body: map[string]any{}, wantSeconds: 5, wantResolution: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adaptor{requestBody: tc.body}
			info := &relaycommon.RelayInfo{}

			got := a.EstimateBilling(nil, info)
			require.NotNil(t, got)
			want := estimateTokens(tc.wantSeconds, tc.wantResolution) * 2 / common.QuotaPerUnit
			assert.InDelta(t, want, got["token_estimate"], 1e-9)
		})
	}

	t.Run("fixed price model skips token estimate", func(t *testing.T) {
		a := &Adaptor{requestBody: map[string]any{"resolution": "720p"}}
		info := &relaycommon.RelayInfo{}
		info.PriceData.UsePrice = true

		got := a.EstimateBilling(nil, info)
		assert.Nil(t, got)
	})
}

func TestBuildRequestBody(t *testing.T) {
	a := &Adaptor{requestBody: map[string]any{
		"model":   "doubao-seedance-2-0",
		"content": []any{map[string]any{"type": "text", "text": "hi"}},
		"ratio":   "16:9",
	}}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "doubao-seedance-2-0-fast"}}

	reader, err := a.BuildRequestBody(nil, info)
	require.NoError(t, err)
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, common.Unmarshal(raw, &got))
	assert.Equal(t, "doubao-seedance-2-0-fast", got["model"])
	assert.Equal(t, "16:9", got["ratio"])
	assert.NotNil(t, got["content"])

	// the cached request body itself is untouched (shallow copy, not a rewrite in place).
	assert.Equal(t, "doubao-seedance-2-0", a.requestBody["model"])
}

func TestBuildRequestBody_FallsBackToOriginModelName(t *testing.T) {
	a := &Adaptor{requestBody: map[string]any{
		"model":   "doubao-seedance-2-0",
		"content": []any{map[string]any{"type": "text", "text": "hi"}},
	}}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	info.OriginModelName = "doubao-seedance-2-0-mini"

	reader, err := a.BuildRequestBody(nil, info)
	require.NoError(t, err)
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, common.Unmarshal(raw, &got))
	assert.Equal(t, "doubao-seedance-2-0-mini", got["model"])
}

func newUpstreamResponse(body string) *http.Response {
	return &http.Response{Body: io.NopCloser(strings.NewReader(body))}
}

func TestParseResponse(t *testing.T) {
	a := &Adaptor{}
	info := &relaycommon.RelayInfo{}

	t.Run("prefers task_id", func(t *testing.T) {
		resp := newUpstreamResponse(`{"id":"vid_1","task_id":"vid_2","object":"video","status":"queued"}`)
		got, taskErr := a.ParseResponse(nil, resp, info)
		require.Nil(t, taskErr)
		assert.Equal(t, "vid_2", got.UpstreamTaskID)
	})

	t.Run("falls back to id", func(t *testing.T) {
		resp := newUpstreamResponse(`{"id":"vid_1","object":"video","status":"queued"}`)
		got, taskErr := a.ParseResponse(nil, resp, info)
		require.Nil(t, taskErr)
		assert.Equal(t, "vid_1", got.UpstreamTaskID)
	})

	t.Run("missing id errors", func(t *testing.T) {
		resp := newUpstreamResponse(`{"object":"video","status":"queued"}`)
		_, taskErr := a.ParseResponse(nil, resp, info)
		require.NotNil(t, taskErr)
		assert.Equal(t, "missing_task_id", taskErr.Code)
	})
}

func TestParseTaskResult(t *testing.T) {
	a := &Adaptor{}
	task := &model.Task{}

	t.Run("success carries url and tokens", func(t *testing.T) {
		body := []byte(`{"code":"success","message":"","data":{"task_id":"vid_1","status":"SUCCESS","progress":"100%","result_url":"https://x/y.mp4","fail_reason":"","total_tokens":50638}}`)
		info, err := a.ParseTaskResult(task, &http.Response{}, body)
		require.NoError(t, err)
		assert.Equal(t, string(model.TaskStatusSuccess), info.Status)
		assert.Equal(t, "https://x/y.mp4", info.Url)
		assert.Equal(t, 50638, info.TotalTokens)
		assert.Equal(t, "100%", info.Progress)
	})

	t.Run("total_tokens decodes from string, float, and rejects junk", func(t *testing.T) {
		for raw, want := range map[string]int{`"50638"`: 50638, `50638.0`: 50638, `" 12 "`: 12, `-5`: 0, `"abc"`: 0, `1e20`: 0, `null`: 0, `true`: 0} {
			body := []byte(`{"code":"success","data":{"status":"SUCCESS","result_url":"https://x/y.mp4","total_tokens":` + raw + `}}`)
			info, err := a.ParseTaskResult(task, &http.Response{}, body)
			require.NoError(t, err, raw)
			assert.Equal(t, want, info.TotalTokens, raw)
		}
	})

	t.Run("failure carries reason", func(t *testing.T) {
		body := []byte(`{"code":"success","data":{"status":"FAILURE","fail_reason":"nsfw content"}}`)
		info, err := a.ParseTaskResult(task, &http.Response{}, body)
		require.NoError(t, err)
		assert.Equal(t, string(model.TaskStatusFailure), info.Status)
		assert.Equal(t, "nsfw content", info.Reason)
	})

	t.Run("failure defaults reason when upstream omits it", func(t *testing.T) {
		body := []byte(`{"code":"success","data":{"status":"FAILURE"}}`)
		info, err := a.ParseTaskResult(task, &http.Response{}, body)
		require.NoError(t, err)
		assert.Equal(t, "task failed", info.Reason)
	})

	t.Run("queued maps to queued", func(t *testing.T) {
		body := []byte(`{"code":"success","data":{"status":"QUEUED","progress":"0%"}}`)
		info, err := a.ParseTaskResult(task, &http.Response{}, body)
		require.NoError(t, err)
		assert.Equal(t, string(model.TaskStatusQueued), info.Status)
	})

	t.Run("unrecognized status maps to unknown", func(t *testing.T) {
		body := []byte(`{"code":"success","data":{"status":"WEIRD"}}`)
		info, err := a.ParseTaskResult(task, &http.Response{}, body)
		require.NoError(t, err)
		assert.Equal(t, string(model.TaskStatusUnknown), info.Status)
		assert.Contains(t, info.Reason, "WEIRD")
	})

	t.Run("non-success code errors", func(t *testing.T) {
		body := []byte(`{"code":"task_not_exist","message":"task_not_exist","data":null}`)
		_, err := a.ParseTaskResult(task, &http.Response{}, body)
		assert.Error(t, err)
	})
}

func TestSettleOnComplete(t *testing.T) {
	cases := []struct {
		name        string
		status      model.TaskStatus
		context     *model.TaskBillingContext
		tokens      int
		wantHandled bool
		wantQuota   int
	}{
		{name: "tokens times model ratio times group ratio", status: model.TaskStatusSuccess, context: &model.TaskBillingContext{ModelRatio: 2, GroupRatio: 1.5}, tokens: 1000, wantHandled: true, wantQuota: 3000},
		{name: "free group ratio settles to zero and is final", status: model.TaskStatusSuccess, context: &model.TaskBillingContext{ModelRatio: 2, GroupRatio: 0}, tokens: 1000, wantHandled: true, wantQuota: 0},
		{name: "zero tokens keep the reservation", status: model.TaskStatusSuccess, context: &model.TaskBillingContext{ModelRatio: 2, GroupRatio: 1.5}, tokens: 0, wantHandled: true, wantQuota: 777},
		{name: "failed task is left to the host refund", status: model.TaskStatusFailure, context: &model.TaskBillingContext{ModelRatio: 2, GroupRatio: 1.5}, tokens: 1000},
		{name: "per-call billing is not handled", status: model.TaskStatusSuccess, context: &model.TaskBillingContext{ModelRatio: 2, GroupRatio: 1.5, PerCallBilling: true}, tokens: 1000},
		{name: "missing billing context is not handled", status: model.TaskStatusSuccess, tokens: 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &model.Task{Status: tc.status, Quota: 777}
			task.PrivateData.BillingContext = tc.context
			quota, clamp, handled := (&Adaptor{}).SettleOnComplete(task, &relaycommon.TaskInfo{TotalTokens: tc.tokens})
			assert.Equal(t, tc.wantHandled, handled)
			assert.Nil(t, clamp)
			if tc.wantHandled {
				assert.Equal(t, tc.wantQuota, quota)
			}
			assert.Zero(t, (&Adaptor{}).AdjustBillingOnComplete(task, &relaycommon.TaskInfo{TotalTokens: tc.tokens}), "settlement is owned by SettleOnComplete only")
		})
	}
}

func TestConvertToOpenAIVideo(t *testing.T) {
	task := &model.Task{TaskID: "task_1", Status: model.TaskStatusSuccess, Progress: "100%", FinishTime: 1700000100}
	task.Properties.OriginModelName = "doubao-seedance-2-0"
	task.PrivateData.ResultURL = "https://x/y.mp4"
	task.Data = []byte(`{"status":"SUCCESS","duration":5,"resolution":"480p","ratio":"16:9"}`)
	raw, err := (&Adaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, common.Unmarshal(raw, &out))
	assert.Equal(t, "task_1", out["id"])
	assert.Equal(t, "completed", out["status"])
	assert.Equal(t, "doubao-seedance-2-0", out["model"])
	assert.Equal(t, "5", out["seconds"])
	assert.Equal(t, map[string]any{"url": "https://x/y.mp4", "resolution": "480p", "ratio": "16:9"}, out["metadata"])

	task = &model.Task{TaskID: "task_3", Status: model.TaskStatusFailure, Progress: "100%", FailReason: "content rejected"}
	raw, err = (&Adaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	out = nil
	require.NoError(t, common.Unmarshal(raw, &out))
	assert.Equal(t, "failed", out["status"])
	assert.Equal(t, map[string]any{"message": "content rejected", "code": "task_failed"}, out["error"])

	task = &model.Task{TaskID: "task_2", Status: model.TaskStatusInProgress, Progress: "30%"}
	raw, err = (&Adaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	out = nil
	require.NoError(t, common.Unmarshal(raw, &out))
	assert.NotContains(t, out, "metadata")
	assert.EqualValues(t, 30, out["progress"])
}

func TestResolutionMaxPixels(t *testing.T) {
	cases := []struct {
		name       string
		resolution string
		wantW      int
		wantH      int
	}{
		{name: "lowercase 480p", resolution: "480p", wantW: 854, wantH: 480},
		{name: "lowercase 720p", resolution: "720p", wantW: 1280, wantH: 720},
		{name: "lowercase 1080p", resolution: "1080p", wantW: 1920, wantH: 1080},
		{name: "lowercase 4k", resolution: "4k", wantW: 3840, wantH: 2160},
		{name: "uppercase 4K normalizes like 4k", resolution: "4K", wantW: 3840, wantH: 2160},
		{name: "uppercase with whitespace 1080P normalizes like 1080p", resolution: " 1080P ", wantW: 1920, wantH: 1080},
		{name: "unknown resolution defaults to 1080p", resolution: "unknown", wantW: 1920, wantH: 1080},
		{name: "absent resolution defaults to 1080p", resolution: "", wantW: 1920, wantH: 1080},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := resolutionMaxPixels(tc.resolution)
			assert.Equal(t, tc.wantW, w)
			assert.Equal(t, tc.wantH, h)
		})
	}
}

func TestSanitizeTaskData(t *testing.T) {
	a := &Adaptor{}

	t.Run("drops upstream gateway account and billing fields", func(t *testing.T) {
		body := []byte(`{
			"code": "success",
			"message": "",
			"data": {
				"id": 42,
				"user_id": 7,
				"group": "default",
				"platform": "volcengine",
				"upstream_task_id": "vid_internal_1",
				"action": "text_to_video",
				"submit_time": 1700000000,
				"properties": {"input": "a cat"},
				"quota": 123456,
				"status": "SUCCESS",
				"progress": "100%",
				"result_url": "https://x/y.mp4",
				"last_frame_url": "https://x/last.jpg",
				"fail_reason": "",
				"duration": 5,
				"resolution": "720p",
				"ratio": "16:9",
				"model": "doubao-seedance-2-0",
				"description": "a cat playing piano",
				"total_tokens": 50638,
				"video_created_at": 1700000001,
				"video_updated_at": 1700000002
			}
		}`)

		got := a.SanitizeTaskData(body)
		require.NotNil(t, got)

		var out map[string]any
		require.NoError(t, common.Unmarshal(got, &out))

		assert.Equal(t, "https://x/y.mp4", out["result_url"])
		assert.Equal(t, float64(50638), out["total_tokens"])
		assert.Equal(t, "720p", out["resolution"])
		assert.Equal(t, "SUCCESS", out["status"])

		for _, leaked := range []string{"quota", "user_id", "group", "platform", "upstream_task_id", "action", "submit_time", "id", "properties"} {
			_, present := out[leaked]
			assert.Falsef(t, present, "sanitized output must not contain %q", leaked)
		}
	})

	t.Run("garbage body returns nil", func(t *testing.T) {
		assert.Nil(t, a.SanitizeTaskData([]byte("not json")))
	})

	t.Run("non-object data returns nil", func(t *testing.T) {
		assert.Nil(t, a.SanitizeTaskData([]byte(`{"code":"task_not_exist","data":null}`)))
	})
}

func TestValidateRequestAcceptsOpenAIVideoShape(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]any
		wantErr    string
		wantAction string
		wantBody   map[string]any
	}{
		{
			name: "prompt with metadata passthrough",
			body: map[string]any{"model": "doubao-seedance-2-0", "prompt": "太空大战", "metadata": map[string]any{
				"resolution": "480p", "ratio": "16:9", "duration": 5, "generate_audio": false, "watermark": false,
			}},
			wantAction: constant.TaskActionTextToVideo,
			wantBody: map[string]any{
				"model": "doubao-seedance-2-0", "resolution": "480p", "ratio": "16:9", "duration": float64(5),
				"generate_audio": false, "watermark": false,
				"content": []any{map[string]any{"type": "text", "text": "太空大战"}},
			},
		},
		{
			name:       "seconds, size and input_reference",
			body:       map[string]any{"model": "doubao-seedance-2-0", "prompt": "cat", "seconds": "8", "size": "1280x720", "input_reference": "https://example.com/cat.png"},
			wantAction: constant.TaskActionImageToVideo,
			wantBody: map[string]any{
				"model": "doubao-seedance-2-0", "duration": float64(8), "resolution": "720p", "ratio": "16:9",
				"content": []any{
					map[string]any{"type": "text", "text": "cat"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/cat.png"}},
				},
			},
		},
		{
			name: "metadata wins over seconds and size",
			body: map[string]any{"model": "doubao-seedance-2-0", "prompt": "cat", "seconds": 8, "size": "1920x1080",
				"metadata": map[string]any{"duration": 5, "resolution": "480p", "ratio": "9:16"}},
			wantAction: constant.TaskActionTextToVideo,
			wantBody: map[string]any{
				"model": "doubao-seedance-2-0", "duration": float64(5), "resolution": "480p", "ratio": "9:16",
				"content": []any{map[string]any{"type": "text", "text": "cat"}},
			},
		},
		{
			name:    "metadata duration still bounded",
			body:    map[string]any{"model": "doubao-seedance-2-0", "prompt": "cat", "metadata": map[string]any{"duration": 4000}},
			wantErr: "invalid_duration",
		},
		{
			name:    "fractional seconds rejected",
			body:    map[string]any{"model": "doubao-seedance-2-0", "prompt": "cat", "seconds": "2.5"},
			wantErr: "invalid_request",
		},
		{
			name:    "empty prompt without references",
			body:    map[string]any{"model": "doubao-seedance-2-0", "prompt": "  "},
			wantErr: "missing_content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestContext(t, tc.body)
			info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}, ChannelMeta: &relaycommon.ChannelMeta{}}
			a := &Adaptor{}
			taskErr := a.ValidateRequestAndSetAction(c, info)
			if tc.wantErr != "" {
				require.NotNil(t, taskErr)
				assert.Equal(t, tc.wantErr, taskErr.Code)
				return
			}
			require.Nil(t, taskErr)
			assert.Equal(t, tc.wantAction, info.Action)

			info.UpstreamModelName = "doubao-seedance-2-0"
			reader, err := a.BuildRequestBody(c, info)
			require.NoError(t, err)
			raw, err := io.ReadAll(reader)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, common.Unmarshal(raw, &got))
			assert.Equal(t, tc.wantBody, got)
		})
	}
}

func TestValidateRequestAcceptsOpenAIVideoMultipart(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("model", "doubao-seedance-2-0"))
	require.NoError(t, writer.WriteField("prompt", "太空大战"))
	require.NoError(t, writer.WriteField("metadata", `{"resolution":"480p","duration":5}`))
	require.NoError(t, writer.Close())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", &buf)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())

	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}, ChannelMeta: &relaycommon.ChannelMeta{}}
	a := &Adaptor{}
	require.Nil(t, a.ValidateRequestAndSetAction(c, info))
	assert.Equal(t, "480p", a.requestBody["resolution"])
	assert.Equal(t, float64(5), a.requestBody["duration"])
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "太空大战"}}, a.requestBody["content"])
}

// --- usage facts (tiered billing) -------------------------------------------

func TestExtractUsageFactsValidated(t *testing.T) {
	cases := []struct {
		name          string
		originModel   string
		upstreamModel string
		channelMeta   bool
		body          map[string]any
		wantRes       string
		wantVideo     string
		wantError     string
	}{
		{
			name:        "explicit tier passes through",
			originModel: "doubao-seedance-2-0",
			body:        map[string]any{"resolution": "720p", "duration": float64(5)},
			wantRes:     "720p",
			wantVideo:   "none",
		},
		{
			name:        "tier name is lowercased",
			originModel: "doubao-seedance-2-0",
			body:        map[string]any{"resolution": "720P"},
			wantRes:     "720p",
			wantVideo:   "none",
		},
		{
			name:        "WxH size maps to a tier",
			originModel: "doubao-seedance-2-0",
			body:        map[string]any{"resolution": "1920x1080"},
			wantRes:     "1080p",
			wantVideo:   "none",
		},
		{
			name:        "absent resolution reserves the highest tier up to 1080p",
			originModel: "doubao-seedance-2-0",
			body:        map[string]any{"duration": float64(5)},
			wantRes:     "1080p",
			wantVideo:   "none",
		},
		{
			name:        "absent resolution reserves the model's highest tier",
			originModel: "doubao-seedance-2-0-fast",
			body:        map[string]any{"duration": float64(5)},
			wantRes:     "720p",
			wantVideo:   "none",
		},
		{
			name:        "tier the model does not offer is rejected",
			originModel: "doubao-seedance-2-0-fast",
			body:        map[string]any{"resolution": "1080p"},
			wantError:   "doubao-seedance-2-0-fast resolution must be one of 480p, 720p",
		},
		{
			name:          "upstream model name decides the profile",
			originModel:   "seedance-alias",
			upstreamModel: "doubao-seedance-2-0-mini",
			channelMeta:   true,
			body:          map[string]any{"resolution": "720p"},
			wantRes:       "720p",
			wantVideo:     "none",
		},
		{
			name:          "upstream model name narrows an alias to a real tier",
			originModel:   "seedance-alias",
			upstreamModel: "doubao-seedance-2-0-mini",
			channelMeta:   true,
			body:          map[string]any{"resolution": "4k"},
			wantError:     "doubao-seedance-2-0-mini resolution must be one of 480p, 720p",
		},
		{
			name:        "reference video is detected by type",
			originModel: "doubao-seedance-2-0",
			body: map[string]any{"resolution": "720p", "content": []any{
				map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://x/in.mp4"}},
			}},
			wantRes:   "720p",
			wantVideo: "video",
		},
		{
			name:        "reference video is detected without an explicit type",
			originModel: "doubao-seedance-2-0",
			body: map[string]any{"resolution": "720p", "content": []any{
				map[string]any{"text": "a cat"},
				map[string]any{"video_url": map[string]any{"url": "https://x/in.mp4"}},
			}},
			wantRes:   "720p",
			wantVideo: "video",
		},
		{
			name:        "an unregistered model accepts every tier",
			originModel: "some-vendor-endpoint-id",
			body:        map[string]any{"resolution": "4k"},
			wantRes:     "4k",
			wantVideo:   "none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adaptor{requestBody: tc.body}
			info := &relaycommon.RelayInfo{
				OriginModelName: tc.originModel,
				TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
			}
			if tc.channelMeta {
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: tc.upstreamModel}
			}

			facts, err := a.ExtractUsageFactsValidated(newTestContext(t, tc.body), info)

			if tc.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantError)
				assert.Nil(t, facts)
				assert.Nil(t, info.TaskRelayInfo.LogUsage, "a rejected request must not publish a submit log")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantRes, facts["resolution"])
			assert.Equal(t, tc.wantVideo, facts["video_input"])
			tokens, ok := facts["tokens"].(float64)
			require.True(t, ok)
			assert.Greater(t, tokens, float64(0))
			require.NotNil(t, info.TaskRelayInfo.LogUsage)
			assert.Equal(t, common.QuotaRound(tokens), info.TaskRelayInfo.LogUsage.CompletionTokens)
			assert.Equal(t, requestSeconds(tc.body), info.TaskRelayInfo.LogUsage.Other["video_requested_seconds"])
		})
	}
}

func TestSubmitLogUsageCarriesRequestedVideoParameters(t *testing.T) {
	body := map[string]any{"resolution": "1080P", "ratio": "16:9", "duration": float64(8)}
	info := &relaycommon.RelayInfo{
		OriginModelName: "doubao-seedance-2-0",
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
	}
	_, err := (&Adaptor{requestBody: body}).ExtractUsageFactsValidated(newTestContext(t, body), info)
	require.NoError(t, err)
	require.NotNil(t, info.TaskRelayInfo.LogUsage)
	assert.Equal(t, 8, info.TaskRelayInfo.LogUsage.Other["video_requested_seconds"])
	assert.Equal(t, "1080p", info.TaskRelayInfo.LogUsage.Other["video_resolution"])
	assert.Equal(t, "16:9", info.TaskRelayInfo.LogUsage.Other["video_ratio"])
}

func TestExtractUsageFactsTokensFollowDurationAndTier(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]any
		wantTokens float64
	}{
		{name: "5s at 480p", body: map[string]any{"resolution": "480p", "duration": float64(5)}, wantTokens: estimateTokens(5, "480p")},
		{name: "10s at 1080p", body: map[string]any{"resolution": "1080p", "duration": float64(10)}, wantTokens: estimateTokens(10, "1080p")},
		{name: "duration -1 reserves the longest clip", body: map[string]any{"resolution": "720p", "duration": float64(-1)}, wantTokens: estimateTokens(15, "720p")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				OriginModelName: "doubao-seedance-2-0",
				TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
			}
			facts, err := (&Adaptor{requestBody: tc.body}).ExtractUsageFactsValidated(newTestContext(t, tc.body), info)
			require.NoError(t, err)
			assert.InDelta(t, tc.wantTokens, facts["tokens"], 1e-9)
		})
	}
}

func TestExtractUsageFactsWithoutTaskRelayInfo(t *testing.T) {
	body := map[string]any{"resolution": "720p"}
	info := &relaycommon.RelayInfo{OriginModelName: "doubao-seedance-2-0"}
	facts := (&Adaptor{requestBody: body}).ExtractUsageFacts(newTestContext(t, body), info)
	assert.Equal(t, "720p", facts["resolution"])
}

func TestCompletionUsageFacts(t *testing.T) {
	task := &model.Task{}
	task.Properties.UpstreamModelName = "doubao-seedance-2-0"
	task.Properties.OriginModelName = "doubao-seedance-2-0"

	t.Run("tokens and a supported resolution", func(t *testing.T) {
		facts := completionUsageFacts(task, pollData{TotalTokens: 50638, Resolution: "720P"})
		assert.Equal(t, map[string]any{"tokens": float64(50638), "resolution": "720p"}, facts)
	})

	t.Run("a tier the model does not offer is dropped", func(t *testing.T) {
		task := &model.Task{}
		task.Properties.UpstreamModelName = "doubao-seedance-2-0-fast"
		facts := completionUsageFacts(task, pollData{TotalTokens: 100, Resolution: "1080p"})
		assert.Equal(t, map[string]any{"tokens": float64(100)}, facts)
	})

	t.Run("zero tokens are dropped so the estimate stands", func(t *testing.T) {
		facts := completionUsageFacts(task, pollData{Resolution: "480p"})
		assert.Equal(t, map[string]any{"resolution": "480p"}, facts)
	})

	t.Run("nothing usable returns nil", func(t *testing.T) {
		assert.Nil(t, completionUsageFacts(task, pollData{Resolution: "8k"}))
	})

	t.Run("a nil task falls back to the shared profile", func(t *testing.T) {
		facts := completionUsageFacts(nil, pollData{TotalTokens: 7, Resolution: "4k"})
		assert.Equal(t, map[string]any{"tokens": float64(7), "resolution": "4k"}, facts)
	})
}

func TestSettleOnCompleteSkipsTieredTasks(t *testing.T) {
	task := &model.Task{Status: model.TaskStatusSuccess, Quota: 3000}
	task.PrivateData.BillingContext = &model.TaskBillingContext{
		ModelRatio:     2,
		GroupRatio:     1.5,
		TieredSnapshot: &billingexpr.BillingSnapshot{ExprString: `tier("base", u("tokens"))`},
	}
	quota, clamp, handled := (&Adaptor{}).SettleOnComplete(task, &relaycommon.TaskInfo{TotalTokens: 1000})
	assert.False(t, handled, "the frozen expression owns settlement; claiming it would settle twice")
	assert.Zero(t, quota)
	assert.Nil(t, clamp)
}

func TestTaskSettlementLog(t *testing.T) {
	task := &model.Task{SubmitTime: 1_700_000_000, FinishTime: 1_700_000_042}
	task.Data = []byte(`{"status":"SUCCESS","duration":5,"resolution":"720p","ratio":"16:9","total_tokens":50638}`)

	logUsage := (&Adaptor{}).TaskSettlementLog(task, &relaycommon.TaskInfo{TotalTokens: 50638})

	assert.Equal(t, 50638, logUsage.CompletionTokens)
	assert.Equal(t, 42, logUsage.UseTimeSeconds)
	assert.Equal(t, true, logUsage.Other["is_task"])
	assert.Equal(t, 42, logUsage.Other["video_generation_seconds"])
	assert.Equal(t, 5, logUsage.Other["video_seconds"])
	assert.Equal(t, "720p", logUsage.Other["video_resolution"])
	assert.Equal(t, "16:9", logUsage.Other["video_ratio"])
}

func TestTaskSettlementLogClampsAndOmits(t *testing.T) {
	t.Run("an absent duration or unparseable snapshot is omitted", func(t *testing.T) {
		task := &model.Task{SubmitTime: 10, FinishTime: 20, Data: []byte(`not json`)}
		logUsage := (&Adaptor{}).TaskSettlementLog(task, nil)
		assert.Zero(t, logUsage.CompletionTokens)
		assert.Equal(t, 10, logUsage.UseTimeSeconds)
		assert.NotContains(t, logUsage.Other, "video_seconds")
		assert.NotContains(t, logUsage.Other, "video_resolution")
	})

	t.Run("a negative submission window is omitted", func(t *testing.T) {
		task := &model.Task{SubmitTime: 20, FinishTime: 10, Data: []byte(`{"duration":5}`)}
		logUsage := (&Adaptor{}).TaskSettlementLog(task, &relaycommon.TaskInfo{})
		assert.Zero(t, logUsage.UseTimeSeconds)
		assert.NotContains(t, logUsage.Other, "video_generation_seconds")
	})

	t.Run("an absurd duration is clamped away", func(t *testing.T) {
		task := &model.Task{Data: []byte(`{"duration":99999}`)}
		logUsage := (&Adaptor{}).TaskSettlementLog(task, nil)
		assert.NotContains(t, logUsage.Other, "video_seconds")
	})

	t.Run("a nil task still reports the task marker", func(t *testing.T) {
		logUsage := (&Adaptor{}).TaskSettlementLog(nil, nil)
		assert.Equal(t, map[string]any{"is_task": true}, logUsage.Other)
	})
}

func TestTokenCountDecoding(t *testing.T) {
	for raw, want := range map[string]int{
		`50638`: 50638, `50638.0`: 50638, `"50638"`: 50638, `" 12 "`: 12,
		`-5`: 0, `"abc"`: 0, `1e20`: 0, `null`: 0, `true`: 0, `2147483648`: 0,
	} {
		var count tokenCount
		require.NoError(t, common.Unmarshal([]byte(raw), &count), raw)
		assert.Equal(t, want, count.Int(), raw)
	}
}

func TestRegisteredUsageSchema(t *testing.T) {
	for _, modelName := range ModelList {
		t.Run(modelName, func(t *testing.T) {
			schema, examples, native := taskusage.ForModel(modelName)
			require.True(t, native, "the adaptor must declare a usage schema for %s", modelName)

			// Keys mirror the doubao plugin's Seedance 2.0 profile so task
			// expressions stay interchangeable between the two.
			assert.Equal(t, []string{"resolution", "tokens", "video_input"}, sortedKeys(schema))
			assert.Equal(t, "token", schema["tokens"].Unit)
			assert.ElementsMatch(t, []string{"none", "video"}, schema["video_input"].Enum)
			assert.ElementsMatch(t, seedanceResolutions[modelName], schema["resolution"].Enum)
			assert.Equal(t, "计费 Token 单价", schema["tokens"].Description["zh"])
			assert.Equal(t, "输出视频分辨率", schema["resolution"].Description["zh"])
			assert.Equal(t, "参考视频输入", schema["video_input"].Description["zh"])

			require.NotEmpty(t, examples)
			for _, example := range examples {
				assert.NotEmpty(t, example.Label)
				assert.Contains(t, example.Facts, "tokens")
				assert.Contains(t, example.Facts, "resolution")
				assert.Contains(t, example.Facts, "video_input")
			}
		})
	}
}

// TestEveryModelAcceptsTieredPricing pins the regression behind the unusable
// pricing editor entry: a model the channel exposes but the adaptor does not
// declare has no usage schema, and saving any tiered expression for it fails
// with "expression references usage keys ... but the model has no task plugin
// usage schema". The upstream serves Seedance under its suffix-free name, so
// that exact name must be declared; every declared name must validate.
func TestEveryModelAcceptsTieredPricing(t *testing.T) {
	// The upstream's /v1/models returns Seedance without the Ark endpoint
	// suffix the doubao plugin carries, so the channel exposes these names.
	assert.Contains(t, ModelList, "doubao-seedance-2-5")
	for _, modelName := range ModelList {
		t.Run(modelName, func(t *testing.T) {
			schema, _, native := taskusage.ForModel(modelName)
			require.True(t, native, "no usage schema registered for %s", modelName)
			for _, expression := range []string{
				`tier("base", u("tokens") * 46 / 1000000)`,
				`u("resolution") == "480p" && u("video_input") == "none" ? tier("480p·none", u("tokens") * 46 / 1000000) : tier("other", u("tokens") * 28 / 1000000)`,
			} {
				assert.NoError(t, billing_setting.SmokeTestTaskExpr(expression, schema), modelName)
			}
		})
	}
}

// sortedKeys keeps the assertion stable regardless of map iteration order.
func sortedKeys(schema map[string]jsplugin.UsageFieldSchema) []string {
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestUsageSchemaMutationsDoNotLeak(t *testing.T) {
	schema, examples, native := taskusage.ForModel("doubao-seedance-2-0")
	require.True(t, native)
	require.NotEmpty(t, schema)

	schema["tokens"] = jsplugin.UsageFieldSchema{Unit: "mutated"}
	examples[0].Facts["tokens"] = "mutated"

	fresh, freshExamples, _ := taskusage.ForModel("doubao-seedance-2-0")
	assert.Equal(t, "token", fresh["tokens"].Unit)
	assert.NotEqual(t, "mutated", freshExamples[0].Facts["tokens"])
}

// TestUsageSchemaMirrorsDoubaoPlugin pins the Go-native Seedance 2.0 usage
// profile to the doubao task plugin's, so one task expression prices the model
// identically whichever of the two serves it.
func TestUsageSchemaMirrorsDoubaoPlugin(t *testing.T) {
	source, err := plugins.Source("doubao")
	require.NoError(t, err)

	// The plugin declares the same capability profile this adaptor does. The
	// plugin keys carry the Ark endpoint suffix (e.g. -260128), so match the
	// client-facing name as a prefix of the quoted capability key.
	lines := strings.Split(source, "\n")
	for name, resolutions := range seedanceResolutions {
		line := ""
		for _, candidate := range lines {
			trimmed := strings.TrimSpace(candidate)
			if strings.HasPrefix(trimmed, `"`+name) && strings.Contains(trimmed, "{ resolutions: [") {
				line = trimmed
				break
			}
		}
		require.NotEmptyf(t, line, "the doubao plugin must declare a profile for %s", name)
		for _, resolution := range resolutions {
			assert.Contains(t, line, `"`+resolution+`"`, name)
		}
		assert.Contains(t, line, "videoInput: true", name)
	}
	// Keys and localized labels must match for expressions to be portable.
	for _, snippet := range []string{
		"tokens: {",
		`unit: "token"`,
		`description: { en: "Billing token unit price", zh: "计费 Token 单价" }`,
		`description: { en: "Output video resolution", zh: "输出视频分辨率" }`,
		`description: { en: "Reference video input", zh: "参考视频输入" }`,
		`enum: ["none", "video"]`,
		`{ en: "No reference video", zh: "无参考视频" }`,
		`{ en: "With reference video", zh: "有参考视频" }`,
	} {
		assert.Contains(t, source, snippet)
	}
	// The token estimate formula and pixel table must agree.
	assert.Contains(t, source, "(seconds * dims[0] * dims[1] * 24) / 1024")
	for resolution, want := range map[string][2]int{"480p": {854, 480}, "720p": {1280, 720}, "1080p": {1920, 1080}, "4k": {3840, 2160}} {
		width, height := resolutionMaxPixels(resolution)
		assert.Equal(t, want, [2]int{width, height}, resolution)
	}
}

// TestDefaultResolutionMirrorsDoubaoPlugin pins the reservation default: the
// model's highest tier up to 1080p, matching the plugin's videoResolution.
func TestDefaultResolutionMirrorsDoubaoPlugin(t *testing.T) {
	for model, want := range map[string]string{
		"doubao-seedance-2-0":      "1080p",
		"doubao-seedance-2-0-fast": "720p",
		"doubao-seedance-2-0-mini": "720p",
		"doubao-seedance-2-5":      "1080p",
	} {
		body := map[string]any{"duration": float64(5)}
		info := &relaycommon.RelayInfo{
			OriginModelName: model,
			TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
		}
		facts, err := (&Adaptor{requestBody: body}).ExtractUsageFactsValidated(newTestContext(t, body), info)
		require.NoError(t, err, model)
		assert.Equal(t, want, facts["resolution"], model)
	}
}
