package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
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

// DefaultKnowledgeCollection is the default class name for the knowledge base.
const DefaultKnowledgeCollection = "KnowledgeArticles"

// DefaultActionsCollection is the default class name for the MCP tool corpus —
// one record per plugin action (tool schema), kept in lockstep with the
// orchestrator's registry by the periodic capability sync. The corpus is the
// semantically searchable copy of every tool description; execution and the
// prompt catalog read the registry, not this class.
const DefaultActionsCollection = "MCPActions"

// articleNS is a UUID v5 namespace for deterministic KnowledgeArticles IDs
// generated from the knowledge sync (e.g. one article per plugin's MCP server
// instructions, one per contributed knowledge section).
var articleNS = uuid.MustParse("e5f9a2b3-6c4d-5e7f-a0b1-2c3d4e5f6a7b")

// actionNS is a UUID v5 namespace for deterministic MCP action object IDs.
// The value is unchanged from the original tool sync, so upserts from this
// restored path address the very same objects an older deployment wrote — a
// stale corpus self-heals in place on the first sync cycle (changed docs
// rewritten via the contentHash diff, missing ones inserted, removed ones
// pruned) without any manual data migration.
var actionNS = uuid.MustParse("d4e8f1a2-5b3c-4d6e-9f0a-1b2c3d4e5f6a")

// MCPSourcePrefix tags KnowledgeArticles records that were generated from a
// plugin's MCP server-instructions text rather than authored manually. Used
// for filtered prune queries — only auto-managed records are deleted.
const MCPSourcePrefix = "mcp:"

// MCPKnowledgeSourcePrefix tags KnowledgeArticles records contributed by a
// plugin via the per-section `knowledge_articles[]` field on the sync. Each
// record's source is "mcp-knowledge:<plugin>:<article-id>". Distinct from
// MCPSourcePrefix ("mcp:") so search_instructions (which scopes to "mcp:")
// can be told apart from the section articles surfaced via ask_knowledge.
const MCPKnowledgeSourcePrefix = "mcp-knowledge:"

// Config is the JSON config block passed via the Init RPC.
type Config struct {
	Host                string         `json:"host"`
	Scheme              string         `json:"scheme"`
	KnowledgeCollection string         `json:"knowledge_collection"`
	ActionsCollection   string         `json:"actions_collection"`
	AutoCreateSchema    *bool          `json:"auto_create_schema"`
	HTTPAddr            string         `json:"http_addr"`
	Token               string         `json:"token"`
	Vectorizer          string         `json:"vectorizer"`
	ModuleConfig        map[string]any `json:"module_config"`
	Timeout             string         `json:"timeout"` // Weaviate HTTP client timeout as duration string (e.g. "2m", "90s"); default "2m"
}

// syncJob is the envelope queued for background processing.
type syncJob struct {
	reqID   string // original request ID for log correlation
	payload string // raw JSON payload (already validated)
}

// syncStatusState exposes counters and timestamps for the background sync worker.
type syncStatusState struct {
	ActionsQueued    int64     `json:"actions_queued"`
	ActionsCompleted int64     `json:"actions_completed"`
	ActionsFailed    int64     `json:"actions_failed"`
	LastActionSync   time.Time `json:"last_action_sync,omitempty"`
	WorkerRunning    bool      `json:"worker_running"`
}

// WeaviateHandler implements plugin.Handler.
type WeaviateHandler struct {
	client              *weaviate.Client
	knowledgeCollection string
	actionsCollection   string
	httpAddr            string
	token               string
	vectorizer          string
	moduleConfig        map[string]any
	clientTimeout       time.Duration

	// Background sync worker.
	syncJobCh  chan syncJob
	syncMu     sync.RWMutex
	syncStatus syncStatusState

	// schemaMu serializes ensureSchemas so the schema reconcile (read live
	// schema, then add any missing property) can't interleave across the two
	// callers — Configure at startup and the refresh action at any time. Without
	// it, two concurrent runs could both see a property missing and both try to
	// add it, and the loser fails with "property already exists". Schema setup is
	// not on a hot path, so a plain mutex is the simplest correct guard.
	schemaMu sync.Mutex
}

// Configure is called by the SDK during the Init RPC with the JSON config block.
func (h *WeaviateHandler) Configure(configJSON string) error {
	log.Println("weaviate-plugin: Configure begin")

	cfg := Config{
		Host:   "localhost:8080",
		Scheme: "http",
	}
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
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

	h.knowledgeCollection = cfg.KnowledgeCollection
	if h.knowledgeCollection == "" {
		h.knowledgeCollection = DefaultKnowledgeCollection
	}

	h.actionsCollection = cfg.ActionsCollection
	if h.actionsCollection == "" {
		h.actionsCollection = DefaultActionsCollection
	}

	h.httpAddr = cfg.HTTPAddr
	h.token = cfg.Token

	h.vectorizer = cfg.Vectorizer
	if h.vectorizer == "" {
		h.vectorizer = "text2vec-transformers"
	}
	h.moduleConfig = cfg.ModuleConfig

	if h.httpAddr != "" && h.token == "" {
		return fmt.Errorf("config.token is required when http_addr is set")
	}

	log.Printf("weaviate-plugin: vectorizer=%s knowledge_collection=%s actions_collection=%s",
		h.vectorizer, h.knowledgeCollection, h.actionsCollection)

	autoCreate := cfg.AutoCreateSchema == nil || *cfg.AutoCreateSchema
	if autoCreate {
		log.Println("weaviate-plugin: auto-creating schemas")
		if err := h.ensureSchemas(context.Background()); err != nil {
			return fmt.Errorf("auto-create schemas: %w", err)
		}
		log.Println("weaviate-plugin: schemas ready")
	}

	// Start the background sync worker. All corpus-sync calls (tool schemas +
	// knowledge) are enqueued here so the orchestrator returns immediately.
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

	log.Printf("weaviate-plugin: init done (Configure) host=%s://%s knowledge_collection=%s actions_collection=%s http=%s",
		cfg.Scheme, cfg.Host, h.knowledgeCollection, h.actionsCollection, h.httpAddr)

	return nil
}

