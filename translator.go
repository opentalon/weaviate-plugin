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
	// Translate returns the translation outcome of `text`. The returned
	// TranslatorOutcome.Text is what the caller should hand downstream —
	// the post-translate string, or the original text when the translation
	// was skipped (target-lang match) or failed (fail-open contract).
	//
	// SourceLang* fields are populated when a /detect call succeeded,
	// regardless of whether the subsequent /translate succeeded. They
	// allow the call site to record the full audit trail (lang + confidence
	// + outcome) into a session_events row — see opentalon/opentalon#256.
	Translate(ctx context.Context, text string) (TranslatorOutcome, error)

	// TargetLang returns the ISO-639-1 code the implementation translates
	// INTO. Used by translateQuery to populate audit-row metadata without
	// reaching into implementation internals. Empty string when no target
	// is configured (e.g. noopTranslator).
	TargetLang() string
}

// TranslatorOutcome is the verbose return of Translator.Translate. Carries
// the post-translate text plus the metadata an audit-log emitter needs to
// reconstruct what the translator did. opentalon/opentalon#256.
type TranslatorOutcome struct {
	// Text is the translated string. Equals the input on SkippedTargetLang
	// or on translate failure (the Translator's fail-open contract).
	Text string

	// SourceLangDetected is the ISO-639-1 code (or empty when the detect
	// call did not run / failed); SourceLangConfidence is the matching
	// 0..1 confidence (or 0 in the same cases).
	SourceLangDetected   string
	SourceLangConfidence float64

	// SkippedTargetLang is true when the implementation short-circuited
	// because the detected source language matched the target with high
	// enough confidence — the /translate call did not run.
	SkippedTargetLang bool
}

// noopTranslator is the zero value used when translation is disabled.
type noopTranslator struct{}

func (noopTranslator) Translate(_ context.Context, text string) (TranslatorOutcome, error) {
	return TranslatorOutcome{Text: text}, nil
}

func (noopTranslator) TargetLang() string { return "" }

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
	defer func() { _ = resp.Body.Close() }()

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
	translatorDetectedLangsTotal.WithLabelValues(parsed[0].Language).Inc()
	return parsed[0].Language, parsed[0].Confidence / 100.0, nil
}

func (t *libreTranslator) TargetLang() string { return t.target }

