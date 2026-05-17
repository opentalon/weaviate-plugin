//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/weaviate/weaviate-go-client/v5/weaviate"
	wmodels "github.com/weaviate/weaviate/entities/models"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func weaviateHost() string {
	if h := os.Getenv("WEAVIATE_HOST"); h != "" {
		return h
	}
	return "localhost:8080"
}

func vectorizerModule() string {
	if m := os.Getenv("WEAVIATE_MODULE"); m != "" {
		return m
	}
	return "none"
}

const testClass = "Article"

var rawClient *weaviate.Client

// ---------------------------------------------------------------------------
// TestMain — setup / teardown around the whole suite
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	cfg := weaviate.Config{Host: weaviateHost(), Scheme: "http"}
	var err error
	rawClient, err = weaviate.NewClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weaviate client: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitReady(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		fmt.Fprintf(os.Stderr,
			"hint: start Weaviate locally with:\n"+
				"  AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true "+
				"DEFAULT_VECTORIZER_MODULE=none "+
				"weaviate --host 0.0.0.0 --port 8080 --scheme http &\n")
		os.Exit(1)
	}

	setupSchema()
	setupRAGSchemas()
	seedData()
	seedRAGData()

	code := m.Run()

	bgCtx := context.Background()
	_ = rawClient.Schema().ClassDeleter().WithClassName(testClass).Do(bgCtx)
	_ = rawClient.Schema().ClassDeleter().WithClassName(DefaultActionsCollection).Do(bgCtx)
	_ = rawClient.Schema().ClassDeleter().WithClassName(DefaultKnowledgeCollection).Do(bgCtx)
	os.Exit(code)
}

func waitReady(ctx context.Context) error {
	for {
		if ok, _ := rawClient.Misc().ReadyChecker().Do(ctx); ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("weaviate at %s not ready within 30s", weaviateHost())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func setupSchema() {
	ctx := context.Background()
	_ = rawClient.Schema().ClassDeleter().WithClassName(testClass).Do(ctx)

	class := &wmodels.Class{
		Class:      testClass,
		Vectorizer: vectorizerModule(),
		Properties: []*wmodels.Property{
			{Name: "title", DataType: []string{"text"}},
			{Name: "body", DataType: []string{"text"}},
		},
	}
	if err := rawClient.Schema().ClassCreator().WithClass(class).Do(ctx); err != nil {
		panic(fmt.Sprintf("create schema: %v", err))
	}
}

func setupRAGSchemas() {
	ctx := context.Background()
	_ = rawClient.Schema().ClassDeleter().WithClassName(DefaultActionsCollection).Do(ctx)
	_ = rawClient.Schema().ClassDeleter().WithClassName(DefaultKnowledgeCollection).Do(ctx)

	actions := &wmodels.Class{
		Class:      DefaultActionsCollection,
		Vectorizer: vectorizerModule(),
		Properties: []*wmodels.Property{
			{Name: "pluginName", DataType: []string{"text"}},
			{Name: "actionName", DataType: []string{"text"}},
			{Name: "description", DataType: []string{"text"}},
			{Name: "parameters", DataType: []string{"text"}},
		},
	}
	if err := rawClient.Schema().ClassCreator().WithClass(actions).Do(ctx); err != nil {
		panic(fmt.Sprintf("create MCPActions: %v", err))
	}

	knowledge := &wmodels.Class{
		Class:      DefaultKnowledgeCollection,
		Vectorizer: vectorizerModule(),
		Properties: []*wmodels.Property{
			{Name: "title", DataType: []string{"text"}},
			{Name: "content", DataType: []string{"text"}},
			{Name: "source", DataType: []string{"text"}},
			{Name: "tags", DataType: []string{"text[]"}},
		},
	}
	if err := rawClient.Schema().ClassCreator().WithClass(knowledge).Do(ctx); err != nil {
		panic(fmt.Sprintf("create KnowledgeArticles: %v", err))
	}
}

type article struct{ Title, Body string }

var corpus = []article{
	{
		Title: "Introduction to Go programming",
		Body:  "Go is a statically typed compiled language designed at Google for building reliable and efficient software.",
	},
	{
		Title: "Python for data science",
		Body:  "Python is widely used in machine learning and data analysis due to its simple syntax and rich ecosystem.",
	},
	{
		Title: "Weaviate vector database",
		Body:  "Weaviate is an open-source vector database that stores and retrieves objects by their semantic meaning.",
	},
}

func seedData() {
	ctx := context.Background()
	for _, a := range corpus {
		_, err := rawClient.Data().Creator().
			WithClassName(testClass).
			WithProperties(map[string]interface{}{
				"title": a.Title,
				"body":  a.Body,
			}).
			Do(ctx)
		if err != nil {
			panic(fmt.Sprintf("seed %q: %v", a.Title, err))
		}
	}
	time.Sleep(300 * time.Millisecond)
}

func seedRAGData() {
	ctx := context.Background()

	// Seed knowledge articles.
	knowledgeArticles := []map[string]interface{}{
		{"title": "Kubernetes Deployment Guide", "content": "How to deploy applications to Kubernetes using ArgoCD and Helm charts.", "source": "wiki"},
		{"title": "API Authentication", "content": "All API requests require a Bearer token in the Authorization header.", "source": "docs"},
		{"title": "Jira Workflow", "content": "Issues in Jira follow the workflow: Open, In Progress, Review, Done.", "source": "wiki"},
	}
	for _, a := range knowledgeArticles {
		_, err := rawClient.Data().Creator().
			WithClassName(DefaultKnowledgeCollection).
			WithProperties(a).
			Do(ctx)
		if err != nil {
			panic(fmt.Sprintf("seed knowledge %q: %v", a["title"], err))
		}
	}

	// Seed MCP actions with deterministic IDs.
	mcpActions := []map[string]interface{}{
		{"pluginName": "jira", "actionName": "create_issue", "description": "Create a new issue in the Jira project tracker"},
		{"pluginName": "jira", "actionName": "list_issues", "description": "List all open issues in a Jira project"},
		{"pluginName": "gitlab", "actionName": "create_mr", "description": "Create a merge request in GitLab"},
		{"pluginName": "gitlab", "actionName": "list_pipelines", "description": "List CI pipelines in a GitLab project"},
	}
	for _, a := range mcpActions {
		id := actionUUID(a["pluginName"].(string), a["actionName"].(string))
		_, err := rawClient.Data().Creator().
			WithClassName(DefaultActionsCollection).
			WithID(id).
			WithProperties(a).
			Do(ctx)
		if err != nil {
			panic(fmt.Sprintf("seed action %s.%s: %v", a["pluginName"], a["actionName"], err))
		}
	}

	time.Sleep(300 * time.Millisecond)
}

// waitSyncDrain polls sync_status until all queued jobs have completed or
// the timeout expires. Used by tests that enqueue background sync work and
// then need to verify Weaviate state.
func waitSyncDrain(t *testing.T, h *WeaviateHandler, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := h.Execute(plugin.Request{ID: "drain-poll", Action: "sync_status"})
		if resp.Error != "" {
			t.Fatalf("sync_status error: %s", resp.Error)
		}
		var status map[string]interface{}
		if err := json.Unmarshal([]byte(resp.Content), &status); err != nil {
			t.Fatalf("parse sync_status: %v", err)
		}
		pending, _ := status["pending"].(float64)
		if pending == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sync jobs did not drain within %s", timeout)
}

func newHandler(t *testing.T) *WeaviateHandler {
	t.Helper()
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":               weaviateHost(),
		"scheme":             "http",
		"collection":         testClass,
		"fields":             []string{"title", "body"},
		"limit":              5,
		"auto_create_schema": false,
	})
	h := &WeaviateHandler{}
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return h
}

