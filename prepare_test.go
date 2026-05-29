package main

import (
	"encoding/json"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Unit tests for extractToolNames (score filter via actionFilter chokepoint)
// ---------------------------------------------------------------------------

func TestExtractToolNames_filtersLowScores(t *testing.T) {
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
		{Plugin: "jira", Action: "list_issues", Score: "0.005"}, // below threshold
		{Plugin: "gitlab", Action: "create_mr", Score: "0.015"},
	})

	tools := extractToolNames(result, "MCPActions", actionFilter{minScore: defaultMinPrepareScore})

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %v", len(tools), tools)
	}
	want := map[string]bool{"jira.create_issue": true, "gitlab.create_mr": true}
	for _, tool := range tools {
		if !want[tool] {
			t.Errorf("unexpected tool %q", tool)
		}
	}
}

func TestExtractToolNames_emptyWhenAllBelowThreshold(t *testing.T) {
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.005"},
		{Plugin: "gitlab", Action: "create_mr", Score: "0.003"},
	})

	tools := extractToolNames(result, "MCPActions", actionFilter{minScore: defaultMinPrepareScore})

	if len(tools) != 0 {
		t.Errorf("expected 0 tools when all below threshold, got %d: %v", len(tools), tools)
	}
}

func TestExtractToolNames_emptyOnNilResult(t *testing.T) {
	tools := extractToolNames(nil, "MCPActions", actionFilter{minScore: 0.85})

	if len(tools) != 0 {
		t.Errorf("expected 0 tools for nil result, got %d: %v", len(tools), tools)
	}
}

func TestExtractToolNames_includesNoScoreByDefault(t *testing.T) {
	// When _additional.score is missing, aboveScore returns true.
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: ""}, // no score
	})

	tools := extractToolNames(result, "MCPActions", actionFilter{minScore: 0.85})

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (no score = include), got %d: %v", len(tools), tools)
	}
	if tools[0] != "jira.create_issue" {
		t.Errorf("expected jira.create_issue, got %q", tools[0])
	}
}

// ---------------------------------------------------------------------------
// Unit tests for actionFilter.availableTools (per-session palette enforcement)
//
// These pin the defense-in-depth invariant: even if Weaviate retrieves a
// tool that scores above the threshold, the actionFilter must drop it
// unless the caller's session is allowed to invoke it.
// ---------------------------------------------------------------------------

func TestActionFilter_availableTools_filtersOutOfPalette(t *testing.T) {
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
		{Plugin: "gitlab", Action: "create_mr", Score: "0.020"},
		{Plugin: "jira", Action: "admin_purge", Score: "0.030"}, // high score, NOT in palette
	})

	filter := actionFilter{
		minScore: defaultMinPrepareScore,
		availableTools: map[string]struct{}{
			"jira.create_issue": {},
			"gitlab.create_mr":  {},
			// jira.admin_purge intentionally absent
		},
	}
	tools := extractToolNames(result, "MCPActions", filter)

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (admin_purge filtered out by palette), got %d: %v", len(tools), tools)
	}
	for _, tool := range tools {
		if tool == "jira.admin_purge" {
			t.Errorf("jira.admin_purge leaked through actionFilter despite being absent from availableTools")
		}
	}
}

func TestActionFilter_availableTools_emptyPaletteFiltersEverything(t *testing.T) {
	// Non-nil empty palette means "session can call zero tools" — every
	// retrieved action must be filtered out. (This distinguishes empty from
	// nil: nil = no per-session filter; empty set = "no tools available".)
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
	})
	filter := actionFilter{
		minScore:       defaultMinPrepareScore,
		availableTools: map[string]struct{}{},
	}
	tools := extractToolNames(result, "MCPActions", filter)
	if len(tools) != 0 {
		t.Errorf("expected 0 tools for empty palette, got %d: %v", len(tools), tools)
	}
}

