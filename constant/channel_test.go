package constant

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetChannelBaseURLIsBoundsSafe(t *testing.T) {
	assert.Empty(t, GetChannelBaseURL(ChannelTypeTaskPlugin))
	assert.Empty(t, GetChannelBaseURL(9999))
}

func TestChannelTypeVolcVideo(t *testing.T) {
	assert.Equal(t, 500, ChannelTypeVolcVideo)
	assert.Equal(t, "Volcengine Video", ChannelTypeNames[ChannelTypeVolcVideo])
	assert.Equal(t, "", GetChannelBaseURL(ChannelTypeVolcVideo))
	assert.Equal(t, 63, ChannelTypeDummy) // guard: community numbering unchanged (Dummy repeats the last literal)
}
