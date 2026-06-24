# LLM Relay Service — Go rewrite

A from-scratch Go port of [LLMRelayService](../Downloads/LLMRelayService-main), a
self-hosted LLM relay gateway with an observability console. This repository
holds the Go rewrite; the original TypeScript/Bun project is untouched.

## Status: ✅ All phases complete (P0–P8)

A from-scratch Go port of the original TypeScript/Bun LLM relay gateway. All
core logic is implemented and tested.

| Phase | Scope | Status |
|---|---|---|
| 0 (spike) | Streaming converter + usage parser + e2e proxy | ✅ |
| P1 | Skeleton: config, CORS, server, graceful shutdown, health/db-reset | ✅ |
| P2 | Data layer: pgxpool, embed 8 migrations, repository layer | ✅ |
| P3 | Routing & config cache: atomic snapshot, singleflight, route resolution | ✅ |
| P4 | Gateway engine: auth, failover/retry, timeouts, responses-compat, streaming | ✅ |
| P5 | Observability: consolestore, logtasks async pool, tokenest, pricing/catalog | ✅ |
| P6 | Provider adapters: buildForwardHeaders, prepareRequest, hasTextualSignal | ✅ |
| P7 | Console API (`/__console/*`), cookie auth (FNV-1a), SPA serving | ✅ |
| P8 | Dockerfile, docker-compose, full test suite | ✅ |

**151 tests pass** across 17 packages. Verified end-to-end against a real
Postgres 17 container (migrations, CRUD, gateway proxy, console auth).

### Phase 0 (spike) ✅ complete

De-risked the single hardest part of the rewrite — the OpenAI Responses API ↔
Chat Completions **bidirectional streaming converter** — plus the usage-parser
path, before committing to porting the rest.

### What the spike proves

| Capability | Evidence |
|---|---|
| SSE block splitting (cross-chunk buffering, CRLF, trailing block w/o blank line) | `internal/sse` — 7 tests |
| Responses → Chat request conversion (messages, tools, tool_choice, text format, MiniMax sanitizing, reasoning effort) | `internal/responsesconv` — `request.go` |
| Chat → Responses non-streaming response (`<think>` static splitting, tool calls, usage translation) | `internal/responsesconv` — `response.go` |
| **Chat → Responses streaming SSE** (full event lifecycle: created→added→delta→done→completed→[DONE]) | `internal/responsesconv` — `stream.go` |
| **`<think>` tag incremental parsing across SSE chunks** (`longestTagSuffixPrefix` partial-tag holdback) | `internal/responsesconv` — `thinktag.go` |
| Provider usage parsing (real Anthropic SSE + OpenAI JSON, both naming conventions) | `internal/providers` — 8 tests |
| End-to-end HTTP pipeline (Responses in → convert → upstream → convert → Responses SSE out, with flushing) | `internal/proxy` — 3 e2e tests |

All **31 tests pass** (`go test ./...`).

### Phase 1 (skeleton) ✅ complete

Built the production skeleton and wired up the boot sequence. Key outcomes:

| Package | What it does |
|---|---|
| `internal/config` | Env loading with the exact timeout-precedence chain (`UPSTREAM_<KIND>_FIRST_BYTE_TIMEOUT_MS → UPSTREAM_REQUEST_TIMEOUT_MS → default`), validation against the original's limits |
| `internal/logging` | Payload/stream-log byte & duration caps (from `logging-constants.ts`) |
| `internal/cors` | Permissive CORS middleware (every response + OPTIONS preflight), with the exact SDK header allowlist |
| `internal/health` | `/health` endpoint + degraded-mode HTML page + atomic migration-status |
| `internal/server` | `cmd/server` entrypoint, signal-based graceful shutdown |
| `cmd/server` | Boot sequence: config → migration (stub) → catalog warm (stub) → server |

**Fixed a real defect in the original service:** the Node version exports
`waitForPendingResponseLogs` but never wires it to a signal handler, so
in-flight background log writes can be lost on kill. The Go server registers a
drain function (via `WithDrain`) that SIGTERM/SIGINT trigger before exit — P5
will hook the logtasks worker pool into it.

Run it:
```bash
DATABASE_URL="postgresql://u:p@localhost:5432/db" GATEWAY_API_KEY=key go run ./cmd/server
curl localhost:3300/health   # {"status":"ok","database":{"state":"success"}}
```

All **60 tests pass** across both phases (`go test ./...`).

### Phase 2 (data layer) ✅ complete

Built the PostgreSQL access layer. Replaced the original Drizzle ORM with
hand-written pgx repositories (chose this over sqlc to avoid a code-gen build
step and keep the queries reviewable in-tree).

