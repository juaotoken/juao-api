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
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	volcAssetsPath            = "/v1/volcengine/assets"
	volcAssetMaxResponseBytes = 1 << 20
	volcAssetNotFoundMessage  = "素材不存在"
	volcAssetSingleKeyMessage = "Volcengine assets require a single-key channel"
	volcAssetMaxGroupNameRune = 200
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

// callVolcAssetUpstreamOrAbort calls the upstream and returns its status and
// body without writing them, so callers can rewrite the body first. Local
// failures are written as 503/502 and reported with ok=false.
func callVolcAssetUpstreamOrAbort(c *gin.Context, channel *model.Channel, method, path string, body []byte) (int, []byte, bool) {
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
	return status, respBody, true
}

// volcAssetGroupNamePrefix namespaces upstream group names per local user.
// All local users share one upstream account per channel, and the upstream
// reuses any existing group with the same name, so un-prefixed names would
// let users write into each other's groups.
func volcAssetGroupNamePrefix(userID int) string {
	return fmt.Sprintf("u%d-", userID)
}

// stripVolcAssetGroupNamePrefix removes the caller's namespace prefix from
// data.group_name in an upstream response, returning the rewritten body and
// the stripped name.
func stripVolcAssetGroupNamePrefix(respBody []byte, userID int) ([]byte, string) {
	groupName := gjson.GetBytes(respBody, "data.group_name")
	if groupName.Type != gjson.String {
		return respBody, ""
	}
	stripped, found := strings.CutPrefix(groupName.String(), volcAssetGroupNamePrefix(userID))
	if !found {
		return respBody, groupName.String()
	}
	rewritten, err := sjson.SetBytes(respBody, "data.group_name", stripped)
	if err != nil {
		return respBody, groupName.String()
	}
	return rewritten, stripped
}

// recordVolcAssetGroupOwnership records the caller as owner of an upstream
// group id on channel; an id already owned by someone else is left alone.
func recordVolcAssetGroupOwnership(c *gin.Context, channel *model.Channel, groupID string) {
	if groupID == "" {
		return
	}
	err := model.RecordVolcAssetGroup(c.GetInt("id"), channel.Id, groupID)
	if errors.Is(err, model.ErrVolcAssetGroupOwnedByOther) {
		logger.LogWarn(c, fmt.Sprintf("volcengine asset group %s on channel #%d is already owned by another user", groupID, channel.Id))
		return
	}
	if err != nil {
		logger.LogError(c, fmt.Sprintf("record volcengine asset group %s ownership failed: %v", groupID, err))
	}
}

