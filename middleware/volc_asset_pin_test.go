package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolcAssetChannelPin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	previousDB := model.DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	previousSQLitePath := common.SQLitePath
	previousMasterNode := common.IsMasterNode
	previousRedis := common.RedisEnabled
	previousMemoryCache := common.MemoryCacheEnabled
	common.SQLitePath = t.TempDir() + "/volc-asset-pin.db"
	common.IsMasterNode = false
	common.RedisEnabled = false
	common.MemoryCacheEnabled = false
	t.Setenv("SQL_DSN", "")
	require.NoError(t, model.InitDB())
	database := model.DB
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.VolcAsset{}))
	t.Cleanup(func() {
		if sqlDB, err := database.DB(); err == nil {
			_ = sqlDB.Close()
		}
		model.DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		common.SQLitePath = previousSQLitePath
		common.IsMasterNode = previousMasterNode
		common.RedisEnabled = previousRedis
		common.MemoryCacheEnabled = previousMemoryCache
	})

	const modelName = "doubao-seedance-2-0"
	channelA := &model.Channel{Type: constant.ChannelTypeVolcVideo, Key: "a", Status: common.ChannelStatusEnabled, Name: "volc-a", Models: modelName, Group: "default"}
	channelB := &model.Channel{Type: constant.ChannelTypeVolcVideo, Key: "b", Status: common.ChannelStatusEnabled, Name: "volc-b", Models: modelName, Group: "default"}
	channelVIP := &model.Channel{Type: constant.ChannelTypeVolcVideo, Key: "v", Status: common.ChannelStatusEnabled, Name: "volc-vip", Models: modelName, Group: "vip"}
	channelMulti := &model.Channel{Type: constant.ChannelTypeVolcVideo, Key: "m1\nm2", Status: common.ChannelStatusEnabled, Name: "volc-multi", Models: modelName, Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2}}
	for _, channel := range []*model.Channel{channelA, channelB, channelVIP, channelMulti} {
		require.NoError(t, database.Create(channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
	}
	for _, asset := range []*model.VolcAsset{
		{LogicalID: "asset-a1", UserID: 7, ChannelID: channelA.Id},
		{LogicalID: "asset-a2", UserID: 7, ChannelID: channelA.Id},
		{LogicalID: "asset-b1", UserID: 7, ChannelID: channelB.Id},
		{LogicalID: "asset-vip", UserID: 7, ChannelID: channelVIP.Id},
		{LogicalID: "asset-foreign", UserID: 8, ChannelID: channelA.Id},
		{LogicalID: "asset-multi", UserID: 7, ChannelID: channelMulti.Id},
	} {
		require.NoError(t, asset.Insert())
	}

	body := func(urls ...string) string {
		content := []map[string]any{{"type": "text", "text": "a cat"}}
		for _, url := range urls {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
		data, err := common.Marshal(map[string]any{"model": modelName, "content": content})
		require.NoError(t, err)
		return string(data)
	}

	tests := []struct {
		name       string
		body       string
		modelLimit map[string]bool
		wantStatus int
		wantPin    int
		wantError  string
	}{
		{name: "no asset reference leaves routing alone", body: body("https://cdn.example/cat.png"), wantStatus: http.StatusOK},
		{name: "owned assets pin their channel", body: body("asset://asset-a1", "asset://asset-a2", "asset://asset-a1"), wantStatus: http.StatusOK, wantPin: channelA.Id},
		{name: "video references are checked too", body: `{"model":"` + modelName + `","content":[{"type":"video_url","video_url":{"url":"asset://asset-foreign"}}]}`, wantStatus: http.StatusBadRequest, wantError: "asset reference not found: asset-foreign"},
		{name: "another user's asset is rejected", body: body("asset://asset-a1", "asset://asset-foreign"), wantStatus: http.StatusBadRequest, wantError: "asset reference not found: asset-foreign"},
		{name: "assets on two channels are rejected", body: body("asset://asset-a1", "asset://asset-b1"), wantStatus: http.StatusBadRequest, wantError: "referenced assets belong to different channels"},
		{name: "uppercase key reference is pinned", body: `{"model":"` + modelName + `","content":[{"type":"image_url","IMAGE_URL":{"URL":"ASSET://asset-a1"}}]}`, wantStatus: http.StatusOK, wantPin: channelA.Id},
		{name: "nested foreign reference under a variant key is rejected", body: `{"model":"` + modelName + `","content":[{"type":"text","text":"x"}],"Extra":{"refs":["asset://asset-foreign"]}}`, wantStatus: http.StatusBadRequest, wantError: "asset reference not found: asset-foreign"},
		{name: "assets on a multi-key channel are rejected", body: body("asset://asset-multi"), wantStatus: http.StatusBadRequest, wantError: "single-key channel"},
		{name: "token model limit still applies", body: body("asset://asset-a1"), modelLimit: map[string]bool{"other-model": true}, wantStatus: http.StatusForbidden},
		{name: "channel outside the caller's group is rejected", body: body("asset://asset-vip"), wantStatus: http.StatusForbidden, wantError: "not available for your group/model"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			pinnedChannel := 0
			nextCalled := false
			engine := gin.New()
			engine.POST("/v1/video/generations", func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUserId, 7)
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				if testCase.modelLimit != nil {
					common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
					common.SetContextKey(c, constant.ContextKeyTokenModelLimit, testCase.modelLimit)
				}
				c.Next()
			}, VolcAssetChannelPin(), func(c *gin.Context) {
				nextCalled = true
				if pin, found, _ := service.GetChannelConstraints(c).ResolvedPin(); found {
					pinnedChannel = pin.ChannelId
					assert.Equal(t, dto.PinRetrySameChannel, pin.RetryMode)
					assert.Equal(t, dto.PinSourceVolcAsset, pin.Source)
				}
				storage, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				replayed, err := storage.Bytes()
				require.NoError(t, err)
				assert.Equal(t, testCase.body, string(replayed))
				c.Status(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/video/generations", bytes.NewBufferString(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			engine.ServeHTTP(recorder, request)

			require.Equal(t, testCase.wantStatus, recorder.Code, recorder.Body.String())
			assert.Equal(t, testCase.wantStatus == http.StatusOK, nextCalled)
			assert.Equal(t, testCase.wantPin, pinnedChannel)
			if testCase.wantError != "" {
				assert.Contains(t, recorder.Body.String(), testCase.wantError)
			}
		})
	}
}
