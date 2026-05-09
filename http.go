package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/filters"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"
)

// listenHTTP starts the token-protected HTTP ingestion server.
//
// /metrics is intentionally OPEN (no token) so a Prometheus scrape job
// running in another namespace can collect the translator + plugin
// metrics without sharing the ingestion bearer token. Scrape access is
// limited at the network layer via the existing weaviate / opentalon
// NetworkPolicies, same pattern as Weaviate itself uses.
func (h *WeaviateHandler) listenHTTP() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", h.handleHealth)
	mux.Handle("GET /metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}))
	mux.HandleFunc("POST /api/v1/articles", h.requireToken(h.handleIngestArticle))
	mux.HandleFunc("POST /api/v1/articles/batch", h.requireToken(h.handleIngestBatch))
	mux.HandleFunc("DELETE /api/v1/articles/{id}", h.requireToken(h.handleDeleteArticle))
	mux.HandleFunc("DELETE /api/v1/articles", h.requireToken(h.handleDeleteArticlesBySource))
	mux.HandleFunc("POST /api/v1/actions/sync", h.requireToken(h.handleSyncActions))
	mux.HandleFunc("POST /api/v1/glossary/sync", h.requireToken(h.handleSyncGlossary))
	mux.HandleFunc("GET /api/v1/sync/status", h.requireToken(h.handleSyncStatus))
	mux.HandleFunc("POST /api/v1/debug/prepare", h.requireToken(h.handleDebugPrepare))
	return http.ListenAndServe(h.httpAddr, mux)
}

// requireToken is middleware that enforces Bearer token authentication.
func (h *WeaviateHandler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" {
			next(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || auth[7:] != h.token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		next(w, r)
	}
}

func (h *WeaviateHandler) handleIngestArticle(w http.ResponseWriter, r *http.Request) {
	var body knowledgeArticle
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	resp := h.Execute(plugin.Request{
		ID:     "http",
		Action: "ingest",
		Args: map[string]string{
			"title":   body.Title,
			"content": body.Content,
			"source":  body.Source,
			"tags":    strings.Join(body.Tags, ","),
		},
	})

	writePluginResponse(w, resp)
}

func (h *WeaviateHandler) handleIngestBatch(w http.ResponseWriter, r *http.Request) {
	var articles []knowledgeArticle
	if err := json.NewDecoder(r.Body).Decode(&articles); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	payload, _ := json.Marshal(articles)
	resp := h.Execute(plugin.Request{
		ID:     "http",
		Action: "ingest_batch",
		Args:   map[string]string{"payload": string(payload)},
	})

	writePluginResponse(w, resp)
}

func (h *WeaviateHandler) handleDeleteArticle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing article id")
		return
	}
	err := h.client.Data().Deleter().
		WithClassName(h.knowledgeCollection).
		WithID(id).
		Do(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("delete: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"deleted":true,"id":%q}`, id)
}

func (h *WeaviateHandler) handleDeleteArticlesBySource(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		writeJSONError(w, http.StatusBadRequest, "missing ?source= query parameter")
		return
	}
	n, err := h.batchDeleteEqual(r.Context(), h.knowledgeCollection, "source", source)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("batch delete: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"deleted":%d,"source":%q}`, n, source)
}

func (h *WeaviateHandler) handleSyncActions(w http.ResponseWriter, r *http.Request) {
	var body syncActionsPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	payload, _ := json.Marshal(body)
	resp := h.Execute(plugin.Request{
		ID:     "http",
		Action: "sync_actions",
		Args:   map[string]string{"payload": string(payload)},
	})

	writePluginResponse(w, resp)
}

func (h *WeaviateHandler) handleSyncGlossary(w http.ResponseWriter, r *http.Request) {
	var body syncGlossaryPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	payload, _ := json.Marshal(body)
	resp := h.Execute(plugin.Request{
		ID:     "http",
		Action: "sync_glossary",
		Args:   map[string]string{"payload": string(payload)},
	})

	writePluginResponse(w, resp)
}

func writePluginResponse(w http.ResponseWriter, resp plugin.Response) {
	w.Header().Set("Content-Type", "application/json")
	if resp.Error != "" {
		w.WriteHeader(http.StatusInternalServerError)
		b, _ := json.Marshal(map[string]string{"error": resp.Error})
		_, _ = w.Write(b)
		return
	}
	_, _ = w.Write([]byte(resp.Content))
}

func (h *WeaviateHandler) handleSyncStatus(w http.ResponseWriter, _ *http.Request) {
	resp := h.Execute(plugin.Request{ID: "http", Action: "sync_status"})
	writePluginResponse(w, resp)
}

func (h *WeaviateHandler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.client == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"status":"not_ready","error":"weaviate client not initialised"}`)
		return
	}
	live, err := h.client.Misc().LiveChecker().Do(context.Background())
	if err != nil || !live {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"not_ready","error":"weaviate not reachable: %v"}`, err)
		return
	}
	_, _ = fmt.Fprint(w, `{"status":"ok"}`)
}

// debugPrepareRequest is the body of POST /api/v1/debug/prepare.
type debugPrepareRequest struct {
	Text           string   `json:"text"`
	AllowedPlugins []string `json:"allowed_plugins,omitempty"`
}