// volcRequestGroups returns the groups a Volcengine asset request may be
// served from: the using group, or the token's ordered auto groups.
func volcRequestGroups(c *gin.Context) []string {
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if usingGroup == "auto" {
		return service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	return []string{usingGroup}
}

// pickVolcVideoChannelOrAbort picks an enabled Volcengine Video channel serving
// the caller's using group: highest priority first, random among equal
// priority. For "auto" tokens the token's ordered auto groups are tried in
// turn. Multi-key channels are skipped. It writes a 503 and returns nil when no channel qualifies.
func pickVolcVideoChannelOrAbort(c *gin.Context) *model.Channel {
	groups := volcRequestGroups(c)
	channels, err := model.GetEnabledChannelsByType(constant.ChannelTypeVolcVideo)
	if err != nil {
		logger.LogError(c, "list volcengine video channels failed: "+err.Error())
		channels = nil
	}
	for _, group := range groups {
		var top []*model.Channel
		for _, channel := range channels {
			// Multi-key channels may hold several upstream accounts, and an
			// asset or group exists in only one of them.
			if channel.ChannelInfo.IsMultiKey || !slices.Contains(channel.GetGroups(), group) {
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
	if channel.ChannelInfo.IsMultiKey {
		volcAssetError(c, http.StatusServiceUnavailable, volcAssetSingleKeyMessage)
		return nil, nil
	}
	return asset, channel
}

// ownedVolcAssetGroupChannelOrAbort resolves the channel an owned group_id
// lives on. The group must be the caller's, and its channel must still be an
// enabled single-key Volcengine Video channel serving the caller's group.
func ownedVolcAssetGroupChannelOrAbort(c *gin.Context, groupID string) *model.Channel {
	group, err := model.GetVolcAssetGroup(c.GetInt("id"), groupID)
	if err != nil {
		logger.LogError(c, "load volcengine asset group failed: "+err.Error())
		volcAssetError(c, http.StatusInternalServerError, "database error")
		return nil
	}
	if group == nil {
		volcAssetError(c, http.StatusBadRequest, "unknown group_id")
		return nil
	}
	channel, err := model.CacheGetChannel(group.ChannelID)
	if err != nil || channel.Type != constant.ChannelTypeVolcVideo || channel.Status != common.ChannelStatusEnabled ||
		!slices.ContainsFunc(volcRequestGroups(c), func(g string) bool { return slices.Contains(channel.GetGroups(), g) }) {
		volcAssetError(c, http.StatusServiceUnavailable, "the Volcengine Video channel that owns this group is unavailable")
		return nil
	}
	if channel.ChannelInfo.IsMultiKey {
		volcAssetError(c, http.StatusServiceUnavailable, volcAssetSingleKeyMessage)
		return nil
	}
	return channel
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

// VolcAssetCreate handles POST /v1/volcengine/assets. group_name is
// namespaced per user upstream (see volcAssetGroupNamePrefix); without a
// group the upstream's name-based auto grouping is pinned to the caller's
// namespace too. A group_id must be one the caller owns, and the asset is
// created on that group's channel.
func VolcAssetCreate(c *gin.Context) {
	body, ok := readVolcAssetBody(c)
	if !ok {
		return
	}
	var req struct {
		SourceURL string `json:"source_url"`
		AssetType string `json:"asset_type"`
		Name      string `json:"name"`
		GroupID   string `json:"group_id"`
		GroupName string `json:"group_name"`
	}
	var fields map[string]any
	if common.Unmarshal(body, &req) != nil || common.Unmarshal(body, &fields) != nil {
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
	userID := c.GetInt("id")
	prefix := volcAssetGroupNamePrefix(userID)
	if req.GroupName != "" {
		if strings.HasPrefix(req.GroupName, "lag_") || strings.HasPrefix(req.GroupName, "newapi-channel-") {
			volcAssetError(c, http.StatusBadRequest, "group_name must not start with lag_ or newapi-channel-")
			return
		}
		if utf8.RuneCountInString(prefix+req.GroupName) > volcAssetMaxGroupNameRune {
			volcAssetError(c, http.StatusBadRequest, fmt.Sprintf("group_name is too long (max %d characters)", volcAssetMaxGroupNameRune-utf8.RuneCountInString(prefix)))
			return
		}
	}

	var channel *model.Channel
	if req.GroupID != "" {
		channel = ownedVolcAssetGroupChannelOrAbort(c, req.GroupID)
	} else {
		delete(fields, "group_id")
		switch name := strings.TrimSpace(req.Name); {
		case req.GroupName != "":
			fields["group_name"] = prefix + req.GroupName
		case name != "":
			// Pin the upstream's name-based auto grouping to the caller.
			fields["group_name"] = string([]rune(prefix + name)[:min(utf8.RuneCountInString(prefix+name), volcAssetMaxGroupNameRune)])
		default:
			fields["group_name"] = prefix + "default"
		}
		channel = pickVolcVideoChannelOrAbort(c)
	}
	if channel == nil {
		return
	}
	upstreamBody, err := common.Marshal(fields)
	if err != nil {
		volcAssetError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	status, respBody, ok := callVolcAssetUpstreamOrAbort(c, channel, http.MethodPost, volcAssetsPath, upstreamBody)
	if !ok {
		return
	}
	respBody, groupName := stripVolcAssetGroupNamePrefix(respBody, userID)
	c.Data(status, "application/json", respBody)
	if status < 200 || status >= 300 {
		return
	}
	var envelope volcAssetUpstreamEnvelope
	if err := common.Unmarshal(respBody, &envelope); err != nil || !envelope.Success || envelope.Data.LogicalID == "" {
		return
	}
	data := envelope.Data
	recordVolcAssetGroupOwnership(c, channel, data.GroupID)
	recordVolcAssetGroupOwnership(c, channel, data.LogicalGroupID)
	assetType := data.AssetType
	if assetType == "" {
		assetType = req.AssetType
	}
	asset := &model.VolcAsset{
		LogicalID:      data.LogicalID,
		UserID:         userID,
		ChannelID:      channel.Id,
		AssetType:      assetType,
		Name:           data.Name,
		LogicalGroupID: data.LogicalGroupID,
		GroupName:      groupName,
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
	status, respBody, ok := callVolcAssetUpstreamOrAbort(c, channel, http.MethodGet, volcAssetsPath+"/"+url.PathEscape(asset.LogicalID), nil)
	if !ok {
		return
	}
	respBody, _ = stripVolcAssetGroupNamePrefix(respBody, asset.UserID)
	c.Data(status, "application/json", respBody)
	if status < 200 || status >= 300 {
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
	status, respBody, ok := callVolcAssetUpstreamOrAbort(c, channel, http.MethodDelete, volcAssetsPath+"/"+url.PathEscape(asset.LogicalID), nil)
	if !ok {
		return
	}
	c.Data(status, "application/json", respBody)
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
	if status, respBody, ok := callVolcAssetUpstreamOrAbort(c, channel, http.MethodPost, volcAssetsPath+"/auth-sessions", body); ok {
		c.Data(status, "application/json", respBody)
	}
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
	status, respBody, ok := callVolcAssetUpstreamOrAbort(c, channel, http.MethodPost, volcAssetsPath+"/groups/by-byted-token", body)
	if !ok {
		return
	}
	c.Data(status, "application/json", respBody)
	if status < 200 || status >= 300 || !gjson.GetBytes(respBody, "success").Bool() {
		return
	}
	recordVolcAssetGroupOwnership(c, channel, gjson.GetBytes(respBody, "data.group_id").String())
}