// Translate runs the optional /detect pre-flight and the /translate call.
// Uses a named return so every error-return path can stay one line:
// `outcome.Text` defaults to the input (fail-open contract), and detect
// output is recorded on the outcome the moment detect succeeds, so a
// failed /translate downstream still produces a useful audit row.
func (t *libreTranslator) Translate(ctx context.Context, text string) (outcome TranslatorOutcome, err error) {
	outcome.Text = text
	if strings.TrimSpace(text) == "" {
		return outcome, nil
	}

	// Optional pre-flight: if the input already looks like the target
	// language with high confidence, skip the translate roundtrip.
	// LibreTranslate's confidence is reported as 0..100 in /detect — we
	// normalise to 0..1 in detect() above. On detect error we fall through
	// to translate — fail-open: we'd rather pay the EN→EN roundtrip than
	// silently lose the cross-lingual fix when the detector is having a
	// bad minute. Source lang/confidence stay zero-valued in that case so
	// the audit reflects "no detect signal".
	if t.skipIfTargetCfg > 0 {
		if lang, conf, detectErr := t.detect(ctx, text); detectErr == nil {
			outcome.SourceLangDetected = lang
			outcome.SourceLangConfidence = conf
			if lang == t.target && conf >= t.skipIfTargetCfg {
				outcome.SkippedTargetLang = true
				return outcome, nil
			}
		}
	}

	body, err := json.Marshal(translateRequest{
		Q:      text,
		Source: t.source,
		Target: t.target,
		Format: "text",
		APIKey: t.apiKey,
	})
	if err != nil {
		return outcome, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.translateURL, bytes.NewReader(body))
	if err != nil {
		return outcome, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return outcome, fmt.Errorf("call %s: %w", t.translateURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return outcome, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return outcome, fmt.Errorf("translator status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var parsed translateResponse
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return outcome, fmt.Errorf("parse response: %w", err)
	}
	if parsed.Error != "" {
		return outcome, fmt.Errorf("translator error: %s", parsed.Error)
	}
	out := strings.TrimSpace(parsed.TranslatedText)
	if out == "" {
		return outcome, fmt.Errorf("translator returned empty translatedText")
	}
	outcome.Text = out
	return outcome, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TranslatorEvent is the per-call metadata bubbled back to the
// orchestrator so it can emit a `translation` session event. Field names
// match the JSON wire shape the orchestrator decodes — see
// PreparerTranslatorEvent in opentalon/internal/orchestrator/orchestrator.go
// and opentalon/opentalon#256. Pointer-returning translateQuery yields
// nil when the call was skipped_disabled or the input was empty (no
// audit row worth emitting); other outcomes populate every field the
// downstream emit helper needs.
type TranslatorEvent struct {
	Callsite             string  `json:"callsite"`
	Outcome              string  `json:"outcome"`
	SourceLangDetected   string  `json:"source_lang_detected,omitempty"`
	SourceLangConfidence float64 `json:"source_lang_confidence,omitempty"`
	TargetLang           string  `json:"target_lang"`
	InputText            string  `json:"input_text,omitempty"`
	OutputText           string  `json:"output_text,omitempty"`
	DurationMS           int64   `json:"duration_ms,omitempty"`
}

// Outcome label vocabulary — wire-stable. Mirrors
// events.TranslationOutcome* in opentalon-core.
const (
	translatorOutcomeTranslated       = "translated"
	translatorOutcomeSkippedTargetLang = "skipped_target_lang"
	translatorOutcomeFailed           = "failed"
)

// translateQuery wraps Translator.Translate with the fail-open contract
// used by every search-side caller in handler.go: any error is logged and
// the ORIGINAL text is returned so the search still runs (just without
// the cross-lingual normalisation).
//
// Records Prometheus metrics (translator_calls_total + duration) AND
// returns a TranslatorEvent for callers that want to bubble the per-call
// metadata back to the orchestrator (today: the `prepare` action — see
// opentalon/opentalon#256). The event is nil for the "no audit signal"
// paths (translator disabled, empty input) since those rows would carry
// no useful information.
func (h *WeaviateHandler) translateQuery(ctx context.Context, text, callsite string) (string, *TranslatorEvent) {
	if h.translator == nil {
		translatorCallsTotal.WithLabelValues(callsite, "skipped_disabled").Inc()
		return text, nil
	}
	if _, ok := h.translator.(noopTranslator); ok {
		translatorCallsTotal.WithLabelValues(callsite, "skipped_disabled").Inc()
		return text, nil
	}
	if strings.TrimSpace(text) == "" {
		return text, nil
	}

	targetLang := h.translator.TargetLang()
	start := time.Now()
	outcome, err := h.translator.Translate(ctx, text)
	elapsed := time.Since(start)

	if err != nil {
		translatorCallsTotal.WithLabelValues(callsite, "failed").Inc()
		translatorDurationSeconds.WithLabelValues(callsite, "failed").Observe(elapsed.Seconds())
		log.Printf("weaviate-plugin: translator: %s: fail-open, using original: %v", callsite, err)
		return text, &TranslatorEvent{
			Callsite:             callsite,
			Outcome:              translatorOutcomeFailed,
			SourceLangDetected:   outcome.SourceLangDetected,
			SourceLangConfidence: outcome.SourceLangConfidence,
			TargetLang:           targetLang,
			InputText:            text,
			DurationMS:           elapsed.Milliseconds(),
		}
	}

	result := translatorOutcomeTranslated
	if outcome.SkippedTargetLang || outcome.Text == text {
		result = translatorOutcomeSkippedTargetLang
	}
	translatorCallsTotal.WithLabelValues(callsite, result).Inc()
	translatorDurationSeconds.WithLabelValues(callsite, result).Observe(elapsed.Seconds())

	if outcome.Text != text {
		log.Printf("weaviate-plugin: translator: %s: %q -> %q", callsite, truncate(text, 80), truncate(outcome.Text, 80))
	}

	return outcome.Text, &TranslatorEvent{
		Callsite:             callsite,
		Outcome:              result,
		SourceLangDetected:   outcome.SourceLangDetected,
		SourceLangConfidence: outcome.SourceLangConfidence,
		TargetLang:           targetLang,
		InputText:            text,
		OutputText:           outcome.Text,
		DurationMS:           elapsed.Milliseconds(),
	}
}
