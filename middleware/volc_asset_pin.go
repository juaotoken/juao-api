package middleware

import (
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	volcAssetURLPrefix     = "asset://"
	maxVolcAssetReferences = 100
)

// VolcAssetChannelPin pins a Volcengine Video submission that references
// asset://<logical_id> media to the channel the caller created those assets
// on: a logical asset only exists in that channel's upstream account.
//
// A pinned request skips the distributor's token model limit and group
// routing, so both are enforced here before the pin is added: the token must
// allow the model, and the pinned channel must serve the model in the
// caller's using group (any of the token's auto groups for "auto").
func VolcAssetChannelPin() gin.HandlerFunc {
	return func(c *gin.Context) {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			c.Next()
			return
		}
		raw, err := storage.Bytes()
		if err != nil {
			c.Next()
			return
		}
		if _, seekErr := storage.Seek(0, io.SeekStart); seekErr == nil {
			c.Request.Body = io.NopCloser(storage)
		}
		// Parse loosely, the same way the volcvideo adaptor does, so a
		// malformed sibling field cannot hide an asset reference from this
		// check while the adaptor still forwards it upstream.
		var body map[string]any
		if common.Unmarshal(raw, &body) != nil {
			c.Next()
			return
		}
		content, _ := body["content"].([]any)
		logicalIDs := make([]string, 0)
		seen := make(map[string]struct{})
		for _, item := range content {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"image_url", "video_url", "audio_url"} {
				// Accept the bare-string form too, so an alternative shape
				// cannot carry an unchecked asset reference upstream.
				url, isString := part[field].(string)
				if !isString {
					media, _ := part[field].(map[string]any)
					url, _ = media["url"].(string)
				}
				url = strings.TrimSpace(url)
				if len(url) < len(volcAssetURLPrefix) || !strings.EqualFold(url[:len(volcAssetURLPrefix)], volcAssetURLPrefix) {
					continue
				}
				logicalID := url[len(volcAssetURLPrefix):]
				if _, dup := seen[logicalID]; dup {
					continue
				}
				seen[logicalID] = struct{}{}
				logicalIDs = append(logicalIDs, logicalID)
			}
		}
		if len(logicalIDs) == 0 {
			c.Next()
			return
		}
		if len(logicalIDs) > maxVolcAssetReferences {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "too many asset references")
			return
		}

		assets, err := model.GetVolcAssetsByLogicalIDs(c.GetInt("id"), logicalIDs)
		if err != nil {
			logger.LogError(c, "load volcengine assets failed: "+err.Error())
			abortWithOpenAiMessage(c, http.StatusInternalServerError, "database error")
			return
		}
		owned := make(map[string]int, len(assets))
		for _, asset := range assets {
			owned[asset.LogicalID] = asset.ChannelID
		}
		channelID := 0
		for _, logicalID := range logicalIDs {
			assetChannelID, ok := owned[logicalID]
			if !ok {
				abortWithOpenAiMessage(c, http.StatusBadRequest, "asset reference not found: "+logicalID)
				return
			}
			if channelID != 0 && assetChannelID != channelID {
				abortWithOpenAiMessage(c, http.StatusBadRequest, "referenced assets belong to different channels")
				return
			}
			channelID = assetChannelID
		}

		modelName, _ := body["model"].(string)
		if !enforceTokenModelLimit(c, modelName) {
			return
		}
		usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		groupAllowed := false
		if usingGroup == "auto" {
			for _, group := range service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup)) {
				if model.IsChannelEnabledForGroupModel(group, modelName, channelID) {
					// The pinned path never runs auto-group selection, so record
					// the resolved group for billing like channel affinity does.
					common.SetContextKey(c, constant.ContextKeyAutoGroup, group)
					groupAllowed = true
					break
				}
			}
		} else {
			groupAllowed = model.IsChannelEnabledForGroupModel(usingGroup, modelName, channelID)
		}
		if !groupAllowed {
			abortWithOpenAiMessage(c, http.StatusForbidden, "the channel of the referenced assets is not available for your group/model")
			return
		}

		service.GetChannelConstraints(c).AddPin(dto.ChannelPin{
			ChannelId: channelID,
			Source:    dto.PinSourceVolcAsset,
			Rank:      dto.PinRankVolcAsset,
			RetryMode: dto.PinRetrySameChannel,
		})
		c.Next()
	}
}
