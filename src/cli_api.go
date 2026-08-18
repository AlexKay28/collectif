package main

// cli_api.go — #46 Phase 3: GET /api/cli endpoint.
//
// Enumerates every registered CLIAdapter and returns their capabilities so the
// frontend can (a) populate the spawn-modal picker and (b) decide which
// per-session panels to render or degrade.
//
// The Version() call shells out to the underlying binary; that's ~O(fork+exec)
// on every request, so we cache the result per-adapter for 60s. The cache is
// keyed by adapter Name() and lives at package level — the adapters map is
// write-once-during-init, so no map-level lock is needed. A single sync.Mutex
// guards the cache itself.

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// cliListResponse describes one adapter in the /api/cli response payload.
// JSON tags match the shape the frontend expects — see index.html / app.js.
type cliListResponse struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	IsDefault    bool         `json:"isDefault"`
	Capabilities Capabilities `json:"capabilities"`
}

// MarshalJSON on Capabilities keeps the wire keys camelCased to match the rest
// of the API surface. Keeping this local (rather than on the struct itself)
// avoids polluting the internal Capabilities type with wire concerns.
func (c Capabilities) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]bool{
		"hooks":                c.Hooks,
		"structuredTranscript": c.StructuredTranscript,
		"toolCallEvents":       c.ToolCallEvents,
		"subagentFiles":        c.SubagentFiles,
		"preCompact":           c.PreCompact,
		"sessionIdPinning":     c.SessionIDPinning,
		// #56: the session view is the notebook now, so the frontend needs
		// to know which adapters can produce one. A session with no
		// document is either waiting for its first turn or backed by a CLI
		// whose transcript we cannot read, and those want different words.
		"transcriptContent": c.TranscriptContent,
	})
}

// versionCacheTTL bounds how stale a cached `<cli> --version` result can be.
// Small enough that a user who just upgraded their CLI sees the new version
// within a minute; large enough that the endpoint doesn't fork on every hit.
const versionCacheTTL = 60 * time.Second

type versionCacheEntry struct {
	version string
	at      time.Time
}

var (
	versionCacheMu sync.Mutex
	versionCache   = map[string]versionCacheEntry{}
)

// cachedVersion returns the adapter's Version() result, calling through the
// adapter at most once per versionCacheTTL. Errors are swallowed (the adapter
// contract says Version may return "" + nil), which means a broken CLI shows
// up in the UI as `unknown` rather than 500ing the endpoint.
func cachedVersion(a CLIAdapter) string {
	name := a.Name()
	versionCacheMu.Lock()
	if e, ok := versionCache[name]; ok && time.Since(e.at) < versionCacheTTL {
		versionCacheMu.Unlock()
		return e.version
	}
	versionCacheMu.Unlock()
	// Shell out outside the lock so a slow `--version` doesn't block other
	// adapters' cache reads.
	v, _ := a.Version()
	versionCacheMu.Lock()
	versionCache[name] = versionCacheEntry{version: v, at: time.Now()}
	versionCacheMu.Unlock()
	return v
}

// resetVersionCache clears the cache. Test-only seam — call from a test that
// needs to observe cache behaviour without waiting out the TTL.
func resetVersionCache() {
	versionCacheMu.Lock()
	versionCache = map[string]versionCacheEntry{}
	versionCacheMu.Unlock()
}

// handleCLIList implements GET /api/cli. Returns every registered adapter,
// sorted with the default first and the rest alphabetically so the frontend
// can render a stable picker without re-sorting.
func handleCLIList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Snapshot names first so we don't allocate the response slice under a
	// map-iteration order that might drift with future adapter registrations.
	names := make([]string, 0, len(adapters))
	for n := range adapters {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		// Default adapter always first — it's the pre-selected picker
		// entry, and users mostly want that on top regardless of alpha
		// order.
		if names[i] == defaultAdapterName {
			return true
		}
		if names[j] == defaultAdapterName {
			return false
		}
		return names[i] < names[j]
	})

	out := make([]cliListResponse, 0, len(names))
	for _, n := range names {
		a := adapters[n]
		out = append(out, cliListResponse{
			Name:         a.Name(),
			Version:      cachedVersion(a),
			IsDefault:    a.Name() == defaultAdapterName,
			Capabilities: a.Capabilities(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
