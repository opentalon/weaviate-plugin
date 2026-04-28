package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/filters"
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

// articleNS is a UUID v5 namespace for deterministic KnowledgeArticles IDs
// generated from sync_actions (e.g. one article per plugin's MCP server
// instructions). Distinct from actionNS so the two ID spaces never collide.
var articleNS = uuid.MustParse("e5f9a2b3-6c4d-5e7f-a0b1-2c3d4e5f6a7b")

// MCPSourcePrefix tags KnowledgeArticles records that were generated from a
// plugin's MCP server-instructions text rather than authored manually. Used
// for filtered prune queries — only auto-managed records are deleted.
const MCPSourcePrefix = "mcp:"

// Config is the JSON config block passed via the Init RPC.
type Config struct {
	Host                string         `json:"host"`
	Scheme              string         `json:"scheme"`
	Collection          string         `json:"collection"`
	ActionsCollection   string         `json:"actions_collection"`
	KnowledgeCollection string         `json:"knowledge_collection"`
	Fields              []string       `json:"fields"`
	Limit               int            `json:"limit"`
	AutoCreateSchema    *bool          `json:"auto_create_schema"`
	HTTPAddr            string         `json:"http_addr"`
	Token               string         `json:"token"`
	Vectorizer          string         `json:"vectorizer"`
	ModuleConfig        map[string]any `json:"module_config"`
	MinPrepareScore     *float64       `json:"min_prepare_score"`
	Timeout             string         `json:"timeout"` // Weaviate HTTP client timeout as duration string (e.g. "2m", "90s"); default "2m"
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
	vectorizer          string
	moduleConfig        map[string]any
	minPrepareScore     float64
	clientTimeout       time.Duration
}