func requiresVectorizer(t *testing.T) {
	t.Helper()
	if vectorizerModule() == "none" {
		t.Skip("nearText requires a vectorizer; set WEAVIATE_MODULE=text2vec-transformers and run docker compose up -d")
	}
}

// ---------------------------------------------------------------------------
// Unit tests — no Weaviate connection needed (beyond Configure)
// ---------------------------------------------------------------------------

func TestCapabilities(t *testing.T) {
	caps := (&WeaviateHandler{}).Capabilities()

	if caps.Name != "weaviate" {
		t.Errorf("name: got %q want %q", caps.Name, "weaviate")
	}

	actions := make(map[string]bool, len(caps.Actions))
	for _, a := range caps.Actions {
		actions[a.Name] = true
	}
	for _, want := range []string{"search", "hybrid_search", "prepare", "sync_actions", "ingest", "ingest_batch", "sync_status", "refresh"} {
		if !actions[want] {
			t.Errorf("missing action %q", want)
		}
	}
}

func TestConfigure_defaults(t *testing.T) {
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":               weaviateHost(),
		"collection":         testClass,
		"auto_create_schema": false,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.limit != 5 {
		t.Errorf("default limit: got %d want 5", h.limit)
	}
	if h.collection != testClass {
		t.Errorf("collection: got %q want %q", h.collection, testClass)
	}
	if h.actionsCollection != DefaultActionsCollection {
		t.Errorf("actions_collection: got %q want %q", h.actionsCollection, DefaultActionsCollection)
	}
	if h.knowledgeCollection != DefaultKnowledgeCollection {
		t.Errorf("knowledge_collection: got %q want %q", h.knowledgeCollection, DefaultKnowledgeCollection)
	}
	// Prepare-fan-out defaults must exceed the orchestrator's downstream
	// budgets, otherwise the `cap_exceeded` dedup reason and Tier 2 tool
	// promotion become unreachable. See defaultPrepareKnowledgeLimit comment.
	if h.prepareKnowledgeLimit != defaultPrepareKnowledgeLimit {
		t.Errorf("prepareKnowledgeLimit: got %d want %d", h.prepareKnowledgeLimit, defaultPrepareKnowledgeLimit)
	}
	if h.prepareActionsLimit != defaultPrepareActionsLimit {
		t.Errorf("prepareActionsLimit: got %d want %d", h.prepareActionsLimit, defaultPrepareActionsLimit)
	}
	if h.prepareGlossaryLimit != defaultPrepareGlossaryLimit {
		t.Errorf("prepareGlossaryLimit: got %d want %d", h.prepareGlossaryLimit, defaultPrepareGlossaryLimit)
	}
	if h.client == nil {
		t.Error("client is nil after Configure")
	}
}

func TestConfigure_prepareLimitsOverride(t *testing.T) {
	// Explicit overrides must be honoured verbatim. Zero / missing falls
	// back to the default (covered by TestConfigure_defaults); a negative
	// or zero override is treated as "not set" so a misconfig can't ship
	// a useless 0-limit query that would silently return nothing.
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":                    weaviateHost(),
		"collection":              testClass,
		"auto_create_schema":      false,
		"prepare_knowledge_limit": 7,
		"prepare_actions_limit":   33,
		"prepare_glossary_limit":  4,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.prepareKnowledgeLimit != 7 {
		t.Errorf("prepareKnowledgeLimit: got %d want 7", h.prepareKnowledgeLimit)
	}
	if h.prepareActionsLimit != 33 {
		t.Errorf("prepareActionsLimit: got %d want 33", h.prepareActionsLimit)
	}
	if h.prepareGlossaryLimit != 4 {
		t.Errorf("prepareGlossaryLimit: got %d want 4", h.prepareGlossaryLimit)
	}
}

func TestConfigure_prepareLimitsZeroFallsBackToDefault(t *testing.T) {
	// A zero override is indistinguishable from "not set" once JSON
	// unmarshal hits a Go int field; the Configure path must treat
	// it as "fall back to default" rather than ship a 0-limit query.
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":                    weaviateHost(),
		"collection":              testClass,
		"auto_create_schema":      false,
		"prepare_knowledge_limit": 0,
		"prepare_actions_limit":   0,
		"prepare_glossary_limit":  0,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.prepareKnowledgeLimit != defaultPrepareKnowledgeLimit {
		t.Errorf("prepareKnowledgeLimit: got %d want %d (default)", h.prepareKnowledgeLimit, defaultPrepareKnowledgeLimit)
	}
	if h.prepareActionsLimit != defaultPrepareActionsLimit {
		t.Errorf("prepareActionsLimit: got %d want %d (default)", h.prepareActionsLimit, defaultPrepareActionsLimit)
	}
	if h.prepareGlossaryLimit != defaultPrepareGlossaryLimit {
		t.Errorf("prepareGlossaryLimit: got %d want %d (default)", h.prepareGlossaryLimit, defaultPrepareGlossaryLimit)
	}
}

func TestConfigure_perCollectionMinPrepareScore_FallbacksToUmbrella(t *testing.T) {
	// When only the umbrella min_prepare_score is set, all three
	// per-collection thresholds adopt the same value. Pins the
	// backward-compat path: a deployment that hasn't migrated to the
	// per-collection knobs keeps the prior single-threshold behaviour.
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":               weaviateHost(),
		"collection":         testClass,
		"auto_create_schema": false,
		"min_prepare_score":  0.55,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.minPrepareScore != 0.55 {
		t.Errorf("umbrella minPrepareScore: got %v want 0.55", h.minPrepareScore)
	}
	if h.minPrepareScoreTools != 0.55 {
		t.Errorf("minPrepareScoreTools fallback: got %v want 0.55", h.minPrepareScoreTools)
	}
	if h.minPrepareScoreKnowledge != 0.55 {
		t.Errorf("minPrepareScoreKnowledge fallback: got %v want 0.55", h.minPrepareScoreKnowledge)
	}
	if h.minPrepareScoreGlossary != 0.55 {
		t.Errorf("minPrepareScoreGlossary fallback: got %v want 0.55", h.minPrepareScoreGlossary)
	}
}

