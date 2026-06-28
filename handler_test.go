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
	_ = rawClient.Schema().ClassDeleter().WithClassName(DefaultKnowledgeCollection).Do(ctx)

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
	for _, want := range []string{"search", "hybrid_search", "sync_actions", "ingest", "ingest_batch", "ask_knowledge", "search_instructions", "list_knowledge_titles", "sync_status", "refresh"} {
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
	if h.knowledgeCollection != DefaultKnowledgeCollection {
		t.Errorf("knowledge_collection: got %q want %q", h.knowledgeCollection, DefaultKnowledgeCollection)
	}
	if h.client == nil {
		t.Error("client is nil after Configure")
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
		"host":                 weaviateHost(),
		"collection":           testClass,
		"knowledge_collection": "CustomKnowledge",
		"auto_create_schema":   false,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.collection != testClass {
		t.Errorf("collection: got %q want %q", h.collection, testClass)
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
	tmpKnowledge := "TestAutoKnowledge"

	// Clean up first.
	_ = rawClient.Schema().ClassDeleter().WithClassName(tmpKnowledge).Do(ctx)

	h := &WeaviateHandler{}
	cfg, _ := json.Marshal(map[string]interface{}{
		"host":                 weaviateHost(),
		"scheme":               "http",
		"collection":           testClass,
		"knowledge_collection": tmpKnowledge,
		"auto_create_schema":   true,
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	exists, err := rawClient.Schema().ClassExistenceChecker().WithClassName(tmpKnowledge).Do(ctx)
	if err != nil {
		t.Fatalf("check %s: %v", tmpKnowledge, err)
	}
	if !exists {
		t.Errorf("%s should exist after auto-create", tmpKnowledge)
	}

	// Cleanup.
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

func TestSyncActions_serverInstructions(t *testing.T) {
	h := newHandler(t)

	const prose = "MCP server.\n\n## Org-units vs Containers\nA Place is a storage unit; an Org-unit is a structural unit."
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName:         "si-test",
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
	const newProse = "MCP server.\nUpdated."
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

	// Seed two plugins, each contributing a server-instructions article. Orphan
	// pruning now only touches KnowledgeArticles (the "mcp:<plugin>" records).
	for _, name := range []string{"prune-keep", "prune-orphan"} {
		payload, _ := json.Marshal(syncActionsPayload{
			PluginName:         name,
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
		PluginName:         "prune-keep",
		ServerInstructions: "instructions for prune-keep",
		KeepPlugins:        []string{"prune-keep"},
	})
	resp := h.Execute(plugin.Request{ID: "prune-call", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("prune call: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// KnowledgeArticles: kept plugin's server-instructions article survives,
	// orphan's article is gone.
	keepArticleID := serverInstructionsUUID("prune-keep")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(keepArticleID).Do(context.Background())
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

	// Seed one server-instructions article.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName:         "skip-prune",
		ServerInstructions: "instructions for skip-prune",
	})
	resp := h.Execute(plugin.Request{ID: "seed-skip", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("seed: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)

	// Send an empty keep_plugins — must NOT trigger a delete-everything-in-class.
	payload, _ = json.Marshal(syncActionsPayload{
		PluginName:         "skip-prune",
		ServerInstructions: "instructions for skip-prune",
		KeepPlugins:        []string{},
	})
	resp = h.Execute(plugin.Request{ID: "skip-prune-call", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("skip-prune call: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Verify the seeded record still exists.
	id := serverInstructionsUUID("skip-prune")
	got, err := rawClient.Data().ObjectsGetter().WithClassName(DefaultKnowledgeCollection).WithID(id).Do(context.Background())
	if err != nil || len(got) != 1 {
		t.Errorf("record was pruned despite empty keep_plugins: err=%v len=%d", err, len(got))
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

	body := `{"plugin_name":"http-plugin","server_instructions":"Server prose synced via HTTP.","knowledge_articles":[{"id":"http-topic","title":"HTTP Topic","content":"A knowledge section synced via HTTP."}]}`
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

func TestListKnowledgeTitles(t *testing.T) {
	h := newHandler(t)

	// Seed two slug-bearing articles plus a server-instructions blob (no slug).
	// No KeepPlugins → no orphan prune, so other tests' data is untouched.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName:         "catalog-test",
		ServerInstructions: "server-level blurb that has no slug and must not appear in the catalog",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "zebra-topic", Title: "Zebra topic", Content: "z"},
			{ID: "alpha-topic", Title: "Alpha topic", Content: "a"},
		},
	})
	resp := h.Execute(plugin.Request{ID: "sync-cat", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	resp = h.Execute(plugin.Request{ID: "cat", Action: "list_knowledge_titles"})
	if resp.Error != "" {
		t.Fatalf("list_knowledge_titles error: %s", resp.Error)
	}
	var entries []catalogEntry
	if err := json.Unmarshal([]byte(resp.Content), &entries); err != nil {
		t.Fatalf("parse catalog: %v (content=%s)", err, resp.Content)
	}

	// Every entry must carry a slug (server-instruction blobs are excluded).
	var seededInOrder []string
	got := map[string]string{}
	for _, e := range entries {
		if e.Slug == "" {
			t.Errorf("catalog entry with empty slug: %+v", e)
		}
		if strings.Contains(e.Title, "server instructions") {
			t.Errorf("server-instruction blob leaked into catalog: %q", e.Title)
		}
		if e.Slug == "alpha-topic" || e.Slug == "zebra-topic" {
			got[e.Slug] = e.Title
			seededInOrder = append(seededInOrder, e.Slug)
		}
	}
	if got["alpha-topic"] != "Alpha topic" || got["zebra-topic"] != "Zebra topic" {
		t.Errorf("catalog missing seeded entries: got %v", got)
	}
	// Catalog is sorted by title → "Alpha topic" appears before "Zebra topic".
	if len(seededInOrder) == 2 && seededInOrder[0] != "alpha-topic" {
		t.Errorf("catalog not sorted by title: %v", seededInOrder)
	}
}

func TestAskKnowledge_slugExactFetch(t *testing.T) {
	h := newHandler(t)

	// Unique slugs (namespaced for this test) to avoid cross-test collisions.
	payload, _ := json.Marshal(syncActionsPayload{
		PluginName: "slug-test",
		KnowledgeArticles: []knowledgeArticleEntry{
			{ID: "slugfetch-categories", Title: "Categories (slug test)", Content: "Categories form an n-level tree."},
			{ID: "slugfetch-tickets", Title: "Tickets (slug test)", Content: "Tickets live at list-tickets."},
		},
	})
	resp := h.Execute(plugin.Request{ID: "sync-slug", Action: "sync_actions", Args: map[string]string{"payload": string(payload)}})
	if resp.Error != "" {
		t.Fatalf("sync error: %s", resp.Error)
	}
	waitSyncDrain(t, h, 10*time.Second)
	time.Sleep(300 * time.Millisecond)

	// Exact slug → exactly that article's body, deterministically (no hybrid).
	resp = h.Execute(plugin.Request{ID: "slug-hit", Action: "ask_knowledge", Args: map[string]string{"slug": "slugfetch-categories"}})
	if resp.Error != "" {
		t.Fatalf("slug fetch error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, "Categories form an n-level tree.") {
		t.Errorf("expected the Categories body, got: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "Tickets live at") {
		t.Errorf("slug fetch leaked a different article: %s", resp.Content)
	}

	// Unknown slug → explicit not-found, no silent fuzzy fallback.
	resp = h.Execute(plugin.Request{ID: "slug-miss", Action: "ask_knowledge", Args: map[string]string{"slug": "does-not-exist-xyz"}})
	if resp.Error != "" {
		t.Fatalf("slug miss error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, "No knowledge article with slug") {
		t.Errorf("expected explicit not-found message, got: %s", resp.Content)
	}

	// Empty slug is ignored: it must fall through to the query path, not error
	// out as a missing slug. A query that matches the seeded body returns it.
	resp = h.Execute(plugin.Request{ID: "slug-empty", Action: "ask_knowledge", Args: map[string]string{"slug": "", "query": "n-level tree"}})
	if resp.Error != "" {
		t.Fatalf("empty-slug+query error: %s", resp.Error)
	}
	if !strings.Contains(resp.Content, "Categories form an n-level tree.") {
		t.Errorf("empty slug should fall through to query search, got: %s", resp.Content)
	}

	// Neither slug nor query → the combined "query or slug is required" error.
	resp = h.Execute(plugin.Request{ID: "slug-none", Action: "ask_knowledge", Args: map[string]string{"slug": ""}})
	if !strings.Contains(resp.Error, "query or slug is required") {
		t.Errorf("expected 'query or slug is required', got error=%q content=%q", resp.Error, resp.Content)
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
		// slug is stored from the article id (the exact-fetch + catalog key).
		if slug, _ := props["slug"].(string); slug != c.articleID {
			t.Errorf("article %s: slug = %q, want %q", c.articleID, slug, c.articleID)
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