// ensureSchemas creates the MCPActions and KnowledgeArticles collections if
// they don't exist.
func (h *WeaviateHandler) ensureSchemas(ctx context.Context) error {
	// Serialize against a concurrent caller (startup vs. refresh) so the
	// read-then-add-missing-property reconcile in ensureClass is atomic.
	h.schemaMu.Lock()
	defer h.schemaMu.Unlock()

	// Property set matches the class as originally created, so ensureClass on a
	// live deployment that still carries the old MCPActions class reconciles to
	// a no-op instead of fighting the existing schema.
	//
	// pluginName is word-tokenized (NOT field-tokenized like KnowledgeArticles'
	// source). The action prunes filter on it with Equal, and Weaviate's Equal
	// on word-tokenized text matches per-token — so if one plugin's name were a
	// token-subset of another's (e.g. "timly" vs "timly-admin"), a prune scoped
	// to the shorter name could also hit the longer plugin's rows. Left as-is on
	// purpose: it matches the class the live corpus already carries (changing the
	// tokenization would break the reconcile-no-op above), no current plugin
	// names token-overlap, and a wrongly wiped kept plugin self-heals on its next
	// sync. If plugin names ever risk overlap, add a client-side exact-match
	// guard on the delete paths (the pattern KnowledgeArticles' prunes already
	// use for source) rather than re-tokenizing.
	if err := h.ensureClass(ctx, h.actionsCollection, []*wmodels.Property{
		{Name: "pluginName", DataType: []string{"text"}},
		{Name: "actionName", DataType: []string{"text"}},
		{Name: "description", DataType: []string{"text"}},
		{Name: "parameters", DataType: []string{"text"}},
		h.contentHashProperty(),
	}); err != nil {
		return err
	}
	return h.ensureClass(ctx, h.knowledgeCollection, []*wmodels.Property{
		{Name: "title", DataType: []string{"text"}},
		// slug is the stable per-article identifier (the MCP server's article
		// id). Field-tokenized so a WHERE Equal matches the whole hyphenated
		// value exactly — the key behind ask_knowledge(slug=…) and the catalog.
		{Name: "slug", DataType: []string{"text"}, Tokenization: "field"},
		{Name: "content", DataType: []string{"text"}},
		// source carries the exact provenance key ("mcp:<plugin>" or
		// "mcp-knowledge:<plugin>:<slug>"). Field-tokenized so a WHERE Equal
		// matches the whole value exactly — the prune path deletes one source
		// at a time via Equal, and with default word tokenization Equal matches
		// per-token (all query tokens present), so deleting "mcp:foo" would also
		// hit "mcp-knowledge:foo:bar" (both contain the tokens mcp+foo) and prune
		// a kept plugin's article. Field tokenization makes Equal an exact match
		// and keeps the Like "<prefix>*" discovery scopes whole-value prefixed.
		{Name: "source", DataType: []string{"text"}, Tokenization: "field"},
		{Name: "tags", DataType: []string{"text[]"}},
		h.contentHashProperty(),
	})
}

// contentHashProperty is the per-doc change-detection digest stored on every
// corpus record (actions and knowledge alike). It is excluded from the
// embedding (skip): it's an opaque hex digest, not semantic content, so
// vectorizing it would only add noise to retrieval.
func (h *WeaviateHandler) contentHashProperty() *wmodels.Property {
	return &wmodels.Property{
		Name:     "contentHash",
		DataType: []string{"text"},
		ModuleConfig: map[string]interface{}{
			h.vectorizer: map[string]interface{}{"skip": true, "vectorizePropertyName": false},
		},
	}
}

func (h *WeaviateHandler) ensureClass(ctx context.Context, name string, props []*wmodels.Property) error {
	return h.ensureClassVec(ctx, name, h.vectorizer, props)
}

// ensureClassVec is ensureClass with an explicit vectorizer, so metadata
// collections can opt out of embedding with vectorizer "none".
func (h *WeaviateHandler) ensureClassVec(ctx context.Context, name, vectorizer string, props []*wmodels.Property) error {
	exists, err := h.client.Schema().ClassExistenceChecker().WithClassName(name).Do(ctx)
	if err != nil {
		return fmt.Errorf("check class %s: %w", name, err)
	}
	if exists {
		// Reconcile properties on an already-existing class so a newly added
		// field (e.g. slug) lands on a live deployment without a manual
		// migration or a destructive recreate.
		return h.ensureProperties(ctx, name, props)
	}
	class := &wmodels.Class{
		Class:      name,
		Vectorizer: vectorizer,
		Properties: props,
	}
	// Only vectorized classes carry the embedding module config; a "none"
	// vectorizer (metadata) has no vectors and needs no module config.
	if vectorizer != "none" && h.moduleConfig != nil {
		class.ModuleConfig = map[string]interface{}{
			vectorizer: h.moduleConfig,
		}
	}
	return h.client.Schema().ClassCreator().WithClass(class).Do(ctx)
}