// debugPrepareResponse exposes the full pre/post-translator pipeline so
// operators can reproduce a single user query end-to-end without fishing
// in pod logs. Surfaces the translation, the actual query string sent to
// Weaviate, and the top results from both collections with scores. Only
// reachable when http_addr is configured AND the bearer token is supplied.
type debugPrepareResponse struct {
	Original         string             `json:"original"`
	SearchText       string             `json:"search_text"`
	Translated       bool               `json:"translated"`
	TranslatorMs     float64            `json:"translator_ms"`
	MinPrepareScore  float64            `json:"min_prepare_score"`
	MatchedTools     []string           `json:"matched_tools"`
	ActionsTop       []debugScoredEntry `json:"actions_top"`
	KnowledgeTop     []debugScoredEntry `json:"knowledge_top"`
	GlossaryTop      []debugScoredEntry `json:"glossary_top"`
	WeaviateMs       float64            `json:"weaviate_ms"`
	KnowledgeQuery   string             `json:"knowledge_query"`
	ActionsCollWhere string             `json:"actions_where,omitempty"`
}

type debugScoredEntry struct {
	Score      float64 `json:"score"`
	PluginName string  `json:"plugin_name,omitempty"`
	ActionName string  `json:"action_name,omitempty"`
	Title      string  `json:"title,omitempty"`
	Source     string  `json:"source,omitempty"`
	Term       string  `json:"term,omitempty"`
}

// handleDebugPrepare runs the SAME pipeline as Execute("prepare", ...) but
// returns a structured trace rather than the LLM-bound message envelope.
// Useful for "what got sent to Weaviate for THIS query?" investigations.
func (h *WeaviateHandler) handleDebugPrepare(w http.ResponseWriter, r *http.Request) {
	var body debugPrepareRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSONError(w, http.StatusBadRequest, "text is required")
		return
	}

	ctx := r.Context()

	tStart := time.Now()
	searchText := h.translateQuery(ctx, body.Text, "debug_prepare")
	translatorMs := float64(time.Since(tStart).Microseconds()) / 1000.0

	knowledgeQuery := searchText
	if len(body.AllowedPlugins) > 0 {
		knowledgeQuery = searchText + " " + strings.Join(body.AllowedPlugins, " ")
	}

	knowledgeFields := []graphql.Field{
		{Name: "title"}, {Name: "content"}, {Name: "source"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}
	actionFields := []graphql.Field{
		{Name: "pluginName"}, {Name: "actionName"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}

	var actionsWhere *filters.WhereBuilder
	whereLabel := ""
	if len(body.AllowedPlugins) > 0 {
		actionsWhere = filters.Where().
			WithPath([]string{"pluginName"}).
			WithOperator(filters.ContainsAny).
			WithValueText(body.AllowedPlugins...)
		whereLabel = "pluginName ContainsAny " + strings.Join(body.AllowedPlugins, ",")
	}

	glossaryFields := []graphql.Field{
		{Name: "term"}, {Name: "definition"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}

	wStart := time.Now()
	knowledgeResult, _ := h.searchCollection(ctx, h.knowledgeCollection, knowledgeFields, knowledgeQuery, 5, nil)
	actionsResult, _ := h.searchCollection(ctx, h.actionsCollection, actionFields, searchText, 10, actionsWhere)
	glossaryResult, _ := h.searchCollection(ctx, h.glossaryCollection, glossaryFields, searchText, 5, nil)
	weaviateMs := float64(time.Since(wStart).Microseconds()) / 1000.0

	tools := extractToolNamesAboveScore(actionsResult, h.actionsCollection, h.minPrepareScore)

	resp := debugPrepareResponse{
		Original:         body.Text,
		SearchText:       searchText,
		Translated:       searchText != body.Text,
		TranslatorMs:     translatorMs,
		MinPrepareScore:  h.minPrepareScore,
		MatchedTools:     tools,
		ActionsTop:       extractScoredActions(actionsResult, h.actionsCollection),
		KnowledgeTop:     extractScoredKnowledge(knowledgeResult, h.knowledgeCollection),
		GlossaryTop:      extractScoredGlossary(glossaryResult, h.glossaryCollection),
		WeaviateMs:       weaviateMs,
		KnowledgeQuery:   knowledgeQuery,
		ActionsCollWhere: whereLabel,
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func extractScoredActions(result interface{}, className string) []debugScoredEntry {
	items := extractItems(result, className)
	out := make([]debugScoredEntry, 0, len(items))
	for _, item := range items {
		entry := debugScoredEntry{Score: extractScore(item)}
		entry.PluginName, _ = item["pluginName"].(string)
		entry.ActionName, _ = item["actionName"].(string)
		out = append(out, entry)
	}
	return out
}

func extractScoredKnowledge(result interface{}, className string) []debugScoredEntry {
	items := extractItems(result, className)
	out := make([]debugScoredEntry, 0, len(items))
	for _, item := range items {
		entry := debugScoredEntry{Score: extractScore(item)}
		entry.Title, _ = item["title"].(string)
		entry.Source, _ = item["source"].(string)
		out = append(out, entry)
	}
	return out
}

func extractScoredGlossary(result interface{}, className string) []debugScoredEntry {
	items := extractItems(result, className)
	out := make([]debugScoredEntry, 0, len(items))
	for _, item := range items {
		entry := debugScoredEntry{Score: extractScore(item)}
		entry.Term, _ = item["term"].(string)
		out = append(out, entry)
	}
	return out
}

func extractScore(obj map[string]interface{}) float64 {
	additional, _ := obj["_additional"].(map[string]interface{})
	if additional == nil {
		return 0
	}
	scoreStr, _ := additional["score"].(string)
	if scoreStr == "" {
		return 0
	}
	var score float64
	_, _ = fmt.Sscanf(scoreStr, "%f", &score)
	return score
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}
