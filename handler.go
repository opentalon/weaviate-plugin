package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/opentalon/opentalon/pkg/plugin"
	"github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/filters"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"
	wmodels "github.com/weaviate/weaviate/entities/models"
)

// Orchestrator context-arg wire names. Declared as constants here so a
// typo at any of the four call sites (Capability InjectContextArgs +
// Execute-time req.Args reads) fails to compile rather than silently
// drifting and leaving the arg unread. Canonical cross-repo source of
// truth is opentalon/pkg/plugin/contextargs - kept as local consts here
// to avoid pinning this plugin to an unreleased opentalon revision
// while the matching PR is in flight. Bump in lock-step with that
// package; both sides MUST agree on the wire name.
const (
	ctxArgAllowedPlugins = "allowed_plugins"
	ctxArgAllowedTools   = "allowed_tools"
)

// Default collection names for the knowledge-augmented RAG system.
const (
	DefaultActionsCollection   = "MCPActions"
	DefaultKnowledgeCollection = "KnowledgeArticles"
	DefaultGlossaryCollection  = "Glossary"
)

// actionNS is a UUID v5 namespace for deterministic MCP action object IDs.
var actionNS = uuid.MustParse("d4e8f1a2-5b3c-4d6e-9f0a-1b2c3d4e5f6a")

// articleNS is a UUID v5 namespace for deterministic KnowledgeArticles IDs
// generated from sync_actions (e.g. one article per plugin's MCP server
// instructions). Distinct from actionNS so the two ID spaces never collide.
var articleNS = uuid.MustParse("e5f9a2b3-6c4d-5e7f-a0b1-2c3d4e5f6a7b")

// glossaryNS is a UUID v5 namespace for deterministic Glossary entry IDs,
// keyed on the term text. Distinct from actionNS and articleNS.
var glossaryNS = uuid.MustParse("f6a0b3c4-7d5e-6f8a-b1c2-3d4e5f6a7b8c")

// MCPSourcePrefix tags KnowledgeArticles records that were generated from a
// plugin's MCP server-instructions text rather than authored manually. Used
// for filtered prune queries — only auto-managed records are deleted.
const MCPSourcePrefix = "mcp:"

// MCPKnowledgeSourcePrefix tags KnowledgeArticles records contributed by a
// plugin via the per-section `knowledge_articles[]` field on sync_actions.
// Each record's source is "mcp-knowledge:<plugin>:<article-id>". Distinct
// from MCPSourcePrefix ("mcp:") so the prepare-path filter that excludes
// the always-injected SystemPromptAddition (mcp:<plugin>) keeps these
// section articles available for [knowledge_context] retrieval — the whole
// point of splitting them out.
const MCPKnowledgeSourcePrefix = "mcp-knowledge:"

// Config is the JSON config block passed via the Init RPC.
type Config struct {
	Host                string   `json:"host"`
	Scheme              string   `json:"scheme"`
	Collection          string   `json:"collection"`
	ActionsCollection   string   `json:"actions_collection"`
	KnowledgeCollection string   `json:"knowledge_collection"`
	Fields              []string `json:"fields"`
	Limit               int      `json:"limit"`
	// Per-collection ceilings for the `prepare` RAG fan-out. The orchestrator's
	// downstream tier-budget (tier1_cap + tier2_cap) and per-turn knowledge cap
	// can only fire when the upstream candidate pool exceeds them — when the
	// prepare limits silently clip the pool first, those code paths become
	// unreachable. Defaults: knowledge=15, actions=25 (covers default
	// tier1_cap=10 + tier2_cap=15), glossary=10. Override per-deployment when
	// tuning Tier 2 width or the knowledge dedup cap.
	PrepareKnowledgeLimit int            `json:"prepare_knowledge_limit"`
	PrepareActionsLimit   int            `json:"prepare_actions_limit"`
	PrepareGlossaryLimit  int            `json:"prepare_glossary_limit"`
	AutoCreateSchema      *bool          `json:"auto_create_schema"`
	HTTPAddr              string         `json:"http_addr"`
	Token                 string         `json:"token"`
	Vectorizer            string         `json:"vectorizer"`
	ModuleConfig          map[string]any `json:"module_config"`
	MinPrepareScore       *float64       `json:"min_prepare_score"` // fallback applied when the per-collection knobs below are unset
	// Per-collection minimum-score gates for `prepare`. Tools, knowledge,
	// and glossary often want different thresholds: the tools retriever
	// can tolerate a permissive cut-off because the orchestrator ranks
	// tools into tiers and most low-score tools end up in Tier 3
	// (name-only) anyway, while a low-score knowledge article is
	// surfaced to the LLM as a full text block where noise costs tokens
	// + can derail the answer. Each falls back to MinPrepareScore (or
	// defaultMinPrepareScore when both are unset) so a deployment that
	// only sets the top-level field keeps the prior behaviour.
	MinPrepareScoreTools     *float64 `json:"min_prepare_score_tools,omitempty"`
	MinPrepareScoreKnowledge *float64 `json:"min_prepare_score_knowledge,omitempty"`
	MinPrepareScoreGlossary  *float64 `json:"min_prepare_score_glossary,omitempty"`
	Timeout                  string   `json:"timeout"` // Weaviate HTTP client timeout as duration string (e.g. "2m", "90s"); default "2m"
	GlossaryCollection       string   `json:"glossary_collection"`
	// Translator is the optional LibreTranslate-compatible query
	// pre-processor. When enabled, non-target-language user queries are
	// translated to TargetLang before they reach Weaviate, fixing
	// cross-lingual BM25 collapse against an EN-indexed corpus. Off by
	// default; see translator.go for the config shape.
	Translator *TranslatorConfig `json:"translator,omitempty"`
}

// syncJobKind identifies the type of background sync work.
type syncJobKind int

const (
	syncJobActions syncJobKind = iota
	syncJobGlossary
)

