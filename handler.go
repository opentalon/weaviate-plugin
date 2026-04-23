package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"
	wmodels "github.com/weaviate/weaviate/entities/models"
)

// Default collection names for the knowledge-augmented RAG system.
const (
	DefaultActionsCollection   = "MCPActions"
	DefaultKnowledgeCollection = "KnowledgeArticles"
)

// actionNS is a UUID v5 namespace for deterministic MCP action object IDs.
var actionNS = uuid.MustParse("d4e8f1a2-5b3c-4d6e-9f0a-1b2c3d4e5f6a")

// Config is the JSON config block passed via the Init RPC.
type Config struct {
	Host                string   `json:"host"`
	Scheme              string   `json:"scheme"`
	Collection          string   `json:"collection"`
	ActionsCollection   string   `json:"actions_collection"`
	KnowledgeCollection string   `json:"knowledge_collection"`
	Fields              []string `json:"fields"`
	Limit               int      `json:"limit"`
	AutoCreateSchema    *bool    `json:"auto_create_schema"`
	HTTPAddr            string   `json:"http_addr"`
	Token               string   `json:"token"`
}

// WeaviateHandler implements plugin.Handler.
type WeaviateHandler struct {
	client              *weaviate.Client
	collection          string
	actionsCollection   string
	knowledgeCollection string
	fields              []string
	limit               int
	httpAddr            string
	token               string
}

// Configure is called by the SDK during the Init RPC with the JSON config block.
func (h *WeaviateHandler) Configure(configJSON string) error {
	cfg := Config{
		Host:   "localhost:8080",
		Scheme: "http",
		Limit:  5,
	}
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if cfg.Collection == "" {
		return fmt.Errorf("config.collection is required")
	}

	client, err := weaviate.NewClient(weaviate.Config{
		Host:   cfg.Host,
		Scheme: cfg.Scheme,
	})
	if err != nil {
		return fmt.Errorf("weaviate client: %w", err)
	}

	h.client = client
	h.collection = cfg.Collection
	h.fields = cfg.Fields
	h.limit = cfg.Limit
	if h.limit <= 0 {
		h.limit = 5
	}

	h.actionsCollection = cfg.ActionsCollection
	if h.actionsCollection == "" {
		h.actionsCollection = DefaultActionsCollection
	}
	h.knowledgeCollection = cfg.KnowledgeCollection
	if h.knowledgeCollection == "" {
		h.knowledgeCollection = DefaultKnowledgeCollection
	}

	h.httpAddr = cfg.HTTPAddr
	h.token = cfg.Token

	if h.httpAddr != "" && h.token == "" {
		return fmt.Errorf("config.token is required when http_addr is set")
	}

	autoCreate := cfg.AutoCreateSchema == nil || *cfg.AutoCreateSchema
	if autoCreate {
		if err := h.ensureSchemas(context.Background()); err != nil {
			return fmt.Errorf("auto-create schemas: %w", err)
		}
	}

	if h.httpAddr != "" {
		go func() {
			if err := h.listenHTTP(); err != nil {
				log.Printf("http server: %v", err)
			}
		}()
	}

	return nil
}

// ensureSchemas creates the MCPActions and KnowledgeArticles collections if they don't exist.
func (h *WeaviateHandler) ensureSchemas(ctx context.Context) error {
	if err := h.ensureClass(ctx, h.actionsCollection, []*wmodels.Property{
		{Name: "pluginName", DataType: []string{"text"}},
		{Name: "actionName", DataType: []string{"text"}},
		{Name: "description", DataType: []string{"text"}},
		{Name: "parameters", DataType: []string{"text"}},
	}); err != nil {
		return err
	}
	return h.ensureClass(ctx, h.knowledgeCollection, []*wmodels.Property{
		{Name: "title", DataType: []string{"text"}},
		{Name: "content", DataType: []string{"text"}},
		{Name: "source", DataType: []string{"text"}},
		{Name: "tags", DataType: []string{"text[]"}},
	})
}

