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
		t.Skip("nearText requires a vectorizer; set WEAVIATE_MODULE=text2vec-contextionary and run docker compose up -d")
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
	for _, want := range []string{"search", "hybrid_search", "prepare", "sync_actions", "ingest", "ingest_batch"} {
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
	if !strings.Contains(resp.Content, `"synced":2`) {
		t.Errorf("expected synced:2, got: %s", resp.Content)
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
	if !strings.Contains(resp.Content, `"synced":1`) {
		t.Errorf("expected synced:1, got: %s", resp.Content)
	}
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
	if !strings.Contains(resp.Content, `"synced":0`) {
		t.Errorf("expected synced:0, got: %s", resp.Content)
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
	if !strings.Contains(w.Body.String(), `"synced":1`) {
		t.Errorf("expected synced:1, got: %s", w.Body.String())
	}
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
	if !strings.Contains(result.Message, "[retrieved_context") {
		t.Error("expected [retrieved_context] block in message")
	}
	if !strings.Contains(result.Message, "create jira issue") {
		t.Error("expected original text in message")
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

	// All returned tools should be from jira only, not gitlab.
	for _, tool := range result.RelevantTools {
		if !strings.HasPrefix(tool, "jira.") {
			t.Errorf("expected only jira tools with allowed_plugins=[jira], got %q", tool)
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

func TestPrepare_knowledgeAndActionsBlocks(t *testing.T) {
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

	if !strings.Contains(result.Message, "[knowledge]") {
		t.Error("expected [knowledge] block in message")
	}
	if !strings.Contains(result.Message, "[actions]") {
		t.Error("expected [actions] block in message")
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