func (k syncJobKind) String() string {
	switch k {
	case syncJobActions:
		return "sync_actions"
	case syncJobGlossary:
		return "sync_glossary"
	default:
		return "unknown"
	}
}

// syncJob is the envelope queued for background processing.
type syncJob struct {
	kind    syncJobKind
	reqID   string // original request ID for log correlation
	payload string // raw JSON payload (already validated)
}

// syncStatusState exposes counters and timestamps for the background sync worker.
type syncStatusState struct {
	ActionsQueued     int64     `json:"actions_queued"`
	ActionsCompleted  int64     `json:"actions_completed"`
	ActionsFailed     int64     `json:"actions_failed"`
	GlossaryQueued    int64     `json:"glossary_queued"`
	GlossaryCompleted int64     `json:"glossary_completed"`
	GlossaryFailed    int64     `json:"glossary_failed"`
	LastActionSync    time.Time `json:"last_action_sync,omitempty"`
	LastGlossarySync  time.Time `json:"last_glossary_sync,omitempty"`
	WorkerRunning     bool      `json:"worker_running"`
}

// WeaviateHandler implements plugin.Handler.
type WeaviateHandler struct {
	client                   *weaviate.Client
	collection               string
	actionsCollection        string
	knowledgeCollection      string
	glossaryCollection       string
	fields                   []string
	limit                    int
	prepareKnowledgeLimit    int
	prepareActionsLimit      int
	prepareGlossaryLimit     int
	httpAddr                 string
	token                    string
	vectorizer               string
	moduleConfig             map[string]any
	minPrepareScore          float64
	minPrepareScoreTools     float64
	minPrepareScoreKnowledge float64
	minPrepareScoreGlossary  float64
	clientTimeout            time.Duration
	translator               Translator

	// Hash-based sync skip: avoid re-writing unchanged data to Weaviate.
	actionHashes map[string]string // pluginName → last-seen hash from sync_actions
	glossaryHash string            // last-seen hash from sync_glossary

	// Background sync worker.
	syncJobCh  chan syncJob
	syncMu     sync.RWMutex
	syncStatus syncStatusState
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

	h.prepareKnowledgeLimit = cfg.PrepareKnowledgeLimit
	if h.prepareKnowledgeLimit <= 0 {
		h.prepareKnowledgeLimit = defaultPrepareKnowledgeLimit
	}
	h.prepareActionsLimit = cfg.PrepareActionsLimit
	if h.prepareActionsLimit <= 0 {
		h.prepareActionsLimit = defaultPrepareActionsLimit
	}
	h.prepareGlossaryLimit = cfg.PrepareGlossaryLimit
	if h.prepareGlossaryLimit <= 0 {
		h.prepareGlossaryLimit = defaultPrepareGlossaryLimit
	}

	h.actionsCollection = cfg.ActionsCollection
	if h.actionsCollection == "" {
		h.actionsCollection = DefaultActionsCollection
	}
	h.knowledgeCollection = cfg.KnowledgeCollection
	if h.knowledgeCollection == "" {
		h.knowledgeCollection = DefaultKnowledgeCollection
	}
	h.glossaryCollection = cfg.GlossaryCollection
	if h.glossaryCollection == "" {
		h.glossaryCollection = DefaultGlossaryCollection
	}

	h.actionHashes = make(map[string]string)

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
	// Each per-collection knob falls back to the umbrella minPrepareScore
	// when unset, so a deployment that only sets min_prepare_score keeps
	// the prior single-threshold behaviour and a deployment that wants
	// asymmetric gates (e.g. permissive for tools, strict for knowledge)
	// only declares the deltas.
	h.minPrepareScoreTools = pickPositive(cfg.MinPrepareScoreTools, h.minPrepareScore)
	h.minPrepareScoreKnowledge = pickPositive(cfg.MinPrepareScoreKnowledge, h.minPrepareScore)
	h.minPrepareScoreGlossary = pickPositive(cfg.MinPrepareScoreGlossary, h.minPrepareScore)

	h.translator = newTranslator(cfg.Translator)

	if h.httpAddr != "" && h.token == "" {
		return fmt.Errorf("config.token is required when http_addr is set")
	}

	log.Printf("weaviate-plugin: collection=%s vectorizer=%s limit=%d min_prepare_score=%.4f fields=%v",
		h.collection, h.vectorizer, h.limit, h.minPrepareScore, h.fields)
	log.Printf("weaviate-plugin: per-collection min_prepare_score tools=%.4f knowledge=%.4f glossary=%.4f",
		h.minPrepareScoreTools, h.minPrepareScoreKnowledge, h.minPrepareScoreGlossary)
	log.Printf("weaviate-plugin: prepare limits knowledge=%d actions=%d glossary=%d",
		h.prepareKnowledgeLimit, h.prepareActionsLimit, h.prepareGlossaryLimit)
	log.Printf("weaviate-plugin: actions_collection=%s knowledge_collection=%s glossary_collection=%s",
		h.actionsCollection, h.knowledgeCollection, h.glossaryCollection)

	autoCreate := cfg.AutoCreateSchema == nil || *cfg.AutoCreateSchema
	if autoCreate {
		log.Println("weaviate-plugin: auto-creating schemas")
		if err := h.ensureSchemas(context.Background()); err != nil {
			return fmt.Errorf("auto-create schemas: %w", err)
		}
		log.Println("weaviate-plugin: schemas ready")
	}

	// Start the background sync worker. All sync_actions and sync_glossary
	// calls are enqueued here so the orchestrator returns immediately.
	h.syncJobCh = make(chan syncJob, 64)
	go h.runSyncWorker(context.Background())

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

// ensureSchemas creates the MCPActions, KnowledgeArticles, and Glossary collections if they don't exist.
func (h *WeaviateHandler) ensureSchemas(ctx context.Context) error {
	if err := h.ensureClass(ctx, h.actionsCollection, []*wmodels.Property{
		{Name: "pluginName", DataType: []string{"text"}},
		{Name: "actionName", DataType: []string{"text"}},
		{Name: "description", DataType: []string{"text"}},
		{Name: "parameters", DataType: []string{"text"}},
	}); err != nil {
		return err
	}
	if err := h.ensureClass(ctx, h.knowledgeCollection, []*wmodels.Property{
		{Name: "title", DataType: []string{"text"}},
		{Name: "content", DataType: []string{"text"}},
		{Name: "source", DataType: []string{"text"}},
		{Name: "tags", DataType: []string{"text[]"}},
	}); err != nil {
		return err
	}
	return h.ensureClass(ctx, h.glossaryCollection, []*wmodels.Property{
		{Name: "term", DataType: []string{"text"}},
		{Name: "definition", DataType: []string{"text"}},
		{Name: "category", DataType: []string{"text"}},
		{Name: "tags", DataType: []string{"text[]"}},
		{Name: "synonyms", DataType: []string{"text[]"}},
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
				},
				// allowed_plugins (profile-level allowlist) and allowed_tools
				// (per-session FQN palette) are orchestrator-managed context,
				// not LLM-facing inputs. Both are delivered via the host's
				// ContextArgProvider registry. allowed_tools enforces the
				// invariant `knowledge_context.tools ⊆ session.tools_available`
				// so RAG retrieval cannot inject a tool the current session
				// has no permission to call.
				InjectContextArgs: []string{ctxArgAllowedPlugins, ctxArgAllowedTools},
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
				},
				// allowed_plugins is orchestrator-managed context (see prepare above).
				InjectContextArgs: []string{ctxArgAllowedPlugins},
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
				Name:        "sync_glossary",
				Description: "Sync glossary term/definition pairs into the Glossary collection for contextual injection in prepare.",
				Parameters: []plugin.ParameterMsg{
					{Name: "payload", Description: `JSON: {"glossary_hash":"...","entries":[{"term":"...","definition":"...","category":"...","tags":[...],"synonyms":[...]}],"is_continuation_batch":false}`, Type: "string", Required: true},
				},
			},
			{
				Name:        "sync_status",
				Description: "Returns the current background sync worker status: queued, completed, and failed job counts.",
				Parameters:  []plugin.ParameterMsg{},
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
		return h.enqueueSyncActions(req)
	case "ingest":
		return h.ingest(req)
	case "ingest_batch":
		return h.ingestBatch(req)
	case "ask_knowledge":
		return h.askKnowledge(req)
	case "search_instructions":
		return h.searchInstructions(req)
	case "sync_glossary":
		return h.enqueueSyncGlossary(req)
	case "sync_status":
		return h.getSyncStatus(req)
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

	ctx := context.Background()
	original := query
	query, _ = h.translateQuery(ctx, query, "search")
	if query != original {
		log.Printf("weaviate-plugin: search: query=%q search_text=%q", original, query)
	}

	result, err := h.client.GraphQL().Get().
		WithClassName(h.collection).
		WithFields(h.resolveFields(req.Args)...).
		WithNearText(h.client.GraphQL().NearTextArgBuilder().WithConcepts([]string{query})).
		WithLimit(h.resolveLimit(req.Args)).
		Do(ctx)

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

	ctx := context.Background()
	original := query
	query, _ = h.translateQuery(ctx, query, "hybrid_search")
	if query != original {
		log.Printf("weaviate-plugin: hybrid_search: query=%q search_text=%q", original, query)
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
		Do(ctx)

	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("weaviate hybrid_search: %v", err)}
	}
	return marshalResponse(req.ID, result)
}

