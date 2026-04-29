package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleDebugPrepare_RequiresAuth covers only the auth path — full
// pipeline behaviour needs Weaviate running and is in handler_test.go
// (integration build tag).
func TestHandleDebugPrepare_RequiresAuth(t *testing.T) {
	h := &WeaviateHandler{token: "secret"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/debug/prepare",
		bytes.NewBufferString(`{"text":"hi"}`))
	h.requireToken(h.handleDebugPrepare)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token should be 401, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/debug/prepare",
		bytes.NewBufferString(`{"text":"hi"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	h.requireToken(h.handleDebugPrepare)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token should be 401, got %d", rec.Code)
	}
}

func TestHandleDebugPrepare_RejectsBlankText(t *testing.T) {
	h := &WeaviateHandler{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/debug/prepare",
		bytes.NewBufferString(`{"text":"  "}`))
	h.handleDebugPrepare(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("blank text should be 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["error"] == "" {
		t.Errorf("expected error message, got %v", resp)
	}
}

func TestHandleDebugPrepare_RejectsBadJSON(t *testing.T) {
	h := &WeaviateHandler{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/debug/prepare",
		bytes.NewBufferString(`not-json`))
	h.handleDebugPrepare(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON should be 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}