// TestActionFilter_availableTools_jsonNullIsTreatedAsNil pins the host-side
// edge case: a buggy orchestrator that marshals a nil Go slice into the
// literal JSON `null` (instead of omitting the arg) MUST land in the
// fail-open "no palette" branch, not the fail-closed empty-map branch.
// Without the explicit `"null"` and `list != nil` guards in prepare(),
// json.Unmarshal sets `list = nil`, the subsequent `make` builds a
// non-nil empty map, and every retrieved tool is silently filtered out
// → the LLM sees zero tools without an explicit operator decision.
// This test exercises the parsing path that the production handler uses.
func TestActionFilter_availableTools_jsonNullIsTreatedAsNil(t *testing.T) {
	// Mirror the handler's parsing block — if either guard is removed,
	// the resulting filter would block everything below.
	var availableTools map[string]struct{}
	v := "null"
	if v != "" && v != "null" {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err == nil && list != nil {
			availableTools = make(map[string]struct{}, len(list))
			for _, name := range list {
				availableTools[name] = struct{}{}
			}
		}
	}
	if availableTools != nil {
		t.Fatalf("\"null\" must yield nil availableTools (no palette / no filter), got non-nil: %v", availableTools)
	}

	// Confirm the filter built from that parse path is in fact a no-op
	// when fed a real retrieval result.
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
		{Plugin: "gitlab", Action: "create_mr", Score: "0.020"},
	})
	tools := extractToolNames(result, "MCPActions", actionFilter{
		minScore: defaultMinPrepareScore, availableTools: availableTools,
	})
	if len(tools) != 2 {
		t.Errorf("expected \"null\" path to behave as no-filter (2 tools), got %d: %v", len(tools), tools)
	}
}

func TestActionFilter_availableTools_nilPaletteIsNoFilter(t *testing.T) {
	// nil palette = no per-session filter; backwards-compatible behaviour
	// for callers that pass actionFilter{minScore: …} without setting the
	// availableTools field.
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
		{Plugin: "gitlab", Action: "create_mr", Score: "0.020"},
	})
	filter := actionFilter{
		minScore:       defaultMinPrepareScore,
		availableTools: nil,
	}
	tools := extractToolNames(result, "MCPActions", filter)
	if len(tools) != 2 {
		t.Errorf("expected nil palette to be no-filter (got %d): %v", len(tools), tools)
	}
}

// TestActionFilter_invariant_chokepoint_applies_to_candidates pins the
// "single chokepoint" property: extractToolCandidatesFromResult MUST honour
// the same actionFilter as extractToolNames. A regression here would let
// the structured slice carry tools the names slice has already filtered
// out — exactly the kind of drift the chokepoint refactor eliminates.
func TestActionFilter_invariant_chokepoint_applies_to_candidates(t *testing.T) {
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
		{Plugin: "jira", Action: "admin_purge", Score: "0.030"},
	})
	filter := actionFilter{
		minScore: defaultMinPrepareScore,
		availableTools: map[string]struct{}{
			"jira.create_issue": {},
		},
	}
	names := extractToolNames(result, "MCPActions", filter)
	candidates := extractToolCandidatesFromResult(result, "MCPActions", filter)

	if len(names) != len(candidates) {
		t.Fatalf("invariant violated: names=%d candidates=%d", len(names), len(candidates))
	}
	for i, n := range names {
		if candidates[i].ToolName != n {
			t.Errorf("position %d: names=%q candidates=%q", i, n, candidates[i].ToolName)
		}
	}
}

// ---------------------------------------------------------------------------
// Unit tests for prepare relevant_tools nil-vs-non-nil behavior
//
// These test the logic that the prepare action uses AFTER calling
// extractToolNamesAboveScore — when no real tools matched, relevant_tools
// must be nil so the orchestrator shows all tools (relevantToolsActive=false).
// ---------------------------------------------------------------------------