// prepareResponse is the structured JSON returned by the prepare action.
//
// KnowledgeCandidates / GlossaryCandidates / ToolCandidates are the
// RFC #249 (opentalon/opentalon#249) structured shape the orchestrator
// consumes for Phase 3 dedup and Phase 4 tier-decision. Emitted
// alongside the legacy Message + RelevantTools fields: when the
// orchestrator runs the new code path it picks the structured slices
// (responseUsesLegacyKnowledgeInjection short-circuits on
// len(KnowledgeCandidates) > 0); older orchestrators ignore the unknown
// fields and continue to read Message + RelevantTools. This dual-shape
// emission is the graceful-migration path that lets the same plugin
// binary run against pre- and post-#251 cores during rollout.
type prepareResponse struct {
	SendToLLM     bool     `json:"send_to_llm"`
	Message       string   `json:"message"`
	RelevantTools []string `json:"relevant_tools"`

	KnowledgeCandidates []KnowledgeCandidate `json:"knowledge_candidates,omitempty"`
	GlossaryCandidates  []GlossaryCandidate  `json:"glossary_candidates,omitempty"`
	ToolCandidates      []ToolCandidate      `json:"tool_candidates,omitempty"`

	// TranslatorEvents bubbles per-translator-call metadata to the
	// orchestrator so it can emit `translation` session_events parented
	// to the user_message — see opentalon/opentalon#256. Nil/empty when
	// the translator is disabled or skipped_disabled (no audit signal).
	// Pre-#256 orchestrators ignore the unknown field; this stays
	// back-compat by virtue of being additive + omitempty.
	TranslatorEvents []TranslatorEvent `json:"translator_events,omitempty"`
}

// defaultMinPrepareScore is the default minimum hybrid-search score for a
// result to be included in the prepare response. Configurable via
// config.min_prepare_score. Per-collection knobs
// (min_prepare_score_tools / _knowledge / _glossary) further override
// this for one collection at a time.
const defaultMinPrepareScore = 0.012

// pickPositive returns *override if it points at a positive float,
// otherwise the supplied fallback. Used for per-collection score
// thresholds: a deployment that only sets the umbrella
// min_prepare_score reuses it for all three collections; one that
// wants asymmetric gates declares only the deltas.
func pickPositive(override *float64, fallback float64) float64 {
	if override != nil && *override > 0 {
		return *override
	}
	return fallback
}

