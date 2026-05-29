package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestContentSHA256_emptyReturnsEmpty(t *testing.T) {
	if got := contentSHA256(""); got != "" {
		t.Errorf("contentSHA256(\"\") = %q, want \"\"", got)
	}
}

func TestContentSHA256_deterministicAndHex(t *testing.T) {
	a := contentSHA256("hello world")
	b := contentSHA256("hello world")
	if a != b {
		t.Errorf("contentSHA256 not deterministic: %q vs %q", a, b)
	}
	want := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("hello world")); return s[:] }())
	if a != want {
		t.Errorf("contentSHA256 = %q, want %q (sha256 hex of input)", a, want)
	}
	if len(a) != 64 {
		t.Errorf("contentSHA256 length = %d, want 64 (hex of 32-byte digest)", len(a))
	}
}

func TestContentSHA256_distinctInputsDiffer(t *testing.T) {
	a := contentSHA256("foo")
	b := contentSHA256("bar")
	if a == b {
		t.Errorf("contentSHA256(%q) == contentSHA256(%q) = %q — must differ", "foo", "bar", a)
	}
}

func TestJoinTitleAndContent(t *testing.T) {
	tests := []struct {
		name           string
		title, content string
		want           string
	}{
		{"both empty", "", "", ""},
		{"title only", "T", "", "T"},
		{"content only", "", "C", "C"},
		{"both present", "T", "C", "T\n\nC"},
		{"trailing whitespace trimmed", "T  \n", "C\r", "T\n\nC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinTitleAndContent(tt.title, tt.content); got != tt.want {
				t.Errorf("joinTitleAndContent(%q, %q) = %q, want %q", tt.title, tt.content, got, tt.want)
			}
		})
	}
}

func mkScore(s string) map[string]interface{} {
	return map[string]interface{}{"score": s}
}

func mkArticle(id, title, content, source, score string) map[string]interface{} {
	return map[string]interface{}{
		"title":       title,
		"content":     content,
		"source":      source,
		"_additional": map[string]interface{}{"id": id, "score": score},
	}
}

func TestExtractKnowledgeCandidates_emptyReturnsNil(t *testing.T) {
	if got := extractKnowledgeCandidates(nil, 0.0); got != nil {
		t.Errorf("extractKnowledgeCandidates(nil) = %+v, want nil", got)
	}
	if got := extractKnowledgeCandidates([]map[string]interface{}{}, 0.0); got != nil {
		t.Errorf("extractKnowledgeCandidates([]) = %+v, want nil", got)
	}
}

func TestExtractKnowledgeCandidates_dropsBelowThreshold(t *testing.T) {
	items := []map[string]interface{}{
		mkArticle("a-1", "T1", "C1", "src", "0.50"),
		mkArticle("a-2", "T2", "C2", "src", "0.10"), // below
		mkArticle("a-3", "T3", "C3", "src", "0.80"),
	}
	got := extractKnowledgeCandidates(items, 0.25)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2 (a-2 filtered)", len(got))
	}
	if got[0].ArticleID != "a-1" || got[1].ArticleID != "a-3" {
		t.Errorf("filtered candidates: got [%s, %s], want [a-1, a-3]", got[0].ArticleID, got[1].ArticleID)
	}
}

func TestExtractKnowledgeCandidates_positionIsPostFilterRank(t *testing.T) {
	// a-2 is filtered out by score; the remaining two must carry
	// PositionInResults 1 and 2 — NOT the pre-filter positions 1 and 3.
	// This matches the LLM's view of "rank N of what reached me".
	items := []map[string]interface{}{
		mkArticle("a-1", "T1", "C1", "", "0.50"),
		mkArticle("a-2", "T2", "C2", "", "0.10"),
		mkArticle("a-3", "T3", "C3", "", "0.80"),
	}
	got := extractKnowledgeCandidates(items, 0.25)
	if got[0].PositionInResults != 1 || got[1].PositionInResults != 2 {
		t.Errorf("positions = [%d, %d], want [1, 2]", got[0].PositionInResults, got[1].PositionInResults)
	}
}

func TestExtractKnowledgeCandidates_articleIDFromAdditional(t *testing.T) {
	items := []map[string]interface{}{mkArticle("uuid-1", "T", "C", "", "1.0")}
	got := extractKnowledgeCandidates(items, 0.0)
	if got[0].ArticleID != "uuid-1" {
		t.Errorf("ArticleID = %q, want %q", got[0].ArticleID, "uuid-1")
	}
}

func TestExtractKnowledgeCandidates_articleIDEmptyWhenAdditionalMissing(t *testing.T) {
	// A response where _additional is absent entirely (legacy weaviate
	// drivers or test fixtures) must yield ArticleID="" — the
	// orchestrator falls back to ContentSHA256 as the dedup key. Crash-
	// free degradation is the contract.
	items := []map[string]interface{}{{"title": "T", "content": "C"}}
	got := extractKnowledgeCandidates(items, 0.0)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].ArticleID != "" {
		t.Errorf("ArticleID = %q, want \"\" (no _additional present)", got[0].ArticleID)
	}
	if got[0].ContentSHA256 == "" {
		t.Error("ContentSHA256 must be populated even when ArticleID is empty")
	}
}

func TestExtractKnowledgeCandidates_dropsAllEmptyItem(t *testing.T) {
	items := []map[string]interface{}{
		{"_additional": map[string]interface{}{"id": "x", "score": "1.0"}},
	}
	if got := extractKnowledgeCandidates(items, 0.0); got != nil {
		t.Errorf("item with empty title AND content must be dropped, got %+v", got)
	}
}

