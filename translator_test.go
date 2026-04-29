package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeTranslator is a configurable in-memory Translator for handler tests.
type fakeTranslator struct {
	out   string
	err   error
	calls int
	last  string
}

func (f *fakeTranslator) Translate(_ context.Context, text string) (string, error) {
	f.calls++
	f.last = text
	if f.err != nil {
		return text, f.err
	}
	return f.out, nil
}

// ---------------------------------------------------------------------------
// newTranslator: factory honours the disabled / misconfigured paths.
// ---------------------------------------------------------------------------

func TestNewTranslator_DisabledByDefault(t *testing.T) {
	tr := newTranslator(nil)
	if _, ok := tr.(noopTranslator); !ok {
		t.Fatalf("nil config should yield noopTranslator, got %T", tr)
	}

	tr = newTranslator(&TranslatorConfig{Enabled: false})
	if _, ok := tr.(noopTranslator); !ok {
		t.Fatalf("Enabled:false should yield noopTranslator, got %T", tr)
	}
}

func TestNewTranslator_EnabledWithoutURLFallsBackToNoop(t *testing.T) {
	tr := newTranslator(&TranslatorConfig{Enabled: true})
	if _, ok := tr.(noopTranslator); !ok {
		t.Fatalf("missing URL should yield noopTranslator (fail-soft), got %T", tr)
	}
}

func TestNewTranslator_DefaultsAreApplied(t *testing.T) {
	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: "http://example/"})
	lt, ok := tr.(*libreTranslator)
	if !ok {
		t.Fatalf("expected libreTranslator, got %T", tr)
	}
	if lt.translateURL != "http://example/translate" {
		t.Errorf("translateURL: got %q", lt.translateURL)
	}
	if lt.detectURL != "http://example/detect" {
		t.Errorf("detectURL: got %q", lt.detectURL)
	}
	if lt.target != "en" {
		t.Errorf("target default: got %q want en", lt.target)
	}
	if lt.source != "auto" {
		t.Errorf("source default: got %q want auto", lt.source)
	}
	if lt.skipIfTargetCfg != 0.7 {
		t.Errorf("skip default: got %v want 0.7", lt.skipIfTargetCfg)
	}
	if lt.client.Timeout != 3*time.Second {
		t.Errorf("timeout default: got %s want 3s", lt.client.Timeout)
	}
}

// ---------------------------------------------------------------------------
// libreTranslator.Translate against a fake LibreTranslate server.
// ---------------------------------------------------------------------------

// libreServer is a minimal stand-in for LibreTranslate's /detect + /translate
// endpoints, used in unit tests so the suite stays hermetic.
type libreServer struct {
	t            *testing.T
	detectLang   string
	detectConf   float64 // 0..100 (LibreTranslate native scale)
	detectStatus int
	translateOut string
	translateErr string
	translateSt  int
	calls        struct {
		detect    int
		translate int
	}
	lastTranslate translateRequest
}

func newLibreServer(t *testing.T) *libreServer {
	return &libreServer{
		t:            t,
		detectLang:   "de",
		detectConf:   99,
		detectStatus: 200,
		translateSt:  200,
	}
}