// Configure is called by the SDK during the Init RPC with the JSON config block.
func (h *WeaviateHandler) Configure(configJSON string) error {
	log.Println("weaviate-plugin: Configure begin")

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

	log.Printf("weaviate-plugin: connecting to %s://%s", cfg.Scheme, cfg.Host)

	clientTimeout := 2 * time.Minute
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			clientTimeout = d
		} else if err != nil {
			log.Printf("weaviate-plugin: invalid timeout %q, using default %s: %v", cfg.Timeout, clientTimeout, err)
		}
	}
	log.Printf("weaviate-plugin: client timeout = %s (config %q)", clientTimeout, cfg.Timeout)
	client, err := weaviate.NewClient(weaviate.Config{
		Host:    cfg.Host,
		Scheme:  cfg.Scheme,
		Timeout: clientTimeout,
	})
	if err != nil {
		return fmt.Errorf("weaviate client: %w", err)
	}

	h.client = client
	h.clientTimeout = clientTimeout
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

	h.vectorizer = cfg.Vectorizer
	if h.vectorizer == "" {
		h.vectorizer = "text2vec-transformers"
	}
	h.moduleConfig = cfg.ModuleConfig

	if cfg.MinPrepareScore != nil && *cfg.MinPrepareScore > 0 {
		h.minPrepareScore = *cfg.MinPrepareScore
	} else {
		h.minPrepareScore = defaultMinPrepareScore
	}

	if h.httpAddr != "" && h.token == "" {
		return fmt.Errorf("config.token is required when http_addr is set")
	}

	log.Printf("weaviate-plugin: collection=%s vectorizer=%s limit=%d min_prepare_score=%.4f fields=%v",
		h.collection, h.vectorizer, h.limit, h.minPrepareScore, h.fields)
	log.Printf("weaviate-plugin: actions_collection=%s knowledge_collection=%s",
		h.actionsCollection, h.knowledgeCollection)

	autoCreate := cfg.AutoCreateSchema == nil || *cfg.AutoCreateSchema
	if autoCreate {
		log.Println("weaviate-plugin: auto-creating schemas")
		if err := h.ensureSchemas(context.Background()); err != nil {
			return fmt.Errorf("auto-create schemas: %w", err)
		}
		log.Println("weaviate-plugin: schemas ready")
	}

	if h.httpAddr != "" {
		log.Printf("weaviate-plugin: starting HTTP server on %s (token protected)", h.httpAddr)
		go func() {
			if err := h.listenHTTP(); err != nil {
				log.Printf("weaviate-plugin: http server error: %v", err)
			}
		}()
	}

	log.Printf("weaviate-plugin: init done (Configure) host=%s://%s collection=%s http=%s",
		cfg.Scheme, cfg.Host, h.collection, h.httpAddr)

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
	class := &wmodels.Class{
		Class:      name,
		Vectorizer: h.vectorizer,
		Properties: props,
	}
	if h.moduleConfig != nil {
		class.ModuleConfig = map[string]interface{}{
			h.vectorizer: h.moduleConfig,
		}
	}
	return h.client.Schema().ClassCreator().WithClass(class).Do(ctx)
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
				Description: "RAG preparer: searches KnowledgeArticles and MCPActions with the user message and returns structured context with relevant tools.",
				Parameters: []plugin.ParameterMsg{
					{Name: "text", Description: "Raw user message (injected by the orchestrator)", Type: "string", Required: true},
					{Name: "allowed_plugins", Description: "JSON array of allowed plugin names for filtering (injected by orchestrator)", Type: "string", Required: false},
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
			{
				Name:        "ask_knowledge",
				Description: "Search the knowledge base for product docs, how-to guides, and tool descriptions. Use BEFORE asking the user when you need more context.",
				Parameters: []plugin.ParameterMsg{
					{Name: "query", Description: "Natural language question for knowledge base search", Type: "string", Required: true},
					{Name: "plugin", Description: "Narrow results to a specific plugin (e.g. 'jira')", Type: "string", Required: false},
					{Name: "source", Description: "Filter knowledge articles by source identifier (e.g. 'help-center')", Type: "string", Required: false},
					{Name: "limit", Description: "Maximum results per collection (default 3)", Type: "integer", Required: false},
					{Name: "allowed_plugins", Description: "JSON array of allowed plugin names (injected by orchestrator)", Type: "string", Required: false},
				},
			},
			{
				Name:        "search_instructions",
				Description: "Search MCP server instructions stored during plugin sync. Returns guidance and context from MCP servers that match the query.",
				Parameters: []plugin.ParameterMsg{
					{Name: "query", Description: "Natural language query to search instructions", Type: "string", Required: true},
					{Name: "plugin", Description: "Narrow results to a specific plugin (e.g. 'timly')", Type: "string", Required: false},
					{Name: "limit", Description: "Maximum results (default 5)", Type: "integer", Required: false},
				},
			},
			{
				Name:        "refresh",
				Description: "Re-create Weaviate collections if they were deleted externally. Called automatically on session clear.",
				Parameters:  []plugin.ParameterMsg{},
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
	case "ask_knowledge":
		return h.askKnowledge(req)
	case "search_instructions":
		return h.searchInstructions(req)
	case "refresh":
		return h.refresh(req)
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

// prepareResponse is the structured JSON returned by the prepare action.
type prepareResponse struct {
	SendToLLM     bool     `json:"send_to_llm"`
	Message       string   `json:"message"`
	RelevantTools []string `json:"relevant_tools"`
}

// defaultMinPrepareScore is the default minimum hybrid-search score for a
// result to be included in the prepare response. Configurable via
// config.min_prepare_score.
const defaultMinPrepareScore = 0.012

func (h *WeaviateHandler) prepare(req plugin.Request) plugin.Response {
	text := req.Args["text"]
	if text == "" {
		return marshalPrepareResponse(req.ID, prepareResponse{
			SendToLLM:     true,
			Message:       "",
			RelevantTools: []string{},
		})
	}

	ctx := context.Background()

	// Parse allowed_plugins filter if provided by the orchestrator.
	var allowedPlugins []string
	if v, ok := req.Args["allowed_plugins"]; ok && v != "" {
		_ = json.Unmarshal([]byte(v), &allowedPlugins)
	}

	// Search KnowledgeArticles (limit 5) with plugin-boosted query.
	knowledgeFields := []graphql.Field{
		{Name: "title"}, {Name: "content"}, {Name: "source"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}
	knowledgeQuery := text
	if len(allowedPlugins) > 0 {
		knowledgeQuery = text + " " + strings.Join(allowedPlugins, " ")
	}
	knowledgeResult, knowledgeErr := h.searchCollection(ctx, h.knowledgeCollection, knowledgeFields, knowledgeQuery, 5, nil)

	// Search MCPActions (limit 5) with optional plugin filter.
	// Only pluginName + actionName + score needed — the orchestrator already
	// has full tool definitions in the system prompt via relevant_tools filtering.
	actionFields := []graphql.Field{
		{Name: "pluginName"}, {Name: "actionName"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}
	var actionsWhere *filters.WhereBuilder
	if len(allowedPlugins) > 0 {
		actionsWhere = filters.Where().
			WithPath([]string{"pluginName"}).
			WithOperator(filters.ContainsAny).
			WithValueText(allowedPlugins...)
	}
	actionsResult, actionsErr := h.searchCollection(ctx, h.actionsCollection, actionFields, text, 5, actionsWhere)

	// Fail-open: if both searches fail, pass through unchanged.
	if knowledgeErr != nil && actionsErr != nil {
		return marshalPrepareResponse(req.ID, prepareResponse{
			SendToLLM:     true,
			Message:       text,
			RelevantTools: []string{},
		})
	}

	// Extract relevant tool names (above score threshold) for system prompt filtering.
	// The orchestrator uses this list to decide which tools to show the LLM.
	tools := extractToolNamesAboveScore(actionsResult, h.actionsCollection, h.minPrepareScore)

	// When no real tools matched, return nil so the orchestrator shows ALL
	// tools (relevantToolsActive=false). Only activate filtering when we
	// actually found relevant actions — otherwise the LLM loses access to
	// every plugin except ask_knowledge.
	if len(tools) > 0 {
		// Add ask_knowledge so the LLM can discover additional tools on demand.
		tools = append(tools, "weaviate.ask_knowledge")
	} else {
		tools = nil
	}

	log.Printf("weaviate-plugin: prepare: query=%q min_score=%.4f matched_tools=%d tools=%v", text, h.minPrepareScore, len(tools), tools)

	// Build message: only include knowledge context (if non-empty).
	// Action/tool context is NOT injected into the message — the orchestrator
	// already provides full tool definitions in the system prompt filtered by
	// relevant_tools. Duplicating them here wastes tokens.
	message := text
	if knowledgeErr == nil {
		knowledgeItems := extractItems(knowledgeResult, h.knowledgeCollection)
		// Exclude MCP server-instructions articles — they are already in the
		// system prompt via SystemPromptAddition. Including them in
		// [knowledge_context] would duplicate them on every LLM call.
		knowledgeItems = filterOutMCPItems(knowledgeItems)
		log.Printf("weaviate-plugin: prepare: knowledge_items=%d knowledge_err=%v", len(knowledgeItems), knowledgeErr)
		if knowledgeText := formatItemsCompact(knowledgeItems, h.minPrepareScore); knowledgeText != "" {
			log.Printf("weaviate-plugin: prepare: injecting knowledge_context len=%d", len(knowledgeText))
			message = fmt.Sprintf("[knowledge_context]\n%s\n[/knowledge_context]\n\n%s", knowledgeText, text)
		}
	}

	return marshalPrepareResponse(req.ID, prepareResponse{
		SendToLLM:     true,
		Message:       message,
		RelevantTools: tools,
	})
}

// searchCollection performs a hybrid search (alpha=0.5) on the given collection
// with an optional where filter.
func (h *WeaviateHandler) searchCollection(
	ctx context.Context,
	className string,
	fields []graphql.Field,
	query string,
	limit int,
	where *filters.WhereBuilder,
) (interface{}, error) {
	builder := h.client.GraphQL().Get().
		WithClassName(className).
		WithFields(fields...).
		WithHybrid(h.client.GraphQL().HybridArgumentBuilder().WithQuery(query)).
		WithLimit(limit)

	if where != nil {
		builder = builder.WithWhere(where)
	}

	return builder.Do(ctx)
}


// extractToolNamesAboveScore extracts "pluginName.actionName" strings from an
// MCPActions GraphQL response, filtering out results below the given score threshold.
func extractToolNamesAboveScore(result interface{}, className string, minScore float64) []string {
	items := extractItems(result, className)
	if len(items) == 0 {
		return []string{}
	}
	tools := make([]string, 0, len(items))
	for _, obj := range items {
		if !aboveScore(obj, minScore) {
			continue
		}
		pluginName, _ := obj["pluginName"].(string)
		actionName, _ := obj["actionName"].(string)
		if pluginName != "" && actionName != "" {
			tools = append(tools, pluginName+"."+actionName)
		}
	}
	return tools
}

// formatItemsCompact formats pre-extracted items above the score threshold as
// compact text (title + content only). Returns "" when no items pass.
func formatItemsCompact(items []map[string]interface{}, minScore float64) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		if !aboveScore(item, minScore) {
			continue
		}
		title, _ := item["title"].(string)
		content, _ := item["content"].(string)
		if title == "" && content == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n---\n")
		}
		fmt.Fprintf(&sb, "%s\n%s", title, content)
	}
	return sb.String()
}

// filterOutMCPItems removes KnowledgeArticles items whose source starts with
// "mcp:" (server-instructions synced via sync_actions). These are already in
// the system prompt via SystemPromptAddition and must not be duplicated as
// [knowledge_context].
func filterOutMCPItems(items []map[string]interface{}) []map[string]interface{} {
	filtered := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		source, _ := item["source"].(string)
		if strings.HasPrefix(source, MCPSourcePrefix) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// aboveScore checks whether a GraphQL result object's _additional.score
// meets the minimum threshold.
func aboveScore(obj map[string]interface{}, minScore float64) bool {
	additional, _ := obj["_additional"].(map[string]interface{})
	if additional == nil {
		return true // no score info → include by default
	}
	scoreStr, _ := additional["score"].(string)
	if scoreStr == "" {
		return true
	}
	score, err := strconv.ParseFloat(scoreStr, 64)
	if err != nil {
		return true
	}
	return score >= minScore
}

func marshalPrepareResponse(callID string, resp prepareResponse) plugin.Response {
	b, _ := json.Marshal(resp)
	return plugin.Response{CallID: callID, Content: string(b)}
}

// ---------------------------------------------------------------------------
// ask_knowledge — LLM-callable knowledge base query (Phase 3)
// ---------------------------------------------------------------------------

func (h *WeaviateHandler) askKnowledge(req plugin.Request) plugin.Response {
	query, ok := req.Args["query"]
	if !ok || query == "" {
		return plugin.Response{CallID: req.ID, Error: "query is required"}
	}

	ctx := context.Background()

	limit := 3
	if v, ok := req.Args["limit"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	// Build plugin filter for MCPActions.
	var actionsWhere *filters.WhereBuilder
	if p := req.Args["plugin"]; p != "" {
		// Specific plugin filter takes precedence.
		actionsWhere = filters.Where().
			WithPath([]string{"pluginName"}).
			WithOperator(filters.Equal).
			WithValueText(p)
	} else if v, ok := req.Args["allowed_plugins"]; ok && v != "" {
		var allowedPlugins []string
		if err := json.Unmarshal([]byte(v), &allowedPlugins); err == nil && len(allowedPlugins) > 0 {
			actionsWhere = filters.Where().
				WithPath([]string{"pluginName"}).
				WithOperator(filters.ContainsAny).
				WithValueText(allowedPlugins...)
		}
	}

	// Build source filter for KnowledgeArticles.
	var knowledgeWhere *filters.WhereBuilder
	if s := req.Args["source"]; s != "" {
		knowledgeWhere = filters.Where().
			WithPath([]string{"source"}).
			WithOperator(filters.Equal).
			WithValueText(s)
	}

	// Search both collections.
	knowledgeFields := []graphql.Field{
		{Name: "title"}, {Name: "content"}, {Name: "source"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}
	knowledgeResult, knowledgeErr := h.searchCollection(ctx, h.knowledgeCollection, knowledgeFields, query, limit, knowledgeWhere)

	actionFields := []graphql.Field{
		{Name: "pluginName"}, {Name: "actionName"}, {Name: "description"}, {Name: "parameters"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}
	actionsResult, actionsErr := h.searchCollection(ctx, h.actionsCollection, actionFields, query, limit, actionsWhere)

	if knowledgeErr != nil && actionsErr != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("knowledge search failed: %v; actions search failed: %v", knowledgeErr, actionsErr)}
	}

	// Format results as human-readable text.
	var sections []string

	if knowledgeErr == nil {
		if text := formatKnowledgeResults(knowledgeResult, h.knowledgeCollection); text != "" {
			sections = append(sections, text)
		}
	}
	if actionsErr == nil {
		if text := formatActionResults(actionsResult, h.actionsCollection); text != "" {
			sections = append(sections, text)
		}
	}

	if len(sections) == 0 {
		return plugin.Response{CallID: req.ID, Content: "No relevant results found."}
	}

	return plugin.Response{CallID: req.ID, Content: strings.Join(sections, "\n\n")}
}

// searchInstructions searches the KnowledgeArticles collection for MCP server
// instructions (source starts with "mcp:"). This surfaces guidance that MCP
// servers declared at init time.
func (h *WeaviateHandler) searchInstructions(req plugin.Request) plugin.Response {
	query, ok := req.Args["query"]
	if !ok || query == "" {
		return plugin.Response{CallID: req.ID, Error: "query is required"}
	}

	ctx := context.Background()

	limit := 5
	if v, ok := req.Args["limit"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	// Filter to MCP-sourced instructions only (source starts with "mcp:").
	where := filters.Where().
		WithPath([]string{"source"}).
		WithOperator(filters.Like).
		WithValueText("mcp:*")

	// Optionally narrow to a specific plugin.
	if p := req.Args["plugin"]; p != "" {
		where = filters.Where().
			WithPath([]string{"source"}).
			WithOperator(filters.Equal).
			WithValueText(MCPSourcePrefix + p)
	}

	fields := []graphql.Field{
		{Name: "title"}, {Name: "content"}, {Name: "source"}, {Name: "tags"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}

	result, err := h.searchCollection(ctx, h.knowledgeCollection, fields, query, limit, where)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("search instructions: %v", err)}
	}

	text := formatKnowledgeResults(result, h.knowledgeCollection)
	if text == "" {
		return plugin.Response{CallID: req.ID, Content: "No MCP server instructions found."}
	}
	return plugin.Response{CallID: req.ID, Content: text}
}

func (h *WeaviateHandler) refresh(req plugin.Request) plugin.Response {
	if err := h.ensureSchemas(context.Background()); err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("refresh: %v", err)}
	}
	log.Println("weaviate-plugin: refresh: schemas verified")
	return plugin.Response{CallID: req.ID, Content: `{"refreshed":true}`}
}

// formatKnowledgeResults formats KnowledgeArticles results as readable text.
func formatKnowledgeResults(result interface{}, className string) string {
	items := extractItems(result, className)
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Knowledge Articles\n")
	for i, item := range items {
		title, _ := item["title"].(string)
		content, _ := item["content"].(string)
		source, _ := item["source"].(string)
		if title == "" && content == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n### %d. %s\n", i+1, title)
		if source != "" {
			fmt.Fprintf(&sb, "Source: %s\n", source)
		}
		fmt.Fprintf(&sb, "%s\n", content)
	}
	return sb.String()
}

// formatActionResults formats MCPActions results as readable text.
func formatActionResults(result interface{}, className string) string {
	items := extractItems(result, className)
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Available Tools\n")
	for i, item := range items {
		pluginName, _ := item["pluginName"].(string)
		actionName, _ := item["actionName"].(string)
		description, _ := item["description"].(string)
		if pluginName == "" || actionName == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n### %d. %s.%s\n", i+1, pluginName, actionName)
		if description != "" {
			fmt.Fprintf(&sb, "%s\n", description)
		}
	}
	return sb.String()
}

// extractItems drills into a GraphQL response and returns the result objects.
func extractItems(result interface{}, className string) []map[string]interface{} {
	if result == nil {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}

	get, ok := raw["Get"].(map[string]interface{})
	if !ok {
		if data, ok := raw["data"].(map[string]interface{}); ok {
			get, _ = data["Get"].(map[string]interface{})
		}
	}
	if get == nil {
		return nil
	}

	arr, ok := get[className].([]interface{})
	if !ok {
		return nil
	}
	items := make([]map[string]interface{}, 0, len(arr))
	for _, v := range arr {
		if obj, ok := v.(map[string]interface{}); ok {
			items = append(items, obj)
		}
	}
	return items
}

// ---------------------------------------------------------------------------
// RAG ingestion actions
// ---------------------------------------------------------------------------

// syncActionsPayload mirrors the JSON shape sent by opentalon's orchestrator.
// ServerInstructions and KeepPlugins are added by opentalon/opentalon#107 — both
// are optional, so older orchestrators (which omit them) keep working.
type syncActionsPayload struct {
	PluginName         string            `json:"plugin_name"`
	Actions            []syncActionEntry `json:"actions"`
	ServerInstructions string            `json:"server_instructions,omitempty"`
	KeepPlugins        []string          `json:"keep_plugins,omitempty"`
	// IsContinuationBatch is set by the orchestrator on batches 1..N of a
	// chunked plugin sync so the plugin skips the per-plugin pre-delete that
	// would otherwise wipe the previous batches' inserts. Batch 0 (default
	// false) keeps the orphan-prune semantic. Older orchestrators omit the
	// field entirely — default false matches the legacy single-batch behaviour.
	IsContinuationBatch bool `json:"is_continuation_batch,omitempty"`
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

	ctx := context.Background()

	// Delete all existing actions for this plugin before re-syncing so that
	// removed/renamed actions don't linger as stale entries (intra-plugin
	// orphan prune from #16). KnowledgeArticles records keyed by deterministic
	// UUID are overwritten by upsert below, so they don't need a pre-delete.
	//
	// Skip on continuation batches (1..N of a chunked sync) — otherwise each
	// batch would wipe the previous batches' inserts, leaving only the last
	// batch's contents persisted. Batch 0 (IsContinuationBatch=false) carries
	// the orphan-prune semantic for the entire plugin.
	if !payload.IsContinuationBatch {
		_, delErr := h.client.Batch().ObjectsBatchDeleter().
			WithClassName(h.actionsCollection).
			WithWhere(filters.Where().
				WithPath([]string{"pluginName"}).
				WithOperator(filters.Equal).
				WithValueText(payload.PluginName)).
			Do(ctx)
		if delErr != nil {
			log.Printf("weaviate-plugin: sync_actions: failed to delete old actions for %s: %v", payload.PluginName, delErr)
		}
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

	// Upsert one KnowledgeArticles record carrying the plugin's server-level
	// instructions text (e.g. an MCP server's `initialize.instructions`) when
	// the orchestrator forwarded it. Keyed by deterministic UUID so re-runs are
	// idempotent and a removed plugin's record is overwritten cleanly on the
	// next sync (or pruned via keep_plugins below).
	var instructionsObj *wmodels.Object
	if payload.ServerInstructions != "" {
		log.Printf("weaviate-plugin: sync_actions: storing server instructions for %s (%d bytes)", payload.PluginName, len(payload.ServerInstructions))
		instructionsObj = &wmodels.Object{
			Class: h.knowledgeCollection,
			ID:    strfmt.UUID(serverInstructionsUUID(payload.PluginName)),
			Properties: map[string]interface{}{
				"title":   payload.PluginName + " MCP server instructions",
				"content": payload.ServerInstructions,
				"source":  MCPSourcePrefix + payload.PluginName,
				"tags":    []string{"opentalon", "mcp", "server-instructions", payload.PluginName},
			},
		}
		objects = append(objects, instructionsObj)
	}

	syncedActions := len(payload.Actions)
	if len(objects) > 0 {
		results, err := h.client.Batch().ObjectsBatcher().WithObjects(objects...).Do(ctx)
		if err != nil {
			return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("batch sync: %v", err)}
		}
		if err := checkBatchErrors(results); err != nil {
			return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("batch sync: %v", err)}
		}
	}

	// Inter-plugin orphan prune: when the orchestrator ships the authoritative
	// live-plugin list, delete records for plugins no longer present. Idempotent
	// across the per-plugin calls of one full startup sync, so it's safe to
	// apply on every call. Skip when keep_plugins is empty — that could
	// legitimately mean "no plugins" but it's also the legacy / corrupted-payload
	// case, and nuking the whole index on a bad input is too costly.
	pruned := 0
	if len(payload.KeepPlugins) > 0 {
		n, err := h.pruneOrphans(ctx, payload.KeepPlugins)
		if err != nil {
			log.Printf("weaviate-plugin: prune_orphans failed: %v", err)
			// Don't fail the sync_actions call on prune error — upserts already
			// succeeded, and a transient delete failure is recoverable on next sync.
		}
		pruned = n
	}

	respBody := fmt.Sprintf(`{"synced":%d`, syncedActions)
	if instructionsObj != nil {
		respBody += `,"server_instructions_synced":1`
	}
	if len(payload.KeepPlugins) > 0 {
		respBody += fmt.Sprintf(`,"orphans_pruned":%d`, pruned)
	}
	respBody += `}`
	return plugin.Response{CallID: req.ID, Content: respBody}
}

