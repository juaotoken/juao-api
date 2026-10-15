package volcvideo

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

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
	raw, err := (&Adaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, common.Unmarshal(raw, &out))
	assert.Equal(t, "task_1", out["id"])
	assert.Equal(t, "completed", out["status"])
	assert.Equal(t, "doubao-seedance-2-0", out["model"])
	assert.Equal(t, map[string]any{"url": "https://x/y.mp4"}, out["metadata"])

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
