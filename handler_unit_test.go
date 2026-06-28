package main

import (
	"testing"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/opentalon/opentalon/pkg/plugin"
	wmodels "github.com/weaviate/weaviate/entities/models"
)

// TestCapabilities_readOnlyAndPinClassification pins the confirmation-gate and
// Tier-0 classification declared by Capabilities(): every pure-read action sets
// ReadOnly so the host skips the per-call confirmation prompt (and any downstream
// narration step) on lookups; write actions must NOT, so they stay gated.
// ask_knowledge is additionally AlwaysInclude so the host pins it to Tier 0 — the
// always-available knowledge-pull lever, mirroring the get_tool_details meta-tool.
//
// This is a pure in-memory assertion over Capabilities() (no Weaviate / network),
// so it lives in an un-tagged unit file and runs on every build without the
// integration suite's live-Weaviate TestMain.
func TestCapabilities_readOnlyAndPinClassification(t *testing.T) {
	caps := (&WeaviateHandler{}).Capabilities()

	byName := make(map[string]plugin.ActionMsg, len(caps.Actions))
	for _, a := range caps.Actions {
		byName[a.Name] = a
	}

	reads := []string{"search", "hybrid_search", "ask_knowledge", "list_knowledge_titles", "search_instructions", "sync_status"}
	writes := []string{"sync_actions", "ingest", "ingest_batch", "refresh"}

	for _, name := range reads {
		a, ok := byName[name]
		if !ok {
			t.Fatalf("read action %q missing from capabilities", name)
		}
		if !a.ReadOnly {
			t.Errorf("read action %q must be ReadOnly so the host skips the confirmation gate", name)
		}
	}
	for _, name := range writes {
		a, ok := byName[name]
		if !ok {
			t.Fatalf("write action %q missing from capabilities", name)
		}
		if a.ReadOnly {
			t.Errorf("write action %q must NOT be ReadOnly — it mutates state and must stay gated", name)
		}
	}

	if !byName["ask_knowledge"].AlwaysInclude {
		t.Error("ask_knowledge must be AlwaysInclude (pinned to Tier 0 as the knowledge-pull lever)")
	}
}

// TestCapabilities_knowledgeSlugAndCatalog pins the knowledge-access primitive's
// surface: ask_knowledge exposes an exact-fetch `slug` param (and `query` is no
// longer mandatory, since either alone is valid), and list_knowledge_titles is
// declared so the orchestrator can render the always-on title catalog.
// Pure in-memory assertion over Capabilities() — no Weaviate, runs every build.
func TestCapabilities_knowledgeSlugAndCatalog(t *testing.T) {
	caps := (&WeaviateHandler{}).Capabilities()

	byName := make(map[string]plugin.ActionMsg, len(caps.Actions))
	for _, a := range caps.Actions {
		byName[a.Name] = a
	}

	// list_knowledge_titles exists (the catalog source the core renders).
	if _, ok := byName["list_knowledge_titles"]; !ok {
		t.Fatal("list_knowledge_titles action missing from capabilities")
	}

	// ask_knowledge: slug param present, query present but no longer required.
	ask, ok := byName["ask_knowledge"]
	if !ok {
		t.Fatal("ask_knowledge action missing from capabilities")
	}
	params := make(map[string]plugin.ParameterMsg, len(ask.Parameters))
	for _, p := range ask.Parameters {
		params[p.Name] = p
	}
	if _, ok := params["slug"]; !ok {
		t.Error("ask_knowledge must expose a `slug` param for exact-article fetch")
	}
	if q, ok := params["query"]; !ok {
		t.Error("ask_knowledge must still expose a `query` param")
	} else if q.Required {
		t.Error("ask_knowledge `query` must be optional now that `slug` is an alternative")
	}
}

// TestSplitChangedDocs pins the per-doc skip decision that drives re-vectorization:
// a doc is re-written only when its contentHash is absent from or differs from
// what's stored, and skipped when it matches exactly. This is the efficiency
// guarantee of the per-document hashing — an unchanged sync must write nothing.
// Pure function, no Weaviate, runs in the unit suite.
func TestSplitChangedDocs(t *testing.T) {
	doc := func(id, hash string) *wmodels.Object {
		return &wmodels.Object{
			ID:         strfmt.UUID(id),
			Properties: map[string]interface{}{"contentHash": hash},
		}
	}
	const (
		idNew       = "11111111-1111-1111-1111-111111111111"
		idChanged   = "22222222-2222-2222-2222-222222222222"
		idUnchanged = "33333333-3333-3333-3333-333333333333"
	)
	candidates := []*wmodels.Object{
		doc(idNew, "hash-new"),         // absent from stored → changed (new doc)
		doc(idChanged, "hash-current"), // stored hash differs → changed
		doc(idUnchanged, "hash-same"),  // stored hash equal → skipped
	}
	stored := map[string]string{
		idChanged:   "hash-OLD",
		idUnchanged: "hash-same",
	}

	changed, skipped := splitChangedDocs(candidates, stored)
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (only the unchanged doc)", skipped)
	}
	gotChanged := map[string]bool{}
	for _, o := range changed {
		gotChanged[string(o.ID)] = true
	}
	if len(changed) != 2 || !gotChanged[idNew] || !gotChanged[idChanged] {
		t.Errorf("changed ids = %v, want exactly {new, changed}", gotChanged)
	}

	// Read-error fallback: filterUnchangedDocs leaves stored empty when the hash
	// read fails, so every candidate must be treated as changed (re-vectorize,
	// never silently skip an update).
	allChanged, noneSkipped := splitChangedDocs(candidates, map[string]string{})
	if len(allChanged) != len(candidates) || noneSkipped != 0 {
		t.Errorf("empty stored: changed=%d skipped=%d, want changed=%d skipped=0",
			len(allChanged), noneSkipped, len(candidates))
	}
}

// ---------------------------------------------------------------------------
// Unit tests for timeout config parsing. Configure with auto_create_schema=false
// touches no network, so these run in the unit suite.
// ---------------------------------------------------------------------------

func TestConfigureTimeout_default(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.clientTimeout != 2*time.Minute {
		t.Errorf("expected default 2m, got %s", h.clientTimeout)
	}
}

func TestConfigureTimeout_customDuration(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"timeout":"5m"}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.clientTimeout != 5*time.Minute {
		t.Errorf("expected 5m, got %s", h.clientTimeout)
	}
}

func TestConfigureTimeout_seconds(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"timeout":"90s"}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.clientTimeout != 90*time.Second {
		t.Errorf("expected 90s, got %s", h.clientTimeout)
	}
}

func TestConfigureTimeout_invalidFallsBackToDefault(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"timeout":"notaduration"}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.clientTimeout != 2*time.Minute {
		t.Errorf("expected default 2m for invalid timeout, got %s", h.clientTimeout)
	}
}

func TestConfigureTimeout_zeroFallsBackToDefault(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"timeout":"0s"}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.clientTimeout != 2*time.Minute {
		t.Errorf("expected default 2m for zero timeout, got %s", h.clientTimeout)
	}
}

func TestConfigureTimeout_negativeFallsBackToDefault(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"timeout":"-5m"}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.clientTimeout != 2*time.Minute {
		t.Errorf("expected default 2m for negative timeout, got %s", h.clientTimeout)
	}
}