// ensureProperties adds any of the desired properties missing from an
// already-existing class. Weaviate has no "add column if not exists", so we
// read the live schema and create only the gaps. Idempotent: a property that
// already exists is skipped. Objects upserted after a property is added carry
// the value; pre-existing rows read empty until the next sync re-upserts them
// (the deterministic-UUID upsert backfills in place).
func (h *WeaviateHandler) ensureProperties(ctx context.Context, name string, want []*wmodels.Property) error {
	class, err := h.client.Schema().ClassGetter().WithClassName(name).Do(ctx)
	if err != nil {
		return fmt.Errorf("get class %s: %w", name, err)
	}
	have := make(map[string]bool, len(class.Properties))
	for _, p := range class.Properties {
		have[p.Name] = true
	}
	for _, p := range want {
		if have[p.Name] {
			continue
		}
		if err := h.client.Schema().PropertyCreator().WithClassName(name).WithProperty(p).Do(ctx); err != nil {
			return fmt.Errorf("add property %s.%s: %w", name, p.Name, err)
		}
		log.Printf("weaviate-plugin: ensureClass: added missing property %s.%s", name, p.Name)
	}
	return nil
}

// Capabilities declares this plugin's name, description, and actions to the host.
func (h *WeaviateHandler) Capabilities() plugin.CapabilitiesMsg {
	return plugin.CapabilitiesMsg{
		Name:        "weaviate",
		Description: "Corpus plugin for a Weaviate-backed vector store. Keeps the searchable copies in sync: every plugin's tool schemas in MCPActions and its knowledge in KnowledgeArticles (semantic search and exact-slug fetch via ask_knowledge, the slug+title catalog via list_knowledge_titles, MCP server-instruction search).",
		Actions: []plugin.ActionMsg{
			{
				Name:        "sync_actions",
				Description: "Upsert a plugin's tool schemas into the MCPActions collection and its MCP server instructions + knowledge articles into KnowledgeArticles. Stale entries are diff-pruned.",
				Parameters: []plugin.ParameterMsg{
					{Name: "payload", Description: `JSON: {"plugin_name":"...","actions":[{"name":"...","description":"...","parameters":...}],"keep_actions":["..."],"server_instructions":"...","knowledge_articles":[{"id":"...","title":"...","content":"..."}]}`, Type: "string", Required: true},
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
				Description: "Search the knowledge base for product docs, how-to guides, and knowledge articles. Use BEFORE asking the user when you need more context. Pass `slug` to fetch one specific article exactly (slugs come from the knowledge catalog); pass `query` for a semantic search.",
				Parameters: []plugin.ParameterMsg{
					{Name: "query", Description: "Natural language question for knowledge base search. Required unless `slug` is given.", Type: "string", Required: false},
					{Name: "slug", Description: "Exact knowledge-article slug (from the catalog) to fetch its full body deterministically. Takes precedence over query.", Type: "string", Required: false},
					{Name: "source", Description: "Filter knowledge articles by source identifier (e.g. 'help-center')", Type: "string", Required: false},
					{Name: "limit", Description: "Maximum results (default 3)", Type: "integer", Required: false},
				},
				// Pure knowledge search — no state mutation → skip the confirmation gate.
				ReadOnly: true,
				// Pin to Tier 0 so the LLM can always call ask_knowledge directly
				// (1-step, no get_tool_details promotion). This is the knowledge-pull
				// lever, mirroring the always-on get_tool_details tool-pull lever.
				AlwaysInclude: true,
			},
			{
				Name:        "list_knowledge_titles",
				Description: "List the slug + title of every available knowledge article. The orchestrator renders this as an always-on catalog so the model knows what it can pull; an article body is then fetched exactly via ask_knowledge(slug=…).",
				Parameters:  []plugin.ParameterMsg{},
				ReadOnly:    true, // pure listing — no state mutation
			},
			{
				Name:        "search_instructions",
				Description: "Search MCP server instructions stored during plugin sync. Returns guidance and context from MCP servers that match the query.",
				Parameters: []plugin.ParameterMsg{
					{Name: "query", Description: "Natural language query to search instructions", Type: "string", Required: true},
					{Name: "plugin", Description: "Narrow results to a specific plugin (e.g. 'timly')", Type: "string", Required: false},
					{Name: "limit", Description: "Maximum results (default 5)", Type: "integer", Required: false},
				},
				ReadOnly: true, // pure search — no state mutation
			},
			{
				Name:        "sync_status",
				Description: "Returns the current background sync worker status: queued, completed, and failed job counts.",
				Parameters:  []plugin.ParameterMsg{},
				ReadOnly:    true, // read-only status query
			},
			{
				Name:        "refresh",
				Description: "Re-create the MCPActions / KnowledgeArticles collections if they were deleted externally. Called automatically on session clear.",
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
	case "sync_actions":
		return h.enqueueSyncActions(req)
	case "ingest":
		return h.ingest(req)
	case "ingest_batch":
		return h.ingestBatch(req)
	case "ask_knowledge":
		return h.askKnowledge(req)
	case "list_knowledge_titles":
		return h.listKnowledgeTitles(req)
	case "search_instructions":
		return h.searchInstructions(req)
	case "sync_status":
		return h.getSyncStatus(req)
	case "refresh":
		return h.refresh(req)
	default:
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("unknown action %q", req.Action)}
	}
}

// ---------------------------------------------------------------------------
// Knowledge search helpers
// ---------------------------------------------------------------------------

// searchCollection performs a hybrid (BM25 + vector) search on the given
// collection with an optional where filter. When alpha is non-nil it sets the
// blend explicitly (0 = keyword only, 1 = vector only); when nil, Weaviate's own
// default (0.75) applies.
func (h *WeaviateHandler) searchCollection(
	ctx context.Context,
	className string,
	fields []graphql.Field,
	query string,
	limit int,
	where *filters.WhereBuilder,
	alpha *float64,
) (interface{}, error) {
	hybrid := h.client.GraphQL().HybridArgumentBuilder().WithQuery(query)
	if alpha != nil {
		hybrid = hybrid.WithAlpha(float32(*alpha))
	}
	builder := h.client.GraphQL().Get().
		WithClassName(className).
		WithFields(fields...).
		WithHybrid(hybrid).
		WithLimit(limit)

	if where != nil {
		builder = builder.WithWhere(where)
	}

	return builder.Do(ctx)
}

// fetchObjects runs a plain Get (no hybrid / vector search) with an optional
// WHERE filter — for exact-key lookups and full-collection listings where
// relevance ranking is meaningless. Mirrors searchCollection minus WithHybrid,
// so an empty query never silently degrades into a "match everything" search.
func (h *WeaviateHandler) fetchObjects(
	ctx context.Context,
	className string,
	fields []graphql.Field,
	limit int,
	where *filters.WhereBuilder,
) (interface{}, error) {
	builder := h.client.GraphQL().Get().
		WithClassName(className).
		WithFields(fields...).
		WithLimit(limit)

	if where != nil {
		builder = builder.WithWhere(where)
	}

	result, err := builder.Do(ctx)
	if err != nil {
		return nil, err
	}
	// A GraphQL-level failure (e.g. an unknown field) returns a nil transport
	// error but a populated Errors list and no data. Surface it as an error so
	// callers don't silently treat a failed query as "no results" — the same
	// check distinctValues makes on its own Get.
	if result != nil && len(result.Errors) > 0 {
		return nil, fmt.Errorf("%s", result.Errors[0].Message)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// ask_knowledge — LLM-callable knowledge base query
// ---------------------------------------------------------------------------

func (h *WeaviateHandler) askKnowledge(req plugin.Request) plugin.Response {
	// Exact-slug fetch takes precedence: deterministic single-article lookup
	// (no hybrid ranking) for a slug the model copied from the always-on catalog.
	if slug := req.Args["slug"]; slug != "" {
		return h.fetchKnowledgeBySlug(req, slug)
	}

	query, ok := req.Args["query"]
	if !ok || query == "" {
		return plugin.Response{CallID: req.ID, Error: "query or slug is required"}
	}

	ctx := context.Background()

	limit := 3
	if v, ok := req.Args["limit"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
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

	knowledgeFields := []graphql.Field{
		{Name: "title"}, {Name: "content"}, {Name: "source"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "score"}}},
	}
	knowledgeResult, err := h.searchCollection(ctx, h.knowledgeCollection, knowledgeFields, query, limit, knowledgeWhere, nil)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("knowledge search failed: %v", err)}
	}

	text := formatKnowledgeResults(knowledgeResult, h.knowledgeCollection)
	if text == "" {
		return plugin.Response{CallID: req.ID, Content: "No relevant results found."}
	}
	return plugin.Response{CallID: req.ID, Content: text}
}

// fetchKnowledgeBySlug returns the single knowledge article whose slug matches
// exactly. Deterministic — a WHERE Equal on the field-tokenized slug, no hybrid
// ranking — so a model that knows the slug (from list_knowledge_titles) gets
// that article's body with full precision. On no match it says so explicitly
// rather than silently degrading to a fuzzy search, so the model can tell a
// wrong slug apart from genuinely-absent knowledge. The limit-1 fetch relies on
// a slug invariant: the sync path prefixes every article id with its source
// plugin name (e.g. "<plugin>__categories"), so slugs are unique across servers
// even when several MCP servers share this collection. A future source that
// synced unprefixed slugs would need this filter scoped by source as well.
func (h *WeaviateHandler) fetchKnowledgeBySlug(req plugin.Request, slug string) plugin.Response {
	where := filters.Where().
		WithPath([]string{"slug"}).
		WithOperator(filters.Equal).
		WithValueText(slug)
	fields := []graphql.Field{
		{Name: "title"}, {Name: "slug"}, {Name: "content"}, {Name: "source"},
	}
	result, err := h.fetchObjects(context.Background(), h.knowledgeCollection, fields, 1, where)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("ask_knowledge slug lookup failed: %v", err)}
	}
	if text := formatKnowledgeResults(result, h.knowledgeCollection); text != "" {
		return plugin.Response{CallID: req.ID, Content: text}
	}
	return plugin.Response{
		CallID:  req.ID,
		Content: fmt.Sprintf("No knowledge article with slug %q. Call list_knowledge_titles for the available slugs, or pass query= for a semantic search.", slug),
	}
}

