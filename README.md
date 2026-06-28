# weaviate-plugin

[![CI](https://github.com/opentalon/weaviate-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/weaviate-plugin/actions/workflows/ci.yml)

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that serves a [Weaviate](https://github.com/weaviate/weaviate)-backed knowledge base. It lets the LLM pull knowledge articles on demand and ingests a plugin's MCP server instructions and knowledge articles into the knowledge collection.

## Actions

| Action | Mode | Description |
|---|---|---|
| `weaviate.ask_knowledge` | LLM tool | Search the knowledge base by `query`, or fetch one article exactly by `slug` |
| `weaviate.list_knowledge_titles` | LLM tool | List the slug + title of every knowledge article (the always-on catalog) |
| `weaviate.search_instructions` | LLM tool | Search synced MCP server-instruction articles |
| `weaviate.sync_actions` | orchestrator | Syncs a plugin's MCP server instructions + knowledge articles into the `KnowledgeArticles` collection. Per-doc hash skip + orphan prune |
| `weaviate.sync_status` | orchestrator | Background sync worker counters |
| `weaviate.ingest` | LLM tool / API | Insert a single knowledge article into the `KnowledgeArticles` collection |
| `weaviate.ingest_batch` | LLM tool / API | Batch insert multiple knowledge articles |
| `weaviate.refresh` | orchestrator | Re-create the `KnowledgeArticles` collection if deleted externally |

> Tool retrieval lives in the orchestrator's tool registry; this plugin serves
> knowledge only. There is no preparer / RAG pre-pass action and no generic
> search over an arbitrary collection.

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

      # Vectorizer (default: "text2vec-transformers")
      vectorizer: "text2vec-transformers"
      # module_config: {}          # optional per-module config (see Vectorizer Options below)

      # Knowledge base
      knowledge_collection: "KnowledgeArticles"  # collection for knowledge articles (default)
      auto_create_schema: true                   # auto-create KnowledgeArticles on startup (default true)

      # Client timeout
      timeout: "2m"                # Weaviate HTTP client timeout (default 2m); increase for large batch syncs

      # HTTP ingestion server (optional)
      http_addr: ":8081"           # address for the HTTP ingestion API
      token: "my-secret-token"     # Bearer token — required when http_addr is set
```

All config fields have defaults (`host: localhost:8080`, `scheme: http`, `knowledge_collection: KnowledgeArticles`, `vectorizer: text2vec-transformers`).

### Observability

When `http_addr` is configured, the plugin exposes `GET /metrics` (open, network-restricted) with Prometheus metrics for the background sync worker:

* `weaviate_plugin_sync_jobs_enqueued_total{type}` — sync jobs enqueued.
* `weaviate_plugin_sync_job_duration_seconds{type,status}` — wall-time per sync job.
* `weaviate_plugin_sync_queue_depth` — pending sync jobs in the queue.

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

When `auto_create_schema` is `true` (the default), the plugin creates the knowledge collection on startup if it doesn't already exist:

| Collection | Properties | Purpose |
|---|---|---|
| `KnowledgeArticles` | `title`, `slug`, `content`, `source`, `tags`, `contentHash` | Domain knowledge, how-to guides, and synced MCP server instructions |

The collection is created with the configured `vectorizer` and `module_config`. Set `auto_create_schema: false` to manage the schema manually.

## HTTP Ingestion API

When `http_addr` is configured, the plugin starts a token-protected HTTP server for external article and knowledge-sync ingestion. A `token` must be set — the plugin refuses to start without one.

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

Sync a plugin's MCP server instructions and knowledge articles into the KnowledgeArticles collection. Uses deterministic UUIDs so repeated syncs upsert rather than duplicate.

```bash
curl -X POST http://localhost:8081/api/v1/actions/sync \
  -H "Authorization: Bearer my-secret-token" \
  -H "Content-Type: application/json" \
  -d '{
    "plugin_name": "jira",
    "server_instructions": "Use create_issue to open a ticket; statuses follow Open → In Progress → Done.",
    "knowledge_articles": [
      {"id": "workflow", "title": "Issue workflow", "content": "Issues move Open → In Progress → Review → Done."}
    ],
    "keep_plugins": ["jira"]
  }'
```

Response: `{"queued":true,"action":"sync_actions"}` (the actual write runs on the background worker; poll `GET /api/v1/sync/status`).

The server-instructions article is stored with source `mcp:<plugin>`; each `knowledge_articles[]` entry is stored with source `mcp-knowledge:<plugin>:<id>` and slug `<id>`. On batch 0, knowledge sections dropped since the last sync are pruned, and whole plugins absent from `keep_plugins` are pruned. Each document carries a `contentHash`, so an unchanged sync re-vectorizes nothing.

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

### Option A — brew (macOS, no vectorizer)

`brew install weaviate` gives you the binary without a vectorizer module. The
knowledge retrieval is hybrid (BM25 + vector), so without a vectorizer the
keyword half still works; semantic relevance is degraded but the suite runs.

```bash
brew install weaviate

# Start Weaviate with anonymous access and no vectorizer
AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true \
DEFAULT_VECTORIZER_MODULE=none \
PERSISTENCE_DATA_PATH="$TMPDIR/weaviate" \
weaviate --host 0.0.0.0 --port 8080 --scheme http &
```

Run the integration tests:

```bash
make test-brew
```

### Option B — Docker Compose (full suite, includes semantic vectors)

Starts Weaviate + the multilingual transformer inference service so the vector
half of knowledge retrieval works:

```bash
make test-docker
```

This uses `sentence-transformers-paraphrase-multilingual-MiniLM-L12-v2` (384 dims, 50+ languages including German).

### Environment variables for tests

| Variable | Default | Description |
|---|---|---|
| `WEAVIATE_HOST` | `localhost:8080` | Address of the running Weaviate instance |
| `WEAVIATE_MODULE` | `none` | Set to `text2vec-transformers` to exercise the vector half of knowledge retrieval |

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
| **integration** | Real Weaviate 1.30 + multilingual transformers via Docker service; runs full test suite including vector retrieval |

The integration job uses Docker service containers declared in the workflow — no manual setup needed.

## Test suite overview

Unit tests (no Weaviate, run by `go test ./...`) cover Capabilities classification, config/timeout parsing, the per-doc change-skip decision, and the content-hash helper.

Integration tests (build tag `integration`, need a live Weaviate) cover:

| Area | What it checks |
|---|---|
| Capabilities | Plugin declares the correct name and the knowledge-only action set |
| Configure | Defaults, custom knowledge collection name, bad-JSON error, `http_addr` requires token, auto-schema creation + idempotency |
| `ask_knowledge` | Semantic search, exact-slug fetch, source filter, limit override, no-results, missing-query error |
| `list_knowledge_titles` | Returns the slug+title catalog |
| `sync_actions` (knowledge sync) | Server-instructions + knowledge-article ingestion, stale-section pruning, orphan-plugin pruning, disjoint source-prefix scopes, missing payload/plugin_name errors |
| `ingest` / `ingest_batch` | Single + batch article ingestion, validation, invalid-skip |
| HTTP API | Article/batch/sync-actions ingestion with token auth, 401 on bad token, 400 on bad JSON, pass-through when no token configured |
| `refresh` | Re-creates the knowledge collection |

## Wiring into OpenTalon

Enable the HTTP ingestion API and auto-schema creation to build a knowledge base the LLM can pull from via `ask_knowledge` / `list_knowledge_titles`:

```yaml
plugins:
  - name: weaviate
    plugin: /usr/local/bin/weaviate-plugin
    enabled: true
    config:
      host: "localhost:8080"
      scheme: "http"
      auto_create_schema: true
      http_addr: ":8081"
      token: "my-secret-token"
```

The orchestrator can call `sync_actions` at startup to ingest each plugin's MCP server instructions and knowledge articles into the `KnowledgeArticles` collection. External systems can push knowledge articles via the HTTP API.

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
kubectl -n weaviate exec svc/weaviate -- wget -qO- http://localhost:8080/v1/schema/KnowledgeArticles | python3 -m json.tool
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
