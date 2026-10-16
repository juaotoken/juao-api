// Package taskusage holds the usage schemas of task models served by Go-native
// task adaptors. It is the counterpart of a JavaScript task plugin's
// meta.usageSchema / meta.usageProfiles: pricing APIs, the pricing editor, and
// expression save validation read a model's schema from here when no task
// plugin declares that model.
//
// The package is a leaf (it depends only on pkg/jsplugin for the schema types)
// so model can import it while adaptors under relay register into it.
package taskusage

import (
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

type profile struct {
	schema   map[string]jsplugin.UsageFieldSchema
	examples []jsplugin.UsageExample
}

var (
	mu       sync.RWMutex
	profiles = make(map[string]profile)
)

// Register declares the usage schema and display examples of the given exact
// model names. A later registration for the same model replaces the earlier
// one.
func Register(models []string, schema map[string]jsplugin.UsageFieldSchema, examples []jsplugin.UsageExample) {
	mu.Lock()
	defer mu.Unlock()
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		profiles[model] = profile{schema: jsplugin.CloneUsageSchema(schema), examples: jsplugin.CloneUsageExamples(examples)}
	}
}

// Unregister removes the registrations of the given models.
func Unregister(models ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, model := range models {
		delete(profiles, strings.TrimSpace(model))
	}
}

// ForModel returns independent copies of the usage schema and examples
// registered for an exact model name.
func ForModel(model string) (map[string]jsplugin.UsageFieldSchema, []jsplugin.UsageExample, bool) {
	mu.RLock()
	defer mu.RUnlock()
	entry, ok := profiles[strings.TrimSpace(model)]
	if !ok {
		return nil, nil, false
	}
	return jsplugin.CloneUsageSchema(entry.schema), jsplugin.CloneUsageExamples(entry.examples), true
}
