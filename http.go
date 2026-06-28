package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// listenHTTP starts the token-protected HTTP ingestion server.
//
// /metrics is intentionally OPEN (no token) so a Prometheus scrape job
// running in another namespace can collect the plugin metrics without
// sharing the ingestion bearer token. Scrape access is limited at the
// network layer via the existing weaviate / opentalon NetworkPolicies,
// same pattern as Weaviate itself uses.
func (h *WeaviateHandler) listenHTTP() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", h.handleHealth)
	mux.Handle("GET /metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}))
	mux.HandleFunc("POST /api/v1/articles", h.requireToken(h.handleIngestArticle))
	mux.HandleFunc("POST /api/v1/articles/batch", h.requireToken(h.handleIngestBatch))
	mux.HandleFunc("DELETE /api/v1/articles/{id}", h.requireToken(h.handleDeleteArticle))
	mux.HandleFunc("DELETE /api/v1/articles", h.requireToken(h.handleDeleteArticlesBySource))
	mux.HandleFunc("POST /api/v1/actions/sync", h.requireToken(h.handleSyncActions))
	mux.HandleFunc("GET /api/v1/sync/status", h.requireToken(h.handleSyncStatus))
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

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}
