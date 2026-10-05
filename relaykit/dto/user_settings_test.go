package dto

import (
	"testing"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// record_ip_log 必须是可空布尔：JSON 里缺失（用户从未表态）与显式 false
// （用户主动关闭）语义不同，前者要记录 IP，后者不记录。
func TestUserSettingRecordIpLogDistinguishesMissingFromFalse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantNil  bool
		wantBool bool
	}{
		{name: "字段缺失表示从未表态", raw: `{"language":"zh"}`, wantNil: true},
		{name: "显式 false 表示用户关闭", raw: `{"record_ip_log":false}`, wantNil: false, wantBool: false},
		{name: "显式 true 表示用户开启", raw: `{"record_ip_log":true}`, wantNil: false, wantBool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var setting UserSetting
			require.NoError(t, kitutil.UnmarshalJsonStr(tc.raw, &setting))
			if tc.wantNil {
				assert.Nil(t, setting.RecordIpLog)
				return
			}
			require.NotNil(t, setting.RecordIpLog)
			assert.Equal(t, tc.wantBool, *setting.RecordIpLog)
		})
	}
}

// 用户从未表态时不得把字段写进 JSON，否则读回来就变成「已关闭」。
func TestUserSettingRecordIpLogOmittedWhenNil(t *testing.T) {
	disabled := false

	raw, err := kitutil.Marshal(UserSetting{NotifyType: NotifyTypeEmail})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "record_ip_log")

	raw, err = kitutil.Marshal(UserSetting{NotifyType: NotifyTypeEmail, RecordIpLog: &disabled})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"record_ip_log":false`)
}
