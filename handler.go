package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"
)

// Config is the JSON config block passed via the Init RPC.
type Config struct {
	Host       string   `json:"host"`        // e.g. "localhost:8080"
	Scheme     string   `json:"scheme"`      // "http" or "https"
	Collection string   `json:"collection"`  // Weaviate class name to search
	Fields     []string `json:"fields"`      // fields to return in results
	Limit      int      `json:"limit"`       // default result limit
}

// WeaviateHandler implements plugin.Handler and plugin.Configurable.
type WeaviateHandler struct {
	client     *weaviate.Client
	collection string
	fields     []string
	limit      int
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
	return nil
}

// Capabilities declares this plugin's name, description, and actions to the host.
func (h *WeaviateHandler) Capabilities() plugin.CapabilitiesMsg {
	return plugin.CapabilitiesMsg{
		Name:        "weaviate",
		Description: "Retrieval plugin for Weaviate vector database. Performs semantic and hybrid search to fetch relevant context before passing to an LLM.",
		Actions: []plugin.ActionMsg{
			// ── Tool actions — LLM-callable ──────────────────────────────────
			{
				Name:        "search",
				Description: "Semantic nearText search — finds objects whose meaning is closest to the query. Use this when you want similarity-based retrieval.",
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
			// ── Preparer action — runs automatically before the LLM ──────────
			// Wire this up in config.yaml as a content_preparer, not as a tool.
			// The orchestrator passes the raw user message as `text`, and this
			// action returns the original message prepended with retrieved context,
			// so the LLM sees enriched input without needing to call any tool itself.
			{
				Name:        "prepare",
				Description: "RAG preparer: searches Weaviate with the user message and returns the message prepended with relevant context. Intended as a content_preparer, not a direct LLM tool.",
				Parameters: []plugin.ParameterMsg{
					{Name: "text", Description: "Raw user message (injected by the orchestrator)", Type: "string", Required: true},
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
	default:
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// search performs a nearText (semantic) query.
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

// hybridSearch performs a hybrid (vector + BM25) query.
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

// prepare is the RAG preparer action. It receives the raw user message as `text`,
// runs a hybrid search, and returns the original message prepended with a context
// block that the LLM can use without having to call any tool itself.
//
// The orchestrator replaces the user message with this return value before the
// first LLM call, so the LLM always sees the enriched input automatically.
func (h *WeaviateHandler) prepare(req plugin.Request) plugin.Response {
	text, ok := req.Args["text"]
	if !ok || text == "" {
		// Nothing to enrich — pass through unchanged.
		return plugin.Response{CallID: req.ID, Content: text}
	}

	// Use hybrid search (alpha=0.5) so both keyword and semantic signals contribute.
	hybrid := h.client.GraphQL().HybridArgumentBuilder().WithQuery(text)

	result, err := h.client.GraphQL().Get().
		WithClassName(h.collection).
		WithFields(h.resolveFields(map[string]string{})...).
		WithHybrid(hybrid).
		WithLimit(h.limit).
		Do(context.Background())

	if err != nil {
		// Fail-open: log the error but still forward the original message so the
		// LLM call is not blocked by a Weaviate outage.
		return plugin.Response{CallID: req.ID, Content: text}
	}

	b, _ := json.MarshalIndent(result, "", "  ")

	// Prepend the retrieved context as a clearly delimited block.
	enriched := fmt.Sprintf(
		"[retrieved_context source=\"weaviate\"]\n%s\n[/retrieved_context]\n\n%s",
		string(b), text,
	)
	return plugin.Response{CallID: req.ID, Content: enriched}
}

// resolveLimit returns the per-call limit or falls back to the configured default.
func (h *WeaviateHandler) resolveLimit(args map[string]string) int {
	if v, ok := args["limit"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return h.limit
}

// resolveFields builds a []graphql.Field from the per-call override, config
// default, or a bare _additional block when nothing is specified.
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

// marshalResponse JSON-encodes the GraphQL response data for the host.
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