func TestConfigure_perCollectionMinPrepareScore_Overrides(t *testing.T) {
	// Each per-collection knob, when explicitly set, wins over the
	// umbrella value. Lets a deployment keep a permissive cut-off for
	// tools (where the orchestrator's tier algorithm relegates noise
	// to Tier 3 anyway) while gating knowledge-text injection more
	// strictly — low-score knowledge articles are surfaced as full
	// text blocks where noise costs LLM tokens + can derail the answer.
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":                        weaviateHost(),
		"collection":                  testClass,
		"auto_create_schema":          false,
		"min_prepare_score":           0.30,
		"min_prepare_score_tools":     0.40,
		"min_prepare_score_knowledge": 0.75,
		"min_prepare_score_glossary":  0.50,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.minPrepareScoreTools != 0.40 {
		t.Errorf("minPrepareScoreTools: got %v want 0.40", h.minPrepareScoreTools)
	}
	if h.minPrepareScoreKnowledge != 0.75 {
		t.Errorf("minPrepareScoreKnowledge: got %v want 0.75", h.minPrepareScoreKnowledge)
	}
	if h.minPrepareScoreGlossary != 0.50 {
		t.Errorf("minPrepareScoreGlossary: got %v want 0.50", h.minPrepareScoreGlossary)
	}
}

func TestConfigure_perCollectionMinPrepareScore_AllUnsetUsesDefault(t *testing.T) {
	// Neither the umbrella nor the per-collection knobs are configured
	// → all three per-collection fields fall back to
	// defaultMinPrepareScore. Locks the chain: per-collection unset →
	// umbrella unset → default.
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":               weaviateHost(),
		"collection":         testClass,
		"auto_create_schema": false,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.minPrepareScoreTools != defaultMinPrepareScore {
		t.Errorf("minPrepareScoreTools: got %v want %v (default)", h.minPrepareScoreTools, defaultMinPrepareScore)
	}
	if h.minPrepareScoreKnowledge != defaultMinPrepareScore {
		t.Errorf("minPrepareScoreKnowledge: got %v want %v (default)", h.minPrepareScoreKnowledge, defaultMinPrepareScore)
	}
	if h.minPrepareScoreGlossary != defaultMinPrepareScore {
		t.Errorf("minPrepareScoreGlossary: got %v want %v (default)", h.minPrepareScoreGlossary, defaultMinPrepareScore)
	}
}

func TestConfigure_missingCollection(t *testing.T) {
	err := (&WeaviateHandler{}).Configure(`{"host":"localhost:8080","auto_create_schema":false}`)
	if err == nil {
		t.Fatal("expected error for missing collection, got nil")
	}
}

func TestConfigure_badJSON(t *testing.T) {
	err := (&WeaviateHandler{}).Configure(`{bad json}`)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestConfigure_httpRequiresToken(t *testing.T) {
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":               weaviateHost(),
		"collection":         testClass,
		"auto_create_schema": false,
		"http_addr":          ":9999",
	})
	err := (&WeaviateHandler{}).Configure(string(cfg))
	if err == nil {
		t.Fatal("expected error when http_addr set without token")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConfigure_customCollectionNames(t *testing.T) {
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":                  weaviateHost(),
		"collection":            testClass,
		"actions_collection":    "CustomActions",
		"knowledge_collection":  "CustomKnowledge",
		"auto_create_schema":    false,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.actionsCollection != "CustomActions" {
		t.Errorf("actions_collection: got %q want %q", h.actionsCollection, "CustomActions")
	}
	if h.knowledgeCollection != "CustomKnowledge" {
		t.Errorf("knowledge_collection: got %q want %q", h.knowledgeCollection, "CustomKnowledge")
	}
}

func TestExecute_unknownAction(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{ID: "x", Action: "delete_everything"})
	if resp.Error == "" {
		t.Error("expected error for unknown action, got none")
	}
}

// ---------------------------------------------------------------------------
// Integration tests — search (existing)
// ---------------------------------------------------------------------------

func TestHybridSearch_keywordOnly(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "hybrid-1",
		Action: "hybrid_search",
		Args: map[string]string{
			"query": "python",
			"alpha": "0",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if resp.CallID != "hybrid-1" {
		t.Errorf("CallID: got %q want %q", resp.CallID, "hybrid-1")
	}
	if !strings.Contains(strings.ToLower(resp.Content), "python") {
		t.Errorf("expected python article in results; got:\n%s", resp.Content)
	}
}

func TestHybridSearch_limitOverride(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "hybrid-2",
		Action: "hybrid_search",
		Args: map[string]string{
			"query": "go",
			"alpha": "0",
			"limit": "1",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(resp.Content), &data); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	get, _ := data["Get"].(map[string]interface{})
	items, _ := get[testClass].([]interface{})
	if len(items) > 1 {
		t.Errorf("limit=1 returned %d items", len(items))
	}
}

func TestHybridSearch_fieldsOverride(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "hybrid-3",
		Action: "hybrid_search",
		Args: map[string]string{
			"query":  "go",
			"alpha":  "0",
			"fields": "title",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if strings.Contains(strings.ToLower(resp.Content), `"body"`) {
		t.Error("expected no 'body' field when fields=title")
	}
}

func TestSearch_semantic(t *testing.T) {
	requiresVectorizer(t)

	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "search-1",
		Action: "search",
		Args: map[string]string{
			"query": "vector database semantic meaning",
			"limit": "3",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(strings.ToLower(resp.Content), "weaviate") {
		t.Logf("full response:\n%s", resp.Content)
		t.Error("expected Weaviate article in top results for 'vector database' query")
	}
}

func TestSearch_missingQuery(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "search-err",
		Action: "search",
		Args:   map[string]string{},
	})
	if resp.Error == "" {
		t.Error("expected error for missing query, got none")
	}
}

func TestHybridSearch_missingQuery(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "hybrid-err",
		Action: "hybrid_search",
		Args:   map[string]string{},
	})
	if resp.Error == "" {
		t.Error("expected error for missing query, got none")
	}
}

// ---------------------------------------------------------------------------
// Integration tests — auto-schema creation
// ---------------------------------------------------------------------------

func TestConfigure_autoCreateSchema(t *testing.T) {
	ctx := context.Background()
	tmpActions := "TestAutoActions"
	tmpKnowledge := "TestAutoKnowledge"

	// Clean up first.
	_ = rawClient.Schema().ClassDeleter().WithClassName(tmpActions).Do(ctx)
	_ = rawClient.Schema().ClassDeleter().WithClassName(tmpKnowledge).Do(ctx)

	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":                  weaviateHost(),
		"scheme":                "http",
		"collection":            testClass,
		"actions_collection":    tmpActions,
		"knowledge_collection":  tmpKnowledge,
		"auto_create_schema":    true,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	exists, err := rawClient.Schema().ClassExistenceChecker().WithClassName(tmpActions).Do(ctx)
	if err != nil {
		t.Fatalf("check %s: %v", tmpActions, err)
	}
	if !exists {
		t.Errorf("%s should exist after auto-create", tmpActions)
	}

	exists, err = rawClient.Schema().ClassExistenceChecker().WithClassName(tmpKnowledge).Do(ctx)
	if err != nil {
		t.Fatalf("check %s: %v", tmpKnowledge, err)
	}
	if !exists {
		t.Errorf("%s should exist after auto-create", tmpKnowledge)
	}

	// Cleanup.
	_ = rawClient.Schema().ClassDeleter().WithClassName(tmpActions).Do(ctx)
	_ = rawClient.Schema().ClassDeleter().WithClassName(tmpKnowledge).Do(ctx)
}

