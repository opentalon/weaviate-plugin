# weaviate-plugin

[![CI](https://github.com/opentalon/weaviate-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/weaviate-plugin/actions/workflows/ci.yml)

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that performs semantic and hybrid search over a [Weaviate](https://github.com/weaviate/weaviate) vector database collection. Use it as a retrieval step (RAG) to fetch relevant context before the LLM generates a response.

## Actions

| Action | Mode | Description |
|---|---|---|
| `weaviate.search` | LLM tool | nearText semantic search — the LLM calls this when it decides retrieval is needed |
| `weaviate.hybrid_search` | LLM tool | Hybrid BM25 + vector. `alpha`: `0` = keyword only, `1` = vector only (default `0.5`) |
| `weaviate.prepare` | **preparer** | Automatic RAG: runs before every LLM call, enriches the user message with retrieved context |

`search` and `hybrid_search` accept `limit` and `fields` per-call overrides.

### Tool mode vs. preparer mode

**Tool mode** (default) — the LLM decides when to retrieve:
```
user → LLM decides to call weaviate.search → results → LLM → answer
```

**Preparer mode** — retrieval is automatic, invisible to the LLM as a tool choice:
```
user message → weaviate.prepare(text=message) → [retrieved_context]…message → LLM → answer
```

Use preparer mode when you always want retrieved context in the prompt (RAG-by-default). Use tool mode when retrieval should be conditional.

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
      collection: "Article"        # Weaviate class to search
      fields:                      # fields to return in results
        - title
        - body
        - url
      limit: 5                     # default result count
```

The `collection` field is required. All others have defaults (`host: localhost:8080`, `scheme: http`, `limit: 5`).

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

Starts Weaviate + the [contextionary](https://github.com/weaviate/contextionary) English word-embedding service so nearText works:

```bash
make test-docker
```

### Environment variables for tests

| Variable | Default | Description |
|---|---|---|
| `WEAVIATE_HOST` | `localhost:8080` | Address of the running Weaviate instance |
| `WEAVIATE_MODULE` | `none` | Set to `text2vec-contextionary` to enable nearText tests |

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
| **integration** | Real Weaviate 1.30 + contextionary via Docker service; runs full test suite including nearText |

The integration job uses Docker service containers declared in the workflow — no manual setup needed.

## Test suite overview

| Test | Needs vectorizer | What it checks |
|---|---|---|
| `TestCapabilities` | no | Plugin declares correct name and actions |
| `TestConfigure_defaults` | no | Default limit=5 applied, client constructed |
| `TestConfigure_missingCollection` | no | Error returned when collection omitted |
| `TestConfigure_badJSON` | no | Error returned for malformed config |
| `TestExecute_unknownAction` | no | Error returned for unrecognised action |
| `TestHybridSearch_keywordOnly` | no | BM25 (alpha=0) returns the Python article for "python" |
| `TestHybridSearch_limitOverride` | no | `limit=1` returns exactly one result |
| `TestHybridSearch_fieldsOverride` | no | Requesting only `title` omits `body` from response |
| `TestSearch_semantic` | **yes** | nearText "vector database" returns the Weaviate article |
| `TestSearch_missingQuery` | no | Error returned when query arg is absent |
| `TestHybridSearch_missingQuery` | no | Error returned when query arg is absent |

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

## References

- Weaviate Go client: https://github.com/weaviate/weaviate-go-client
- Weaviate server: https://github.com/weaviate/weaviate
- Contextionary: https://github.com/weaviate/contextionary
- OpenTalon plugin SDK: https://github.com/opentalon/opentalon (see `pkg/plugin`)
