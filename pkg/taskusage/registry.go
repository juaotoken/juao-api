// Package taskusage holds the usage schemas of task models served by Go-native
// task adaptors. It is the counterpart of a JavaScript task plugin's
// meta.usageSchema / meta.usageProfiles: pricing APIs, the pricing editor, and
// expression save validation read a model's schema from here when no task
// plugin declares that model.
//
// Besides exact model names it accepts name prefixes, so a family whose
// provider keeps releasing new members (Seedance 3.0, 3.5, ...) stays priceable
// without a code change for every release. An exact registration always wins
// over a prefix, and the longest matching prefix wins over shorter ones.
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
	prefixes = make(map[string]profile)
)

// Register declares the usage schema and display examples of the given exact
// model names. A later registration for the same model replaces the earlier
// one, and an exact registration always wins over a matching prefix.
func Register(models []string, schema map[string]jsplugin.UsageFieldSchema, examples []jsplugin.UsageExample) {
	mu.Lock()
	defer mu.Unlock()
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		profiles[model] = newProfile(schema, examples)
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

// RegisterPrefix declares the usage schema and examples shared by every exact
// model name starting with one of the given prefixes, for a provider family
// whose members are not all known in advance. A later registration for the
// same prefix replaces the earlier one. Existing exact registrations are
// unaffected: ForModel still prefers them.
func RegisterPrefix(prefixList []string, schema map[string]jsplugin.UsageFieldSchema, examples []jsplugin.UsageExample) {
	mu.Lock()
	defer mu.Unlock()
	for _, prefix := range prefixList {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		prefixes[prefix] = newProfile(schema, examples)
	}
}

// UnregisterPrefix removes the prefix registrations of the given prefixes.
func UnregisterPrefix(prefixList ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, prefix := range prefixList {
		delete(prefixes, strings.TrimSpace(prefix))
	}
}

func newProfile(schema map[string]jsplugin.UsageFieldSchema, examples []jsplugin.UsageExample) profile {
	return profile{schema: jsplugin.CloneUsageSchema(schema), examples: jsplugin.CloneUsageExamples(examples)}
}

// ForModel returns independent copies of the usage schema and examples
// registered for a model name, either exactly or through the longest matching
// prefix.
func ForModel(model string) (map[string]jsplugin.UsageFieldSchema, []jsplugin.UsageExample, bool) {
	model = strings.TrimSpace(model)
	mu.RLock()
	defer mu.RUnlock()
	entry, ok := profiles[model]
	if !ok {
		entry, ok = longestPrefixProfile(model)
	}
	if !ok {
		return nil, nil, false
	}
	return jsplugin.CloneUsageSchema(entry.schema), jsplugin.CloneUsageExamples(entry.examples), true
}

// longestPrefixProfile returns the profile of the longest registered prefix
// model starts with. A nested family such as "doubao-seedance-2-0-" must win
// over "doubao-seedance-", whatever order the two were registered in.
func longestPrefixProfile(model string) (profile, bool) {
	var (
		best     profile
		bestLen  int
		bestSeen bool
	)
	for prefix, entry := range prefixes {
		if len(prefix) <= bestLen || !strings.HasPrefix(model, prefix) {
			continue
		}
		best, bestLen, bestSeen = entry, len(prefix), true
	}
	return best, bestSeen
}