func TestConfigure_autoCreateSchemaIdempotent(t *testing.T) {
	// Calling Configure twice with auto_create_schema should not fail.
	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":               weaviateHost(),
		"scheme":             "http",
		"collection":         testClass,
		"auto_create_schema": true,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("first Configure: %v", err)
	}
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("second Configure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integration tests — sync_actions
// ---------------------------------------------------------------------------

func TestSyncActions(t *testing.T) {
	h := newHandler(t)

	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "test-plugin",
		Actions: []syncActionEntry{
			{Name: "create_issue", Description: "Create a new issue in the tracker"},
			{Name: "list_issues", Description: "List all open issues"},
		},
	})

	resp := h.Execute(plugin.Request{
		ID:     "sync-1",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"queued":true`) {
		t.Errorf("expected queued:true, got: %s", resp.Content)
	}
	waitSyncDrain(t, h, 10*time.Second)

	// Verify both actions exist in Weaviate.
	for _, name := range []string{"create_issue", "list_issues"} {
		id := actionUUID("test-plugin", name)
		objs, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultActionsCollection).WithID(id).Do(context.Background())
		if err != nil || len(objs) == 0 {
			t.Errorf("action %s missing after sync: err=%v", name, err)
		}
	}
}

func TestSyncActions_upsert(t *testing.T) {
	h := newHandler(t)

	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "upsert-plugin",
		Actions: []syncActionEntry{
			{Name: "do_thing", Description: "Original description"},
		},
	})

	resp := h.Execute(plugin.Request{
		ID:     "sync-u1",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})
	if resp.Error != "" {
		t.Fatalf("first sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)

	// Sync again with updated description — should not fail.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName: "upsert-plugin",
		Actions: []syncActionEntry{
			{Name: "do_thing", Description: "Updated description"},
		},
	})

	resp = h.Execute(plugin.Request{
		ID:     "sync-u2",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})
	if resp.Error != "" {
		t.Fatalf("upsert sync error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"queued":true`) {
		t.Errorf("expected queued:true, got: %s", resp.Content)
	}
	waitSyncDrain(t, h, 10*time.Second)
}

func TestSyncActions_missingPayload(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "sync-err",
		Action: "sync_actions",
		Args:   map[string]string{},
	})
	if resp.Error == "" {
		t.Error("expected error for missing payload, got none")
	}
}

func TestSyncActions_missingPluginName(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "sync-err2",
		Action: "sync_actions",
		Args:   map[string]string{"payload": `{"actions":[{"name":"x","description":"y"}]}`},
	})
	if resp.Error == "" {
		t.Error("expected error for missing plugin_name, got none")
	}
}

func TestSyncActions_deletesStaleActions(t *testing.T) {
	h := newHandler(t)

	// First sync: two actions.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "stale-plugin",
		Actions: []syncActionEntry{
			{Name: "old_action", Description: "Will be removed"},
			{Name: "keep_action", Description: "Will be kept"},
		},
	})
	resp := h.Execute(plugin.Request{
		ID:     "sync-stale1",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})
	if resp.Error != "" {
		t.Fatalf("first sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Second sync: only keep_action — old_action should be deleted.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName: "stale-plugin",
		Actions: []syncActionEntry{
			{Name: "keep_action", Description: "Updated description"},
		},
	})
	resp = h.Execute(plugin.Request{
		ID:     "sync-stale2",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})
	if resp.Error != "" {
		t.Fatalf("second sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Verify old_action no longer exists by searching for it.
	oldID := actionUUID("stale-plugin", "old_action")
	objs, err := rawClient.Data().ObjectsGetter().
		WithClassName(DefaultActionsCollection).
		WithID(oldID).
		Do(context.Background())
	if err == nil && len(objs) > 0 {
		t.Error("old_action should have been deleted by the second sync, but it still exists")
	}

	// Verify keep_action still exists.
	keepID := actionUUID("stale-plugin", "keep_action")
	objs, err = rawClient.Data().ObjectsGetter().
		WithClassName(DefaultActionsCollection).
		WithID(keepID).
		Do(context.Background())
	if err != nil {
		t.Fatalf("check keep_action: %v", err)
	}
	if len(objs) == 0 {
		t.Error("keep_action should still exist after second sync")
	}
}

// TestSyncActions_continuationBatchSkipsPreDelete verifies the fix for the
// multi-batch sync truncation bug: when the orchestrator chunks a plugin's
// actions across multiple sync_actions calls, batches 1..N must NOT pre-delete
// the plugin's actions (which would wipe what batch 0 inserted). Only batch 0
// performs the orphan-prune; subsequent batches are pure inserts.
func TestSyncActions_continuationBatchSkipsPreDelete(t *testing.T) {
	h := newHandler(t)

	// Batch 0: insert 3 actions, IsContinuationBatch=false (pre-delete + insert).
	batch0, _ := json.Marshal(syncActionsPayload{
		PluginName: "multi-batch-plugin",
		Actions: []syncActionEntry{
			{Name: "alpha", Description: "Alpha action"},
			{Name: "bravo", Description: "Bravo action"},
			{Name: "charlie", Description: "Charlie action"},
		},
	})
	if resp := h.Execute(plugin.Request{
		ID: "sync-batch-0", Action: "sync_actions", Args: map[string]string{"payload": string(batch0)},
	}); resp.Error != "" {
		t.Fatalf("batch 0 error: %s", resp.Error)
	}

	// Batch 1: insert 3 more actions, IsContinuationBatch=true (insert only).
	// Without the fix, batch 1's pre-delete would wipe alpha / bravo / charlie.
	batch1, _ := json.Marshal(syncActionsPayload{
		PluginName: "multi-batch-plugin",
		Actions: []syncActionEntry{
			{Name: "delta", Description: "Delta action"},
			{Name: "echo", Description: "Echo action"},
			{Name: "foxtrot", Description: "Foxtrot action"},
		},
		IsContinuationBatch: true,
	})
	if resp := h.Execute(plugin.Request{
		ID: "sync-batch-1", Action: "sync_actions", Args: map[string]string{"payload": string(batch1)},
	}); resp.Error != "" {
		t.Fatalf("batch 1 error: %s", resp.Error)
	}

	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// All six actions from both batches must be present.
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		id := actionUUID("multi-batch-plugin", name)
		objs, err := rawClient.Data().ObjectsGetter().
			WithClassName(DefaultActionsCollection).
			WithID(id).
			Do(context.Background())
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if len(objs) == 0 {
			t.Errorf("expected %s to be present after multi-batch sync, but it was deleted", name)
		}
	}
}