// knowledgeCatalogLimit bounds the title listing. The knowledge corpus is a
// curated per-product set (tens to low hundreds of articles), far under this
// ceiling; the cap only guards against an unbounded fetch if the corpus ever
// grows unexpectedly large.
const knowledgeCatalogLimit = 10000

// catalogEntry is one row of the knowledge-title catalog the orchestrator
// renders as an always-on system-prompt section.
type catalogEntry struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// listKnowledgeTitles returns every slug-bearing knowledge article as
// {slug,title} pairs, sorted by title for a stable, reproducible catalog. Only
// MCP-synced section articles carry a slug, so server-instruction blobs and
// manually-ingested articles (no slug) are excluded — exactly the set that is
// slug-addressable via ask_knowledge(slug=…).
func (h *WeaviateHandler) listKnowledgeTitles(req plugin.Request) plugin.Response {
	fields := []graphql.Field{{Name: "title"}, {Name: "slug"}}
	result, err := h.fetchObjects(context.Background(), h.knowledgeCollection, fields, knowledgeCatalogLimit, nil)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("list_knowledge_titles: %v", err)}
	}
	items := extractItems(result, h.knowledgeCollection)
	if len(items) >= knowledgeCatalogLimit {
		// At the ceiling the listing may be truncated, so the rendered catalog
		// would be incomplete and the model couldn't see the omitted articles.
		// Log it so the limit gets raised before that ever happens silently.
		log.Printf("weaviate-plugin: list_knowledge_titles hit the %d-article cap — catalog may be truncated", knowledgeCatalogLimit)
	}
	entries := make([]catalogEntry, 0, len(items))
	for _, item := range items {
		slug, _ := item["slug"].(string)
		title, _ := item["title"].(string)
		if slug == "" || title == "" {
			continue
		}
		entries = append(entries, catalogEntry{Slug: slug, Title: title})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Title < entries[j].Title })
	body, _ := json.Marshal(entries)
	return plugin.Response{CallID: req.ID, Content: string(body)}
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

	result, err := h.searchCollection(ctx, h.knowledgeCollection, fields, query, limit, where, nil)
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