| Package | What it does |
|---|---|
| `internal/db` | `pgxpool.Pool` connection management + lazy `PoolHolder` singleton (mirrors db/client.ts) |
| `internal/migrate` | golang-migrate with `//go:embed` SQL; advisory lock (20817,1); retry-until-ready with exponential backoff; `ResetDB` for the degraded-mode recovery endpoint |
| `internal/schema` | Go structs for all 7 tables |
| `internal/repo` | Repositories: `ProviderRepo`, `AliasRepo`, `APIKeyRepo`, `SettingsRepo` + pure helpers (SHA-256 key hashing, micro-USD quota math, model allowlist matching) |
| `migrations/` | 8 SQL files converted from Drizzle format (`statement-breakpoint` markers stripped, renamed to golang-migrate `NNNNNN_name.up.sql`) |

**Verified end-to-end against a real Postgres 17 container:**
migrations create all 7 tables; full CRUD on providers/aliases/api-keys/settings;
`ResetDB` drops and re-applies; `cmd/server` runs migrations at boot and
`/health` reports `success`. 13 unit tests (no DB) + 6 integration tests (real DB).

**Migration runner ports the original's reliability semantics:** advisory lock
so concurrent replicas don't race; retry-until-ready (30 retries, 500ms→5s
backoff) for PG still starting up; never crashes on failure (degrades so
`/api/db/reset` can recover).

### Phase 3 (routing & config cache) ✅ complete

The bridge between the data layer and the gateway engine: an in-memory config
cache plus the full route-resolution logic.

| Package | What it does |
|---|---|
| `internal/configstore` | Immutable `Snapshot` behind `atomic.Pointer` (lock-free reads); `singleflight` load coordination; row→entry conversion (auth normalization, responsesMode extraction, alias targets) |
| `internal/routing` | `ResolveRoute` (explicit `/providers/{ch}` prefix), `ResolveRoutesByModel` (model + alias routing with priority), failover resolvers, `Models()`/`ChannelModels()` listing |

**Key design:** the original relied on module-level mutable variables safe
only because Node is single-threaded. The Go port stores the *entire*
configuration in an immutable snapshot; writes build a new snapshot and swap it
atomically, so thousands of concurrent requests read without locks and can
never see a data race.

**Route resolution ports every nuance of the original:** the `/v1`-stripping
path rule for OpenAI (vs kept for Anthropic), priority-desc/name-asc ordering,
alias isolation (an alias resolves *only* to its bound target, never to other
channels with the same model name), explicit-only visibility filtering, and
type inference (`/v1/messages`→anthropic, other `/v1/*`→openai). 29 routing +
8 conversion tests cover all these cases.

### The hardest part, verified

The `<think>` reasoning-tag splitter has two forms: a one-shot splitter for
non-streaming responses and a stateful parser for streams. The streaming
parser must hold back a partial tag at the end of each chunk (e.g. `"<thi"`)
so it isn't emitted as visible text when the closing `</think>` arrives split
across the next SSE chunk. This is faithfully ported, including the
`longestTagSuffixPrefix` holdback algorithm, and verified by
`TestThinkParser_SplitAcrossChunks` and `TestTransformResponse_ThinkStreaming`.

## Running the tests

```bash
# Pure-logic tests (no database needed)
go test ./...

# Integration tests (need a running Postgres)
docker run -d --name lrs-test-pg -e POSTGRES_USER=lrs -e POSTGRES_PASSWORD=lrs \
  -e POSTGRES_DB=lrs_test -p 5433:5432 postgres:17-alpine
TEST_DATABASE_URL="postgresql://lrs:lrs@127.0.0.1:5433/lrs_test" \
  go test ./internal/repo/ -tags integration -v
```

Integration tests use a build tag (`integration`) and are table-driven across
every configured database dialect. Each dialect is enabled by an env var, and
SQLite always runs (a temp file, no external service). Run against all three:

```bash
# Start optional Postgres + MySQL containers for full dialect coverage.
docker run -d --rm --name lrs-test-pg -e POSTGRES_USER=lrs -e POSTGRES_PASSWORD=lrs \
  -e POSTGRES_DB=lrs_test -p 5433:5432 postgres:17-alpine
docker run -d --rm --name lrs-test-mysql -e MYSQL_ROOT_PASSWORD=lrs -e MYSQL_DATABASE=lrs_test \
  -e MYSQL_USER=lrs -e MYSQL_PASSWORD=lrs -p 3307:3306 mysql:8.4

TEST_DATABASE_URL="postgresql://lrs:lrs@127.0.0.1:5433/lrs_test" \
TEST_MYSQL_URL="mysql://lrs:lrs@tcp(127.0.0.1:3307)/lrs_test" \
  go test -p 1 -tags integration ./internal/repo/ ./internal/consoleapi/ -v
```