func TestSyncActions_emptyActions(t *testing.T) {
	h := newHandler(t)
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "empty-plugin",
		Actions:    []syncActionEntry{},
	})
	resp := h.Execute(plugin.Request{
		ID:     "sync-empty",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})
	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"queued":true`) {
		t.Errorf("expected queued:true, got: %s", resp.Content)
	}
	waitSyncDrain(t, h, 10*time.Second)
}

func TestSyncActions_serverInstructions(t *testing.T) {
	h := newHandler(t)

	const prose = "Timly MCP server.\n\n## Org-units vs Containers\nA Place is a storage unit; an Org-unit is a structural unit."
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName:         "si-test",
		Actions:            []syncActionEntry{{Name: "list-items", Description: "List items"}},
		ServerInstructions: prose,
	})
	resp := h.Execute(plugin.Request{ID: "sync-si", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"queued":true`) {
		t.Errorf("expected queued:true, got: %s", resp.Content)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Verify the article exists with the deterministic UUID and expected fields.
	id := serverInstructionsUUID("si-test")
	got, err := rawClient.Data().ObjectsGetter().
		WithClassName(DefaultKnowledgeCollection).
		WithID(id).
		Do(context.Background())
	if err != nil {
		t.Fatalf("get article by ID %s: %v", id, err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 article, got %d", len(got))
	}
	props := got[0].Properties.(map[string]interface{})
	if src, _ := props["source"].(string); src != "mcp:si-test" {
		t.Errorf("source = %q, want %q", src, "mcp:si-test")
	}
	if content, _ := props["content"].(string); content != prose {
		t.Errorf("content mismatch:\ngot:  %q\nwant: %q", content, prose)
	}

	// Idempotency: re-syncing with updated prose must overwrite, not duplicate.
	const newProse = "Timly MCP server.\nUpdated."
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName:         "si-test",
		ServerInstructions: newProse,
	})
	resp = h.Execute(plugin.Request{ID: "sync-si-2", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("re-sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	got, err = rawClient.Data().ObjectsGetter().
		WithClassName(DefaultKnowledgeCollection).
		WithID(id).
		Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("re-fetch: err=%v len=%d", err, len(got))
	}
	props = got[0].Properties.(map[string]interface{})
	if content, _ := props["content"].(string); content != newProse {
		t.Errorf("content not updated: %q", content)
	}
}

func TestSyncActions_pruneOrphans(t *testing.T) {
	h := newHandler(t)

	// Seed two plugins (each with one action and instructions).
	for _, name := range []string{"prune-keep", "prune-orphan"} {
		payload, _ := json.Marshal(syncActionsPayload{
			PluginName:         name,
			Actions:            []syncActionEntry{{Name: "act", Description: "x"}},
			ServerInstructions: "instructions for " + name,
		})
		resp := h.Execute(plugin.Request{ID: "seed-" + name, Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
		if resp.Error != "" {
			t.Fatalf("seed %s: %s", name, resp.Error)
		}
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Re-sync with keep_plugins listing only the kept plugin — orphan must vanish.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName:  "prune-keep",
		Actions:     []syncActionEntry{{Name: "act", Description: "x"}},
		KeepPlugins: []string{"prune-keep"},
	})
	resp := h.Execute(plugin.Request{ID: "prune-call", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("prune call: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// MCPActions: kept plugin's action survives, orphan's action is gone.
	keepActionID := actionUUID("prune-keep", "act")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultActionsCollection).WithID(keepActionID).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("kept action missing: err=%v len=%d", err, len(got))
	}
	orphanActionID := actionUUID("prune-orphan", "act")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultActionsCollection).WithID(orphanActionID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("orphan action still present after prune: %d records", len(got))
	}

	// KnowledgeArticles: kept plugin's article survives, orphan's article is gone.
	keepArticleID := serverInstructionsUUID("prune-keep")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(keepArticleID).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("kept article missing: err=%v len=%d", err, len(got))
	}
	orphanArticleID := serverInstructionsUUID("prune-orphan")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(orphanArticleID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("orphan article still present after prune: %d records", len(got))
	}
}

func TestSyncActions_emptyKeepPluginsSkipsPrune(t *testing.T) {
	h := newHandler(t)

	// Seed one record.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "skip-prune",
		Actions:    []syncActionEntry{{Name: "act", Description: "x"}},
	})
	resp := h.Execute(plugin.Request{ID: "seed-skip", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("seed: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)

	// Send an empty keep_plugins — must NOT trigger a delete-everything-in-class.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName:  "skip-prune",
		Actions:     []syncActionEntry{{Name: "act", Description: "x"}},
		KeepPlugins: []string{},
	})
	resp = h.Execute(plugin.Request{ID: "skip-prune-call", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("skip-prune call: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Verify the seeded record still exists.
	id := actionUUID("skip-prune", "act")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultActionsCollection).WithID(id).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("record was pruned despite empty keep_plugins: err=%v len=%d", err, len(got))
	}
}

func TestSyncActions_pruneAlsoWithoutInstructions(t *testing.T) {
	// Verify a sync_actions call with keep_plugins works even when the new
	// optional fields aren't present together — only KeepPlugins set, no
	// ServerInstructions on this call.
	h := newHandler(t)

	// Seed an orphan.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "no-instr-orphan",
		Actions:    []syncActionEntry{{Name: "act", Description: "x"}},
	})
	resp := h.Execute(plugin.Request{ID: "seed-noinstr", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("seed: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Now sync a different plugin and prune.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName:  "no-instr-keep",
		Actions:     []syncActionEntry{{Name: "act", Description: "x"}},
		KeepPlugins: []string{"no-instr-keep"},
	})
	resp = h.Execute(plugin.Request{ID: "prune-noinstr", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("prune call: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	orphanID := actionUUID("no-instr-orphan", "act")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultActionsCollection).WithID(orphanID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("orphan still present without ServerInstructions path: %d records", len(got))
	}
}

// ---------------------------------------------------------------------------
// Integration tests — ingest
// ---------------------------------------------------------------------------

func TestIngest(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ingest-1",
		Action: "ingest",
		Args: map[string]string{
			"title":   "Test Article",
			"content": "This is a test article for the knowledge base.",
			"source":  "test-suite",
			"tags":    "test,integration",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"ingested":1`) {
		t.Errorf("expected ingested:1, got: %s", resp.Content)
	}
}

func TestIngest_missingFields(t *testing.T) {
	h := newHandler(t)

	// Missing content.
	resp := h.Execute(plugin.Request{
		ID:     "ingest-err1",
		Action: "ingest",
		Args:   map[string]string{"title": "No content"},
	})
	if resp.Error == "" {
		t.Error("expected error for missing content, got none")
	}

	// Missing title.
	resp = h.Execute(plugin.Request{
		ID:     "ingest-err2",
		Action: "ingest",
		Args:   map[string]string{"content": "No title"},
	})
	if resp.Error == "" {
		t.Error("expected error for missing title, got none")
	}
}

func TestIngest_noOptionalFields(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ingest-min",
		Action: "ingest",
		Args: map[string]string{
			"title":   "Minimal Article",
			"content": "Just title and content, no source or tags.",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"ingested":1`) {
		t.Errorf("expected ingested:1, got: %s", resp.Content)
	}
}

// ---------------------------------------------------------------------------
// Integration tests — ingest_batch
// ---------------------------------------------------------------------------

func TestIngestBatch(t *testing.T) {
	h := newHandler(t)

	articles := []knowledgeArticle{
		{Title: "Batch Article 1", Content: "First batch article.", Source: "batch-test", Tags: []string{"batch"}},
		{Title: "Batch Article 2", Content: "Second batch article.", Source: "batch-test", Tags: []string{"batch"}},
		{Title: "Batch Article 3", Content: "Third batch article."},
	}
	payload, _ := json.Marshal(articles)

	resp := h.Execute(plugin.Request{
		ID:     "batch-1",
		Action: "ingest_batch",
		Args:   map[string]string{"payload": string(payload)},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"ingested":3`) {
		t.Errorf("expected ingested:3, got: %s", resp.Content)
	}
}

func TestIngestBatch_missingPayload(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "batch-err",
		Action: "ingest_batch",
		Args:   map[string]string{},
	})
	if resp.Error == "" {
		t.Error("expected error for missing payload, got none")
	}
}

func TestIngestBatch_skipsInvalid(t *testing.T) {
	h := newHandler(t)

	// One valid, one missing title, one missing content.
	articles := []knowledgeArticle{
		{Title: "Valid", Content: "Valid content."},
		{Title: "", Content: "No title"},
		{Title: "No content", Content: ""},
	}
	payload, _ := json.Marshal(articles)

	resp := h.Execute(plugin.Request{
		ID:     "batch-skip",
		Action: "ingest_batch",
		Args:   map[string]string{"payload": string(payload)},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"ingested":1`) {
		t.Errorf("expected ingested:1 (invalid articles skipped), got: %s", resp.Content)
	}
}

func TestIngestBatch_badJSON(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "batch-bad",
		Action: "ingest_batch",
		Args:   map[string]string{"payload": "{bad json}"},
	})
	if resp.Error == "" {
		t.Error("expected error for bad JSON, got none")
	}
}

// ---------------------------------------------------------------------------
// HTTP handler tests
// ---------------------------------------------------------------------------

func newHandlerWithToken(t *testing.T, token string) *WeaviateHandler {
	t.Helper()
	h := newHandler(t)
	h.token = token
	return h
}

func TestHTTP_ingestArticle(t *testing.T) {
	h := newHandlerWithToken(t, "test-secret")

	body := `{"title":"HTTP Article","content":"Ingested via HTTP.","source":"http-test","tags":["http"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/articles", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.requireToken(h.handleIngestArticle)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ingested":1`) {
		t.Errorf("expected ingested:1, got: %s", w.Body.String())
	}
}

