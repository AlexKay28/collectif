package main

// provider_api.go — what the browser is told about the transports. #53.
//
// Same shape as nb_fidelity.go and for the same reason. A capability is a
// property of the build, not of the document, so none of this is folded,
// stored or snapshotted: it is derived on every read and would otherwise
// go on asserting "this transport cannot report caching" after somebody
// configured one that can.
//
// The specific wrongness this exists to prevent: M2.5 renders a cache
// percentage on every prompt cell, deliberately as a warning at zero,
// because a re-run serving nothing from cache means the projected prefix
// stopped matching. On Ollama, llama.cpp or vLLM there is no cached-token
// count at all, so that same chip would read "0% cached" on every cell
// forever — a miss that never happened, pointing at a bug that is not
// there.

import (
	"net/http"
)

// NotebookProvider is the transport a detached notebook's prompt cells run
// on. Nil for a mirrored session, where the CLI is the agent and chose its
// own provider long before collectif attached (ADR 0002 D10) — claiming
// one there would be the same fiction D11 rules out for projection.
type NotebookProvider struct {
	Name         string               `json:"name"`
	Model        string               `json:"model,omitempty"`
	Capabilities ProviderCapabilities `json:"capabilities"`
}

// providerInfoFor resolves the notebook's default transport.
func providerInfoFor(doc *Notebook) *NotebookProvider {
	if doc == nil || doc.Meta.SessionID != "" {
		return nil
	}
	p := providerForModel(doc.Meta.Model)
	if p == nil {
		// Nothing configured. The run path already answers 503 with an
		// explanation; there is nothing truthful to say here.
		return nil
	}
	model := doc.Meta.Model
	if model == "" {
		if catalog := p.Models(); len(catalog) > 0 {
			model = catalog[0].ID
		}
	}
	return &NotebookProvider{Name: p.Name(), Model: model, Capabilities: p.Capabilities()}
}

// annotateCacheModes fills in each prompt cell's cache mode.
//
// Per cell rather than per notebook because a cell can name its own model
// and so its own transport — the cache claim shown against a cell has to
// be the claim of whatever answered it. Resolutions are memoised by model
// id: this runs on every document read, and a long notebook whose cells
// all use the default would otherwise walk every catalog once per cell.
func annotateCacheModes(doc *Notebook) {
	if doc == nil || doc.Meta.SessionID != "" {
		return
	}
	seen := map[string]CacheMode{}
	for i := range doc.Cells {
		if doc.Cells[i].Type != CellPrompt {
			continue
		}
		model := doc.Cells[i].Meta.Model
		if model == "" {
			model = doc.Meta.Model
		}
		mode, ok := seen[model]
		if !ok {
			if p := providerForModel(model); p != nil {
				mode = p.Capabilities().Cache
			}
			seen[model] = mode
		}
		doc.Cells[i].CacheMode = mode
	}
}

// ─── GET /api/providers ─────────────────────────────────────────────────

// providerListResponse is one configured transport, its catalog and what
// it can do. ADR 0001 §5 specified the endpoint; ADR 0002's note on #53
// specified that it follow the derived-never-stored pattern.
type providerListResponse struct {
	Name         string               `json:"name"`
	IsDefault    bool                 `json:"isDefault"`
	Capabilities ProviderCapabilities `json:"capabilities"`
	Models       []ModelInfo          `json:"models"`
}

func handleProviderList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// An empty list, never an error: "no provider is configured" is a
	// state the picker has to render, and a 503 here would make a
	// perfectly usable session notebook look broken.
	out := make([]providerListResponse, 0, len(activeProviders))
	for _, p := range activeProviders {
		out = append(out, providerListResponse{
			Name:         p.Name(),
			IsDefault:    p == activeProvider,
			Capabilities: p.Capabilities(),
			Models:       p.Models(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
