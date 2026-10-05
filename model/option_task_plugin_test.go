package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskPluginEnabledOptionUpdatesRegistry(t *testing.T) {
	originalEnabled := constant.TaskPluginEnabled
	originalMap := common.OptionMap
	common.OptionMap = map[string]string{}
	const key = "option-master-off"
	source := `
export const meta = {apiVersion: 1, key: "option-master-off", name: "Option Master", version: "1.0.0", author: {name: "Test"}, models: ["option-master-model"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		constant.TaskPluginEnabled = originalEnabled
		jsplugin.DefaultRegistry.SetEnabled(originalEnabled)
		common.OptionMap = originalMap
	})

	_, ok := jsplugin.DefaultRegistry.Get(key)
	require.True(t, ok)

	require.NoError(t, updateOptionMap("TaskPluginEnabled", "false"))

	assert.False(t, constant.TaskPluginEnabled)
	assert.Equal(t, "false", common.OptionMap["TaskPluginEnabled"])
	_, ok = jsplugin.DefaultRegistry.Get(key)
	assert.False(t, ok)

	require.NoError(t, updateOptionMap("TaskPluginEnabled", "true"))
	_, ok = jsplugin.DefaultRegistry.Get(key)
	assert.True(t, ok)
}

func TestTaskPluginDisabledFactoryKeysOptionUpdatesRegistry(t *testing.T) {
	originalMap := common.OptionMap
	common.OptionMap = map[string]string{}
	const key = "option-factory-off"
	source := `
export const meta = {apiVersion: 1, key: "option-factory-off", name: "Option Factory", version: "1.0.0", author: {name: "Test"}, models: ["option-factory-model"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		jsplugin.DefaultRegistry.SetDisabledFactoryKeys(nil)
		common.OptionMap = originalMap
	})

	_, ok := jsplugin.DefaultRegistry.Get(key)
	require.True(t, ok)

	require.NoError(t, updateOptionMap(setting.TaskPluginDisabledFactoryKeysKey, `["option-factory-off"]`))

	assert.Equal(t, `["option-factory-off"]`, common.OptionMap[setting.TaskPluginDisabledFactoryKeysKey])
	_, ok = jsplugin.DefaultRegistry.Get(key)
	assert.False(t, ok)
	assert.Equal(t, []string{key}, jsplugin.DefaultRegistry.Snapshot().DisabledFactory)
}

func TestErrorLogRawRequestEnabledOptionUpdatesLiveValue(t *testing.T) {
	originalEnabled := common.ErrorLogRawRequestEnabled
	originalMap := common.OptionMap
	common.OptionMap = map[string]string{}
	t.Cleanup(func() {
		common.ErrorLogRawRequestEnabled = originalEnabled
		common.OptionMap = originalMap
	})

	// 站点默认必须为开：InitOptionMap 从变量播种，DB 里没有该 key 时，播种值就是最终生效值
	// （管理端 GetOptions 也靠这行播种才能看到这个开关）。先显式置 true 排除其他测试的遗留状态，
	// 再断言播种结果——只读进程全局变量是同义反复，覆盖不到播种行本身。
	common.ErrorLogRawRequestEnabled = true
	InitOptionMap()
	assert.True(t, common.ErrorLogRawRequestEnabled, "InitOptionMap 不应改变开关的运行时值")
	assert.Equal(t, "true", common.OptionMap["ErrorLogRawRequestEnabled"])

	require.NoError(t, updateOptionMap("ErrorLogRawRequestEnabled", "false"))
	assert.False(t, common.ErrorLogRawRequestEnabled)
	assert.Equal(t, "false", common.OptionMap["ErrorLogRawRequestEnabled"])

	require.NoError(t, updateOptionMap("ErrorLogRawRequestEnabled", "true"))
	assert.True(t, common.ErrorLogRawRequestEnabled)
	assert.Equal(t, "true", common.OptionMap["ErrorLogRawRequestEnabled"])
}