// pruneOrphans deletes any MCPActions and any auto-generated KnowledgeArticles
// (those with source matching MCPSourcePrefix) whose plugin is NOT in keep.
//
// Implementation note: instead of a single batch-delete with a NotEqual filter
// (Weaviate's NotEqual on tokenized text fields produces unreliable results
// for our purposes), we discover the set of distinct pluginName / source
// values currently indexed and issue one Equal-based batch-delete per orphan.
// Equal on tokenized text matches the full property value reliably — same
// pattern PR #16's intra-plugin pre-delete depends on.
//
// Returns the total deletion count across both collections (best-effort:
// per-class errors are recorded but do not abort the second class delete).
func (h *WeaviateHandler) pruneOrphans(ctx context.Context, keep []string) (int, error) {
	if len(keep) == 0 {
		return 0, nil
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}

	total := 0
	var firstErr error

	// MCPActions: discover distinct pluginNames, delete each orphan by Equal.
	plugins, err := h.distinctValues(ctx, h.actionsCollection, "pluginName", nil)
	if err != nil {
		firstErr = fmt.Errorf("MCPActions distinct: %w", err)
	}
	for _, p := range plugins {
		if keepSet[p] {
			continue
		}
		n, err := h.batchDeleteEqual(ctx, h.actionsCollection, "pluginName", p)
		total += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("MCPActions delete %s: %w", p, err)
		}
	}

	// KnowledgeArticles: scope to MCP-managed records (source LIKE "mcp:*"),
	// discover distinct sources, delete each orphan by Equal on source.
	mcpScope := filters.Where().
		WithPath([]string{"source"}).
		WithOperator(filters.Like).
		WithValueText(MCPSourcePrefix + "*")
	sources, err := h.distinctValues(ctx, h.knowledgeCollection, "source", mcpScope)
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("KnowledgeArticles distinct: %w", err)
	}
	for _, src := range sources {
		plugin := strings.TrimPrefix(src, MCPSourcePrefix)
		if keepSet[plugin] {
			continue
		}
		n, err := h.batchDeleteEqual(ctx, h.knowledgeCollection, "source", src)
		total += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("KnowledgeArticles delete %s: %w", src, err)
		}
	}
	return total, firstErr
}

