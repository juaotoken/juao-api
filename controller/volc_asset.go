package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	volcAssetsPath            = "/v1/volcengine/assets"
	volcAssetMaxResponseBytes = 1 << 20
	volcAssetNotFoundMessage  = "素材不存在"
)

// errVolcChannelUnavailable marks channel misconfiguration (no key, no base
// URL) that callers report as 503 rather than an upstream transport failure.
var errVolcChannelUnavailable = errors.New("volcengine video channel unavailable")

var volcAssetTypes = []string{"Image", "Video", "Audio"}

type volcAssetUpstreamData struct {
	LogicalID      string `json:"logical_id"`
	LogicalGroupID string `json:"logical_group_id"`
	GroupName      string `json:"group_name"`
	GroupID        string `json:"group_id"`
	Name           string `json:"name"`
	AssetType      string `json:"asset_type"`
	Status         string `json:"status"`
}

type volcAssetUpstreamEnvelope struct {
	Success bool                  `json:"success"`
	Data    volcAssetUpstreamData `json:"data"`
}

type volcAssetListItem struct {
	LogicalID      string `json:"logical_id"`
	LogicalGroupID string `json:"logical_group_id"`
	GroupName      string `json:"group_name"`
	GroupID        string `json:"group_id"`
	Name           string `json:"name"`
	AssetType      string `json:"asset_type"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"created_at"`
}

func volcAssetError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"success": false, "message": message})
}

// volcAssetUpstream performs one authenticated call to the channel's upstream
// asset API and returns the status code and (size-limited) response body.
func volcAssetUpstream(c *gin.Context, channel *model.Channel, method, path string, body []byte) (int, []byte, error) {
	baseURL := strings.TrimSuffix(channel.GetBaseURL(), "/")
	if baseURL == "" {
		return 0, nil, errVolcChannelUnavailable
	}
	key, _, keyErr := channel.GetNextEnabledKey()
	if keyErr != nil || key == "" {
		return 0, nil, errVolcChannelUnavailable
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client, err := service.GetHttpClientWithProxy(channel.GetSetting().Proxy)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, volcAssetMaxResponseBytes))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// respondVolcAssetUpstream calls the upstream and relays its status and body,
// mapping local failures to 503/502. It returns ok=false when an error
// response was already written.
func respondVolcAssetUpstream(c *gin.Context, channel *model.Channel, method, path string, body []byte) (int, []byte, bool) {
	status, respBody, err := volcAssetUpstream(c, channel, method, path, body)
	if errors.Is(err, errVolcChannelUnavailable) {
		volcAssetError(c, http.StatusServiceUnavailable, "Volcengine Video channel is not configured")
		return 0, nil, false
	}
	if err != nil {
		logger.LogError(c, fmt.Sprintf("volcengine asset upstream %s %s failed (channel #%d): %v", method, path, channel.Id, err))
		volcAssetError(c, http.StatusBadGateway, "upstream request failed")
		return 0, nil, false
	}
	c.Data(status, "application/json", respBody)
	return status, respBody, true
}