// syncActionsPayload mirrors the JSON shape sent by opentalon's orchestrator:
// the plugin's tool schemas (actions, chunked across batches) plus its
// knowledge fields (server instructions + per-section knowledge articles).
// All fields except plugin_name are optional.
type syncActionsPayload struct {
	PluginName         string            `json:"plugin_name"`
	Actions            []syncActionEntry `json:"actions,omitempty"`
	ServerInstructions string            `json:"server_instructions,omitempty"`
	// KnowledgeArticles ships per-section knowledge contributed by the plugin
	// via the MCP `initialize._meta.knowledge_articles` field. Each entry is
	// stored as one KnowledgeArticles record with source
	// "mcp-knowledge:<plugin>:<id>" so ask_knowledge can pull just the relevant
	// sections. Optional; older orchestrators that omit the field keep the
	// legacy single-blob (server-instructions only) shape.
	KnowledgeArticles []knowledgeArticleEntry `json:"knowledge_articles,omitempty"`
	KeepPlugins       []string                `json:"keep_plugins,omitempty"`
	// IsContinuationBatch is set by the orchestrator on batches 1..N of a
	// chunked plugin sync so the plugin skips the per-plugin pre-delete that
	// would otherwise wipe the previous batches' inserts. Batch 0 (default
	// false) keeps the orphan-prune semantic. Older orchestrators omit the
	// field entirely — default false matches the legacy single-batch behaviour.
	IsContinuationBatch bool `json:"is_continuation_batch,omitempty"`
	// KeepActions is the authoritative full list of the plugin's current action
	// names. The action set is chunked across batches, so no single call sees
	// every action — the orchestrator therefore sends the complete list on
	// batch 0. When present, the pre-sync cleanup deletes only stored actions
	// NOT in this list (an in-place diff) instead of the legacy
	// delete-all-then-reinsert. A still-valid action that has not been
	// re-upserted yet (it arrives in a later batch) is matched by name and left
	// in place, so a reader on a peer pod never sees the plugin's tools briefly
	// vanish during a resync. Older orchestrators omit it — the plugin then
	// falls back to the blanket pre-delete (correct, but with the old window).
	KeepActions []string `json:"keep_actions,omitempty"`
}

// syncActionEntry is one tool schema in sync_actions's actions[]: the wire
// name, the full description, and the raw JSON parameter schema.
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

	h.syncJobCh <- syncJob{reqID: req.ID, payload: raw}

	h.syncMu.Lock()
	h.syncStatus.ActionsQueued++
	h.syncMu.Unlock()
	syncJobsEnqueued.WithLabelValues("sync_actions").Inc()
	syncQueueDepth.Inc()

	log.Printf("weaviate-plugin: sync_actions: queued for %s (req=%s)", payload.PluginName, req.ID)
	return plugin.Response{CallID: req.ID, Content: `{"queued":true,"action":"sync_actions"}`}
}

