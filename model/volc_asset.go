package model

import (
	"errors"
	"time"

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