// Default per-collection ceilings for `prepare`. See Config.PrepareKnowledgeLimit
// for the rationale. These need to exceed the orchestrator's downstream
// tier/cap settings; if they don't, the dedup `cap_exceeded` and tier 2
// promotion code paths are unreachable.
const (
	defaultPrepareKnowledgeLimit = 15
	defaultPrepareActionsLimit   = 25
	defaultPrepareGlossaryLimit  = 10
)

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

	// Translate the user query to the target language (default EN) before
	// it reaches Weaviate, so non-EN queries can BM25-match an EN-indexed
	// corpus. Only the SEARCH side is translated — the original `text`
	// stays in the Message we return so the LLM still sees the user's
	// query in their original language. The returned event (nil when
	// translator was disabled / input empty) is bubbled to the orchestrator
	// in prepareResponse.TranslatorEvents — opentalon/opentalon#256.
	searchText, translatorEvent := h.translateQuery(ctx, text, "prepare")
	translatorEvents := translatorEventsOf(translatorEvent)

	// Parse allowed_plugins (profile-level allowlist, applied as the
	// MCPActions GraphQL WHERE filter so search cost stays bounded).
	var allowedPlugins []string
	if v, ok := req.Args[ctxArgAllowedPlugins]; ok && v != "" {
		_ = json.Unmarshal([]byte(v), &allowedPlugins)
	}

	// Parse allowed_tools into the per-session FQN palette applied
	// post-retrieval inside the actionFilter chokepoint. Defense-safe
	// default: when the host does NOT inject the arg — or injects an
	// unparseable / null value — every retrieved action is dropped.
	//
	//   - arg omitted / "" / "null" / malformed → fail-closed empty map →
	//     every retrieved action filtered out. The fail-open alternative
	//     would silently degrade a restricted session to "no filter"
	//     whenever the orchestrator forgot to inject the arg (deploy
	//     ordering, missing provider, config drift) — re-opening the
	//     defense-in-depth gap the palette exists to close.
	//   - arg present as "[]" → fail-closed empty map (same outcome).
	//   - arg present as JSON array → strict subset enforcement.
	//
	// Deploy contract: opentalon-core ≥ the commit that adds the
	// allowed_tools ContextArgProvider MUST be paired with this plugin.
	// An older Core without the provider will leave the arg absent and
	// every prepare() call will return zero tools. This is intentional
	// — failing closed beats silently dropping the gate.
	availableTools := make(map[string]struct{})
	if v, ok := req.Args[ctxArgAllowedTools]; ok && v != "" && v != "null" {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err == nil && list != nil {
			availableTools = make(map[string]struct{}, len(list))
			for _, name := range list {
				availableTools[name] = struct{}{}
			}
		}
	}
	actionsFilter := actionFilter{
		minScore:       h.minPrepareScoreTools,
		availableTools: availableTools,
	}

	// Search KnowledgeArticles with plugin-boosted query. Limit is
	// h.prepareKnowledgeLimit (default 15) — needs to exceed the
	// orchestrator's cap_per_turn so the dedup `cap_exceeded` reason can fire.
	// `_additional.id` is requested so structured KnowledgeCandidates can
	// carry a stable ArticleID downstream (RFC #249 Phase 3 dedup uses
	// ContentSHA256 as the primary key, but ArticleID is the audit-trail
	// identifier the api-plugin surfaces in events).
	knowledgeFields := []graphql.Field{
		{Name: "title"}, {Name: "content"}, {Name: "source"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "id"}, {Name: "score"}}},
	}
	knowledgeQuery := searchText
	if len(allowedPlugins) > 0 {
		knowledgeQuery = searchText + " " + strings.Join(allowedPlugins, " ")
	}
	knowledgeResult, knowledgeErr := h.searchCollection(ctx, h.knowledgeCollection, knowledgeFields, knowledgeQuery, h.prepareKnowledgeLimit, nil)

	// Search MCPActions with optional plugin filter. Limit is
	// h.prepareActionsLimit (default 25) — needs to exceed
	// tier1_cap + tier2_cap so the orchestrator can actually populate Tier 2;
	// when the limit is ≤ tier1_cap, every retrieved tool fits into Tier 1
	// and Tier 2 stays empty. Only pluginName + actionName + score needed —
	// the orchestrator already has full tool definitions in the system prompt
	// via relevant_tools filtering.
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
	actionsResult, actionsErr := h.searchCollection(ctx, h.actionsCollection, actionFields, searchText, h.prepareActionsLimit, actionsWhere)

	// Search Glossary for matching terms/definitions. Limit is
	// h.prepareGlossaryLimit (default 10). `_additional.id` is requested for
	// the same reason as the KnowledgeArticles search above — feeds
	// structured GlossaryCandidates downstream.
	glossaryFields := []graphql.Field{
		{Name: "term"}, {Name: "definition"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "id"}, {Name: "score"}}},
	}
	glossaryResult, glossaryErr := h.searchCollection(ctx, h.glossaryCollection, glossaryFields, searchText, h.prepareGlossaryLimit, nil)

	// Fail-open: if all searches fail, pass through unchanged.
	if knowledgeErr != nil && actionsErr != nil && glossaryErr != nil {
		return marshalPrepareResponse(req.ID, prepareResponse{
			SendToLLM:        true,
			Message:          text,
			RelevantTools:    []string{},
			TranslatorEvents: translatorEvents,
		})
	}

	// Extract relevant tool names (post-filter chokepoint) for system prompt
	// filtering. The orchestrator uses this list to decide which tools to
	// show the LLM. The same filter feeds toolCandidates below so both
	// outputs honour the session's allowed_tools palette.
	tools := extractToolNames(actionsResult, h.actionsCollection, actionsFilter)

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

	log.Printf("weaviate-plugin: prepare: query=%q search_text=%q min_score_tools=%.4f matched_tools=%d tools=%v",
		text, searchText, h.minPrepareScoreTools, len(tools), tools)

	// Build message: inject glossary and knowledge context blocks (if non-empty).
	// Action/tool context is NOT injected into the message — the orchestrator
	// already provides full tool definitions in the system prompt filtered by
	// relevant_tools. Duplicating them here wastes tokens.
	//
	// Alongside the legacy text blocks, populate the structured
	// *Candidates slices (RFC #249) from the same filtered item set so
	// the orchestrator's Phase 3 dedup + Phase 4 tier-decision pick the
	// structured path. The legacy blocks stay in Message for backwards
	// compatibility with pre-#251 cores; the orchestrator's
	// applyDedupToContent strips the parsed-out [knowledge_context]
	// block before re-rendering from the deduped Candidates so there's
	// no double injection.
	message := text
	var knowledgeCandidates []KnowledgeCandidate
	var glossaryCandidates []GlossaryCandidate
	if knowledgeErr == nil {
		knowledgeItems := extractItems(knowledgeResult, h.knowledgeCollection)
		// Exclude MCP server-instructions articles — they are already in the
		// system prompt via SystemPromptAddition. Including them in
		// [knowledge_context] would duplicate them on every LLM call.
		knowledgeItems = filterOutMCPItems(knowledgeItems)
		log.Printf("weaviate-plugin: prepare: knowledge_items=%d knowledge_err=%v", len(knowledgeItems), knowledgeErr)
		knowledgeCandidates = extractKnowledgeCandidates(knowledgeItems, h.minPrepareScoreKnowledge)
		if knowledgeText := formatItemsCompact(knowledgeItems, h.minPrepareScoreKnowledge); knowledgeText != "" {
			log.Printf("weaviate-plugin: prepare: injecting knowledge_context len=%d", len(knowledgeText))
			message = fmt.Sprintf("[knowledge_context]\n%s\n[/knowledge_context]\n\n%s", knowledgeText, text)
		}
	}
	if glossaryErr == nil {
		glossaryItems := extractItems(glossaryResult, h.glossaryCollection)
		glossaryCandidates = extractGlossaryCandidates(glossaryItems, h.minPrepareScoreGlossary)
		if glossaryText := formatGlossaryItems(glossaryItems, h.minPrepareScoreGlossary); glossaryText != "" {
			log.Printf("weaviate-plugin: prepare: injecting glossary_context len=%d", len(glossaryText))
			message = fmt.Sprintf("[glossary_context]\n%s\n[/glossary_context]\n\n%s", glossaryText, message)
		}
	}
	var toolCandidates []ToolCandidate
	if actionsErr == nil {
		toolCandidates = extractToolCandidatesFromResult(actionsResult, h.actionsCollection, actionsFilter)
	}

	return marshalPrepareResponse(req.ID, prepareResponse{
		SendToLLM:           true,
		Message:             message,
		RelevantTools:       tools,
		KnowledgeCandidates: knowledgeCandidates,
		GlossaryCandidates:  glossaryCandidates,
		ToolCandidates:      toolCandidates,
		TranslatorEvents:    translatorEvents,
	})
}