// syncActionsWork performs the actual corpus-sync Weaviate operations.
// Called by the background worker goroutine. It upserts the plugin's tool
// schemas into the MCPActions collection and its MCP server instructions +
// per-section knowledge articles into the KnowledgeArticles collection.
func (h *WeaviateHandler) syncActionsWork(reqID string, raw string) error {
	var payload syncActionsPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return fmt.Errorf("parse payload: %v", err)
	}

	ctx := context.Background()

	// Remove the plugin's removed/renamed actions before re-upserting so they
	// don't linger as stale entries. Two strategies, chosen by whether the
	// orchestrator sent the authoritative action list (keep_actions):
	//
	//   - Diff prune (keep_actions present): delete only the stored actions that
	//     are no longer in the current set. Actions that still exist — including
	//     ones that have not been re-upserted yet because they arrive in a later
	//     continuation batch — are matched by name and left untouched. There is
	//     no moment where the plugin's actions are absent, so a reader on a peer
	//     pod always sees a complete tool set during a resync.
	//
	//   - Blanket pre-delete (keep_actions absent — older orchestrator, or a
	//     plugin that dropped to zero actions but still ships instructions or
	//     knowledge this sync): delete everything for the plugin; the per-doc
	//     pass below then re-writes the current set from scratch (every doc reads
	//     as new). Legacy behaviour with a brief actions-absent window; kept for
	//     back-compat. NB: a plugin that drops to zero actions AND zero
	//     instructions AND zero knowledge sends no sync at all (the orchestrator
	//     short-circuits), so its now-stale action rows are cleaned only by the
	//     inter-plugin pruneOrphans when it also leaves keep_plugins — an
	//     orchestrator-side edge, not handled here.
	//
	// Both run only on batch 0 (IsContinuationBatch=false): batch 0 carries the
	// authoritative keep_actions, and the diff is order-independent, so later
	// batches only need to send their chunk.
	if !payload.IsContinuationBatch {
		if payload.KeepActions != nil {
			if err := h.pruneStaleActions(ctx, payload.PluginName, payload.KeepActions); err != nil {
				log.Printf("weaviate-plugin: sync_actions: prune stale actions for %s failed: %v", payload.PluginName, err)
			}
		} else {
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
	}

	// Build candidate docs (actions + server instructions + knowledge sections),
	// each tagged with a contentHash over its own canonical content. The per-doc
	// skip below re-vectorizes only the docs whose content actually changed.
	// Stale actions/sections are diff-pruned on batch 0; whole removed plugins
	// are pruned via pruneOrphans below.
	actionObjs := make([]*wmodels.Object, 0, len(payload.Actions))
	for _, a := range payload.Actions {
		params := ""
		if len(a.Parameters) > 0 {
			params = string(a.Parameters)
		}
		actionObjs = append(actionObjs, &wmodels.Object{
			Class: h.actionsCollection,
			ID:    strfmt.UUID(actionUUID(payload.PluginName, a.Name)),
			Properties: map[string]interface{}{
				"pluginName":  payload.PluginName,
				"actionName":  a.Name,
				"description": a.Description,
				"parameters":  params,
				"contentHash": contentSHA256(a.Name + "\x00" + a.Description + "\x00" + params),
			},
		})
	}

	knowledgeObjs := make([]*wmodels.Object, 0, len(payload.KnowledgeArticles)+1)

	// The plugin's server-level instructions text (e.g. an MCP server's
	// `initialize.instructions`) rides as one KnowledgeArticles record — handled
	// uniformly with every other doc (own contentHash, own skip). Keyed by
	// deterministic UUID so re-runs are idempotent.
	hasInstructions := false
	if payload.ServerInstructions != "" {
		hasInstructions = true
		title := payload.PluginName + " MCP server instructions"
		tags := []string{"opentalon", "mcp", "server-instructions", payload.PluginName}
		knowledgeObjs = append(knowledgeObjs, &wmodels.Object{
			Class: h.knowledgeCollection,
			ID:    strfmt.UUID(serverInstructionsUUID(payload.PluginName)),
			Properties: map[string]interface{}{
				"title":       title,
				"content":     payload.ServerInstructions,
				"source":      MCPSourcePrefix + payload.PluginName,
				"tags":        tags,
				"contentHash": contentSHA256(title + "\x00" + payload.ServerInstructions + "\x00" + strings.Join(tags, ",")),
			},
		})
	}

	// Remove knowledge sections the plugin dropped/renamed between syncs. The full
	// section set always arrives together on batch 0 (knowledge is not chunked),
	// so we diff against the authoritative slug set: delete only the stored
	// "mcp-knowledge:<plugin>:<slug>" records whose slug is no longer present.
	if !payload.IsContinuationBatch && len(payload.KnowledgeArticles) > 0 {
		keepSlugs := make([]string, 0, len(payload.KnowledgeArticles))
		for _, ka := range payload.KnowledgeArticles {
			keepSlugs = append(keepSlugs, ka.ID)
		}
		if err := h.pruneStaleKnowledgeSections(ctx, payload.PluginName, keepSlugs); err != nil {
			log.Printf("weaviate-plugin: sync_actions: prune stale knowledge for %s failed: %v", payload.PluginName, err)
		}
	}

	for _, ka := range payload.KnowledgeArticles {
		if ka.ID == "" || ka.Title == "" || ka.Content == "" {
			log.Printf("weaviate-plugin: sync_actions: skipping invalid knowledge article for %s (id=%q title=%q)", payload.PluginName, ka.ID, ka.Title)
			continue
		}
		tags := append([]string{"opentalon", "mcp", "knowledge", payload.PluginName}, ka.Tags...)
		knowledgeObjs = append(knowledgeObjs, &wmodels.Object{
			Class: h.knowledgeCollection,
			ID:    strfmt.UUID(knowledgeArticleUUID(payload.PluginName, ka.ID)),
			Properties: map[string]interface{}{
				"title":       ka.Title,
				"slug":        ka.ID,
				"content":     ka.Content,
				"source":      MCPKnowledgeSourcePrefix + payload.PluginName + ":" + ka.ID,
				"tags":        tags,
				"contentHash": contentSHA256(ka.Title + "\x00" + ka.Content + "\x00" + strings.Join(tags, ",")),
			},
		})
	}

	// Per-doc skip: re-vectorize only the docs whose contentHash differs from
	// what's stored. This single mechanism covers every case uniformly — new doc
	// (absent → written), changed doc (hash differs → rewritten), unchanged doc
	// (hash equal → skipped) — and makes an unchanged restart a no-op, because the
	// hashes live in Weaviate and survive a process restart. Stale docs (no longer
	// in the source) are removed by the prunes above (actions/sections) and below
	// (whole plugins).
	changedActions, skippedActions := h.filterUnchangedDocs(ctx, h.actionsCollection, actionObjs)
	changedKnowledge, skippedKnowledge := h.filterUnchangedDocs(ctx, h.knowledgeCollection, knowledgeObjs)

	objects := append(changedActions, changedKnowledge...)
	if len(objects) > 0 {
		results, err := h.client.Batch().ObjectsBatcher().WithObjects(objects...).Do(ctx)
		if err != nil {
			return fmt.Errorf("batch sync: %v", err)
		}
		if err := checkBatchErrors(results); err != nil {
			return fmt.Errorf("batch sync: %v", err)
		}
	}

	// Inter-plugin orphan prune (whole plugins removed since the last sync).
	pruned := 0
	if len(payload.KeepPlugins) > 0 {
		n, err := h.pruneOrphans(ctx, payload.KeepPlugins)
		if err != nil {
			log.Printf("weaviate-plugin: prune_orphans failed: %v", err)
		}
		pruned = n
	}

	log.Printf("weaviate-plugin: sync_actions: completed for %s (req=%s actions_written=%d/%d knowledge_written=%d/%d instructions=%v skipped=%d pruned=%d)",
		payload.PluginName, reqID, len(changedActions), len(actionObjs), len(changedKnowledge), len(knowledgeObjs),
		hasInstructions, skippedActions+skippedKnowledge, pruned)
	return nil
}

