package model

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// VolcAsset records local ownership of a logical asset created through a
// Volcengine Video (type 500) channel. It is the authorization boundary for
// the asset API: list/detail/delete only see rows owned by the caller, and
// video submissions referencing asset://<logical_id> are pinned to ChannelID.
type VolcAsset struct {
	ID             int64  `json:"id" gorm:"primaryKey"`
	LogicalID      string `json:"logical_id" gorm:"type:varchar(64);uniqueIndex"`
	UserID         int    `json:"user_id" gorm:"index"`
	ChannelID      int    `json:"channel_id" gorm:"index"`
	AssetType      string `json:"asset_type" gorm:"type:varchar(32)"`
	Name           string `json:"name" gorm:"type:varchar(255)"`
	LogicalGroupID string `json:"logical_group_id" gorm:"type:varchar(64);index"`
	GroupName      string `json:"group_name" gorm:"type:varchar(255)"`
	GroupID        string `json:"group_id" gorm:"type:varchar(128);index"`
	Status         string `json:"status" gorm:"type:varchar(32)"`
	CreatedAt      int64  `json:"created_at" gorm:"bigint"`
	UpdatedAt      int64  `json:"updated_at" gorm:"bigint"`
}

// VolcAssetGroup records which user owns an upstream asset group id on a
// Volcengine Video channel: a real-person group from by-byted-token, a local
// logical group (lag_ prefix), or an upstream physical group. Creating an
// asset into a group_id is only allowed for its owner, and is routed to the
// channel (upstream account) the group lives on.
type VolcAssetGroup struct {
	ID        int64  `json:"id" gorm:"primaryKey"`
	UserID    int    `json:"user_id" gorm:"index"`
	ChannelID int    `json:"channel_id" gorm:"uniqueIndex:idx_volc_asset_groups_channel_group,priority:1"`
	GroupID   string `json:"group_id" gorm:"type:varchar(128);uniqueIndex:idx_volc_asset_groups_channel_group,priority:2"`
	CreatedAt int64  `json:"created_at" gorm:"bigint"`
}

const (
	// MaxVolcAssetReferences bounds how many distinct asset:// references a
	// single video submission may carry.
	MaxVolcAssetReferences = 100

	volcAssetURLPrefix         = "asset://"
	maxVolcAssetReferenceDepth = 64
)

// ErrVolcAssetGroupOwnedByOther reports that a (channel, group_id) pair is
// already recorded for a different user.
var ErrVolcAssetGroupOwnedByOther = errors.New("volcengine asset group is owned by another user")

// CollectVolcAssetReferences walks a decoded JSON value (maps and slices, at
// any depth up to a fixed bound) and returns the logical IDs of every string
// that starts with "asset://" (case-insensitive, surrounding whitespace
// ignored), deduplicated in first-seen order. Map keys are visited in sorted
// order so the result is deterministic. It does not depend on field names, so
// case-variant or nested keys cannot smuggle an unchecked reference upstream.
func CollectVolcAssetReferences(value any) []string {
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	collectVolcAssetReferences(value, 0, seen, &ids)
	return ids
}

func collectVolcAssetReferences(value any, depth int, seen map[string]struct{}, ids *[]string) {
	if depth > maxVolcAssetReferenceDepth {
		return
	}
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if len(text) < len(volcAssetURLPrefix) || !strings.EqualFold(text[:len(volcAssetURLPrefix)], volcAssetURLPrefix) {
			return
		}
		logicalID := strings.TrimSpace(text[len(volcAssetURLPrefix):])
		if _, dup := seen[logicalID]; dup {
			return
		}
		seen[logicalID] = struct{}{}
		*ids = append(*ids, logicalID)
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			collectVolcAssetReferences(v[key], depth+1, seen, ids)
		}
	case []any:
		for _, item := range v {
			collectVolcAssetReferences(item, depth+1, seen, ids)
		}
	}
}