func TestExtractKnowledgeCandidates_contentSHA256MatchesTitleAndContentJoin(t *testing.T) {
	// The dedup key MUST be derivable by anyone with the same title+content
	// pair — the orchestrator never recomputes the hash, but a downstream
	// consumer (e.g. api-plugin / event log) might verify it. Lock the
	// canonical encoding here so a future "performance" rewrite that
	// changes the join doesn't silently break dedup across plugin
	// versions.
	items := []map[string]interface{}{mkArticle("a", "Title", "Content", "", "1.0")}
	got := extractKnowledgeCandidates(items, 0.0)
	wantBody := "Title\n\nContent"
	if got[0].Content != wantBody {
		t.Errorf("Content = %q, want %q", got[0].Content, wantBody)
	}
	wantHash := contentSHA256(wantBody)
	if got[0].ContentSHA256 != wantHash {
		t.Errorf("ContentSHA256 = %q, want %q (hash of %q)", got[0].ContentSHA256, wantHash, wantBody)
	}
}

func TestExtractGlossaryCandidates_dropsItemWithoutBothFields(t *testing.T) {
	items := []map[string]interface{}{
		{"term": "Item", "_additional": mkScore("1.0")}, // missing definition
		{"definition": "x", "_additional": mkScore("1.0")}, // missing term
		{"term": "Tool", "definition": "An action you can call", "_additional": mkScore("1.0")},
	}
	got := extractGlossaryCandidates(items, 0.0)
	if len(got) != 1 || got[0].Term != "Tool" {
		t.Errorf("glossary candidates = %+v, want exactly one Tool entry", got)
	}
}

func TestExtractGlossaryCandidates_contentIsDefinition(t *testing.T) {
	items := []map[string]interface{}{
		{"term": "Item", "definition": "Physical asset", "_additional": mkScore("1.0")},
	}
	got := extractGlossaryCandidates(items, 0.0)
	if got[0].Content != "Physical asset" {
		t.Errorf("Content = %q, want %q (definition body only)", got[0].Content, "Physical asset")
	}
	if got[0].ContentSHA256 != contentSHA256("Physical asset") {
		t.Errorf("ContentSHA256 mismatch")
	}
}

// fakeGraphQLResult mirrors the shape Weaviate's go client returns —
// a top-level Data map keyed by "Get", then by className, with a slice
// of result objects. extractItems reaches in through that shape, so the
// fixture has to follow it exactly.
type fakeGraphQLResult struct {
	Data map[string]interface{} `json:"data"`
}

func fakeResult(className string, items []map[string]interface{}) interface{} {
	return &fakeGraphQLResult{
		Data: map[string]interface{}{
			"Get": map[string]interface{}{
				className: items,
			},
		},
	}
}

func TestExtractToolCandidatesFromResult_buildsPluginDotAction(t *testing.T) {
	items := []map[string]interface{}{
		{"pluginName": "timly", "actionName": "list-items", "_additional": mkScore("0.80")},
		{"pluginName": "weaviate", "actionName": "ask_knowledge", "_additional": mkScore("0.10")}, // below
		{"pluginName": "timly", "actionName": "show-item", "_additional": mkScore("0.60")},
	}
	got := extractToolCandidatesFromResult(fakeResult("MCPActions", items), "MCPActions", actionFilter{minScore: 0.25})
	if len(got) != 2 {
		t.Fatalf("got %d tool candidates, want 2", len(got))
	}
	if got[0].ToolName != "timly.list-items" {
		t.Errorf("first ToolName = %q, want %q", got[0].ToolName, "timly.list-items")
	}
	if got[1].ToolName != "timly.show-item" {
		t.Errorf("second ToolName = %q, want %q", got[1].ToolName, "timly.show-item")
	}
	if got[0].PositionInResults != 1 || got[1].PositionInResults != 2 {
		t.Errorf("positions = [%d, %d], want [1, 2]", got[0].PositionInResults, got[1].PositionInResults)
	}
}

func TestExtractToolCandidatesFromResult_emptyClassReturnsNil(t *testing.T) {
	got := extractToolCandidatesFromResult(fakeResult("MCPActions", nil), "MCPActions", actionFilter{})
	if got != nil {
		t.Errorf("empty class slice must yield nil, got %+v", got)
	}
}

func TestExtractToolCandidatesFromResult_dropsItemWithoutPluginOrActionName(t *testing.T) {
	items := []map[string]interface{}{
		{"pluginName": "timly", "_additional": mkScore("0.9")}, // missing action
		{"actionName": "list-items", "_additional": mkScore("0.9")}, // missing plugin
		{"pluginName": "timly", "actionName": "show-item", "_additional": mkScore("0.9")},
	}
	got := extractToolCandidatesFromResult(fakeResult("MCPActions", items), "MCPActions", actionFilter{})
	if len(got) != 1 || got[0].ToolName != "timly.show-item" {
		t.Errorf("tool candidates = %+v, want exactly one timly.show-item entry", got)
	}
}

func TestExtractKnowledgeCandidates_titleOrContentOnlyKept(t *testing.T) {
	// joinTitleAndContent guarantees a non-empty body when at least one
	// half is present — make sure the extractor accepts that path rather
	// than dropping the candidate.
	items := []map[string]interface{}{
		mkArticle("a-title", "OnlyTitle", "", "", "1.0"),
		mkArticle("a-content", "", "OnlyContent", "", "1.0"),
	}
	got := extractKnowledgeCandidates(items, 0.0)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if !strings.HasSuffix(got[0].Content, "OnlyTitle") || got[0].Title != "OnlyTitle" {
		t.Errorf("title-only: Content=%q Title=%q", got[0].Content, got[0].Title)
	}
	if got[1].Content != "OnlyContent" {
		t.Errorf("content-only: Content=%q, want %q", got[1].Content, "OnlyContent")
	}
}