func TestHTTP_ingestArticleUnauthorized(t *testing.T) {
	h := newHandlerWithToken(t, "test-secret")

	body := `{"title":"Should Fail","content":"No token."}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/articles", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.requireToken(h.handleIngestArticle)(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHTTP_ingestArticleWrongToken(t *testing.T) {
	h := newHandlerWithToken(t, "test-secret")

	body := `{"title":"Should Fail","content":"Wrong token."}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/articles", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.requireToken(h.handleIngestArticle)(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHTTP_syncActions(t *testing.T) {
	h := newHandlerWithToken(t, "test-secret")

	body := `{"plugin_name":"http-plugin","actions":[{"name":"http_action","description":"An action synced via HTTP"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions/sync", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.requireToken(h.handleSyncActions)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"queued":true`) {
		t.Errorf("expected queued:true, got: %s", w.Body.String())
	}
	waitSyncDrain(t, h, 10*time.Second)
}

func TestHTTP_ingestBatch(t *testing.T) {
	h := newHandlerWithToken(t, "test-secret")

	body := `[{"title":"HTTP Batch 1","content":"First."},{"title":"HTTP Batch 2","content":"Second."}]`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/articles/batch", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.requireToken(h.handleIngestBatch)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ingested":2`) {
		t.Errorf("expected ingested:2, got: %s", w.Body.String())
	}
}

func TestHTTP_badJSON(t *testing.T) {
	h := newHandlerWithToken(t, "test-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/articles", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer test-secret")

	w := httptest.NewRecorder()
	h.requireToken(h.handleIngestArticle)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHTTP_noTokenConfigured(t *testing.T) {
	// When no token is configured, requests should pass through.
	h := newHandler(t) // no token set

	body := `{"title":"Open Access","content":"No auth required."}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/articles", strings.NewReader(body))

	w := httptest.NewRecorder()
	h.requireToken(h.handleIngestArticle)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Integration tests — prepare (Phase 2: dual-collection RAG)
// ---------------------------------------------------------------------------

func TestPrepare_dualCollection(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "prepare-1",
		Action: "prepare",
		Args:   map[string]string{"text": "create jira issue"},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var result prepareResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		t.Fatalf("unmarshal prepare response: %v\nraw: %s", err, resp.Content)
	}
	if !result.SendToLLM {
		t.Error("expected send_to_llm=true")
	}
	if result.Message == "" {
		t.Error("expected non-empty message")
	}
	if !strings.Contains(result.Message, "create jira issue") {
		t.Error("expected original text in message")
	}
	// Actions should NOT be in message — relevant_tools handles filtering.
	if strings.Contains(result.Message, "[actions]") {
		t.Error("message should NOT contain [actions] block")
	}
}

func TestPrepare_returnsRelevantTools(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "prepare-tools",
		Action: "prepare",
		Args:   map[string]string{"text": "Create a new issue in the Jira project tracker list open issues"},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var result prepareResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(result.RelevantTools) == 0 {
		t.Logf("prepare response: %s", resp.Content)
		t.Fatal("expected relevant_tools to be populated")
	}

	// Verify tool names are in "plugin.action" format.
	for _, tool := range result.RelevantTools {
		if !strings.Contains(tool, ".") {
			t.Errorf("tool %q should be in 'plugin.action' format", tool)
		}
	}
}