// filterUnchangedDocs returns the candidates whose contentHash differs from
// what's already stored (and the count skipped). Each candidate's deterministic
// UUID is both the read filter (id ContainsAny) and the join key, so the lookup
// works uniformly for any collection without a separate per-collection key. On a
// read error every candidate is treated as changed — safe (re-vectorize) rather
// than silently dropping an update.
func (h *WeaviateHandler) filterUnchangedDocs(ctx context.Context, className string, candidates []*wmodels.Object) (changed []*wmodels.Object, skipped int) {
	if len(candidates) == 0 {
		return nil, 0
	}
	ids := make([]string, len(candidates))
	for i, o := range candidates {
		ids[i] = string(o.ID)
	}
	stored := make(map[string]string, len(ids))
	where := filters.Where().WithPath([]string{"id"}).WithOperator(filters.ContainsAny).WithValueText(ids...)
	fields := []graphql.Field{{Name: "_additional", Fields: []graphql.Field{{Name: "id"}}}, {Name: "contentHash"}}
	if result, err := h.fetchObjects(ctx, className, fields, len(ids), where); err != nil {
		log.Printf("weaviate-plugin: sync_actions: read stored hashes for %s failed (treating all as changed): %v", className, err)
	} else {
		for _, it := range extractItems(result, className) {
			if id := additionalID(it); id != "" {
				ch, _ := it["contentHash"].(string)
				stored[id] = ch
			}
		}
	}
	return splitChangedDocs(candidates, stored)
}

// splitChangedDocs partitions candidates by whether their contentHash matches
// the stored hash for the same id: a candidate with no stored entry (new doc) or
// a differing hash (changed doc) goes to changed; an exact match is skipped. Kept
// pure (no Weaviate dependency) so the skip decision is unit-tested directly — an
// empty stored map is exactly filterUnchangedDocs's read-error fallback (treat
// every candidate as changed).
func splitChangedDocs(candidates []*wmodels.Object, stored map[string]string) (changed []*wmodels.Object, skipped int) {
	for _, o := range candidates {
		props, _ := o.Properties.(map[string]interface{})
		ch, _ := props["contentHash"].(string)
		if prev, ok := stored[string(o.ID)]; ok && prev == ch {
			skipped++
			continue
		}
		changed = append(changed, o)
	}
	return changed, skipped
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

	err := h.syncActionsWork(job.reqID, job.payload)
	h.syncMu.Lock()
	if err != nil {
		h.syncStatus.ActionsFailed++
	} else {
		h.syncStatus.ActionsCompleted++
		h.syncStatus.LastActionSync = time.Now()
	}
	h.syncMu.Unlock()

	duration := time.Since(start).Seconds()
	status := "ok"
	if err != nil {
		status = "error"
		log.Printf("weaviate-plugin: background sync job failed (sync_actions, req=%s): %v", job.reqID, err)
	}
	syncJobDuration.WithLabelValues("sync_actions", status).Observe(duration)
}