// translatorEventsOf is a small helper for the prepare response builder:
// wraps a non-nil translator event into a single-element slice so the
// JSON field omits cleanly (omitempty drops nil/empty slices alike).
// Keeps the prepare handler readable — the two call sites stay one line
// each rather than open-coded nil-checks.
func translatorEventsOf(e *TranslatorEvent) []TranslatorEvent {
	if e == nil {
		return nil
	}
	return []TranslatorEvent{*e}
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

// MCPActions extractors (extractToolNames, extractToolCandidatesFromResult)
// live in candidates.go and share the actionFilter chokepoint.

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

// formatGlossaryItems formats Glossary search results above the score threshold
// as a compact list of term→definition pairs for injection into the LLM message.
func formatGlossaryItems(items []map[string]interface{}, minScore float64) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		if !aboveScore(item, minScore) {
			continue
		}
		term, _ := item["term"].(string)
		definition, _ := item["definition"].(string)
		if term == "" || definition == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "- **%s**: %s", term, definition)
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
	original := query
	query, _ = h.translateQuery(ctx, query, "ask_knowledge")
	if query != original {
		log.Printf("weaviate-plugin: ask_knowledge: query=%q search_text=%q", original, query)
	}

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
	} else if v, ok := req.Args[ctxArgAllowedPlugins]; ok && v != "" {
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
	original := query
	query, _ = h.translateQuery(ctx, query, "search_instructions")
	if query != original {
		log.Printf("weaviate-plugin: search_instructions: query=%q search_text=%q", original, query)
	}

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
	// KnowledgeArticles ships per-section knowledge contributed by the plugin
	// via the MCP `initialize._meta.knowledge_articles` field. Each entry is
	// stored as one KnowledgeArticles record with source
	// "mcp-knowledge:<plugin>:<id>" so the prepare-path RAG can pull just the
	// relevant sections into [knowledge_context] instead of injecting the full
	// per-plugin instructions blob into every system prompt. Optional; older
	// orchestrators that omit the field keep the legacy single-blob shape.
	KnowledgeArticles []knowledgeArticleEntry `json:"knowledge_articles,omitempty"`
	KeepPlugins       []string                `json:"keep_plugins,omitempty"`
	// Hash is an opaque string computed by the orchestrator over the combined
	// actions + server_instructions + knowledge_articles for this plugin. When
	// present and matching the last-seen hash, the plugin skips the upsert
	// (but still runs orphan prune). Older orchestrators that omit it get the
	// legacy always-sync.
	Hash string `json:"hash,omitempty"`
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

// knowledgeArticleEntry is one section payload from sync_actions's
// knowledge_articles[]. ID, Title and Content are required. Tags is optional
// and gets augmented with ["opentalon", "mcp", "knowledge", <plugin>] on
// storage so list/search by tag works the same as for the other auto-managed
// KnowledgeArticles records.
type knowledgeArticleEntry struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// enqueueSyncActions validates the payload synchronously and enqueues the
// actual Weaviate work for the background sync worker. Returns immediately.
func (h *WeaviateHandler) enqueueSyncActions(req plugin.Request) plugin.Response {
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

	h.syncJobCh <- syncJob{kind: syncJobActions, reqID: req.ID, payload: raw}

	h.syncMu.Lock()
	h.syncStatus.ActionsQueued++
	h.syncMu.Unlock()
	syncJobsEnqueued.WithLabelValues("sync_actions").Inc()
	syncQueueDepth.Inc()

	log.Printf("weaviate-plugin: sync_actions: queued for %s (req=%s)", payload.PluginName, req.ID)
	return plugin.Response{CallID: req.ID, Content: `{"queued":true,"action":"sync_actions"}`}
}

// syncActionsWork performs the actual sync_actions Weaviate operations.
// Called by the background worker goroutine.
func (h *WeaviateHandler) syncActionsWork(reqID string, raw string) error {
	var payload syncActionsPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return fmt.Errorf("parse payload: %v", err)
	}

	ctx := context.Background()

	// Hash-based skip: when the orchestrator sends a hash and it matches the
	// last-seen value for this plugin, skip the expensive upsert cycle. Orphan
	// prune via keep_plugins still runs — it's cheap and keeps the index clean
	// across plugin additions/removals. Continuation batches skip the hash
	// check (the decision was made on batch 0).
	h.syncMu.RLock()
	hashMatch := !payload.IsContinuationBatch && payload.Hash != "" && payload.Hash == h.actionHashes[payload.PluginName]
	h.syncMu.RUnlock()

	if hashMatch {
		log.Printf("weaviate-plugin: sync_actions: hash match for %s (%s), skipping upsert", payload.PluginName, payload.Hash)
		if len(payload.KeepPlugins) > 0 {
			if n, err := h.pruneOrphans(ctx, payload.KeepPlugins); err != nil {
				log.Printf("weaviate-plugin: prune_orphans failed: %v", err)
			} else if n > 0 {
				log.Printf("weaviate-plugin: sync_actions: pruned %d orphans", n)
			}
		}
		return nil
	}

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
	hasInstructions := false
	if payload.ServerInstructions != "" {
		log.Printf("weaviate-plugin: sync_actions: storing server instructions for %s (%d bytes)", payload.PluginName, len(payload.ServerInstructions))
		hasInstructions = true
		objects = append(objects, &wmodels.Object{
			Class: h.knowledgeCollection,
			ID:    strfmt.UUID(serverInstructionsUUID(payload.PluginName)),
			Properties: map[string]interface{}{
				"title":   payload.PluginName + " MCP server instructions",
				"content": payload.ServerInstructions,
				"source":  MCPSourcePrefix + payload.PluginName,
				"tags":    []string{"opentalon", "mcp", "server-instructions", payload.PluginName},
			},
		})
	}

	// Pre-delete all "mcp-knowledge:<plugin>:*" records on batch 0 so sections
	// that the plugin removed or renamed between syncs vanish from the index
	// (deterministic-UUID upsert overwrites in place but cannot delete a row
	// no longer in payload). Mirrors the per-plugin actions pre-delete above.
	// Skipped on continuation batches — those would wipe earlier batches' inserts.
	if !payload.IsContinuationBatch && len(payload.KnowledgeArticles) > 0 {
		_, delErr := h.client.Batch().ObjectsBatchDeleter().
			WithClassName(h.knowledgeCollection).
			WithWhere(filters.Where().
				WithPath([]string{"source"}).
				WithOperator(filters.Like).
				WithValueText(MCPKnowledgeSourcePrefix + payload.PluginName + ":*")).
			Do(ctx)
		if delErr != nil {
			log.Printf("weaviate-plugin: sync_actions: failed to delete old knowledge articles for %s: %v", payload.PluginName, delErr)
		}
	}

	knowledgeCount := 0
	for _, ka := range payload.KnowledgeArticles {
		if ka.ID == "" || ka.Title == "" || ka.Content == "" {
			log.Printf("weaviate-plugin: sync_actions: skipping invalid knowledge article for %s (id=%q title=%q)", payload.PluginName, ka.ID, ka.Title)
			continue
		}
		tags := append([]string{"opentalon", "mcp", "knowledge", payload.PluginName}, ka.Tags...)
		objects = append(objects, &wmodels.Object{
			Class: h.knowledgeCollection,
			ID:    strfmt.UUID(knowledgeArticleUUID(payload.PluginName, ka.ID)),
			Properties: map[string]interface{}{
				"title":   ka.Title,
				"content": ka.Content,
				"source":  MCPKnowledgeSourcePrefix + payload.PluginName + ":" + ka.ID,
				"tags":    tags,
			},
		})
		knowledgeCount++
	}

	syncedActions := len(payload.Actions)
	if len(objects) > 0 {
		results, err := h.client.Batch().ObjectsBatcher().WithObjects(objects...).Do(ctx)
		if err != nil {
			return fmt.Errorf("batch sync: %v", err)
		}
		if err := checkBatchErrors(results); err != nil {
			return fmt.Errorf("batch sync: %v", err)
		}
	}

	// Inter-plugin orphan prune.
	pruned := 0
	if len(payload.KeepPlugins) > 0 {
		n, err := h.pruneOrphans(ctx, payload.KeepPlugins)
		if err != nil {
			log.Printf("weaviate-plugin: prune_orphans failed: %v", err)
		}
		pruned = n
	}

	// Store hash on successful sync so next call with the same hash can skip.
	if payload.Hash != "" {
		h.syncMu.Lock()
		h.actionHashes[payload.PluginName] = payload.Hash
		h.syncMu.Unlock()
	}

	log.Printf("weaviate-plugin: sync_actions: completed for %s (req=%s synced=%d instructions=%v knowledge=%d pruned=%d)",
		payload.PluginName, reqID, syncedActions, hasInstructions, knowledgeCount, pruned)
	return nil
}

