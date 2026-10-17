package volcvideo

// ChannelTypeName is the display name for channel type 500 (Volcengine Video).
const ChannelTypeName = "Volcengine Video"

// ModelList enumerates the task models served through this adaptor. It has to
// list every name a channel exposes, because a name no entry covers gets no
// usage schema and its tiered pricing expression cannot be saved. The upstream
// serves Seedance without the Ark endpoint suffix the doubao plugin uses
// (doubao-seedance-2-5, not doubao-seedance-2-5-260628).
var ModelList = []string{
	"doubao-seedance-2-0",
	"doubao-seedance-2-0-fast",
	"doubao-seedance-2-0-mini",
	"doubao-seedance-2-5",
}
