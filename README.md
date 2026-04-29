# weaviate-plugin

[![CI](https://github.com/opentalon/weaviate-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/weaviate-plugin/actions/workflows/ci.yml)

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that performs semantic and hybrid search over a [Weaviate](https://github.com/weaviate/weaviate) vector database collection. Use it as a retrieval step (RAG) to fetch relevant context before the LLM generates a response, manage MCP action indexing, and ingest knowledge articles.

## Actions

| Action | Mode | Description |
|---|---|---|
| `weaviate.search` | LLM tool | nearText semantic search — the LLM calls this when it decides retrieval is needed |
| `weaviate.hybrid_search` | LLM tool | Hybrid BM25 + vector. `alpha`: `0` = keyword only, `1` = vector only (default `0.5`) |
| `weaviate.prepare` | **preparer** | Automatic RAG: runs before every LLM call, enriches the user message with retrieved context |
| `weaviate.sync_actions` | orchestrator | Upserts plugin action definitions into the `MCPActions` collection for retrieval-based tool filtering. Supports hash-based skip |
| `weaviate.sync_glossary` | orchestrator | Syncs glossary term/definition pairs into the `Glossary` collection for automatic context injection. Supports hash-based skip and continuation batches |
| `weaviate.ingest` | LLM tool / API | Insert a single knowledge article into the `KnowledgeArticles` collection |
| `weaviate.ingest_batch` | LLM tool / API | Batch insert multiple knowledge articles |

`search` and `hybrid_search` accept `limit` and `fields` per-call overrides.

### Tool mode vs. preparer mode

**Tool mode** (default) — the LLM decides when to retrieve:
```
user -> LLM decides to call weaviate.search -> results -> LLM -> answer
```

**Preparer mode** — retrieval is automatic, invisible to the LLM as a tool choice:
```
user message -> weaviate.prepare(text=message) -> [retrieved_context]...message -> LLM -> answer
```

Use preparer mode when you always want retrieved context in the prompt (RAG-by-default). Use tool mode when retrieval should be conditional.

**Glossary injection** — when glossary entries have been synced via `sync_glossary`, `prepare` also searches the Glossary collection and injects matching term definitions:

```
[glossary_context]
- **SLA**: Service Level Agreement — contractual response time for support tickets
- **P1 Incident**: Production-critical outage affecting >50% of users, 15min response required
[/glossary_context]

[knowledge_context]
...matching knowledge articles...
[/knowledge_context]

What's the SLA for P1 incidents?
```

## Configuration

Add to your OpenTalon `config.yaml`:

```yaml
plugins:
  - name: weaviate
    plugin: /usr/local/bin/weaviate-plugin   # path to the compiled binary
    enabled: true
    config:
      host: "localhost:8080"       # Weaviate address
      scheme: "http"               # "http" or "https"
      collection: "Article"        # Weaviate class for search/hybrid_search/prepare
      fields:                      # fields to return in search results
        - title
        - body
        - url
      limit: 5                     # default result count

      # Vectorizer (default: "text2vec-transformers")
      vectorizer: "text2vec-transformers"
      # module_config: {}          # optional per-module config (see Vectorizer Options below)

      # Knowledge-augmented RAG (Phase 1)
      actions_collection: "MCPActions"           # collection for indexed plugin capabilities (default)
      knowledge_collection: "KnowledgeArticles"  # collection for knowledge articles (default)
      glossary_collection: "Glossary"            # collection for glossary term/definition pairs (default)
      auto_create_schema: true                   # auto-create MCPActions, KnowledgeArticles & Glossary on startup (default true)

      # Client timeout
      timeout: "2m"                # Weaviate HTTP client timeout (default 2m); increase for large batch syncs

      # Prepare-phase filtering
      min_prepare_score: 0.012     # minimum hybrid-search score for prepare results (default 0.012)

      # HTTP ingestion server (optional)
      http_addr: ":8081"           # address for the HTTP ingestion API
      token: "my-secret-token"     # Bearer token — required when http_addr is set

      # Optional cross-lingual query pre-processor (off by default)
      translator:
        enabled: false                                              # opt-in
        url: "http://libretranslate.libretranslate.svc:5000"        # LibreTranslate-compatible endpoint
        target_lang: "en"                                           # default "en"
        source_lang: "auto"                                         # default "auto" (let translator detect)
        timeout: "3s"                                               # per-request timeout (default 3s)
        skip_if_target_confidence: 0.7                              # /detect first; skip /translate when source==target with at least this confidence (default 0.7, 0 disables)
        api_key: ""                                                 # only if the translator requires LT_API_KEYS
```

### Translator observability

When `http_addr` is configured, the plugin exposes:

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /metrics` | open (network-restricted) | Prometheus exposition with translator + plugin metrics |
| `POST /api/v1/debug/prepare` | bearer token | Run a query through the full prepare pipeline and return a structured trace |

Translator metrics:

* `weaviate_plugin_translator_calls_total{callsite,result}` — `result` is `translated` / `skipped_target_lang` / `skipped_disabled` / `failed`. The `callsite` label tells you whether a translate happened in `prepare`, `search`, `hybrid_search`, `ask_knowledge`, `search_instructions`, or `debug_prepare`.
* `weaviate_plugin_translator_duration_seconds{callsite,result}` — wall-time of the full translator step (detect + translate, or just detect for the skip case). 1ms..2s buckets.
* `weaviate_plugin_translator_detected_lang_total{lang}` — source-language distribution from the `/detect` short-circuit.

Debug trace (sample):

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"text":"Wieviele Lagerartikel habe ich?"}' \
     http://weaviate-plugin:8082/api/v1/debug/prepare
```

```json
{
  "original": "Wieviele Lagerartikel habe ich?",
  "search_text": "How many storage items do I have?",
  "translated": true,
  "translator_ms": 84.2,
  "min_prepare_score": 0.45,
  "matched_tools": ["timly.list-items", "weaviate.ask_knowledge"],
  "actions_top": [
    {"score": 0.81, "plugin_name": "timly", "action_name": "list-items"},
    {"score": 0.42, "plugin_name": "timly", "action_name": "list-locations"}
  ],
  "knowledge_top": [
    {"score": 0.31, "title": "timly MCP server instructions", "source": "mcp:timly"}
  ],
  "weaviate_ms": 38.1,
  "knowledge_query": "How many storage items do I have?"
}
```

The debug endpoint runs through the same code path as `prepare` (translator,
hybrid search, score-threshold filter), so it's a faithful reproduction of
what the orchestrator sees — minus the LLM-bound message envelope.

### Translator (cross-lingual query pre-processor)

When the corpus is indexed in one language (typically English) but users ask
questions in another (DE / FR / ES / …), BM25 contributes ~0 to the hybrid
score because the foreign-language tokens never appear in the corpus. With
`alpha=0.5` the resulting score is roughly halved, often dropping good hits
below `min_prepare_score`.

The translator addresses this end-to-end without re-indexing: every query
that reaches `prepare`, `search`, `hybrid_search`, `ask_knowledge`, or
`search_instructions` is translated to `target_lang` (default `en`) before
the lookup runs. The original-language `text` is still echoed back into the
`prepare` response message so the LLM sees the user's query in their own
language and replies in kind.

Behaviour at a glance:

* `enabled: false` (default) → no-op, zero overhead.
* `enabled: true` with no `url` → silently downgrades to no-op (logged).
* Detect-first short-circuit: if `/detect` says the input is already in
  `target_lang` with ≥ `skip_if_target_confidence`, the `/translate` call is
  skipped (~30 ms `/detect` only).
* Any error (network, non-2xx, empty response) → fail-open: the original
  text is returned and the search runs untranslated.


The `collection` field is required. All others have defaults (`host: localhost:8080`, `scheme: http`, `limit: 5`, `vectorizer: text2vec-transformers`).

### `min_prepare_score`

During the `prepare` phase the plugin runs a hybrid search against the actions and knowledge collections. Each result comes back with a relevance score. Results scoring below `min_prepare_score` are discarded.

| Setting | Value |
|---|---|
| Config key | `min_prepare_score` |
| Type | float |
| Default | `0.012` |

- **Higher values** (e.g. `0.30`) return only high-confidence matches, reducing noise but potentially dropping relevant results.
- **Lower values** surface more results at the cost of relevance.
- **`0` or omitted** falls back to the default (`0.012`).

Tuning tips:
- Start with the default and check plugin logs (`min_score=... matched_tools=...`) to see what scores your data produces.
- If the plugin returns too many irrelevant tools, raise the threshold incrementally.
- If relevant tools are being filtered out, lower it.

### Vectorizer options

The `vectorizer` field tells Weaviate which module to use for generating embeddings. The plugin sets this per-collection when `auto_create_schema` creates them. Three options are supported:

#### 1. In-cluster transformers (default) — multilingual, strong German

```yaml
config:
  vectorizer: "text2vec-transformers"
```

Weaviate calls a transformer inference service running in-cluster to generate embeddings. The language support depends on which model image is deployed (see [Kubernetes Deployment](#kubernetes-deployment-argocd) below).

Recommended models (all support 50+ languages including German):

| Helm `tag` / Docker image tag | Dims | Memory | Notes |
|---|---|---|---|
| `sentence-transformers-paraphrase-multilingual-MiniLM-L12-v2` | 384 | ~1.5Gi | **Default.** Fast, CPU-friendly, good German |
| `sentence-transformers-distiluse-base-multilingual-cased-v2` | 512 | ~2Gi | Slightly better quality |
| `sentence-transformers-paraphrase-multilingual-mpnet-base-v2` | 768 | ~3Gi | Best quality, heaviest |

All images available at `cr.weaviate.io/semitechnologies/transformers-inference:<tag>`.

#### 2. OVH AI Endpoints — best German quality, hosted EU

```yaml
config:
  vectorizer: "text2vec-openai"
  module_config:
    baseURL: "https://bge-m3-xxx.endpoints.kepler.ai.cloud.ovh.net"
    model: "bge-m3"
```

Uses OVH AI Endpoints (OpenAI-API-compatible) to generate embeddings externally. Requires:
- `text2vec-openai` enabled in Weaviate modules
- `OPENAI_APIKEY` environment variable set on the Weaviate pod with the OVH API token (see [Using OVH AI Endpoints](#using-ovh-ai-endpoints-instead) for k8s Secret setup)

OVH datacenters are in France/Germany (EU data residency).

#### 3. Legacy contextionary — English only

```yaml
config:
  vectorizer: "text2vec-contextionary"
```

Weaviate's legacy English-only word-embedding vectorizer. Not recommended for German or multilingual content.

### Auto-schema creation

When `auto_create_schema` is `true` (the default), the plugin creates three Weaviate collections on startup if they don't already exist:

| Collection | Properties | Purpose |
|---|---|---|
| `MCPActions` | `pluginName`, `actionName`, `description`, `parameters` | Indexed plugin capabilities for retrieval-based tool filtering |
| `KnowledgeArticles` | `title`, `content`, `source`, `tags` | Domain knowledge and how-to guides |
| `Glossary` | `term`, `definition`, `category`, `tags`, `synonyms` | Domain terminology definitions, auto-injected via prepare |

Collections are created with the configured `vectorizer` and `module_config`. Set `auto_create_schema: false` to manage schemas manually.

## HTTP Ingestion API

When `http_addr` is configured, the plugin starts a token-protected HTTP server for external article and action ingestion. A `token` must be set — the plugin refuses to start without one.

All endpoints require the header `Authorization: Bearer <token>`.

### POST /api/v1/articles

Ingest a single knowledge article.

```bash
curl -X POST http://localhost:8081/api/v1/articles \
  -H "Authorization: Bearer my-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"title":"Deployment guide","content":"Steps to deploy...","source":"wiki","tags":["ops","deploy"]}'
```

Response: `{"ingested":1}`

### POST /api/v1/articles/batch

Batch ingest multiple articles.

```bash
curl -X POST http://localhost:8081/api/v1/articles/batch \
  -H "Authorization: Bearer my-secret-token" \
  -H "Content-Type: application/json" \
  -d '[
    {"title":"Article 1","content":"Content 1.","tags":["guide"]},
    {"title":"Article 2","content":"Content 2.","source":"docs"}
  ]'
```

Response: `{"ingested":2}`

### POST /api/v1/actions/sync

Sync plugin action definitions into the MCPActions collection. Uses deterministic UUIDs so repeated syncs upsert rather than duplicate.

```bash
curl -X POST http://localhost:8081/api/v1/actions/sync \
  -H "Authorization: Bearer my-secret-token" \
  -H "Content-Type: application/json" \
  -d '{
    "plugin_name": "jira",
    "actions": [
      {"name": "create_issue", "description": "Create a Jira issue", "parameters": [{"name":"project","type":"string"}]},
      {"name": "list_issues", "description": "List open issues"}
    ]
  }'
```

Response: `{"synced":2}`

### POST /api/v1/glossary/sync

Sync glossary term/definition pairs into the Glossary collection. Uses deterministic UUIDs (keyed on term) and supports hash-based skip + continuation batches.

```bash
curl -X POST http://localhost:8081/api/v1/glossary/sync \
  -H "Authorization: Bearer my-secret-token" \
  -H "Content-Type: application/json" \
  -d '{
    "glossary_hash": "sha256:9f86d0...",
    "entries": [
      {"term": "SLA", "definition": "Service Level Agreement — contractual response time", "category": "support", "tags": ["contracts"], "synonyms": ["Service Level Agreement"]},
      {"term": "P1 Incident", "definition": "Production-critical outage affecting >50% of users", "category": "incidents", "synonyms": ["Priority 1", "Sev1"]}
    ],
    "is_continuation_batch": false
  }'
```

Response: `{"synced":2}`

If the hash matches the previous sync: `{"skipped":true,"reason":"hash_match"}`

For large glossaries, split into batches. Batch 0 (`is_continuation_batch: false`) deletes old entries and inserts. Batches 1..N (`is_continuation_batch: true`) only insert.

### Hash-based sync skip

Both `sync_actions` and `sync_glossary` support hash-based skip to avoid re-writing unchanged data to Weaviate.

**sync_actions**: add a `hash` field to the payload. The plugin stores the last-seen hash per plugin. On batch 0, if the hash matches, the upsert is skipped (orphan prune via `keep_plugins` still runs).

```json
{"plugin_name": "jira", "hash": "sha256:abc...", "actions": [...], "keep_plugins": ["jira"]}
```

**sync_glossary**: the `glossary_hash` field works the same way. On batch 0, if the hash matches, the entire sync is skipped. Continuation batches always upsert.

The hash is opaque to the plugin — the orchestrator computes it over the source data (e.g. SHA-256) and the plugin simply compares strings.

## Install

```bash
git clone https://github.com/opentalon/weaviate-plugin
cd weaviate-plugin
make install          # builds and copies to /usr/local/bin/weaviate-plugin
```

Or install a specific prefix:

```bash
make install PREFIX=~/.local/bin
```

## Quick start

### Option A — brew (macOS, keyword tests only)

`brew install weaviate` gives you the binary without a vectorizer module, so `search` (nearText) is unavailable but `hybrid_search` with `alpha=0` works fine.

```bash
brew install weaviate

# Start Weaviate with anonymous access and no vectorizer
AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true \
DEFAULT_VECTORIZER_MODULE=none \
PERSISTENCE_DATA_PATH="$TMPDIR/weaviate" \
weaviate --host 0.0.0.0 --port 8080 --scheme http &
```

Run the keyword-only integration tests:

```bash
make test-brew
# nearText tests are automatically skipped (WEAVIATE_MODULE defaults to "none")
```

### Option B — Docker Compose (full suite, includes semantic search)

Starts Weaviate + the multilingual transformer inference service so nearText works:

```bash
make test-docker
```

This uses `sentence-transformers-paraphrase-multilingual-MiniLM-L12-v2` (384 dims, 50+ languages including German).

### Environment variables for tests

| Variable | Default | Description |
|---|---|---|
| `WEAVIATE_HOST` | `localhost:8080` | Address of the running Weaviate instance |
| `WEAVIATE_MODULE` | `none` | Set to `text2vec-transformers` to enable nearText tests |

## Build

```bash
make build           # current platform
make build-linux     # cross-compile Linux amd64 (for k8s)
```

### Local development against a cloned opentalon repo

```bash
go mod edit -replace github.com/opentalon/opentalon=../opentalon
go mod tidy
```

Revert before committing:

```bash
go mod edit -dropreplace github.com/opentalon/opentalon
go mod tidy
```

## CI

GitHub Actions runs three jobs on every push/PR:

| Job | What it does |
|---|---|
| **lint** | `golangci-lint` |
| **unit** | `go test ./...` (no Weaviate required) |
| **integration** | Real Weaviate 1.30 + multilingual transformers via Docker service; runs full test suite including nearText |

The integration job uses Docker service containers declared in the workflow — no manual setup needed.

## Test suite overview

| Test | Needs vectorizer | What it checks |
|---|---|---|
| `TestCapabilities` | no | Plugin declares correct name and all 6 actions |
| `TestConfigure_defaults` | no | Default limit=5, default collection names applied |
| `TestConfigure_missingCollection` | no | Error returned when collection omitted |
| `TestConfigure_badJSON` | no | Error returned for malformed config |
| `TestConfigure_httpRequiresToken` | no | Error when http_addr set without token |
| `TestConfigure_customCollectionNames` | no | Custom actions/knowledge collection names are applied |
| `TestConfigure_autoCreateSchema` | no | MCPActions and KnowledgeArticles created on startup |
| `TestConfigure_autoCreateSchemaIdempotent` | no | Repeated Configure with auto_create_schema does not fail |
| `TestExecute_unknownAction` | no | Error returned for unrecognised action |
| `TestHybridSearch_keywordOnly` | no | BM25 (alpha=0) returns the Python article for "python" |
| `TestHybridSearch_limitOverride` | no | `limit=1` returns exactly one result |
| `TestHybridSearch_fieldsOverride` | no | Requesting only `title` omits `body` from response |
| `TestSearch_semantic` | **yes** | nearText "vector database" returns the Weaviate article |
| `TestSearch_missingQuery` | no | Error returned when query arg is absent |
| `TestHybridSearch_missingQuery` | no | Error returned when query arg is absent |
| `TestSyncActions` | no | Sync 2 actions, verify synced count |
| `TestSyncActions_upsert` | no | Re-sync with updated description succeeds |
| `TestSyncActions_missingPayload` | no | Error when payload arg is absent |
| `TestSyncActions_missingPluginName` | no | Error when plugin_name is missing from payload |
| `TestSyncActions_emptyActions` | no | Empty actions array returns synced:0 |
| `TestIngest` | no | Ingest single article with all fields |
| `TestIngest_missingFields` | no | Error when title or content is missing |
| `TestIngest_noOptionalFields` | no | Ingest with only title and content succeeds |
| `TestIngestBatch` | no | Batch ingest 3 articles |
| `TestIngestBatch_missingPayload` | no | Error when payload arg is absent |
| `TestIngestBatch_skipsInvalid` | no | Invalid articles (missing title/content) are skipped |
| `TestIngestBatch_badJSON` | no | Error for malformed JSON payload |
| `TestHTTP_ingestArticle` | no | HTTP POST article with valid token succeeds |
| `TestHTTP_ingestArticleUnauthorized` | no | HTTP POST without token returns 401 |
| `TestHTTP_ingestArticleWrongToken` | no | HTTP POST with wrong token returns 401 |
| `TestHTTP_syncActions` | no | HTTP POST sync_actions with valid token succeeds |
| `TestHTTP_ingestBatch` | no | HTTP POST batch with valid token succeeds |
| `TestHTTP_badJSON` | no | HTTP POST with invalid JSON returns 400 |
| `TestHTTP_noTokenConfigured` | no | Requests pass through when no token is configured |

## Wiring into OpenTalon

### Tool mode (LLM-callable)

The LLM sees `search` and `hybrid_search` as available tools and calls them when it decides retrieval is useful:

```yaml
plugins:
  - name: weaviate
    plugin: /usr/local/bin/weaviate-plugin
    enabled: true
    config:
      host: "localhost:8080"
      scheme: "http"
      collection: "Article"
      fields: [title, body]
      limit: 5
```

### Preparer mode (automatic RAG — retrieval before every LLM call)

Register `weaviate.prepare` as a `content_preparer`. The orchestrator calls it with the raw user message as `text` before the first LLM call, and replaces the message with the enriched version (a `[retrieved_context]` block prepended):

```yaml
plugins:
  - name: weaviate
    plugin: /usr/local/bin/weaviate-plugin
    enabled: true
    config:
      host: "localhost:8080"
      scheme: "http"
      collection: "Article"
      fields: [title, body]
      limit: 5

content_preparers:
  - plugin: weaviate
    action: prepare
    # arg_key defaults to "text" — matches the prepare action parameter
    fail_open: true   # Weaviate outage passes the original message through instead of blocking
```

With `fail_open: true` and the plugin's own fail-open logic, a Weaviate outage never blocks the LLM call.

### Knowledge-augmented RAG mode

Enable the HTTP ingestion API and auto-schema creation to build a knowledge base alongside MCP action indexing:

```yaml
plugins:
  - name: weaviate
    plugin: /usr/local/bin/weaviate-plugin
    enabled: true
    config:
      host: "localhost:8080"
      scheme: "http"
      collection: "Article"
      fields: [title, body]
      limit: 5
      auto_create_schema: true
      http_addr: ":8081"
      token: "my-secret-token"
```

The orchestrator can call `sync_actions` at startup to index all plugin capabilities into the `MCPActions` collection. External systems can push knowledge articles via the HTTP API.

## Kubernetes Deployment (ArgoCD)

Deploy Weaviate to Kubernetes using the included ArgoCD Application manifest. The Weaviate Helm chart automatically deploys the transformer inference model as a sidecar — no separate manifest is needed.

### Architecture

```
Plugin ──► Weaviate pod ──► Transformer inference container (sidecar)
           (ArgoCD/Helm)    (auto-deployed by Helm chart)
```

The Helm chart:
1. Deploys Weaviate
2. Deploys the transformer model container alongside it
3. Wires `TRANSFORMERS_INFERENCE_API` internally
4. You just set `enabled: true` and a `tag` (model image) in the Helm values

### Prerequisites

- Kubernetes 1.23+
- ArgoCD installed in the `argocd` namespace
- PersistentVolume provisioner available in the cluster

### Deploy

```bash
kubectl apply -f deploy/weaviate/application.yaml
```

ArgoCD will automatically:
- Create the `weaviate` namespace
- Install Weaviate 1.30.0 via the official Helm chart (v17.8.0)
- Deploy the `paraphrase-multilingual-MiniLM-L12-v2` transformer model as a sidecar (multilingual, supports German)
- Provision a 32Gi persistent volume
- Auto-sync and self-heal

### Verify

```bash
# Check ArgoCD status
argocd app get weaviate

# Check pods — you should see both weaviate and transformers containers
kubectl -n weaviate get pods

# Check readiness
kubectl -n weaviate exec svc/weaviate -- wget -qO- http://localhost:8080/v1/.well-known/ready

# Verify the vectorizer is set correctly on a collection
kubectl -n weaviate exec svc/weaviate -- wget -qO- http://localhost:8080/v1/schema/MCPActions | python3 -m json.tool
```

### Connect the plugin

Once Weaviate is running in-cluster, point the plugin at the Kubernetes service:

```yaml
plugins:
  - name: weaviate
    plugin: /usr/local/bin/weaviate-plugin
    enabled: true
    config:
      host: "weaviate.weaviate.svc.cluster.local:8080"
      scheme: "http"
      collection: "Article"
      fields: [title, body]
      limit: 5
      vectorizer: "text2vec-transformers"
      auto_create_schema: true
```

### Choosing a different model

To change the transformer model, edit the `tag` field in `deploy/weaviate/application.yaml`:

```yaml
modules:
  text2vec-transformers:
    enabled: true
    tag: sentence-transformers-paraphrase-multilingual-mpnet-base-v2  # change this
```

Available multilingual models:

| Helm `tag` value | Dims | German | Memory |
|---|---|---|---|
| `sentence-transformers-paraphrase-multilingual-MiniLM-L12-v2` | 384 | Good | ~1.5Gi |
| `sentence-transformers-distiluse-base-multilingual-cased-v2` | 512 | Good | ~2Gi |
| `sentence-transformers-paraphrase-multilingual-mpnet-base-v2` | 768 | Best | ~3Gi |

After changing the model, delete existing collections (vector dimensions will differ) and let `auto_create_schema` recreate them. Re-ingest data via `sync_actions` and the HTTP API.

### Using OVH AI Endpoints instead

To use OVH-hosted embeddings (bge-m3, best German quality) instead of in-cluster transformers:

1. Create a Kubernetes Secret with your OVH API token:

```bash
kubectl -n weaviate create secret generic ovh-api-key \
  --from-literal=OPENAI_APIKEY='your-ovh-api-token'
```

2. Update the Helm values in `application.yaml`:

```yaml
default_vectorizer_module: text2vec-openai
modules:
  text2vec-openai:
    enabled: true
  # Remove or disable text2vec-transformers
env:
  OPENAI_APIKEY:
    valueFrom:
      secretKeyRef:
        name: ovh-api-key
        key: OPENAI_APIKEY
```

3. Configure the plugin:

```yaml
config:
  vectorizer: "text2vec-openai"
  module_config:
    baseURL: "https://bge-m3-xxx.endpoints.kepler.ai.cloud.ovh.net"
    model: "bge-m3"
```

The `text2vec-openai` module reads the API key from the `OPENAI_APIKEY` environment variable on the Weaviate pod. OVH AI Endpoints are OpenAI-API-compatible, so no other changes are needed.

### Customization

Edit the inline `helm.values` block in `deploy/weaviate/application.yaml` to adjust:

| Setting | Default | Description |
|---|---|---|
| `image.tag` | `1.30.0` | Weaviate version |
| `replicas` | `1` | Number of nodes (increase for HA) |
| `storage.size` | `32Gi` | Persistent volume size |
| `resources.requests.cpu` | `1` | CPU request |
| `resources.requests.memory` | `2Gi` | Memory request |
| `modules.text2vec-transformers.tag` | `sentence-transformers-paraphrase-multilingual-MiniLM-L12-v2` | Transformer model |
| `authentication.anonymous_access.enabled` | `true` | Set `false` and configure API keys for production |

## References

- Weaviate Go client: https://github.com/weaviate/weaviate-go-client
- Weaviate server: https://github.com/weaviate/weaviate
- Weaviate Helm chart: https://github.com/weaviate/weaviate-helm
- Transformer models: https://github.com/weaviate/t2v-transformers-models
- Weaviate transformers docs: https://weaviate.io/developers/weaviate/model-providers/transformers/embeddings
- OpenTalon plugin SDK: https://github.com/opentalon/opentalon (see `pkg/plugin`)
- Knowledge-augmented RAG issue: https://github.com/opentalon/opentalon/issues/97