func (h *WeaviateHandler) ensureClass(ctx context.Context, name string, props []*wmodels.Property) error {
	exists, err := h.client.Schema().ClassExistenceChecker().WithClassName(name).Do(ctx)
	if err != nil {
		return fmt.Errorf("check class %s: %w", name, err)
	}
	if exists {
		return nil
	}
	return h.client.Schema().ClassCreator().WithClass(&wmodels.Class{
		Class:      name,
		Properties: props,
	}).Do(ctx)
}

// Capabilities declares this plugin's name, description, and actions to the host.
func (h *WeaviateHandler) Capabilities() plugin.CapabilitiesMsg {
	return plugin.CapabilitiesMsg{
		Name:        "weaviate",
		Description: "Retrieval plugin for Weaviate vector database. Performs semantic and hybrid search, manages MCP action indexing, and knowledge article ingestion.",
		Actions: []plugin.ActionMsg{
			{
				Name:        "search",
				Description: "Semantic nearText search — finds objects whose meaning is closest to the query.",
				Parameters: []plugin.ParameterMsg{
					{Name: "query", Description: "Natural language query", Type: "string", Required: true},
					{Name: "limit", Description: "Maximum number of results (overrides config default)", Type: "integer", Required: false},
					{Name: "fields", Description: "Comma-separated list of fields to return (overrides config default)", Type: "string", Required: false},
				},
			},
			{
				Name:        "hybrid_search",
				Description: "Hybrid search combining vector similarity and BM25 keyword matching. Alpha controls the blend: 0 = keyword only, 1 = vector only (default 0.5).",
				Parameters: []plugin.ParameterMsg{
					{Name: "query", Description: "Search query", Type: "string", Required: true},
					{Name: "alpha", Description: "Float 0–1, balance between keyword (0) and vector (1). Default 0.5", Type: "number", Required: false},
					{Name: "limit", Description: "Maximum number of results (overrides config default)", Type: "integer", Required: false},
					{Name: "fields", Description: "Comma-separated list of fields to return (overrides config default)", Type: "string", Required: false},
				},
			},
			{
				Name:        "prepare",
				Description: "RAG preparer: searches Weaviate with the user message and returns the message prepended with relevant context.",
				Parameters: []plugin.ParameterMsg{
					{Name: "text", Description: "Raw user message (injected by the orchestrator)", Type: "string", Required: true},
				},
			},
			{
				Name:        "sync_actions",
				Description: "Upsert plugin action definitions into the MCPActions collection for retrieval-based tool filtering.",
				Parameters: []plugin.ParameterMsg{
					{Name: "payload", Description: `JSON: {"plugin_name":"...","actions":[{"name":"...","description":"...","parameters":...}]}`, Type: "string", Required: true},
				},
			},
			{
				Name:        "ingest",
				Description: "Insert a single knowledge article into the KnowledgeArticles collection.",
				Parameters: []plugin.ParameterMsg{
					{Name: "title", Description: "Article title", Type: "string", Required: true},
					{Name: "content", Description: "Article content", Type: "string", Required: true},
					{Name: "source", Description: "Source identifier", Type: "string", Required: false},
					{Name: "tags", Description: "Comma-separated tags", Type: "string", Required: false},
				},
			},
			{
				Name:        "ingest_batch",
				Description: "Batch insert multiple knowledge articles into the KnowledgeArticles collection.",
				Parameters: []plugin.ParameterMsg{
					{Name: "payload", Description: `JSON array: [{"title":"...","content":"...","source":"...","tags":["..."]}]`, Type: "string", Required: true},
				},
			},
		},
	}
}