// ---------------------------------------------------------------------------
// Background sync worker
// ---------------------------------------------------------------------------

// runSyncWorker is a long-lived goroutine that drains syncJobCh sequentially,
// preserving continuation batch ordering (FIFO).
func (h *WeaviateHandler) runSyncWorker(ctx context.Context) {
	log.Println("weaviate-plugin: background sync worker started")
	h.syncMu.Lock()
	h.syncStatus.WorkerRunning = true
	h.syncMu.Unlock()

	for {
		select {
		case <-ctx.Done():
			log.Println("weaviate-plugin: background sync worker stopping")
			h.syncMu.Lock()
			h.syncStatus.WorkerRunning = false
			h.syncMu.Unlock()
			return
		case job := <-h.syncJobCh:
			h.processSyncJob(job)
		}
	}
}

func (h *WeaviateHandler) processSyncJob(job syncJob) {
	syncQueueDepth.Dec()
	start := time.Now()
	var err error

	switch job.kind {
	case syncJobActions:
		err = h.syncActionsWork(job.reqID, job.payload)
		h.syncMu.Lock()
		if err != nil {
			h.syncStatus.ActionsFailed++
		} else {
			h.syncStatus.ActionsCompleted++
			h.syncStatus.LastActionSync = time.Now()
		}
		h.syncMu.Unlock()
	case syncJobGlossary:
		err = h.syncGlossaryWork(job.reqID, job.payload)
		h.syncMu.Lock()
		if err != nil {
			h.syncStatus.GlossaryFailed++
		} else {
			h.syncStatus.GlossaryCompleted++
			h.syncStatus.LastGlossarySync = time.Now()
		}
		h.syncMu.Unlock()
	}

	duration := time.Since(start).Seconds()
	status := "ok"
	if err != nil {
		status = "error"
		log.Printf("weaviate-plugin: background sync job failed (%s, req=%s): %v", job.kind, job.reqID, err)
	}
	syncJobDuration.WithLabelValues(job.kind.String(), status).Observe(duration)
}

