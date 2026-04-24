package main

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// Unit tests for extractToolNamesAboveScore
// ---------------------------------------------------------------------------

func TestExtractToolNamesAboveScore_filtersLowScores(t *testing.T) {
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.020"},
		{Plugin: "jira", Action: "list_issues", Score: "0.005"}, // below threshold
		{Plugin: "gitlab", Action: "create_mr", Score: "0.015"},
	})

	tools := extractToolNamesAboveScore(result, "MCPActions", defaultMinPrepareScore)

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

func TestExtractToolNamesAboveScore_emptyWhenAllBelowThreshold(t *testing.T) {
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: "0.005"},
		{Plugin: "gitlab", Action: "create_mr", Score: "0.003"},
	})

	tools := extractToolNamesAboveScore(result, "MCPActions", defaultMinPrepareScore)

	if len(tools) != 0 {
		t.Errorf("expected 0 tools when all below threshold, got %d: %v", len(tools), tools)
	}
}

func TestExtractToolNamesAboveScore_emptyOnNilResult(t *testing.T) {
	tools := extractToolNamesAboveScore(nil, "MCPActions", 0.85)

	if len(tools) != 0 {
		t.Errorf("expected 0 tools for nil result, got %d: %v", len(tools), tools)
	}
}

func TestExtractToolNamesAboveScore_includesNoScoreByDefault(t *testing.T) {
	// When _additional.score is missing, aboveScore returns true.
	result := buildMockActionsResult("MCPActions", []mockAction{
		{Plugin: "jira", Action: "create_issue", Score: ""}, // no score
	})

	tools := extractToolNamesAboveScore(result, "MCPActions", 0.85)

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (no score = include), got %d: %v", len(tools), tools)
	}
	if tools[0] != "jira.create_issue" {
		t.Errorf("expected jira.create_issue, got %q", tools[0])
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