// Execute dispatches to the correct action handler.
func (h *WeaviateHandler) Execute(req plugin.Request) plugin.Response {
	if h.client == nil {
		return plugin.Response{CallID: req.ID, Error: "weaviate client not initialised — check plugin config"}
	}
	switch req.Action {
	case "search":
		return h.search(req)
	case "hybrid_search":
		return h.hybridSearch(req)
	case "prepare":
		return h.prepare(req)
	case "sync_actions":
		return h.syncActions(req)
	case "ingest":
		return h.ingest(req)
	case "ingest_batch":
		return h.ingestBatch(req)
	default:
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// ---------------------------------------------------------------------------
// Search actions (existing)
// ---------------------------------------------------------------------------

func (h *WeaviateHandler) search(req plugin.Request) plugin.Response {
	query, ok := req.Args["query"]
	if !ok || query == "" {
		return plugin.Response{CallID: req.ID, Error: "query is required"}
	}

	result, err := h.client.GraphQL().Get().
		WithClassName(h.collection).
		WithFields(h.resolveFields(req.Args)...).
		WithNearText(h.client.GraphQL().NearTextArgBuilder().WithConcepts([]string{query})).
		WithLimit(h.resolveLimit(req.Args)).
		Do(context.Background())

	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("weaviate search: %v", err)}
	}
	return marshalResponse(req.ID, result)
}

func (h *WeaviateHandler) hybridSearch(req plugin.Request) plugin.Response {
	query, ok := req.Args["query"]
	if !ok || query == "" {
		return plugin.Response{CallID: req.ID, Error: "query is required"}
	}

	hybrid := h.client.GraphQL().HybridArgumentBuilder().WithQuery(query)
	if v, ok := req.Args["alpha"]; ok && v != "" {
		if alpha, err := strconv.ParseFloat(v, 32); err == nil {
			hybrid = hybrid.WithAlpha(float32(alpha))
		}
	}

	result, err := h.client.GraphQL().Get().
		WithClassName(h.collection).
		WithFields(h.resolveFields(req.Args)...).
		WithHybrid(hybrid).
		WithLimit(h.resolveLimit(req.Args)).
		Do(context.Background())

	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("weaviate hybrid_search: %v", err)}
	}
	return marshalResponse(req.ID, result)
}

func (h *WeaviateHandler) prepare(req plugin.Request) plugin.Response {
	text, ok := req.Args["text"]
	if !ok || text == "" {
		return plugin.Response{CallID: req.ID, Content: text}
	}

	hybrid := h.client.GraphQL().HybridArgumentBuilder().WithQuery(text)

	result, err := h.client.GraphQL().Get().
		WithClassName(h.collection).
		WithFields(h.resolveFields(map[string]string{})...).
		WithHybrid(hybrid).
		WithLimit(h.limit).
		Do(context.Background())

	if err != nil {
		// Fail-open: forward the original message so the LLM call is not blocked.
		return plugin.Response{CallID: req.ID, Content: text}
	}

	b, _ := json.MarshalIndent(result, "", "  ")

	enriched := fmt.Sprintf(
		"[retrieved_context source=\"weaviate\"]\n%s\n[/retrieved_context]\n\n%s",
		string(b), text,
	)
	return plugin.Response{CallID: req.ID, Content: enriched}
}

// ---------------------------------------------------------------------------
// RAG ingestion actions (new)
// ---------------------------------------------------------------------------

type syncActionsPayload struct {
	PluginName string            `json:"plugin_name"`
	Actions    []syncActionEntry `json:"actions"`
}

type syncActionEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (h *WeaviateHandler) syncActions(req plugin.Request) plugin.Response {
	raw, ok := req.Args["payload"]
	if !ok || raw == "" {
		return plugin.Response{CallID: req.ID, Error: "payload is required"}
	}

	var payload syncActionsPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("parse payload: %v", err)}
	}
	if payload.PluginName == "" {
		return plugin.Response{CallID: req.ID, Error: "plugin_name is required"}
	}

	objects := make([]*wmodels.Object, 0, len(payload.Actions))
	for _, a := range payload.Actions {
		params := ""
		if len(a.Parameters) > 0 {
			params = string(a.Parameters)
		}
		objects = append(objects, &wmodels.Object{
			Class: h.actionsCollection,
			ID:    strfmt.UUID(actionUUID(payload.PluginName, a.Name)),
			Properties: map[string]interface{}{
				"pluginName":  payload.PluginName,
				"actionName":  a.Name,
				"description": a.Description,
				"parameters":  params,
			},
		})
	}

	if len(objects) == 0 {
		return plugin.Response{CallID: req.ID, Content: `{"synced":0}`}
	}

	results, err := h.client.Batch().ObjectsBatcher().WithObjects(objects...).Do(context.Background())
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("batch sync: %v", err)}
	}
	if err := checkBatchErrors(results); err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("batch sync: %v", err)}
	}

	return plugin.Response{CallID: req.ID, Content: fmt.Sprintf(`{"synced":%d}`, len(objects))}
}