// getSyncStatus returns the current background sync worker state.
func (h *WeaviateHandler) getSyncStatus(req plugin.Request) plugin.Response {
	h.syncMu.RLock()
	s := h.syncStatus
	h.syncMu.RUnlock()

	pending := (s.ActionsQueued - s.ActionsCompleted - s.ActionsFailed) +
		(s.GlossaryQueued - s.GlossaryCompleted - s.GlossaryFailed)

	body, _ := json.Marshal(map[string]interface{}{
		"actions_queued":     s.ActionsQueued,
		"actions_completed":  s.ActionsCompleted,
		"actions_failed":     s.ActionsFailed,
		"glossary_queued":    s.GlossaryQueued,
		"glossary_completed": s.GlossaryCompleted,
		"glossary_failed":    s.GlossaryFailed,
		"pending":            pending,
		"worker_running":     s.WorkerRunning,
		"last_action_sync":   s.LastActionSync,
		"last_glossary_sync": s.LastGlossarySync,
	})
	return plugin.Response{CallID: req.ID, Content: string(body)}
}

// pruneOrphans deletes any MCPActions and any auto-generated KnowledgeArticles
// (those with source matching MCPSourcePrefix or MCPKnowledgeSourcePrefix)
// whose plugin is NOT in keep.
//
// Implementation note: instead of a single batch-delete with a NotEqual filter
// (Weaviate's NotEqual on tokenized text fields produces unreliable results
// for our purposes), we discover the set of distinct pluginName / source
// values currently indexed and issue one Equal-based batch-delete per orphan.
// Equal on tokenized text matches the full property value reliably — same
// pattern PR #16's intra-plugin pre-delete depends on.
//
// Returns the total deletion count across all collections (best-effort:
// per-class errors are recorded but do not abort subsequent deletes).
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

	// KnowledgeArticles, server-instructions records: scope to MCP-managed
	// (source LIKE "mcp:*"), discover distinct sources, delete each orphan
	// by Equal on source. Source format: "mcp:<plugin>" (1:1 plugin mapping).
	//
	// IMPORTANT — Weaviate's Like operator on tokenized text fields matches
	// per-token, so the pattern "mcp:*" returns ANY record whose tokens
	// include "mcp" (e.g. "mcp-knowledge:foo:bar" tokens contain "mcp"
	// because "-" is a token separator). Filter the discovered sources by
	// exact-string prefix before treating them as server-instructions
	// records — without that, mcp-knowledge:* rows would be misclassified
	// here, fail the TrimPrefix-based plugin extraction, and get deleted
	// as orphans on every prune.
	mcpScope := filters.Where().
		WithPath([]string{"source"}).
		WithOperator(filters.Like).
		WithValueText(MCPSourcePrefix + "*")
	sources, err := h.distinctValues(ctx, h.knowledgeCollection, "source", mcpScope)
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("KnowledgeArticles distinct: %w", err)
	}
	for _, src := range sources {
		if !strings.HasPrefix(src, MCPSourcePrefix) || strings.HasPrefix(src, MCPKnowledgeSourcePrefix) {
			continue
		}
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

	// KnowledgeArticles, knowledge-section records: scope to plugin-contributed
	// sections (source LIKE "mcp-knowledge:*"), discover distinct sources, then
	// delete each orphan. Source format: "mcp-knowledge:<plugin>:<article-id>"
	// — extract plugin between the first and second ':'. Same per-token
	// over-match concern as above; gate on exact-string prefix.
	knowledgeScope := filters.Where().
		WithPath([]string{"source"}).
		WithOperator(filters.Like).
		WithValueText(MCPKnowledgeSourcePrefix + "*")
	knowledgeSources, err := h.distinctValues(ctx, h.knowledgeCollection, "source", knowledgeScope)
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("KnowledgeArticles knowledge-distinct: %w", err)
	}
	for _, src := range knowledgeSources {
		if !strings.HasPrefix(src, MCPKnowledgeSourcePrefix) {
			continue
		}
		rest := strings.TrimPrefix(src, MCPKnowledgeSourcePrefix)
		plugin := rest
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			plugin = rest[:i]
		}
		if keepSet[plugin] {
			continue
		}
		n, err := h.batchDeleteEqual(ctx, h.knowledgeCollection, "source", src)
		total += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("KnowledgeArticles knowledge-delete %s: %w", src, err)
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

