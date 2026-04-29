package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// TranslatorConfig is the JSON config block for the optional query
// translation pre-processor. When enabled, user queries hitting the
// search/RAG actions are translated to the target language before they
// reach Weaviate, so non-EN queries can BM25-match against an EN-indexed
// corpus (cross-lingual hybrid search collapse fix).
//
// Disabled by default — opt-in via `translator.enabled: true`.
type TranslatorConfig struct {
	Enabled    bool   `json:"enabled"`
	URL        string `json:"url"`         // base URL of LibreTranslate, e.g. http://libretranslate.libretranslate.svc.cluster.local:5000
	TargetLang string `json:"target_lang"` // default "en"
	SourceLang string `json:"source_lang"` // default "auto" (let LibreTranslate detect)
	Timeout    string `json:"timeout"`     // duration string, default "3s"
	APIKey     string `json:"api_key"`     // optional, only if LT_API_KEYS is set on the translator
	// SkipIfTargetConfidence, when > 0, calls /detect first and skips the
	// /translate roundtrip if the detected language equals TargetLang with
	// at least this confidence. Default 0.7 — set to 0 to disable.
	SkipIfTargetConfidence *float64 `json:"skip_if_target_confidence,omitempty"`
}

// Translator translates a single short query string. Implementations must
// be safe for concurrent use.
type Translator interface {
	// Translate returns the translation of `text`. On any failure (network,
	// non-2xx, parse) it MUST return the original text plus a non-nil error
	// so callers can fail open without branching.
	Translate(ctx context.Context, text string) (string, error)
}

// noopTranslator is the zero value used when translation is disabled.
type noopTranslator struct{}

func (noopTranslator) Translate(_ context.Context, text string) (string, error) {
	return text, nil
}

// libreTranslator calls a LibreTranslate-compatible HTTP endpoint.
type libreTranslator struct {
	translateURL    string // full URL of /translate
	detectURL       string // full URL of /detect
	source          string
	target          string
	apiKey          string
	skipIfTargetCfg float64 // when > 0, call /detect first and skip translate if detected==target and confidence>=this
	client          *http.Client
}

// newTranslator returns the configured translator, or a noop if disabled
// or misconfigured. Misconfiguration is logged but not fatal — translation
// is a best-effort enrichment, never a hard dependency.
func newTranslator(cfg *TranslatorConfig) Translator {
	if cfg == nil || !cfg.Enabled {
		return noopTranslator{}
	}
	if strings.TrimSpace(cfg.URL) == "" {
		log.Println("weaviate-plugin: translator enabled but url empty, disabling")
		return noopTranslator{}
	}

	target := cfg.TargetLang
	if target == "" {
		target = "en"
	}
	source := cfg.SourceLang
	if source == "" {
		source = "auto"
	}

	timeout := 3 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		} else if err != nil {
			log.Printf("weaviate-plugin: translator: invalid timeout %q, using default %s: %v", cfg.Timeout, timeout, err)
		}
	}

	skipCfg := 0.7
	if cfg.SkipIfTargetConfidence != nil {
		skipCfg = *cfg.SkipIfTargetConfidence
	}

	base := strings.TrimRight(cfg.URL, "/")
	log.Printf("weaviate-plugin: translator enabled: base=%s source=%s target=%s timeout=%s skip_if_target_confidence=%.2f",
		base, source, target, timeout, skipCfg)

	return &libreTranslator{
		translateURL:    base + "/translate",
		detectURL:       base + "/detect",
		source:          source,
		target:          target,
		apiKey:          cfg.APIKey,
		skipIfTargetCfg: skipCfg,
		client:          &http.Client{Timeout: timeout},
	}
}

type translateRequest struct {
	Q      string `json:"q"`
	Source string `json:"source"`
	Target string `json:"target"`
	Format string `json:"format"`
	APIKey string `json:"api_key,omitempty"`
}

type translateResponse struct {
	TranslatedText string `json:"translatedText"`
	Error          string `json:"error,omitempty"`
}

type detectRequest struct {
	Q      string `json:"q"`
	APIKey string `json:"api_key,omitempty"`
}

type detectResponse struct {
	Confidence float64 `json:"confidence"`
	Language   string  `json:"language"`
}

// detect asks the translator to identify the source language. Returns
// (lang, confidence) where confidence is 0..1. Errors are propagated;
// the caller decides whether to fall through to a translate or skip.
func (t *libreTranslator) detect(ctx context.Context, text string) (string, float64, error) {
	body, err := json.Marshal(detectRequest{Q: text, APIKey: t.apiKey})
	if err != nil {
		return "", 0, fmt.Errorf("marshal detect: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.detectURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build detect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("call %s: %w", t.detectURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("read detect response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("detect status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// LibreTranslate returns a JSON array of { language, confidence } sorted
	// by confidence desc — take the first.
	var parsed []detectResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", 0, fmt.Errorf("parse detect response: %w", err)
	}
	if len(parsed) == 0 {
		return "", 0, fmt.Errorf("empty detect response")
	}
	return parsed[0].Language, parsed[0].Confidence / 100.0, nil
}

func (t *libreTranslator) Translate(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}

	// Optional pre-flight: if the input already looks like the target
	// language with high confidence, skip the translate roundtrip.
	// LibreTranslate's confidence is reported as 0..100 in /detect — we
	// normalise to 0..1 in detect() above.
	if t.skipIfTargetCfg > 0 {
		lang, conf, err := t.detect(ctx, text)
		if err == nil && lang == t.target && conf >= t.skipIfTargetCfg {
			return text, nil
		}
		// On detect error we fall through to translate — fail-open: we'd
		// rather pay the EN→EN roundtrip than silently lose the cross-lingual
		// fix when the detector is having a bad minute.
	}

	body, err := json.Marshal(translateRequest{
		Q:      text,
		Source: t.source,
		Target: t.target,
		Format: "text",
		APIKey: t.apiKey,
	})
	if err != nil {
		return text, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.translateURL, bytes.NewReader(body))
	if err != nil {
		return text, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return text, fmt.Errorf("call %s: %w", t.translateURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return text, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return text, fmt.Errorf("translator status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var parsed translateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return text, fmt.Errorf("parse response: %w", err)
	}
	if parsed.Error != "" {
		return text, fmt.Errorf("translator error: %s", parsed.Error)
	}
	out := strings.TrimSpace(parsed.TranslatedText)
	if out == "" {
		return text, fmt.Errorf("translator returned empty translatedText")
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// translateQuery wraps Translator.Translate with the fail-open contract
// used by every search-side caller in handler.go: any error is logged and
// the ORIGINAL text is returned so the search still runs (just without
// the cross-lingual normalisation).
func (h *WeaviateHandler) translateQuery(ctx context.Context, text, callsite string) string {
	if h.translator == nil {
		return text
	}
	if _, ok := h.translator.(noopTranslator); ok {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return text
	}
	out, err := h.translator.Translate(ctx, text)
	if err != nil {
		log.Printf("weaviate-plugin: translator: %s: fail-open, using original: %v", callsite, err)
		return text
	}
	if out != text {
		log.Printf("weaviate-plugin: translator: %s: %q -> %q", callsite, truncate(text, 80), truncate(out, 80))
	}
	return out
}