`TEST_SQLITE_URL` defaults to a temp file (omit it). Use `-p 1` when multiple
packages share a test database so resets don't race.

## Multiple database backends

The gateway supports **PostgreSQL, MySQL, and SQLite** through GORM. The
dialect is auto-detected from the `DATABASE_URL` scheme, so the same binary
works against any backend with no code change:

```bash
# PostgreSQL
DATABASE_URL=postgresql://user:pass@host:5432/db ./llm-relay

# MySQL
DATABASE_URL=mysql://user:pass@tcp(host:3306)/db ./llm-relay

# SQLite (single file, zero-config — great for local/single-node deploys)
DATABASE_URL=sqlite:///data/lrs.db ./llm-relay
```

Schema is managed by GORM `AutoMigrate` (additive: creates tables/indexes,
never drops columns), which is dialect-agnostic. The `/health` endpoint and the
dashboard's usage page report the active backend. Note: SQLite uses a pure-Go
driver (`glebarez/sqlite`), so the build stays CGO-free (`CGO_ENABLED=0`).

## Model pricing

Per-model pricing (USD per 1M tokens, used for the dashboard's cost column) is
sourced from [models.dev](https://models.dev) and **committed to the repo** as
`internal/catalog/models-dev.json`, which the gateway `//go:embed`s at build
time. At boot the catalog is parsed into memory — pricing lookups never touch
the DB or the network.

Update prices periodically by running the sync script, then commit + redeploy:

```bash
./scripts/sync-prices.sh             # download + verify (no commit)
./scripts/sync-prices.sh --commit    # download + verify + git commit
```

The same model id offered by multiple providers (e.g. `claude-opus-4-5` via
anthropic, azure, …) collapses to one entry, preferring the first-party provider
(anthropic / openai) whose pricing is most complete. Per-(channel, model)
manual overrides can still be set from the dashboard's Models page (stored in
`model_metadata_overrides`) and take precedence over the catalog.

## Run the spike

```bash
# 1. Configure (point at any OpenAI-compatible upstream)
cp .env.example .env
# edit .env: set UPSTREAM_BASE_URL and UPSTREAM_AUTH_VALUE

# 2. Run
go run ./cmd/spike

# 3. Test end-to-end (streaming)
curl -N http://localhost:3300/v1/responses \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","input":"Say hello in one word.","stream":true}'
```

## Project layout

```
cmd/
  spike/                end-to-end validation binary (Phase 0)
  server/               production entrypoint
internal/
  sse/                  SSE block reader (foundation for all streaming)
  responsesconv/        Responses ↔ Chat converter (request, response, streaming)
  providers/            Anthropic/OpenAI adapters (usage, headers, prepare, signal)
  proxy/                minimal ReverseProxy (spike)
  gateway/              full proxy engine: auth, failover, timeouts, forwarding
  config/               env loading + Config struct (timeout precedence chain)
  logging/              payload/stream-log byte & duration caps
  cors/                 permissive CORS middleware
  health/               /health endpoint + degraded-mode page + migration status
  server/               HTTP server + signal-based graceful shutdown + root mux
  db/                   pgxpool connection management + PoolHolder
  migrate/              golang-migrate runner (embed, advisory lock, retry)
  schema/               Go structs for the 7 database tables
  repo/                 repositories: provider, alias, apikey, settings
  configstore/          atomic snapshot config cache + row→entry conversion
  routing/              route resolution (explicit prefix, model, alias, fallback)
  consolestore/         request-log storage + query/aggregation
  logtasks/             async log orchestration (WaitGroup + per-key serialization)
  tokenest/             tiktoken-based token estimation with fallbacks
  pricing/              per-model cost calculation
  catalog/              models.dev pricing catalog (//go:embed, in-memory)
  consoleauth/          cookie auth (FNV-1a hash, byte-for-byte port)
  consoleapi/           /__console/* API routes for the dashboard
  web/                  embedded SPA static serving
migrations/             8 SQL files (golang-migrate format)
Dockerfile              multi-stage build (frontend → Go → distroless)
docker-compose.yml      Postgres + gateway
.env.example
```

The spike covered the only phase with genuine technical uncertainty (the
streaming converter). Everything from P1 onward is deterministic porting of
logic already validated in the original codebase.