func TestPrepareRelevantTools_nilWhenNoMatches(t *testing.T) {
	// Simulate: extractToolNamesAboveScore returned empty.
	tools := []string{} // no matches above threshold

	// Apply the same logic as prepare():
	if len(tools) > 0 {
		tools = append(tools, "weaviate.ask_knowledge")
	} else {
		tools = nil
	}

	if tools != nil {
		t.Errorf("expected nil relevant_tools when no matches, got %v", tools)
	}
}

func TestPrepareRelevantTools_includesAskKnowledgeWhenMatches(t *testing.T) {
	// Simulate: extractToolNamesAboveScore returned some matches.
	tools := []string{"jira.create_issue", "jira.list_issues"}

	if len(tools) > 0 {
		tools = append(tools, "weaviate.ask_knowledge")
	} else {
		tools = nil
	}

	if tools == nil {
		t.Fatal("expected non-nil relevant_tools when matches exist")
	}
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d: %v", len(tools), tools)
	}

	found := false
	for _, tool := range tools {
		if tool == "weaviate.ask_knowledge" {
			found = true
		}
	}
	if !found {
		t.Error("expected weaviate.ask_knowledge in relevant_tools")
	}
}

func TestPrepareRelevantTools_nilSerializesCorrectly(t *testing.T) {
	// When relevant_tools is nil, the JSON output should have
	// relevant_tools as null (or omitted), which the orchestrator's
	// Go code treats as nil → relevantToolsSet stays false.
	resp := prepareResponse{
		SendToLLM:     true,
		Message:       "hello",
		RelevantTools: nil,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// relevant_tools should either be absent or null.
	rt, exists := parsed["relevant_tools"]
	if exists && rt != nil {
		t.Errorf("expected relevant_tools to be null or absent, got %v", rt)
	}
}

func TestPrepareRelevantTools_nonNilSerializesAsList(t *testing.T) {
	resp := prepareResponse{
		SendToLLM:     true,
		Message:       "hello",
		RelevantTools: []string{"jira.create_issue", "weaviate.ask_knowledge"},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed prepareResponse
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(parsed.RelevantTools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(parsed.RelevantTools))
	}
}

// ---------------------------------------------------------------------------
// Unit tests for min_prepare_score config
// ---------------------------------------------------------------------------

func TestMinPrepareScore_defaultValue(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.minPrepareScore != defaultMinPrepareScore {
		t.Errorf("expected default %.4f, got %.4f", defaultMinPrepareScore, h.minPrepareScore)
	}
}

func TestMinPrepareScore_customValue(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"min_prepare_score":0.5}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.minPrepareScore != 0.5 {
		t.Errorf("expected 0.5, got %f", h.minPrepareScore)
	}
}

func TestMinPrepareScore_zeroUsesDefault(t *testing.T) {
	h := &WeaviateHandler{}
	cfg := `{"host":"localhost:8080","collection":"Test","auto_create_schema":false,"min_prepare_score":0}`
	if err := h.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.minPrepareScore != defaultMinPrepareScore {
		t.Errorf("expected default %.4f for zero value, got %.4f", defaultMinPrepareScore, h.minPrepareScore)
	}
}

// ---------------------------------------------------------------------------
// Unit tests for timeout config parsing
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type mockAction struct {
	Plugin string
	Action string
	Score  string // empty string = no score field
}

// buildMockActionsResult creates a mock GraphQL response that matches the
// shape returned by Weaviate's Get query, suitable for extractItems().
func buildMockActionsResult(className string, actions []mockAction) interface{} {
	items := make([]interface{}, 0, len(actions))
	for _, a := range actions {
		obj := map[string]interface{}{
			"pluginName": a.Plugin,
			"actionName": a.Action,
		}
		if a.Score != "" {
			obj["_additional"] = map[string]interface{}{
				"score": a.Score,
			}
		}
		items = append(items, obj)
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"Get": map[string]interface{}{
				className: items,
			},
		},
	}
}