func (s *libreServer) start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/detect", func(w http.ResponseWriter, r *http.Request) {
		s.calls.detect++
		if s.detectStatus != 200 {
			http.Error(w, "boom", s.detectStatus)
			return
		}
		_ = json.NewEncoder(w).Encode([]detectResponse{
			{Language: s.detectLang, Confidence: s.detectConf},
		})
	})
	mux.HandleFunc("/translate", func(w http.ResponseWriter, r *http.Request) {
		s.calls.translate++
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &s.lastTranslate)
		if s.translateSt != 0 && s.translateSt != 200 {
			http.Error(w, "boom", s.translateSt)
			return
		}
		out := s.translateOut
		if out == "" {
			out = strings.ToUpper(s.lastTranslate.Q) // dummy "translation"
		}
		resp := translateResponse{TranslatedText: out, Error: s.translateErr}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

func TestLibreTranslator_Translate_HappyPath(t *testing.T) {
	srv := newLibreServer(t)
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	got, err := tr.Translate(context.Background(), "Wieviele Lagerartikel habe ich?")
	if err != nil {
		t.Fatalf("translate err: %v", err)
	}
	if got != "WIEVIELE LAGERARTIKEL HABE ICH?" {
		t.Errorf("translate output: got %q", got)
	}
	if srv.calls.detect != 1 {
		t.Errorf("expected 1 /detect call, got %d", srv.calls.detect)
	}
	if srv.calls.translate != 1 {
		t.Errorf("expected 1 /translate call, got %d", srv.calls.translate)
	}
	if srv.lastTranslate.Source != "auto" || srv.lastTranslate.Target != "en" {
		t.Errorf("translate request fields: %+v", srv.lastTranslate)
	}
}

func TestLibreTranslator_SkipsTranslateWhenAlreadyTarget(t *testing.T) {
	srv := newLibreServer(t)
	srv.detectLang = "en"
	srv.detectConf = 95
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	got, err := tr.Translate(context.Background(), "How many stock items do I have?")
	if err != nil {
		t.Fatalf("translate err: %v", err)
	}
	if got != "How many stock items do I have?" {
		t.Errorf("expected pass-through, got %q", got)
	}
	if srv.calls.detect != 1 {
		t.Errorf("expected 1 /detect call, got %d", srv.calls.detect)
	}
	if srv.calls.translate != 0 {
		t.Errorf("expected /translate to be skipped, got %d", srv.calls.translate)
	}
}

func TestLibreTranslator_TranslatesWhenTargetConfidenceTooLow(t *testing.T) {
	srv := newLibreServer(t)
	srv.detectLang = "en"
	srv.detectConf = 50 // below 70 default threshold
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	_, err := tr.Translate(context.Background(), "ambiguous")
	if err != nil {
		t.Fatalf("translate err: %v", err)
	}
	if srv.calls.translate != 1 {
		t.Errorf("low confidence should fall through to translate, got %d", srv.calls.translate)
	}
}

func TestLibreTranslator_DetectErrorFallsThroughToTranslate(t *testing.T) {
	srv := newLibreServer(t)
	srv.detectStatus = 500
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	_, err := tr.Translate(context.Background(), "something")
	if err != nil {
		t.Fatalf("detect failure should fail open via translate, got err %v", err)
	}
	if srv.calls.translate != 1 {
		t.Errorf("expected fallthrough to translate, got %d", srv.calls.translate)
	}
}

func TestLibreTranslator_SkipDisabledViaConfig(t *testing.T) {
	srv := newLibreServer(t)
	srv.detectLang = "en" // would normally trigger skip
	ts := srv.start()
	defer ts.Close()

	zero := 0.0
	tr := newTranslator(&TranslatorConfig{
		Enabled:                true,
		URL:                    ts.URL,
		SkipIfTargetConfidence: &zero,
	})
	_, err := tr.Translate(context.Background(), "How many stock items?")
	if err != nil {
		t.Fatalf("translate err: %v", err)
	}
	if srv.calls.detect != 0 {
		t.Errorf("skip-disabled should not call /detect, got %d", srv.calls.detect)
	}
	if srv.calls.translate != 1 {
		t.Errorf("translate must run when skip is disabled, got %d", srv.calls.translate)
	}
}

func TestLibreTranslator_NonOKReturnsErrorWithOriginal(t *testing.T) {
	srv := newLibreServer(t)
	srv.translateSt = 502
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	got, err := tr.Translate(context.Background(), "input")
	if err == nil {
		t.Fatalf("expected error on 502")
	}
	if got != "input" {
		t.Errorf("on error must return original, got %q", got)
	}
}

func TestLibreTranslator_EmptyResponseReturnsErrorWithOriginal(t *testing.T) {
	srv := newLibreServer(t)
	srv.translateOut = "   " // trimmed → empty
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	got, err := tr.Translate(context.Background(), "input")
	if err == nil {
		t.Fatalf("expected error on empty translatedText")
	}
	if got != "input" {
		t.Errorf("on error must return original, got %q", got)
	}
}

func TestLibreTranslator_BlankInputIsNoop(t *testing.T) {
	srv := newLibreServer(t)
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	got, err := tr.Translate(context.Background(), "   ")
	if err != nil {
		t.Fatalf("blank translate: %v", err)
	}
	if got != "   " {
		t.Errorf("blank input: got %q want %q", got, "   ")
	}
	if srv.calls.detect+srv.calls.translate != 0 {
		t.Errorf("blank input should not hit translator, calls detect=%d translate=%d",
			srv.calls.detect, srv.calls.translate)
	}
}

// ---------------------------------------------------------------------------
// translateQuery: handler-side fail-open contract.
// ---------------------------------------------------------------------------

func TestTranslateQuery_FailsOpenOnError(t *testing.T) {
	h := &WeaviateHandler{translator: &fakeTranslator{err: errors.New("nope")}}
	got := h.translateQuery(context.Background(), "Wie geht's?", "test")
	if got != "Wie geht's?" {
		t.Errorf("fail-open: got %q want original", got)
	}
}

func TestTranslateQuery_NilTranslatorIsNoop(t *testing.T) {
	h := &WeaviateHandler{}
	got := h.translateQuery(context.Background(), "anything", "test")
	if got != "anything" {
		t.Errorf("nil translator: got %q", got)
	}
}

func TestTranslateQuery_NoopTranslatorIsNoop(t *testing.T) {
	h := &WeaviateHandler{translator: noopTranslator{}}
	got := h.translateQuery(context.Background(), "anything", "test")
	if got != "anything" {
		t.Errorf("noop translator: got %q", got)
	}
}

func TestTranslateQuery_TranslatesNonEmpty(t *testing.T) {
	f := &fakeTranslator{out: "How many stock items do I have?"}
	h := &WeaviateHandler{translator: f}
	got := h.translateQuery(context.Background(), "Wieviele Lagerartikel habe ich?", "prepare")
	if got != "How many stock items do I have?" {
		t.Errorf("translate: got %q", got)
	}
	if f.calls != 1 {
		t.Errorf("expected 1 call, got %d", f.calls)
	}
}

func TestTranslateQuery_SkipsBlankInput(t *testing.T) {
	f := &fakeTranslator{out: "should-not-be-used"}
	h := &WeaviateHandler{translator: f}
	got := h.translateQuery(context.Background(), "   ", "prepare")
	if got != "   " {
		t.Errorf("blank: got %q", got)
	}
	if f.calls != 0 {
		t.Errorf("blank input should bypass translator, calls=%d", f.calls)
	}
}

// ---------------------------------------------------------------------------
// Metrics: counters increment with the right labels.
// ---------------------------------------------------------------------------

func resetMetrics() {
	translatorCallsTotal.Reset()
	translatorDurationSeconds.Reset()
	translatorDetectedLangsTotal.Reset()
}

func TestMetrics_TranslatedIncrementsCounterAndHistogram(t *testing.T) {
	resetMetrics()
	h := &WeaviateHandler{translator: &fakeTranslator{out: "How many?"}}
	h.translateQuery(context.Background(), "Wieviele?", "prepare")

	got := testutil.ToFloat64(translatorCallsTotal.WithLabelValues("prepare", "translated"))
	if got != 1 {
		t.Errorf("translated counter: got %v want 1", got)
	}
	if c := testutil.CollectAndCount(translatorDurationSeconds, "weaviate_plugin_translator_duration_seconds"); c == 0 {
		t.Errorf("expected at least one duration sample, got %d", c)
	}
}

func TestMetrics_FailedIncrementsFailCounter(t *testing.T) {
	resetMetrics()
	h := &WeaviateHandler{translator: &fakeTranslator{err: errors.New("boom")}}
	h.translateQuery(context.Background(), "anything", "search")

	got := testutil.ToFloat64(translatorCallsTotal.WithLabelValues("search", "failed"))
	if got != 1 {
		t.Errorf("failed counter: got %v want 1", got)
	}
}

func TestMetrics_NoopIncrementsSkippedDisabled(t *testing.T) {
	resetMetrics()
	h := &WeaviateHandler{translator: noopTranslator{}}
	h.translateQuery(context.Background(), "x", "ask_knowledge")

	got := testutil.ToFloat64(translatorCallsTotal.WithLabelValues("ask_knowledge", "skipped_disabled"))
	if got != 1 {
		t.Errorf("skipped_disabled counter: got %v want 1", got)
	}
}

func TestMetrics_NilTranslatorIncrementsSkippedDisabled(t *testing.T) {
	resetMetrics()
	h := &WeaviateHandler{}
	h.translateQuery(context.Background(), "x", "hybrid_search")

	got := testutil.ToFloat64(translatorCallsTotal.WithLabelValues("hybrid_search", "skipped_disabled"))
	if got != 1 {
		t.Errorf("nil translator: got %v want 1", got)
	}
}

func TestMetrics_SkippedTargetLangCounted(t *testing.T) {
	resetMetrics()
	// Translate returns the same text → counted as skipped_target_lang.
	h := &WeaviateHandler{translator: &fakeTranslator{out: "x"}}
	h.translateQuery(context.Background(), "x", "prepare")

	got := testutil.ToFloat64(translatorCallsTotal.WithLabelValues("prepare", "skipped_target_lang"))
	if got != 1 {
		t.Errorf("skipped_target_lang counter: got %v want 1", got)
	}
}

func TestMetrics_DetectedLangsCounted(t *testing.T) {
	resetMetrics()
	srv := newLibreServer(t)
	srv.detectLang = "fr"
	srv.detectConf = 50 // below threshold so we'll still translate
	ts := srv.start()
	defer ts.Close()

	tr := newTranslator(&TranslatorConfig{Enabled: true, URL: ts.URL})
	_, _ = tr.Translate(context.Background(), "salut")

	got := testutil.ToFloat64(translatorDetectedLangsTotal.WithLabelValues("fr"))
	if got != 1 {
		t.Errorf("detected lang fr counter: got %v want 1", got)
	}
}
