package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestVolcAssetOwnershipAndListing(t *testing.T) {
	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	var err error
	DB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, DB.AutoMigrate(&VolcAsset{}))

	seed := []*VolcAsset{
		{LogicalID: "la_1", UserID: 1, ChannelID: 10, LogicalGroupID: "lag_a", GroupID: "group_x", Status: "Processing"},
		{LogicalID: "la_2", UserID: 1, ChannelID: 10, LogicalGroupID: "lag_a", GroupID: "group_y"},
		{LogicalID: "la_3", UserID: 1, ChannelID: 11, LogicalGroupID: "lag_b", GroupID: "group_x"},
		{LogicalID: "la_4", UserID: 2, ChannelID: 10, LogicalGroupID: "lag_a", GroupID: "group_x"},
	}
	for _, a := range seed {
		require.NoError(t, a.Insert())
		assert.NotZero(t, a.CreatedAt)
	}

	owned, err := GetVolcAsset(1, "la_1")
	require.NoError(t, err)
	require.NotNil(t, owned)
	assert.Equal(t, 10, owned.ChannelID)

	foreign, err := GetVolcAsset(2, "la_1")
	require.NoError(t, err)
	assert.Nil(t, foreign)

	byIDs, err := GetVolcAssetsByLogicalIDs(1, []string{"la_1", "la_4", "la_missing"})
	require.NoError(t, err)
	require.Len(t, byIDs, 1)
	assert.Equal(t, "la_1", byIDs[0].LogicalID)

	all, total, err := ListVolcAssets(1, "", "", 0, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	require.Len(t, all, 2)
	assert.Equal(t, "la_3", all[0].LogicalID)
	assert.Equal(t, "la_2", all[1].LogicalID)

	filtered, total, err := ListVolcAssets(1, "lag_a", "group_x", 0, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, filtered, 1)
	assert.Equal(t, "la_1", filtered[0].LogicalID)

	require.NoError(t, UpdateVolcAssetStatus(owned.ID, "Active"))
	owned, err = GetVolcAsset(1, "la_1")
	require.NoError(t, err)
	assert.Equal(t, "Active", owned.Status)

	dup := &VolcAsset{LogicalID: "la_1", UserID: 2}
	assert.Error(t, dup.Insert())

	require.NoError(t, DeleteVolcAsset(owned.ID))
	owned, err = GetVolcAsset(1, "la_1")
	require.NoError(t, err)
	assert.Nil(t, owned)
}

func TestCollectVolcAssetReferences(t *testing.T) {
	var body map[string]any
	require.NoError(t, common.UnmarshalJsonStr(`{
		"model": "m",
		"content": [
			{"type": "text", "text": "asset://in-text"},
			{"type": "image_url", "IMAGE_URL": {"URL": " ASSET://upper "}},
			{"type": "video_url", "video_url": "asset://bare"},
			{"nested": {"deeper": [["asset://deep"]]}},
			{"image_url": {"url": "https://cdn.example/a.png"}},
			{"image_url": {"url": "asset://upper"}}
		]
	}`, &body))
	assert.Equal(t, []string{"in-text", "upper", "bare", "deep"}, CollectVolcAssetReferences(body))
	assert.Empty(t, CollectVolcAssetReferences(map[string]any{"x": 1.0, "y": nil}))

	var deep any = "asset://too-deep"
	for range 100 {
		deep = []any{deep}
	}
	assert.Empty(t, CollectVolcAssetReferences(deep), "references beyond the depth bound are not descended into")
}
