package main

import (
	"testing"

	"github.com/opentalon/opentalon/pkg/plugin"
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
	writes := []string{"sync_actions", "ingest", "ingest_batch", "sync_glossary", "refresh"}

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
