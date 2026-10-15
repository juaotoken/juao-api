package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type volcUpstreamCall struct {
	Method string
	Path   string
	Auth   string
	Body   string
}

type volcAssetFixture struct {
	mu          sync.Mutex
	calls       []volcUpstreamCall
	channelID   int
	upstreamURL string
}

func (f *volcAssetFixture) takeCalls() []volcUpstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func setupVolcAssetFixture(t *testing.T) *volcAssetFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMemoryCache, previousRedis := common.MemoryCacheEnabled, common.RedisEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.VolcAsset{}, &model.VolcAssetGroup{}))
	model.DB, model.LOG_DB = database, database
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.MemoryCacheEnabled, common.RedisEnabled = previousMemoryCache, previousRedis
	})

	f := &volcAssetFixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, volcUpstreamCall{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/volcengine/assets":
			// Echo the forwarded group_name like the upstream does.
			var req struct {
				GroupName string `json:"group_name"`
			}
			_ = common.Unmarshal(body, &req)
			groupName, _ := common.Marshal(req.GroupName)
			_, _ = io.WriteString(w, `{"success":true,"message":"ok","data":{"logical_id":"la_new","logical_group_id":"lag_y","group_name":`+string(groupName)+`,"status":"Processing","name":"n","asset_type":"Image","group_id":"group_volc123"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/volcengine/assets/la_a":
			_, _ = io.WriteString(w, `{"success":true,"data":{"logical_id":"la_a","status":"Active","group_name":"u1-mine"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/volcengine/assets/la_a":
			_, _ = io.WriteString(w, `{"success":true,"message":"Asset deleted successfully"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/volcengine/assets/groups/by-byted-token" && strings.Contains(string(body), `"good"`):
			_, _ = io.WriteString(w, `{"success":true,"data":{"group_id":"group_person1"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/volcengine/assets/groups/by-byted-token":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"invalid byted token","success":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	channel := model.Channel{
		Type:    constant.ChannelTypeVolcVideo,
		Name:    "volc-video",
		Key:     "sk-up",
		BaseURL: &upstream.URL,
		Group:   "default",
		Status:  common.ChannelStatusEnabled,
	}
	require.NoError(t, database.Create(&channel).Error)
	f.channelID = channel.Id
	f.upstreamURL = upstream.URL
	return f
}

func serveVolcAsset(userID int, group, method, target, body string) *httptest.ResponseRecorder {
	engine := gin.New()
	auth := func(c *gin.Context) {
		c.Set("id", userID)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
	}
	assets := engine.Group("/v1/volcengine/assets", auth)
	assets.POST("", VolcAssetCreate)
	assets.GET("", VolcAssetList)
	assets.GET("/:logical_id", VolcAssetDetail)
	assets.DELETE("/:logical_id", VolcAssetDelete)
	assets.POST("/auth-sessions", VolcAssetAuthSession)
	assets.POST("/groups/by-byted-token", VolcAssetGroupByBytedToken)

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func TestVolcAssetCreateRecordsOwnership(t *testing.T) {
	f := setupVolcAssetFixture(t)

	resp := serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image"}`)
	require.Equal(t, http.StatusOK, resp.Code)
	assert.Contains(t, resp.Body.String(), `"logical_id":"la_new"`)
	calls := f.takeCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, volcUpstreamCall{Method: http.MethodPost, Path: "/v1/volcengine/assets", Auth: "Bearer sk-up", Body: calls[0].Body}, calls[0])
	assert.JSONEq(t, `{"source_url":"https://x/a.png","asset_type":"Image","group_name":"u1-default"}`, calls[0].Body, "auto grouping is pinned to the caller's namespace")

	asset, err := model.GetVolcAsset(1, "la_new")
	require.NoError(t, err)
	require.NotNil(t, asset)
	assert.Equal(t, f.channelID, asset.ChannelID)
	assert.Equal(t, "lag_y", asset.LogicalGroupID)
	assert.Equal(t, "group_volc123", asset.GroupID)
	assert.Equal(t, "Processing", asset.Status)

	for _, body := range []string{
		`{"source_url":"https://x/a.png","asset_type":"Text"}`,
		`{"asset_type":"Image"}`,
		`{"source_url":"https://x/a.png","asset_type":"Image","group_id":"g1","group_name":"n"}`,
	} {
		resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", body)
		assert.Equal(t, http.StatusBadRequest, resp.Code, body)
	}
	assert.Empty(t, f.takeCalls())
}

func TestVolcAssetListIsOwnerScoped(t *testing.T) {
	f := setupVolcAssetFixture(t)
	for _, a := range []*model.VolcAsset{
		{LogicalID: "la_a1", UserID: 1, ChannelID: f.channelID, LogicalGroupID: "lag_1"},
		{LogicalID: "la_a2", UserID: 1, ChannelID: f.channelID, LogicalGroupID: "lag_2"},
		{LogicalID: "la_b1", UserID: 2, ChannelID: f.channelID, LogicalGroupID: "lag_1"},
	} {
		require.NoError(t, a.Insert())
	}

	resp := serveVolcAsset(1, "default", http.MethodGet, "/v1/volcengine/assets?page_size=0", "")
	require.Equal(t, http.StatusOK, resp.Code)
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Items []map[string]any `json:"items"`
			Total int64            `json:"total"`
			Page  int              `json:"page"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(resp.Body.Bytes(), &out))
	assert.True(t, out.Success)
	assert.EqualValues(t, 2, out.Data.Total)
	require.Len(t, out.Data.Items, 1, "page_size clamps to 1")
	assert.Equal(t, "la_a2", out.Data.Items[0]["logical_id"])
	assert.NotContains(t, out.Data.Items[0], "channel_id")
	assert.NotContains(t, out.Data.Items[0], "user_id")

	resp = serveVolcAsset(1, "default", http.MethodGet, "/v1/volcengine/assets?group_id=lag_1", "")
	require.NoError(t, common.Unmarshal(resp.Body.Bytes(), &out))
	require.Len(t, out.Data.Items, 1)
	assert.Equal(t, "la_a1", out.Data.Items[0]["logical_id"])
	assert.Empty(t, f.takeCalls())
}

func TestVolcAssetDetailAndDeleteRequireOwnership(t *testing.T) {
	f := setupVolcAssetFixture(t)
	require.NoError(t, (&model.VolcAsset{LogicalID: "la_a", UserID: 1, ChannelID: f.channelID, Status: "Processing"}).Insert())

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		resp := serveVolcAsset(2, "default", method, "/v1/volcengine/assets/la_a", "")
		assert.Equal(t, http.StatusNotFound, resp.Code, method)
		assert.JSONEq(t, `{"success":false,"message":"素材不存在"}`, resp.Body.String())
	}
	assert.Empty(t, f.takeCalls())

	resp := serveVolcAsset(1, "default", http.MethodGet, "/v1/volcengine/assets/la_a", "")
	require.Equal(t, http.StatusOK, resp.Code)
	assert.JSONEq(t, `{"success":true,"data":{"logical_id":"la_a","status":"Active","group_name":"mine"}}`, resp.Body.String(), "the caller's namespace prefix is stripped")
	asset, err := model.GetVolcAsset(1, "la_a")
	require.NoError(t, err)
	assert.Equal(t, "Active", asset.Status)

	resp = serveVolcAsset(1, "default", http.MethodDelete, "/v1/volcengine/assets/la_a", "")
	require.Equal(t, http.StatusOK, resp.Code)
	calls := f.takeCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, http.MethodDelete, calls[1].Method)
	assert.Equal(t, "Bearer sk-up", calls[1].Auth)
	asset, err = model.GetVolcAsset(1, "la_a")
	require.NoError(t, err)
	assert.Nil(t, asset)
}

func TestVolcAssetPassthroughAndChannelSelection(t *testing.T) {
	f := setupVolcAssetFixture(t)

	resp := serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets/groups/by-byted-token", `{"byted_token":"bt"}`)
	assert.Equal(t, http.StatusInternalServerError, resp.Code)
	assert.JSONEq(t, `{"message":"invalid byted token","success":false}`, resp.Body.String())
	resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets/groups/by-byted-token", `{}`)
	assert.Equal(t, http.StatusBadRequest, resp.Code)

	resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets/auth-sessions", "")
	assert.Equal(t, http.StatusNotFound, resp.Code, "fake upstream has no auth-sessions route; status is passed through")
	calls := f.takeCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, `{"byted_token":"bt"}`, calls[0].Body)
	assert.Equal(t, `{}`, calls[1].Body)

	resp = serveVolcAsset(1, "vip", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image"}`)
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	assert.JSONEq(t, `{"success":false,"message":"no available Volcengine Video channel"}`, resp.Body.String())

	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", f.channelID).Update("status", common.ChannelStatusManuallyDisabled).Error)
	resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets/auth-sessions", `{}`)
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	require.NoError(t, (&model.VolcAsset{LogicalID: "la_a", UserID: 1, ChannelID: f.channelID}).Insert())
	resp = serveVolcAsset(1, "default", http.MethodGet, "/v1/volcengine/assets/la_a", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code, "owning channel disabled")
	assert.Empty(t, f.takeCalls())
}

func TestVolcAssetCreateIsolatesGroupsPerUser(t *testing.T) {
	f := setupVolcAssetFixture(t)

	resp := serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image","group_name":"cats","extra":1}`)
	require.Equal(t, http.StatusOK, resp.Code)
	assert.Contains(t, resp.Body.String(), `"group_name":"cats"`)
	calls := f.takeCalls()
	require.Len(t, calls, 1)
	assert.JSONEq(t, `{"source_url":"https://x/a.png","asset_type":"Image","group_name":"u1-cats","extra":1}`, calls[0].Body)
	asset, err := model.GetVolcAsset(1, "la_new")
	require.NoError(t, err)
	require.NotNil(t, asset)
	assert.Equal(t, "cats", asset.GroupName)
	for _, groupID := range []string{"group_volc123", "lag_y"} {
		group, err := model.GetVolcAssetGroup(1, groupID)
		require.NoError(t, err)
		require.NotNil(t, group, groupID)
		assert.Equal(t, f.channelID, group.ChannelID)
	}
	require.NoError(t, model.DeleteVolcAsset(asset.ID))

	resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image","name":" dog "}`)
	require.Equal(t, http.StatusOK, resp.Code)
	calls = f.takeCalls()
	require.Len(t, calls, 1)
	assert.JSONEq(t, `{"source_url":"https://x/a.png","asset_type":"Image","name":" dog ","group_name":"u1-dog"}`, calls[0].Body)
	assert.Contains(t, resp.Body.String(), `"group_name":"dog"`)

	// Another user cannot create into a group it does not own, even one that
	// exists for someone else.
	resp = serveVolcAsset(2, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image","group_id":"group_volc123"}`)
	assert.Equal(t, http.StatusBadRequest, resp.Code)
	assert.JSONEq(t, `{"success":false,"message":"unknown group_id"}`, resp.Body.String())
	assert.Empty(t, f.takeCalls())
}

func TestVolcAssetCreateRoutesOwnedGroupToItsChannel(t *testing.T) {
	f := setupVolcAssetFixture(t)
	upstreamURL := f.upstreamURL
	other := model.Channel{Type: constant.ChannelTypeVolcVideo, Name: "volc-other", Key: "sk-other", BaseURL: &upstreamURL, Group: "default", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(&other).Error)

	resp := serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets/groups/by-byted-token", `{"byted_token":"good"}`)
	require.Equal(t, http.StatusOK, resp.Code)
	calls := f.takeCalls()
	require.Len(t, calls, 1)
	group, err := model.GetVolcAssetGroup(1, "group_person1")
	require.NoError(t, err)
	require.NotNil(t, group)
	tokenChannelID, tokenKey := f.channelID, "Bearer sk-up"
	if calls[0].Auth == "Bearer sk-other" {
		tokenChannelID, tokenKey = other.Id, "Bearer sk-other"
	}
	assert.Equal(t, tokenChannelID, group.ChannelID)

	for range 8 {
		resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image","group_id":"group_person1"}`)
		require.Equal(t, http.StatusOK, resp.Code)
		calls = f.takeCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, tokenKey, calls[0].Auth, "create into an owned group uses that group's channel")
		assert.JSONEq(t, `{"source_url":"https://x/a.png","asset_type":"Image","group_id":"group_person1"}`, calls[0].Body)
		require.NoError(t, model.DB.Where("logical_id = ?", "la_new").Delete(&model.VolcAsset{}).Error)
	}

	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", tokenChannelID).Update("status", common.ChannelStatusManuallyDisabled).Error)
	resp = serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image","group_id":"group_person1"}`)
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code, "a disabled group channel is not replaced by another account")
	assert.Empty(t, f.takeCalls())
}

func TestVolcAssetSkipsMultiKeyChannels(t *testing.T) {
	f := setupVolcAssetFixture(t)
	require.NoError(t, (&model.VolcAsset{LogicalID: "la_a", UserID: 1, ChannelID: f.channelID}).Insert())
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", f.channelID).Updates(map[string]any{
		"key":          "sk-1\nsk-2",
		"channel_info": model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
	}).Error)

	resp := serveVolcAsset(1, "default", http.MethodPost, "/v1/volcengine/assets", `{"source_url":"https://x/a.png","asset_type":"Image"}`)
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	assert.JSONEq(t, `{"success":false,"message":"no available Volcengine Video channel"}`, resp.Body.String())
	resp = serveVolcAsset(1, "default", http.MethodGet, "/v1/volcengine/assets/la_a", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
	assert.JSONEq(t, `{"success":false,"message":"Volcengine assets require a single-key channel"}`, resp.Body.String())
	assert.Empty(t, f.takeCalls())
}