type knowledgeArticle struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Source  string   `json:"source"`
	Tags    []string `json:"tags"`
}

func (h *WeaviateHandler) ingest(req plugin.Request) plugin.Response {
	title := req.Args["title"]
	content := req.Args["content"]
	if title == "" || content == "" {
		return plugin.Response{CallID: req.ID, Error: "title and content are required"}
	}

	props := map[string]interface{}{
		"title":   title,
		"content": content,
	}
	if v := req.Args["source"]; v != "" {
		props["source"] = v
	}
	if v := req.Args["tags"]; v != "" {
		props["tags"] = splitCSV(v)
	}

	_, err := h.client.Data().Creator().
		WithClassName(h.knowledgeCollection).
		WithProperties(props).
		Do(context.Background())
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("ingest: %v", err)}
	}

	return plugin.Response{CallID: req.ID, Content: `{"ingested":1}`}
}

func (h *WeaviateHandler) ingestBatch(req plugin.Request) plugin.Response {
	raw, ok := req.Args["payload"]
	if !ok || raw == "" {
		return plugin.Response{CallID: req.ID, Error: "payload is required"}
	}

	var articles []knowledgeArticle
	if err := json.Unmarshal([]byte(raw), &articles); err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("parse payload: %v", err)}
	}

	objects := make([]*wmodels.Object, 0, len(articles))
	for _, a := range articles {
		if a.Title == "" || a.Content == "" {
			continue
		}
		props := map[string]interface{}{
			"title":   a.Title,
			"content": a.Content,
		}
		if a.Source != "" {
			props["source"] = a.Source
		}
		if len(a.Tags) > 0 {
			props["tags"] = a.Tags
		}
		objects = append(objects, &wmodels.Object{
			Class:      h.knowledgeCollection,
			Properties: props,
		})
	}

	if len(objects) == 0 {
		return plugin.Response{CallID: req.ID, Content: `{"ingested":0}`}
	}

	results, err := h.client.Batch().ObjectsBatcher().WithObjects(objects...).Do(context.Background())
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("batch ingest: %v", err)}
	}
	if err := checkBatchErrors(results); err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("batch ingest: %v", err)}
	}

	return plugin.Response{CallID: req.ID, Content: fmt.Sprintf(`{"ingested":%d}`, len(objects))}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *WeaviateHandler) resolveLimit(args map[string]string) int {
	if v, ok := args["limit"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return h.limit
}

func (h *WeaviateHandler) resolveFields(args map[string]string) []graphql.Field {
	var names []string

	if v, ok := args["fields"]; ok && v != "" {
		names = splitCSV(v)
	} else if len(h.fields) > 0 {
		names = h.fields
	}

	fields := make([]graphql.Field, 0, len(names)+1)
	for _, name := range names {
		fields = append(fields, graphql.Field{Name: name})
	}
	fields = append(fields, graphql.Field{
		Name:   "_additional",
		Fields: []graphql.Field{{Name: "distance"}, {Name: "score"}},
	})
	return fields
}

func marshalResponse(callID string, result interface{}) plugin.Response {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return plugin.Response{CallID: callID, Error: fmt.Sprintf("marshal result: %v", err)}
	}
	return plugin.Response{CallID: callID, Content: string(b)}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func actionUUID(pluginName, actionName string) string {
	return uuid.NewSHA1(actionNS, []byte(pluginName+"/"+actionName)).String()
}

func checkBatchErrors(results []wmodels.ObjectsGetResponse) error {
	var msgs []string
	for _, r := range results {
		if r.Result != nil && r.Result.Errors != nil {
			for _, e := range r.Result.Errors.Error {
				if e != nil {
					msgs = append(msgs, e.Message)
				}
			}
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}