// ---------------------------------------------------------------------------
// Glossary sync
// ---------------------------------------------------------------------------

type syncGlossaryPayload struct {
	GlossaryHash        string          `json:"glossary_hash"`
	Entries             []glossaryEntry `json:"entries"`
	IsContinuationBatch bool            `json:"is_continuation_batch,omitempty"`
}

type glossaryEntry struct {
	Term       string   `json:"term"`
	Definition string   `json:"definition"`
	Category   string   `json:"category,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Synonyms   []string `json:"synonyms,omitempty"`
}

// enqueueSyncGlossary validates the payload and enqueues glossary sync
// for the background worker. Returns immediately.
func (h *WeaviateHandler) enqueueSyncGlossary(req plugin.Request) plugin.Response {
	raw, ok := req.Args["payload"]
	if !ok || raw == "" {
		return plugin.Response{CallID: req.ID, Error: "payload is required"}
	}
	var payload syncGlossaryPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("parse payload: %v", err)}
	}

	h.syncJobCh <- syncJob{kind: syncJobGlossary, reqID: req.ID, payload: raw}

	h.syncMu.Lock()
	h.syncStatus.GlossaryQueued++
	h.syncMu.Unlock()
	syncJobsEnqueued.WithLabelValues("sync_glossary").Inc()
	syncQueueDepth.Inc()

	log.Printf("weaviate-plugin: sync_glossary: queued (req=%s entries=%d)", req.ID, len(payload.Entries))
	return plugin.Response{CallID: req.ID, Content: `{"queued":true,"action":"sync_glossary"}`}
}

// syncGlossaryWork performs the actual glossary sync Weaviate operations.
// Called by the background worker goroutine.
func (h *WeaviateHandler) syncGlossaryWork(reqID string, raw string) error {
	var payload syncGlossaryPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return fmt.Errorf("parse payload: %v", err)
	}

	ctx := context.Background()

	// Hash-based skip on batch 0.
	h.syncMu.RLock()
	hashMatch := !payload.IsContinuationBatch && payload.GlossaryHash != "" && payload.GlossaryHash == h.glossaryHash
	h.syncMu.RUnlock()

	if hashMatch {
		log.Printf("weaviate-plugin: sync_glossary: hash match (%s), skipping", payload.GlossaryHash)
		return nil
	}

	// Batch 0: delete all existing glossary entries for a clean full replace.
	if !payload.IsContinuationBatch {
		_, delErr := h.client.Batch().ObjectsBatchDeleter().
			WithClassName(h.glossaryCollection).
			WithWhere(filters.Where().
				WithPath([]string{"term"}).
				WithOperator(filters.Like).
				WithValueText("*")).
			Do(ctx)
		if delErr != nil {
			log.Printf("weaviate-plugin: sync_glossary: failed to delete old entries: %v", delErr)
		}
	}

	objects := make([]*wmodels.Object, 0, len(payload.Entries))
	for _, e := range payload.Entries {
		if e.Term == "" || e.Definition == "" {
			continue
		}
		props := map[string]interface{}{
			"term":       e.Term,
			"definition": e.Definition,
		}
		if e.Category != "" {
			props["category"] = e.Category
		}
		if len(e.Tags) > 0 {
			props["tags"] = e.Tags
		}
		if len(e.Synonyms) > 0 {
			props["synonyms"] = e.Synonyms
		}
		objects = append(objects, &wmodels.Object{
			Class:      h.glossaryCollection,
			ID:         strfmt.UUID(glossaryUUID(e.Term)),
			Properties: props,
		})
	}

	if len(objects) > 0 {
		results, err := h.client.Batch().ObjectsBatcher().WithObjects(objects...).Do(ctx)
		if err != nil {
			return fmt.Errorf("batch sync glossary: %v", err)
		}
		if err := checkBatchErrors(results); err != nil {
			return fmt.Errorf("batch sync glossary: %v", err)
		}
	}

	// Store hash after successful sync.
	if payload.GlossaryHash != "" {
		h.syncMu.Lock()
		h.glossaryHash = payload.GlossaryHash
		h.syncMu.Unlock()
	}

	log.Printf("weaviate-plugin: sync_glossary: completed (req=%s synced=%d hash=%s continuation=%v)",
		reqID, len(objects), payload.GlossaryHash, payload.IsContinuationBatch)
	return nil
}

// serverInstructionsUUID returns a deterministic UUID v5 for the
// per-plugin server-instructions article. Re-runs upsert in place.
func serverInstructionsUUID(pluginName string) string {
	return uuid.NewSHA1(articleNS, []byte("mcp-server-instructions:"+pluginName)).String()
}

// knowledgeArticleUUID returns a deterministic UUID v5 for one section
// article contributed by a plugin via sync_actions's knowledge_articles[].
// The key embeds plugin + article ID so that two plugins shipping the same
// article ID get distinct rows, and re-runs upsert in place.
func knowledgeArticleUUID(pluginName, articleID string) string {
	return uuid.NewSHA1(articleNS, []byte(MCPKnowledgeSourcePrefix+pluginName+":"+articleID)).String()
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

func glossaryUUID(term string) string {
	return uuid.NewSHA1(glossaryNS, []byte(term)).String()
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
