package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// #53 M4. Two transports mean two things the loop did not have to do
// before: pick one, and say what the one it picked can tell you.
//
// The picking is the per-cell model override — the thing ADR 0001 called
// "exactly the kind of knob a notebook is good at exposing" and the whole
// remaining draw of this phase under ADR 0002: run the cell above on a
// frontier model and this one on a local one, in the same document.

// secondProvider is a transport with a catalog of its own, so a model id
// can be routed by which catalog claims it.
type secondProvider struct {
	fakeProvider
}

func (p *secondProvider) Name() string { return "second" }
func (p *secondProvider) Models() []ModelInfo {
	return []ModelInfo{{ID: "local-model", ContextWindow: 32_000, MaxOutput: 4_000}}
}
func (p *secondProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{Cache: CacheNone, Reasoning: true, Usage: true}
}

func withProviders(t *testing.T, ps ...Provider) {
	t.Helper()
	prevList, prevDefault := activeProviders, activeProvider
	activeProviders = ps
	if len(ps) > 0 {
		activeProvider = ps[0]
	} else {
		activeProvider = nil
	}
	t.Cleanup(func() { activeProviders, activeProvider = prevList, prevDefault })
}

func TestProviderForModel_RoutesByWhicheverCatalogClaimsIt(t *testing.T) {
	first := &fakeProvider{}
	second := &secondProvider{}
	withProviders(t, first, second)

	if got := providerForModel("local-model"); got != Provider(second) {
		t.Errorf("local-model resolved to %v, want the transport whose catalog has it", got)
	}
	if got := providerForModel("fake-1"); got != Provider(first) {
		t.Errorf("fake-1 resolved to %v, want the first transport", got)
	}
	// A dated snapshot resolves like its alias — the same longest-prefix
	// rule the context-window lookup uses, so a pinned id does not fall
	// off the catalog.
	if got := providerForModel("local-model-20260101"); got != Provider(second) {
		t.Errorf("a dated id resolved to %v, want the same transport as its alias", got)
	}
	// An id nobody claims is not an error: every local server has model
	// ids that exist only on that machine, so the default transport takes
	// it and the endpoint gets to answer for itself.
	if got := providerForModel("something-nobody-catalogued"); got != Provider(first) {
		t.Errorf("an unknown id resolved to %v, want the default transport", got)
	}
	if got := providerForModel(""); got != Provider(first) {
		t.Errorf("an empty id resolved to %v, want the default transport", got)
	}
}