// GetEnabledChannelsByType returns every enabled channel of channelType. It is
// served from the memory cache when enabled so callers share the cached
// instance (multi-key polling state), and falls back to the database.
func GetEnabledChannelsByType(channelType int) ([]*Channel, error) {
	channels := make([]*Channel, 0)
	if !common.MemoryCacheEnabled {
		err := DB.Where("type = ? AND status = ?", channelType, common.ChannelStatusEnabled).Find(&channels).Error
		return channels, err
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	for _, channel := range channelsIDM {
		if channel.Type == channelType && channel.Status == common.ChannelStatusEnabled {
			channels = append(channels, channel)
		}
	}
	return channels, nil
}

// GetVolcAsset returns the asset owned by userID, or (nil, nil) when the
// asset does not exist or belongs to another user.
func GetVolcAsset(userID int, logicalID string) (*VolcAsset, error) {
	var asset VolcAsset
	err := DB.Where("user_id = ? AND logical_id = ?", userID, logicalID).First(&asset).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &asset, nil
}

// GetVolcAssetsByLogicalIDs returns the subset of logicalIDs owned by userID.
func GetVolcAssetsByLogicalIDs(userID int, logicalIDs []string) ([]*VolcAsset, error) {
	assets := make([]*VolcAsset, 0, len(logicalIDs))
	if len(logicalIDs) == 0 {
		return assets, nil
	}
	err := DB.Where("user_id = ? AND logical_id IN ?", userID, logicalIDs).Find(&assets).Error
	return assets, err
}

func (a *VolcAsset) Insert() error {
	now := time.Now().Unix()
	a.CreatedAt = now
	a.UpdatedAt = now
	return DB.Create(a).Error
}

func UpdateVolcAssetStatus(id int64, status string) error {
	return DB.Model(&VolcAsset{}).Where("id = ?", id).Updates(map[string]any{
		"status":     status,
		"updated_at": time.Now().Unix(),
	}).Error
}

func DeleteVolcAsset(id int64) error {
	return DB.Where("id = ?", id).Delete(&VolcAsset{}).Error
}

// ListVolcAssets lists the caller's assets newest first. Empty
// logicalGroupID / groupID mean no filter.
func ListVolcAssets(userID int, logicalGroupID, groupID string, offset, limit int) ([]*VolcAsset, int64, error) {
	query := DB.Model(&VolcAsset{}).Where("user_id = ?", userID)
	if logicalGroupID != "" {
		query = query.Where("logical_group_id = ?", logicalGroupID)
	}
	if groupID != "" {
		query = query.Where("group_id = ?", groupID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	assets := make([]*VolcAsset, 0)
	err := query.Order("id desc").Offset(offset).Limit(limit).Find(&assets).Error
	return assets, total, err
}

// RecordVolcAssetGroup records that userID owns groupID on channelID. It is
// idempotent for the same owner and never reassigns a pair already recorded
// for another user (ErrVolcAssetGroupOwnedByOther).
func RecordVolcAssetGroup(userID, channelID int, groupID string) error {
	var existing VolcAssetGroup
	err := DB.Where("channel_id = ? AND group_id = ?", channelID, groupID).First(&existing).Error
	if err == nil {
		if existing.UserID != userID {
			return ErrVolcAssetGroupOwnedByOther
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	group := &VolcAssetGroup{UserID: userID, ChannelID: channelID, GroupID: groupID, CreatedAt: time.Now().Unix()}
	createErr := DB.Create(group).Error
	if createErr == nil {
		return nil
	}
	// A concurrent insert may have won the unique index; re-read the winner.
	if err := DB.Where("channel_id = ? AND group_id = ?", channelID, groupID).First(&existing).Error; err == nil {
		if existing.UserID != userID {
			return ErrVolcAssetGroupOwnedByOther
		}
		return nil
	}
	return createErr
}

// GetVolcAssetGroup returns the caller-owned group record for groupID, or
// (nil, nil) when the caller owns no such group.
func GetVolcAssetGroup(userID int, groupID string) (*VolcAssetGroup, error) {
	var group VolcAssetGroup
	err := DB.Where("user_id = ? AND group_id = ?", userID, groupID).Order("id desc").First(&group).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &group, nil
}
