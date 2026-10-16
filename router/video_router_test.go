package router

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetOpenAIVideoRouteRendersJimengTask(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousSQLitePath := common.SQLitePath
	previousMasterNode := common.IsMasterNode
	previousRedisEnabled := common.RedisEnabled
	common.SQLitePath = t.TempDir() + "/router-video.db"
	common.IsMasterNode = false
	common.RedisEnabled = false
	t.Setenv("SQL_DSN", "")
	require.NoError(t, model.InitDB())
	database := model.DB
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Task{}))
	t.Cleanup(func() {
		sqlDB, closeErr := database.DB()
		require.NoError(t, closeErr)
		require.NoError(t, sqlDB.Close())
		model.DB = previousDB
		common.SetDatabaseTypes(previousDatabaseType, previousLogDatabaseType)
		common.SQLitePath = previousSQLitePath
		common.IsMasterNode = previousMasterNode
		common.RedisEnabled = previousRedisEnabled
	})

	require.NoError(t, database.Create(&model.User{
		Id:          91,
		Username:    "jimeng-fetch-user",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       100,
		Group:       "default",
		AuthVersion: 1,
	}).Error)
	require.NoError(t, database.Create(&model.Token{
		Id:             1,
		UserId:         91,
		Key:            "jimengfetch",
		Status:         common.TokenStatusEnabled,
		Name:           "jimeng fetch",
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)
	require.NoError(t, database.Create(&model.Channel{
		Id:     17,
		Type:   constant.ChannelTypeJimeng,
		Key:    "unused",
		Status: common.ChannelStatusEnabled,
		Name:   "jimeng fetch",
		Models: "jimeng_vgfm_t2v_l20",
		Group:  "default",
	}).Error)

	task := &model.Task{
		CreatedAt: 1710000000,
		UpdatedAt: 1710000060,
		TaskID:    "task_jimeng_public",
		Platform:  constant.TaskPlatform("jimeng"),
		UserId:    91,
		Group:     "default",
		ChannelId: 17,
		Status:    model.TaskStatusSuccess,
		Progress:  "100%",
		PrivateData: model.TaskPrivateData{
			ResultURL: "data:video/mp4;base64,ZGF0YQ==",
		},
	}
	task.SetData(map[string]any{
		"code": 10000,
		"data": map[string]any{
			"status":    "done",
			"task_id":   "jimeng-private-1",
			"video_url": "https://cdn.example/video.mp4",
		},
		"message": "success",
	})
	require.NoError(t, database.Create(task).Error)

	engine := gin.New()
	SetVideoRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	request := httptest.NewRequest(http.MethodGet, "/v1/videos/task_jimeng_public", nil)
	request.Header.Set("Authorization", "Bearer sk-jimengfetch")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Status      string `json:"status"`
		Progress    int    `json:"progress"`
		CreatedAt   int64  `json:"created_at"`
		CompletedAt int64  `json:"completed_at"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "task_jimeng_public", response.ID)
	assert.Equal(t, "video", response.Object)
	assert.Equal(t, "completed", response.Status)
	assert.Equal(t, 100, response.Progress)
	assert.Equal(t, int64(1710000000), response.CreatedAt)
	assert.Equal(t, int64(1710000060), response.CompletedAt)
	assert.NotContains(t, recorder.Body.String(), "cdn.example")
	assert.NotContains(t, recorder.Body.String(), "jimeng-private-1")

	for _, testCase := range []struct {
		name          string
		authorization string
		query         string
		wantStatus    int
	}{
		{name: "missing credential rejected", wantStatus: http.StatusUnauthorized},
		{name: "access rejected", query: "?access=not-a-video-credential", wantStatus: http.StatusUnauthorized},
		{name: "bearer accepted", authorization: "Bearer sk-jimengfetch", wantStatus: http.StatusOK},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodGet,
				"/v1/videos/task_jimeng_public/content"+testCase.query,
				nil,
			)
			if testCase.authorization != "" {
				request.Header.Set("Authorization", testCase.authorization)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, testCase.wantStatus, recorder.Code, recorder.Body.String())
			if testCase.wantStatus == http.StatusOK {
				assert.Equal(t, "data", recorder.Body.String())
			}
		})
	}
}

func TestVolcVideoListRoute(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}))

	users := []model.User{
		{Username: "volc-list-caller", AffCode: "volc-list-caller", Status: common.UserStatusEnabled, Group: "default", Quota: 100},
		{Username: "volc-list-other", AffCode: "volc-list-other", Status: common.UserStatusEnabled, Group: "default", Quota: 100},
	}
	for index := range users {
		require.NoError(t, model.DB.Create(&users[index]).Error)
	}
	caller, other := users[0], users[1]
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:             caller.Id,
		Key:                "volclistkey",
		Status:             common.TokenStatusEnabled,
		ExpiredTime:        -1,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: true,
		ModelLimits:        "doubao-seedance-2-0",
	}).Error)
	volcPlatform := constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeVolcVideo))
	for _, task := range []*model.Task{
		{TaskID: "task_volc_1", Platform: volcPlatform, UserId: caller.Id, ChannelId: 41, Status: model.TaskStatusSuccess},
		{TaskID: "task_volc_other", Platform: volcPlatform, UserId: other.Id, ChannelId: 41, Status: model.TaskStatusSuccess},
		{TaskID: "task_volc_2", Platform: volcPlatform, UserId: caller.Id, ChannelId: 41, Status: model.TaskStatusInProgress},
		{TaskID: "task_jimeng", Platform: constant.TaskPlatform("jimeng"), UserId: caller.Id, ChannelId: 17, Status: model.TaskStatusSuccess},
		{TaskID: "task_volc_3", Platform: volcPlatform, UserId: caller.Id, ChannelId: 41, Status: model.TaskStatusSuccess},
	} {
		require.NoError(t, model.DB.Create(task).Error)
	}

	engine := gin.New()
	SetVideoRouter(engine)
	SetVolcAssetRouter(engine)

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantIDs    []string
		wantTotal  int64
	}{
		{name: "newest first by default", wantStatus: http.StatusOK, wantIDs: []string{"task_volc_3", "task_volc_2", "task_volc_1"}, wantTotal: 3},
		{name: "ascending page", query: "?order=asc&page_num=2&page_size=2", wantStatus: http.StatusOK, wantIDs: []string{"task_volc_3"}, wantTotal: 3},
		{name: "status filter", query: "?status=SUCCESS", wantStatus: http.StatusOK, wantIDs: []string{"task_volc_3", "task_volc_1"}, wantTotal: 2},
		{name: "invalid status", query: "?status=done", wantStatus: http.StatusBadRequest},
		{name: "invalid order", query: "?order=up", wantStatus: http.StatusBadRequest},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/video/generations"+testCase.query, nil)
			request.Header.Set("Authorization", "Bearer sk-volclistkey")
			recorder := httptest.NewRecorder()

			engine.ServeHTTP(recorder, request)

			require.Equal(t, testCase.wantStatus, recorder.Code, recorder.Body.String())
			if testCase.wantStatus != http.StatusOK {
				return
			}
			var response struct {
				Object string `json:"object"`
				Data   []struct {
					TaskID    string `json:"task_id"`
					ChannelID int    `json:"channel_id"`
				} `json:"data"`
				Total int64 `json:"total"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, "list", response.Object)
			assert.Equal(t, testCase.wantTotal, response.Total)
			ids := make([]string, 0, len(response.Data))
			for _, item := range response.Data {
				ids = append(ids, item.TaskID)
				assert.Zero(t, item.ChannelID)
			}
			assert.Equal(t, testCase.wantIDs, ids)
		})
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/volcengine/assets", strings.NewReader(`{}`))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code, recorder.Body.String())
}