func TestPrepare_allowedPlugins(t *testing.T) {
	h := newHandler(t)

	allowed, _ := json.Marshal([]string{"jira"})
	resp := h.Execute(plugin.Request{
		ID:     "prepare-filter",
		Action: "prepare",
		Args: map[string]string{
			"text":            "create issue or merge request",
			"allowed_plugins": string(allowed),
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var result prepareResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// All returned tools should be from jira (or the always-included ask_knowledge), not gitlab.
	for _, tool := range result.RelevantTools {
		if !strings.HasPrefix(tool, "jira.") && tool != "weaviate.ask_knowledge" {
			t.Errorf("expected only jira tools (+ ask_knowledge) with allowed_plugins=[jira], got %q", tool)
		}
	}
}

func TestPrepare_emptyText(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "prepare-empty",
		Action: "prepare",
		Args:   map[string]string{"text": ""},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var result prepareResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.SendToLLM {
		t.Error("expected send_to_llm=true")
	}
	if result.Message != "" {
		t.Errorf("expected empty message for empty text, got %q", result.Message)
	}
	if len(result.RelevantTools) != 0 {
		t.Errorf("expected empty relevant_tools, got %v", result.RelevantTools)
	}
}

func TestPrepare_noTextArg(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "prepare-notext",
		Action: "prepare",
		Args:   map[string]string{},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var result prepareResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.SendToLLM {
		t.Error("expected send_to_llm=true")
	}
}

func TestPrepare_structuredJSONFormat(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "prepare-json",
		Action: "prepare",
		Args:   map[string]string{"text": "deploy kubernetes"},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	// Verify the response is valid JSON with expected fields.
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(resp.Content), &raw); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	if _, ok := raw["send_to_llm"]; !ok {
		t.Error("missing send_to_llm field")
	}
	if _, ok := raw["message"]; !ok {
		t.Error("missing message field")
	}
	if _, ok := raw["relevant_tools"]; !ok {
		t.Error("missing relevant_tools field")
	}
}

func TestPrepare_knowledgeContextAndRelevantTools(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "prepare-blocks",
		Action: "prepare",
		Args:   map[string]string{"text": "jira issue workflow"},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	var result prepareResponse
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Action context must NOT be in the message — relevant_tools handles tool filtering.
	if strings.Contains(result.Message, "[actions]") {
		t.Error("message should NOT contain [actions] block — tool filtering uses relevant_tools")
	}

	// relevant_tools should be populated with matched actions above score threshold.
	if result.RelevantTools == nil {
		t.Error("relevant_tools should not be nil")
	}
}

// ---------------------------------------------------------------------------
// Integration tests — ask_knowledge (Phase 3)
// ---------------------------------------------------------------------------

func TestAskKnowledge(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-1",
		Action: "ask_knowledge",
		Args:   map[string]string{"query": "Kubernetes deployment ArgoCD Helm"},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if resp.Content == "" {
		t.Fatal("expected non-empty response")
	}
	// Should contain knowledge articles section.
	if !strings.Contains(resp.Content, "Knowledge Articles") {
		t.Logf("response: %s", resp.Content)
		t.Error("expected Knowledge Articles section in response")
	}
}

func TestAskKnowledge_returnsTools(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-tools",
		Action: "ask_knowledge",
		Args:   map[string]string{"query": "Create a new issue in the Jira project tracker"},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, "Available Tools") {
		t.Logf("response: %s", resp.Content)
		t.Error("expected Available Tools section in response")
	}
}

func TestAskKnowledge_missingQuery(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-err",
		Action: "ask_knowledge",
		Args:   map[string]string{},
	})
	if resp.Error == "" {
		t.Error("expected error for missing query, got none")
	}
}

