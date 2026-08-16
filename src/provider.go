package main

// provider.go — model metadata shared by every CLI adapter, and later by
// the native providers in M2 (#50).
//
// Introduced by #48. The context-pressure gauge previously read its limits
// from a package-level table in harness.go that nothing owned and nothing
// could verify; it had drifted to the point where every current model was
// reported as a 200k window, so a 1M-window session read five times high.
// Model metadata now belongs to whoever actually talks to the model.

import "strings"

// ModelInfo describes one model's budgets. ContextWindow is the total
// input+output window in tokens; MaxOutput is the per-response ceiling.
type ModelInfo struct {
	ID            string
	ContextWindow int
	MaxOutput     int
}

// defaultContextLimit is the fallback for a model we don't recognise.
//
// It stays deliberately conservative. An unrecognised id is more likely an
// older or smaller-window model than a newer one, and the failure modes are
// not symmetric: guessing too small warns a user who had room to spare,
// while guessing too large stays silent through a compaction. Warning early
// is the cheaper mistake.
const defaultContextLimit = 200_000

// lookupModel resolves a model id against a catalog by longest-prefix
// match, so a dated snapshot (claude-opus-4-7-20260115) resolves to the
// same entry as its alias (claude-opus-4-7) without needing its own row.
// Longest match wins so a more specific id is never shadowed by a shorter
// one that happens to share a prefix.
func lookupModel(catalog []ModelInfo, model string) (ModelInfo, bool) {
	if model == "" {
		return ModelInfo{}, false
	}
	best := -1
	for i, m := range catalog {
		if !strings.HasPrefix(model, m.ID) {
			continue
		}
		if best < 0 || len(m.ID) > len(catalog[best].ID) {
			best = i
		}
	}
	if best < 0 {
		return ModelInfo{}, false
	}
	return catalog[best], true
}

// contextWindowOr resolves a model's context window from a catalog,
// falling back to defaultContextLimit. Adapters use this so the fallback
// behaviour is identical across every CLI.
func contextWindowOr(catalog []ModelInfo, model string) int {
	if m, ok := lookupModel(catalog, model); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return defaultContextLimit
}
