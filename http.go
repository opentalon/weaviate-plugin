package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// listenHTTP starts the token-protected HTTP ingestion server.
func (h *WeaviateHandler) listenHTTP() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/articles", h.requireToken(h.handleIngestArticle))
	mux.HandleFunc("POST /api/v1/articles/batch", h.requireToken(h.handleIngestBatch))
	mux.HandleFunc("POST /api/v1/actions/sync", h.requireToken(h.handleSyncActions))
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

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}