// pickVolcVideoChannelOrAbort picks an enabled Volcengine Video channel serving
// the caller's using group: highest priority first, random among equal
// priority. For "auto" tokens the token's ordered auto groups are tried in
// turn. It writes a 503 and returns nil when no channel qualifies.
func pickVolcVideoChannelOrAbort(c *gin.Context) *model.Channel {
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	groups := []string{usingGroup}
	if usingGroup == "auto" {
		groups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	channels, err := model.GetEnabledChannelsByType(constant.ChannelTypeVolcVideo)
	if err != nil {
		logger.LogError(c, "list volcengine video channels failed: "+err.Error())
		channels = nil
	}
	for _, group := range groups {
		var top []*model.Channel
		for _, channel := range channels {
			if !slices.Contains(channel.GetGroups(), group) {
				continue
			}
			if len(top) > 0 && channel.GetPriority() < top[0].GetPriority() {
				continue
			}
			if len(top) > 0 && channel.GetPriority() > top[0].GetPriority() {
				top = top[:0]
			}
			top = append(top, channel)
		}
		if len(top) > 0 {
			return top[rand.Intn(len(top))]
		}
	}
	volcAssetError(c, http.StatusServiceUnavailable, "no available Volcengine Video channel")
	return nil
}

// ownedVolcAssetChannelOrAbort resolves the caller-owned asset and the channel
// it was created on; it writes the error response and returns nils otherwise.
func ownedVolcAssetChannelOrAbort(c *gin.Context) (*model.VolcAsset, *model.Channel) {
	asset, err := model.GetVolcAsset(c.GetInt("id"), c.Param("logical_id"))
	if err != nil {
		logger.LogError(c, "load volcengine asset failed: "+err.Error())
		volcAssetError(c, http.StatusInternalServerError, "database error")
		return nil, nil
	}
	if asset == nil {
		volcAssetError(c, http.StatusNotFound, volcAssetNotFoundMessage)
		return nil, nil
	}
	channel, err := model.CacheGetChannel(asset.ChannelID)
	if err != nil || channel.Type != constant.ChannelTypeVolcVideo || channel.Status != common.ChannelStatusEnabled {
		volcAssetError(c, http.StatusServiceUnavailable, "the Volcengine Video channel that owns this asset is unavailable")
		return nil, nil
	}
	return asset, channel
}

func readVolcAssetBody(c *gin.Context) ([]byte, bool) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		volcAssetError(c, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	body, err := storage.Bytes()
	if err != nil {
		volcAssetError(c, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	return body, true
}

// VolcAssetCreate handles POST /v1/volcengine/assets.
func VolcAssetCreate(c *gin.Context) {
	body, ok := readVolcAssetBody(c)
	if !ok {
		return
	}
	var req struct {
		SourceURL string `json:"source_url"`
		AssetType string `json:"asset_type"`
		GroupID   string `json:"group_id"`
		GroupName string `json:"group_name"`
	}
	if err := common.Unmarshal(body, &req); err != nil {
		volcAssetError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.SourceURL) == "" {
		volcAssetError(c, http.StatusBadRequest, "source_url is required")
		return
	}
	if !slices.Contains(volcAssetTypes, req.AssetType) {
		volcAssetError(c, http.StatusBadRequest, "asset_type must be one of Image, Video, Audio")
		return
	}
	if req.GroupID != "" && req.GroupName != "" {
		volcAssetError(c, http.StatusBadRequest, "group_id and group_name are mutually exclusive")
		return
	}
	channel := pickVolcVideoChannelOrAbort(c)
	if channel == nil {
		return
	}
	status, respBody, ok := respondVolcAssetUpstream(c, channel, http.MethodPost, volcAssetsPath, body)
	if !ok || status < 200 || status >= 300 {
		return
	}
	var envelope volcAssetUpstreamEnvelope
	if err := common.Unmarshal(respBody, &envelope); err != nil || !envelope.Success || envelope.Data.LogicalID == "" {
		return
	}
	data := envelope.Data
	assetType := data.AssetType
	if assetType == "" {
		assetType = req.AssetType
	}
	asset := &model.VolcAsset{
		LogicalID:      data.LogicalID,
		UserID:         c.GetInt("id"),
		ChannelID:      channel.Id,
		AssetType:      assetType,
		Name:           data.Name,
		LogicalGroupID: data.LogicalGroupID,
		GroupName:      data.GroupName,
		GroupID:        data.GroupID,
		Status:         data.Status,
	}
	if err := asset.Insert(); err != nil {
		logger.LogError(c, fmt.Sprintf("record volcengine asset %s ownership failed: %v", data.LogicalID, err))
	}
}

// VolcAssetList handles GET /v1/volcengine/assets from the local ownership
// table only, so callers never see assets of other users on the same upstream
// account.
func VolcAssetList(c *gin.Context) {
	page, err := strconv.Atoi(c.Query("page"))
	if err != nil || page < 1 {
		page = 1
	}
	pageSize, err := strconv.Atoi(c.Query("page_size"))
	if err != nil {
		pageSize = 20
	}
	pageSize = min(max(pageSize, 1), 100)
	assets, total, err := model.ListVolcAssets(c.GetInt("id"), c.Query("group_id"), c.Query("asset_group_id"), (page-1)*pageSize, pageSize)
	if err != nil {
		logger.LogError(c, "list volcengine assets failed: "+err.Error())
		volcAssetError(c, http.StatusInternalServerError, "database error")
		return
	}
	items := make([]volcAssetListItem, 0, len(assets))
	for _, asset := range assets {
		items = append(items, volcAssetListItem{
			LogicalID:      asset.LogicalID,
			LogicalGroupID: asset.LogicalGroupID,
			GroupName:      asset.GroupName,
			GroupID:        asset.GroupID,
			Name:           asset.Name,
			AssetType:      asset.AssetType,
			Status:         asset.Status,
			CreatedAt:      asset.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"items": items, "total": total, "page": page},
	})
}

// VolcAssetDetail handles GET /v1/volcengine/assets/:logical_id.
func VolcAssetDetail(c *gin.Context) {
	asset, channel := ownedVolcAssetChannelOrAbort(c)
	if asset == nil {
		return
	}
	status, respBody, ok := respondVolcAssetUpstream(c, channel, http.MethodGet, volcAssetsPath+"/"+url.PathEscape(asset.LogicalID), nil)
	if !ok || status < 200 || status >= 300 {
		return
	}
	var envelope volcAssetUpstreamEnvelope
	if err := common.Unmarshal(respBody, &envelope); err != nil {
		return
	}
	if envelope.Data.Status != "" && envelope.Data.Status != asset.Status {
		if err := model.UpdateVolcAssetStatus(asset.ID, envelope.Data.Status); err != nil {
			logger.LogError(c, fmt.Sprintf("update volcengine asset %s status failed: %v", asset.LogicalID, err))
		}
	}
}

// VolcAssetDelete handles DELETE /v1/volcengine/assets/:logical_id. An upstream
// 404 means the asset is already gone, so the local row is removed too.
func VolcAssetDelete(c *gin.Context) {
	asset, channel := ownedVolcAssetChannelOrAbort(c)
	if asset == nil {
		return
	}
	status, _, ok := respondVolcAssetUpstream(c, channel, http.MethodDelete, volcAssetsPath+"/"+url.PathEscape(asset.LogicalID), nil)
	if !ok {
		return
	}
	if (status >= 200 && status < 300) || status == http.StatusNotFound {
		if err := model.DeleteVolcAsset(asset.ID); err != nil {
			logger.LogError(c, fmt.Sprintf("delete volcengine asset %s record failed: %v", asset.LogicalID, err))
		}
	}
}

// VolcAssetAuthSession handles POST /v1/volcengine/assets/auth-sessions.
func VolcAssetAuthSession(c *gin.Context) {
	body, ok := readVolcAssetBody(c)
	if !ok {
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	channel := pickVolcVideoChannelOrAbort(c)
	if channel == nil {
		return
	}
	respondVolcAssetUpstream(c, channel, http.MethodPost, volcAssetsPath+"/auth-sessions", body)
}

// VolcAssetGroupByBytedToken handles POST /v1/volcengine/assets/groups/by-byted-token.
func VolcAssetGroupByBytedToken(c *gin.Context) {
	body, ok := readVolcAssetBody(c)
	if !ok {
		return
	}
	var req struct {
		BytedToken string `json:"byted_token"`
	}
	if err := common.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.BytedToken) == "" {
		volcAssetError(c, http.StatusBadRequest, "byted_token is required")
		return
	}
	channel := pickVolcVideoChannelOrAbort(c)
	if channel == nil {
		return
	}
	respondVolcAssetUpstream(c, channel, http.MethodPost, volcAssetsPath+"/groups/by-byted-token", body)
}