func TestAskKnowledge_pluginFilter(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-plugin",
		Action: "ask_knowledge",
		Args: map[string]string{
			"query":  "create issue merge request",
			"plugin": "jira",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	// Should not contain gitlab tools when filtered to jira.
	if strings.Contains(resp.Content, "gitlab.") {
		t.Error("expected no gitlab tools when plugin=jira")
	}
}

func TestAskKnowledge_sourceFilter(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-source",
		Action: "ask_knowledge",
		Args: map[string]string{
			"query":  "Kubernetes deployment",
			"source": "wiki",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	// Should succeed; source filter narrows to wiki articles.
	if resp.Content == "" {
		t.Fatal("expected non-empty response")
	}
}

func TestAskKnowledge_allowedPlugins(t *testing.T) {
	h := newHandler(t)

	allowed, _ := json.Marshal([]string{"gitlab"})
	resp := h.Execute(plugin.Request{
		ID:     "ask-allowed",
		Action: "ask_knowledge",
		Args: map[string]string{
			"query":            "create issue merge request pipelines",
			"allowed_plugins":  string(allowed),
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	// Should not contain jira tools when allowed_plugins=["gitlab"].
	if strings.Contains(resp.Content, "jira.") {
		t.Error("expected no jira tools when allowed_plugins=[gitlab]")
	}
}

func TestAskKnowledge_limitOverride(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-limit",
		Action: "ask_knowledge",
		Args: map[string]string{
			"query": "issue",
			"limit": "1",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if resp.Content == "" {
		t.Fatal("expected non-empty response")
	}
}

func TestAskKnowledge_noResults(t *testing.T) {
	h := newHandler(t)

	resp := h.Execute(plugin.Request{
		ID:     "ask-noresults",
		Action: "ask_knowledge",
		Args: map[string]string{
			"query":  "completely irrelevant xyzzy foobar",
			"source": "nonexistent-source-that-has-no-articles",
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	// With a very restrictive filter, we may get no results.
	// The response should still be valid (either empty message or "No relevant results").
	if resp.Content == "" {
		t.Fatal("expected non-empty response even with no results")
	}
}

func TestCapabilities_includesAskKnowledge(t *testing.T) {
	caps := (&WeaviateHandler{}).Capabilities()

	actions := make(map[string]bool, len(caps.Actions))
	for _, a := range caps.Actions {
		actions[a.Name] = true
	}
	if !actions["ask_knowledge"] {
		t.Error("missing ask_knowledge action in capabilities")
	}
}

func TestRefresh(t *testing.T) {
	h := newHandler(t)
	resp := h.Execute(plugin.Request{
		ID:     "refresh-1",
		Action: "refresh",
		Args:   map[string]string{},
	})
	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, `"refreshed":true`) {
		t.Errorf("expected refreshed:true, got: %s", resp.Content)
	}
}

func TestSyncActions_knowledgeArticles(t *testing.T) {
	h := newHandler(t)

	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "ka-test",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "org-units-vs-containers", Title: "Org-units vs Containers (Places)", Content: "A Place is a storage unit; an Org-unit is a structural unit.", Tags: []string{"places"}},
			{ID: "users-vs-persons", Title: "Users vs Persons", Content: "A User logs in; a Person is a managed record."},
		},
	})
	resp := h.Execute(plugin.Request{ID: "sync-ka", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Both records exist with deterministic UUID, expected source format and tag set.
	cases := []struct {
		articleID string
		title     string
		source    string
		hasTag    string
	}{
		{"org-units-vs-containers", "Org-units vs Containers (Places)", "mcp-knowledge:ka-test:org-units-vs-containers", "places"},
		{"users-vs-persons", "Users vs Persons", "mcp-knowledge:ka-test:users-vs-persons", "knowledge"},
	}
	for _, c := range cases {
		id := knowledgeArticleUUID("ka-test", c.articleID)
		got, err := rawClient.Data().ObjectsGetter().
			WithClassName(DefaultKnowledgeCollection).
			WithID(id).
			Do(context.Background())
		if err != nil {
			t.Fatalf("get article by ID %s: %v", id, err)
		}
		if len(got) != 1 {
			t.Fatalf("article %s: expected 1, got %d", c.articleID, len(got))
		}
		props := got[0].Properties.(map[string]interface{})
		if title, _ := props["title"].(string); title != c.title {
			t.Errorf("article %s: title = %q, want %q", c.articleID, title, c.title)
		}
		if src, _ := props["source"].(string); src != c.source {
			t.Errorf("article %s: source = %q, want %q", c.articleID, src, c.source)
		}
		tags, _ := props["tags"].([]interface{})
		found := false
		for _, tg := range tags {
			if s, _ := tg.(string); s == c.hasTag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("article %s: missing tag %q in %v", c.articleID, c.hasTag, tags)
		}
	}

	// Idempotency: re-sync with updated content overwrites in place.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName: "ka-test",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "org-units-vs-containers", Title: "Org-units vs Containers (Places)", Content: "Revised content."},
			{ID: "users-vs-persons", Title: "Users vs Persons", Content: "A User logs in; a Person is a managed record."},
		},
	})
	resp = h.Execute(plugin.Request{ID: "sync-ka-2", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("re-sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	id := knowledgeArticleUUID("ka-test", "org-units-vs-containers")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(id).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("re-fetch: err=%v len=%d", err, len(got))
	}
	props := got[0].Properties.(map[string]interface{})
	if content, _ := props["content"].(string); content != "Revised content." {
		t.Errorf("content not updated: %q", content)
	}
}

func TestSyncActions_knowledgeArticlesStaleSectionRemoved(t *testing.T) {
	h := newHandler(t)

	// Seed two sections.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "ka-stale-test",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "section-a", Title: "A", Content: "first"},
			{ID: "section-b", Title: "B", Content: "second"},
		},
	})
	resp := h.Execute(plugin.Request{ID: "stale-seed", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("seed: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Re-sync without section-b — it must be cleared by the per-plugin pre-delete.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName: "ka-stale-test",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "section-a", Title: "A", Content: "first"},
		},
	})
	resp = h.Execute(plugin.Request{ID: "stale-resync", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("re-sync: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// section-a still exists.
	keepID := knowledgeArticleUUID("ka-stale-test", "section-a")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(keepID).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("section-a missing: err=%v len=%d", err, len(got))
	}
	// section-b is gone.
	staleID := knowledgeArticleUUID("ka-stale-test", "section-b")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(staleID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("section-b still present after re-sync: %d records", len(got))
	}
}

func TestSyncActions_knowledgeArticlesPruneOrphans(t *testing.T) {
	h := newHandler(t)

	// Seed two plugins, each with one knowledge article.
	for _, name := range []string{"ka-prune-keep", "ka-prune-orphan"} {
		payload, _ := json.Marshal(syncActionsPayload{
			PluginName: name,
			KnowledgeArticles: []knowledgeArticleEntry{
				{ID: "section-a", Title: "Section A for " + name, Content: "x"},
			},
		})
		resp := h.Execute(plugin.Request{ID: "ka-seed-" + name, Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
		if resp.Error != "" {
			t.Fatalf("seed %s: %s", name, resp.Error)
		}
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Re-sync with keep_plugins listing only the kept plugin — orphan plugin's
	// knowledge article must vanish through the new mcp-knowledge:* prune scan.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "ka-prune-keep",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "section-a", Title: "Section A for ka-prune-keep", Content: "x"},
		},
		KeepPlugins: []string{"ka-prune-keep"},
	})
	resp := h.Execute(plugin.Request{ID: "ka-prune-call", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("prune call: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	keepID := knowledgeArticleUUID("ka-prune-keep", "section-a")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(keepID).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("kept knowledge article missing: err=%v len=%d", err, len(got))
	}
	orphanID := knowledgeArticleUUID("ka-prune-orphan", "section-a")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(orphanID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("orphan knowledge article still present after prune: %d records", len(got))
	}
}

func TestSyncActions_pruneScopesAreDisjointAcrossSourcePrefixes(t *testing.T) {
	// Regression: Weaviate's Like operator on tokenized text matches per-token,
	// so the prune scope `Like mcp:*` over-matches mcp-knowledge:* records
	// (both share the "mcp" token). Without an explicit HasPrefix gate, the
	// over-matched mcp-knowledge:* sources fall through TrimPrefix unchanged
	// and look like orphans, getting deleted on every prune cycle even when
	// their plugin is in keep_plugins.
	h := newHandler(t)

	// One plugin contributes BOTH a server-instructions blob (mcp:<plugin>)
	// AND per-section knowledge articles (mcp-knowledge:<plugin>:<id>).
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName:         "mixed-keep",
		ServerInstructions: "Server prose for mixed-keep.",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "section-a", Title: "A", Content: "first"},
		},
	})
	h.Execute(plugin.Request{ID: "mix-seed", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})

	// A second plugin to be pruned as orphan.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName:         "mixed-orphan",
		ServerInstructions: "Server prose for mixed-orphan.",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "section-a", Title: "B", Content: "second"},
		},
	})
	h.Execute(plugin.Request{ID: "mix-seed-orphan", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})

	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Re-sync mixed-keep with KeepPlugins=[mixed-keep]. Both prune scopes
	// (mcp:* and mcp-knowledge:*) execute. The kept plugin's records — across
	// BOTH prefixes — must survive.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName:         "mixed-keep",
		ServerInstructions: "Server prose for mixed-keep.",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "section-a", Title: "A", Content: "first"},
		},
		KeepPlugins: []string{"mixed-keep"},
	})
	h.Execute(plugin.Request{ID: "mix-resync", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Kept plugin's server-instructions article (source = "mcp:mixed-keep") survives.
	keepInstrID := serverInstructionsUUID("mixed-keep")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(keepInstrID).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("kept server-instructions article missing after cross-prefix prune: err=%v len=%d", err, len(got))
	}

	// Kept plugin's knowledge-section article (source = "mcp-knowledge:mixed-keep:section-a")
	// survives. Without the HasPrefix gate this would be deleted by the mcp:*
	// scan misclassifying it as a foreign plugin.
	keepKnowledgeID := knowledgeArticleUUID("mixed-keep", "section-a")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(keepKnowledgeID).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("kept knowledge-section article missing after cross-prefix prune: err=%v len=%d", err, len(got))
	}

	// Orphan plugin's records — both prefixes — vanish.
	orphanInstrID := serverInstructionsUUID("mixed-orphan")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(orphanInstrID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("orphan server-instructions article still present: %d records", len(got))
	}
	orphanKnowledgeID := knowledgeArticleUUID("mixed-orphan", "section-a")
	got, err = rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(orphanKnowledgeID).Do(context.Background())
	if err == nil && len(got) != 0 {
		t.Errorf("orphan knowledge-section article still present: %d records", len(got))
	}
}

func TestSyncActions_knowledgeArticlesPrepareNotFiltered(t *testing.T) {
	h := newHandler(t)

	// A knowledge article with content that the prepare path can match against
	// a related query ("places", "containers").
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "ka-prepare-test",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "org-units-vs-containers", Title: "Org-units vs Containers (Places)", Content: "A Place is a storage unit (container, garage, cabinet, box) — discovered via list-containers."},
		},
	})
	resp := h.Execute(plugin.Request{ID: "prep-seed", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("seed: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Article must be retrievable directly (sanity).
	id := knowledgeArticleUUID("ka-prepare-test", "org-units-vs-containers")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(id).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("article missing: err=%v len=%d", err, len(got))
	}
	src, _ := got[0].Properties.(map[string]interface{})["source"].(string)

	// filterOutMCPItems is what the prepare path uses to drop the always-injected
	// "mcp:" records. mcp-knowledge:* records must NOT be filtered out — they're
	// the whole point of the per-section split.
	filtered := filterOutMCPItems([]map[string]interface{}{{"source": src, "title": "x"}})
	if len(filtered) != 1 {
		t.Errorf("filterOutMCPItems dropped a knowledge article (source=%q); knowledge sections must reach [knowledge_context]", src)
	}
}
