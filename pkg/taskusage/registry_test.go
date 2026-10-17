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

func TestForModelFallsBackToTheLongestMatchingPrefix(t *testing.T) {
	t.Cleanup(func() {
		UnregisterPrefix("taskusage-family-", "taskusage-family-special-")
		Unregister("taskusage-family-exact-model")
	})

	RegisterPrefix([]string{"taskusage-family-"}, map[string]jsplugin.UsageFieldSchema{
		"tokens": {Type: "number", Unit: "token"},
	}, []jsplugin.UsageExample{{Label: "family"}})
	RegisterPrefix([]string{"taskusage-family-special-"}, map[string]jsplugin.UsageFieldSchema{
		"clips": {Type: "number", Unit: "count"},
	}, []jsplugin.UsageExample{{Label: "special"}})

	// An exact registration wins over both prefixes, whatever their length.
	Register([]string{"taskusage-family-exact-model"}, map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number", Unit: "second"},
	}, []jsplugin.UsageExample{{Label: "exact"}})

	exact, exactExamples, ok := ForModel("taskusage-family-exact-model")
	require.True(t, ok)
	assert.Equal(t, []string{"seconds"}, keys(exact))
	assert.Equal(t, "exact", exactExamples[0].Label)

	special, specialExamples, ok := ForModel("taskusage-family-special-3-0")
	require.True(t, ok)
	assert.Equal(t, []string{"clips"}, keys(special))
	assert.Equal(t, "special", specialExamples[0].Label)

	family, _, ok := ForModel("taskusage-family-3-0")
	require.True(t, ok)
	assert.Equal(t, []string{"tokens"}, keys(family))

	_, _, unknown := ForModel("taskusage-other-3-0")
	assert.False(t, unknown)

	_, _, blank := ForModel("")
	assert.False(t, blank, "a blank name must not match a prefix")
}

func TestPrefixRegistrationsReturnIndependentCopies(t *testing.T) {
	const model = "taskusage-copy-3-0"
	t.Cleanup(func() { UnregisterPrefix("taskusage-copy-") })

	RegisterPrefix([]string{"taskusage-copy-"}, map[string]jsplugin.UsageFieldSchema{
		"tokens": {Type: "number", Unit: "token"},
	}, []jsplugin.UsageExample{{Label: "5s", Facts: map[string]any{"tokens": 1}}})

	schema, examples, ok := ForModel(model)
	require.True(t, ok)
	schema["tokens"] = jsplugin.UsageFieldSchema{Unit: "mutated"}
	examples[0].Facts["tokens"] = "mutated"

	fresh, freshExamples, _ := ForModel(model)
	assert.Equal(t, "token", fresh["tokens"].Unit)
	assert.Equal(t, 1, freshExamples[0].Facts["tokens"])
}

func TestUnregisterPrefixRemovesOnlyTheGivenPrefixes(t *testing.T) {
	t.Cleanup(func() { UnregisterPrefix("taskusage-keep-", "taskusage-drop-") })

	RegisterPrefix([]string{"taskusage-keep-", "taskusage-drop-"}, map[string]jsplugin.UsageFieldSchema{
		"tokens": {Type: "number", Unit: "token"},
	}, nil)

	UnregisterPrefix("taskusage-drop-")

	_, _, dropped := ForModel("taskusage-drop-3-0")
	assert.False(t, dropped)
	_, _, kept := ForModel("taskusage-keep-3-0")
	assert.True(t, kept)
}