// The per-cell override, end to end: the notebook runs on one transport
// and the cell that names another model runs on that one.
func TestPromptCell_ModelOverrideChoosesTheTransport(t *testing.T) {
	f := newNBFixture(t)
	first := &fakeProvider{turns: []scriptedTurn{{text: "from the default"}}}
	second := &secondProvider{fakeProvider: fakeProvider{turns: []scriptedTurn{{text: "from the local one"}}}}
	withProviders(t, first, second)

	cell := f.addCell(t, "prompt", "run me somewhere else")
	rec := nbRequest(t, f.srv, http.MethodPatch, f.base+"/cells/"+cell,
		map[string]any{"meta": map[string]any{"model": "local-model", "effort": "low"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set cell meta: %d %s", rec.Code, rec.Body.String())
	}

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	if n := len(first.sent()); n != 0 {
		t.Errorf("the default transport was called %d times — the cell's model override was ignored", n)
	}
	sent := second.sent()
	if len(sent) != 1 {
		t.Fatalf("the overridden transport was called %d times, want 1", len(sent))
	}
	if sent[0].Model != "local-model" {
		t.Errorf("Model = %q, want the cell's override", sent[0].Model)
	}
	if sent[0].Effort != "low" {
		t.Errorf("Effort = %q, want the cell's override", sent[0].Effort)
	}
}

// modelInfoFor decides the pre-flight context check and whether a dollar
// budget can be enforced at all. It matched ids exactly, so every dated
// snapshot — which is what a notebook pins when it wants a reproducible
// run — resolved to no pricing and no window.
func TestModelInfoFor_ResolvesDatedSnapshotsLikeTheirAlias(t *testing.T) {
	p := &anthropicProvider{}
	info := modelInfoFor(p, "claude-opus-5-20260115")
	if info.ContextWindow != 1_000_000 {
		t.Errorf("context window = %d, want the alias's 1000000", info.ContextWindow)
	}
	if info.InputUSDPerMTok == 0 {
		t.Error("no pricing — a notebook with a dollar budget would refuse to run on a pinned model")
	}
	// An id from no catalog still gets a conservative window rather than
	// none, so the pre-flight check happens either way.
	if got := modelInfoFor(p, "some-local-thing").ContextWindow; got != defaultContextLimit {
		t.Errorf("unknown model window = %d, want the conservative default", got)
	}
}

// ─── What the notebook says about its transport ─────────────────────────

// M2.5 puts a cache figure on every prompt cell so that a zero is
// noticed. On a transport with no cache reporting that zero is not
// evidence of anything, and showing it sends the user hunting for a bug
// that is not there. The document has to carry the difference.
func TestNotebook_ReportsWhatItsTransportCanSayAboutCaching(t *testing.T) {
	f := newNBFixture(t)
	withProviders(t, &secondProvider{fakeProvider: fakeProvider{turns: []scriptedTurn{{text: "done"}}}})

	cell := f.addCell(t, "prompt", "a question")
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	doc := f.st.Doc()
	if doc.Provider == nil {
		t.Fatal("a detached notebook did not say which transport runs it")
	}
	if doc.Provider.Name != "second" {
		t.Errorf("provider name = %q", doc.Provider.Name)
	}
	if doc.Provider.Capabilities.Cache != CacheNone {
		t.Errorf("cache mode = %q, want none", doc.Provider.Capabilities.Cache)
	}
	i := indexOfCell(doc, cell)
	if got := doc.Cells[i].CacheMode; got != CacheNone {
		t.Errorf("cell cache mode = %q, want %q — the cell would show 0%% cached, which reads as a miss and then as a bug",
			got, CacheNone)
	}
}

// The same cell on a transport with breakpoints keeps the warning it was
// built for: zero there really does mean the prefix stopped matching.
func TestNotebook_KeepsTheColdCacheWarningWhereItMeansSomething(t *testing.T) {
	f := newNBFixture(t)
	withProviders(t, &fakeProvider{turns: []scriptedTurn{{text: "done"}}})

	cell := f.addCell(t, "prompt", "a question")
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	doc := f.st.Doc()
	if got := doc.Cells[indexOfCell(doc, cell)].CacheMode; got != CacheExplicit {
		t.Errorf("cell cache mode = %q, want %q", got, CacheExplicit)
	}
}

// A cell that names another model is answered by another transport, so
// the cache claim shown against it has to be that transport's.
func TestNotebook_CacheClaimFollowsTheCellsOwnModel(t *testing.T) {
	f := newNBFixture(t)
	withProviders(t, &fakeProvider{}, &secondProvider{})

	cell := f.addCell(t, "prompt", "a question")
	nbRequest(t, f.srv, http.MethodPatch, f.base+"/cells/"+cell,
		map[string]any{"meta": map[string]any{"model": "local-model"}})

	doc := f.st.Doc()
	if got := doc.Cells[indexOfCell(doc, cell)].CacheMode; got != CacheNone {
		t.Errorf("cell cache mode = %q, want the overridden transport's %q", got, CacheNone)
	}
}

// A mirrored session has no provider of ours at all: the CLI is the agent
// and it chose its own. Claiming one would be the same fiction ADR 0002
// D11 rules out for projection fidelity.
func TestNotebook_SaysNothingAboutAProviderItDoesNotRun(t *testing.T) {
	f := newNBFixture(t)
	withProviders(t, &fakeProvider{})
	if _, err := f.st.Append(evMetaSet, metaSetPayload{
		Meta: &NotebookMeta{SessionID: "sess-1", CLI: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if p := f.st.Doc().Provider; p != nil {
		t.Errorf("a session notebook claimed provider %+v", p)
	}
}

// ─── GET /api/providers ─────────────────────────────────────────────────

// The endpoint ADR 0001 §5 specified, following NotebookFidelity's
// pattern: derived on every read, stored nowhere.
func TestProvidersAPI_ListsEachTransportWithItsCatalogAndCapabilities(t *testing.T) {
	f := newNBFixture(t)
	withProviders(t, &fakeProvider{}, &secondProvider{})

	rec := nbRequest(t, f.srv, http.MethodGet, "/api/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/providers = %d %s", rec.Code, rec.Body.String())
	}
	var out []providerListResponse
	decodeJSON(t, rec, &out)
	if len(out) != 2 {
		t.Fatalf("got %d providers, want 2: %+v", len(out), out)
	}
	if !out[0].IsDefault || out[1].IsDefault {
		t.Errorf("default flag = %v/%v, want it on the first only", out[0].IsDefault, out[1].IsDefault)
	}
	if out[1].Name != "second" || len(out[1].Models) != 1 || out[1].Models[0].ID != "local-model" {
		t.Errorf("second provider = %+v, want its own catalog", out[1])
	}
	if out[1].Capabilities.Cache != CacheNone {
		t.Errorf("capabilities = %+v, want the transport's own claim", out[1].Capabilities)
	}
}

// With nothing configured the answer is an empty list rather than an
// error: "no provider" is a state the UI has to render, not a failure.
func TestProvidersAPI_EmptyWhenNothingIsConfigured(t *testing.T) {
	f := newNBFixture(t)
	withProviders(t)

	rec := nbRequest(t, f.srv, http.MethodGet, "/api/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/providers = %d", rec.Code)
	}
	var out []providerListResponse
	decodeJSON(t, rec, &out)
	if len(out) != 0 {
		t.Errorf("got %+v, want an empty list", out)
	}
}

// initProviders installs every transport that is configured, not the first
// one it finds — a machine with both an Anthropic key and a local Ollama
// is the case this phase exists for.
func TestInitProviders_InstallsEveryConfiguredTransport(t *testing.T) {
	prevP, prevList, prevT := activeProvider, activeProviders, activeTools
	t.Cleanup(func() { activeProvider, activeProviders, activeTools = prevP, prevList, prevT })

	for _, env := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BEARER_TOKEN", "OPENAI_BASE_URL", "OPENAI_API_KEY"} {
		t.Setenv(env, "")
	}
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir())
	t.Setenv("OLLAMA_HOST", "localhost:11434")

	activeProvider, activeProviders = nil, nil
	initProviders()

	if len(activeProviders) != 1 {
		t.Fatalf("installed %d transports, want 1 (the local one)", len(activeProviders))
	}
	if activeProvider == nil || activeProvider.Name() != "ollama" {
		t.Errorf("default transport = %v, want ollama", activeProvider)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	activeProvider, activeProviders = nil, nil
	initProviders()
	if len(activeProviders) != 2 {
		t.Fatalf("installed %d transports, want 2", len(activeProviders))
	}
	// Anthropic first: it is the transport with the fuller feature set,
	// and the default is what a notebook with no model setting gets.
	if activeProvider.Name() != "anthropic" {
		t.Errorf("default transport = %q, want anthropic", activeProvider.Name())
	}
}

// A guard on the seam itself: every provider the process installs has to
// answer the capability question, because the UI renders that answer
// rather than guessing from the numbers.
func TestActiveProviders_AllDeclareCapabilities(t *testing.T) {
	withProviders(t, &fakeProvider{}, &secondProvider{})
	for _, p := range activeProviders {
		if p.Capabilities().Cache == "" {
			t.Errorf("%s declares no cache mode", p.Name())
		}
	}
	_ = context.Background()
}
