package taskusage

import (
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterAndForModel(t *testing.T) {
	const model = "taskusage-probe-model"
	t.Cleanup(func() { Unregister(model) })

	Register([]string{model}, map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number", Unit: "second"},
	}, []jsplugin.UsageExample{{Label: "5s", Facts: map[string]any{"seconds": 5}}})

	schema, examples, native := ForModel(model)
	require.True(t, native)
	assert.Equal(t, "second", schema["seconds"].Unit)
	require.Len(t, examples, 1)
	assert.Equal(t, map[string]any{"seconds": 5}, examples[0].Facts)

	_, _, unknown := ForModel("taskusage-unknown-model")
	assert.False(t, unknown)
}

func TestRegisterTrimsAndSkipsBlankNames(t *testing.T) {
	t.Cleanup(func() { Unregister("taskusage-trimmed-model") })

	Register([]string{"  taskusage-trimmed-model  ", "   "}, map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number"},
	}, nil)

	schema, _, native := ForModel("taskusage-trimmed-model")
	require.True(t, native)
	assert.Contains(t, schema, "seconds")

	_, _, blank := ForModel("")
	assert.False(t, blank, "a blank model name must not be registered")
}

func TestForModelReturnsIndependentCopies(t *testing.T) {
	const model = "taskusage-clone-model"
	t.Cleanup(func() { Unregister(model) })

	Register([]string{model}, map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number", Unit: "second"},
	}, []jsplugin.UsageExample{{Label: "5s", Facts: map[string]any{"seconds": 5}}})

	schema, examples, _ := ForModel(model)
	schema["seconds"] = jsplugin.UsageFieldSchema{Unit: "mutated"}
	examples[0].Facts["seconds"] = "mutated"

	fresh, freshExamples, _ := ForModel(model)
	assert.Equal(t, "second", fresh["seconds"].Unit, "callers must not be able to mutate the registry")
	assert.Equal(t, 5, freshExamples[0].Facts["seconds"])
}

func TestRegisterReplacesPreviousProfile(t *testing.T) {
	const model = "taskusage-replace-model"
	t.Cleanup(func() { Unregister(model) })

	Register([]string{model}, map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number", Unit: "second"},
	}, []jsplugin.UsageExample{{Label: "old"}})
	Register([]string{model}, map[string]jsplugin.UsageFieldSchema{
		"clips": {Type: "number", Unit: "count"},
	}, []jsplugin.UsageExample{{Label: "new"}})

	schema, examples, native := ForModel(model)
	require.True(t, native)
	assert.Equal(t, []string{"clips"}, keys(schema))
	assert.Equal(t, "new", examples[0].Label)
}

func TestUnregisterRemovesOnlyTheGivenModels(t *testing.T) {
	Register([]string{"taskusage-keep-model", "taskusage-drop-model"}, map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number"},
	}, nil)
	t.Cleanup(func() { Unregister("taskusage-keep-model", "taskusage-drop-model") })

	Unregister("taskusage-drop-model")

	_, _, dropped := ForModel("taskusage-drop-model")
	assert.False(t, dropped)
	_, _, kept := ForModel("taskusage-keep-model")
	assert.True(t, kept)
}

func keys(schema map[string]jsplugin.UsageFieldSchema) []string {
	result := make([]string, 0, len(schema))
	for key := range schema {
		result = append(result, key)
	}
	return result
}