// getSyncStatus returns the current background sync worker state.
func (h *WeaviateHandler) getSyncStatus(req plugin.Request) plugin.Response {
	h.syncMu.RLock()
	s := h.syncStatus
	h.syncMu.RUnlock()

	pending := s.ActionsQueued - s.ActionsCompleted - s.ActionsFailed

	body, _ := json.Marshal(map[string]interface{}{
		"actions_queued":    s.ActionsQueued,
		"actions_completed": s.ActionsCompleted,
		"actions_failed":    s.ActionsFailed,
		"pending":           pending,
		"worker_running":    s.WorkerRunning,
		"last_action_sync":  s.LastActionSync,
	})
	return plugin.Response{CallID: req.ID, Content: string(body)}
}

// pruneOrphans deletes any MCPActions and any auto-generated KnowledgeArticles
// (those with source matching MCPSourcePrefix or MCPKnowledgeSourcePrefix)
// whose plugin is NOT in keep.
//
// Implementation note: instead of a single batch-delete with a NotEqual filter
// (Weaviate's NotEqual on tokenized text fields produces unreliable results
// for our purposes), we discover the set of distinct source values currently
// indexed and issue one Equal-based batch-delete per orphan. Equal on tokenized
// text matches the full property value reliably.
//
// Returns the total deletion count (best-effort: per-scope errors are recorded
// but do not abort subsequent deletes).
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

// actionUUID returns a deterministic UUID v5 for one plugin action, so a
// re-synced action upserts its existing MCPActions record in place instead of
// accumulating duplicates.
func actionUUID(pluginName, actionName string) string {
	return uuid.NewSHA1(actionNS, []byte(pluginName+"/"+actionName)).String()
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

// pruneStaleActions deletes the plugin's stored actions whose name is no
// longer in keep — the in-place diff behind the keep_actions contract (see
// syncActionsPayload.KeepActions).
func (h *WeaviateHandler) pruneStaleActions(ctx context.Context, pluginName string, keep []string) error {
	pluginScope := filters.Where().
		WithPath([]string{"pluginName"}).
		WithOperator(filters.Equal).
		WithValueText(pluginName)
	existing, err := h.distinctValues(ctx, h.actionsCollection, "actionName", pluginScope)
	if err != nil {
		return fmt.Errorf("list existing actions: %w", err)
	}
	stale := diffNotIn(existing, keep)
	// Best-effort, matching pruneOrphans: keep deleting the rest even if one
	// delete fails, so a single un-deletable record can't permanently block the
	// cleanup of every record after it. Surface the first error to the caller.
	pruned := 0
	var firstErr error
	for _, name := range stale {
		if err := h.client.Data().Deleter().
			WithClassName(h.actionsCollection).
			WithID(actionUUID(pluginName, name)).
			Do(ctx); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("delete stale action %q: %w", name, err)
			}
			continue
		}
		pruned++
	}
	if pruned > 0 {
		log.Printf("weaviate-plugin: sync_actions: pruned %d stale action(s) for %s", pruned, pluginName)
	}
	return firstErr
}

// diffNotIn returns the elements of a that are not in b.
func diffNotIn(a, b []string) []string {
	keep := make(map[string]struct{}, len(b))
	for _, v := range b {
		keep[v] = struct{}{}
	}
	var out []string
	for _, v := range a {
		if _, ok := keep[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}

// pruneStaleKnowledgeSections deletes the plugin's knowledge sections whose slug
// is no longer in keepSlugs. Sources are "mcp-knowledge:<plugin>:<slug>". Like
// pruneOrphans it gates on the exact-string prefix: Weaviate's Like operator
// matches per token, so "mcp-knowledge:foo:*" can over-match (e.g. a "foobar"
// plugin); the HasPrefix check on the full "<plugin>:" prefix filters those out.
func (h *WeaviateHandler) pruneStaleKnowledgeSections(ctx context.Context, pluginName string, keepSlugs []string) error {
	prefix := MCPKnowledgeSourcePrefix + pluginName + ":"
	scope := filters.Where().
		WithPath([]string{"source"}).
		WithOperator(filters.Like).
		WithValueText(prefix + "*")
	sources, err := h.distinctValues(ctx, h.knowledgeCollection, "source", scope)
	if err != nil {
		return fmt.Errorf("list existing knowledge sections: %w", err)
	}
	keep := make(map[string]struct{}, len(keepSlugs))
	for _, slug := range keepSlugs {
		keep[prefix+slug] = struct{}{}
	}
	// Best-effort, matching pruneOrphans: a single failed delete must not skip
	// the remaining stale sections. Surface the first error to the caller.
	pruned := 0
	var firstErr error
	for _, src := range sources {
		if !strings.HasPrefix(src, prefix) {
			continue // Like over-match guard
		}
		if _, ok := keep[src]; ok {
			continue
		}
		n, err := h.batchDeleteEqual(ctx, h.knowledgeCollection, "source", src)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("delete stale knowledge %q: %w", src, err)
			}
			continue
		}
		pruned += n
	}
	if pruned > 0 {
		log.Printf("weaviate-plugin: sync_actions: pruned %d stale knowledge section(s) for %s", pruned, pluginName)
	}
	return firstErr
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