// distinctValues scans `class` (optionally filtered by `where`) and returns
// the distinct non-empty values of property `path`. The 10000-record limit
// is well above any realistic plugin count and the typical sync-managed
// article count; trip it and we'd need to switch to GraphQL Aggregate.
func (h *WeaviateHandler) distinctValues(ctx context.Context, class, path string, where *filters.WhereBuilder) ([]string, error) {
	builder := h.client.GraphQL().Get().
		WithClassName(class).
		WithFields(graphql.Field{Name: path}).
		WithLimit(10000)
	if where != nil {
		builder = builder.WithWhere(where)
	}
	result, err := builder.Do(ctx)
	if err != nil {
		return nil, err
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("%s", result.Errors[0].Message)
	}
	seen := map[string]bool{}
	for _, item := range extractItems(result, class) {
		if v, ok := item[path].(string); ok && v != "" {
			seen[v] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	return out, nil
}

// batchDeleteEqual deletes objects in class where path Equal value.
func (h *WeaviateHandler) batchDeleteEqual(ctx context.Context, class, path, value string) (int, error) {
	where := filters.Where().
		WithPath([]string{path}).
		WithOperator(filters.Equal).
		WithValueText(value)
	resp, err := h.client.Batch().ObjectsBatchDeleter().
		WithClassName(class).
		WithWhere(where).
		Do(ctx)
	if err != nil {
		return 0, err
	}
	if resp != nil && resp.Results != nil {
		return int(resp.Results.Successful), nil
	}
	return 0, nil
}

// serverInstructionsUUID returns a deterministic UUID v5 for the
// per-plugin server-instructions article. Re-runs upsert in place.
func serverInstructionsUUID(pluginName string) string {
	return uuid.NewSHA1(articleNS, []byte("mcp-server-instructions:"+pluginName)).String()
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
