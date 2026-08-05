# Code map

What each package is responsible for, which ADR it implements, and what it depends on. The design
lives in [`DESIGN.md`](./DESIGN.md); this is where each decision landed in code.

Deliberately no line numbers — they go stale on the first edit above them, and a stale pointer is
worse than none. Symbol names are stable; `grep` finds them.

```bash
go test -race ./...              # 57 tests, ~6s
go test -race -tags duckdb ./... # 58 — adds the DuckDB join engine (cgo)
```

---

## Start here, by what you want

| You want to see… | Symbol | Why |
|---|---|---|
| **The security argument**, end to end | [`exec`](internal/exec/exec.go) · `verifyPushedSecurityPredicates` | The runtime half of ADR-002. Re-applies every `ENFORCED` predicate after fetch, so a connector that lied about enforcing it fails closed rather than leaking |
| **Whether the design is real** | [`plan`](internal/plan/invariant.go) · `AssertInvariant` | The plan-time half. Proves no security predicate went missing between policy and plan |
| **One request, beginning to end** | [`server`](internal/server/server.go) · `handleQuery` → `buildAndRoute` → `run` | Admission, policy, planning, execution, envelope |
| **The hardest thing here** | [`exec`](internal/exec/exec.go) · `runJoin` | The N-way semi-join cascade across SaaS APIs, and the chunking that keeps each probe bounded |

---

## Packages: responsibility and dependencies

Dependencies are internal packages only, computed from imports. **Five packages depend on nothing**
— they can be read in isolation, and they are where the vocabulary is defined.

| Package | Responsibility | Depends on |
|---|---|---|
| [`catalog`](internal/catalog/) | Per-`(table, column, op)` capability with `ENFORCED`/`ADVISORY`, `max_in_list`, masking support. **The vocabulary pushdown decisions are written in** | — |
| [`policy`](internal/policy/) | Role → RLS residuals, CLS masks, policy version, shape hash. Stands in for OPA's Compile API | — |
| [`identity`](internal/identity/) | Issuer registry, tenant derivation from the verified `iss`, and the `ErrUnauthenticated` (401) / `ErrPrincipalUnresolved` (503) split | — |
| [`ratelimit`](internal/ratelimit/) | Token buckets per connector; `Gate` is the per-outbound-call closure the connector consults | — |
| [`obs`](internal/obs/) | Prometheus metrics and OTel tracing, dual-emitted so tests and `/metrics` cannot drift | — |
| [`connector`](internal/connector/) | The `Source` contract and its HTTP implementation: pagination, ETag revalidation, per-page quota gate, error classification | `obs` |
| [`plan`](internal/plan/) | Parse → capability-aware pushdown → RLS/CLS injection → the plan-time invariant. N-way `Join{Sides, Links}` | `catalog`, `policy` |
| [`plancache`](internal/plancache/) | The six-field cache key and `combinedCapShape`, which folds every referenced table's capability profile | `catalog`, `plan`, `policy` |
| [`freshness`](internal/freshness/) | Result cache keyed on `principal`, `table`, `columns`, `filters`; entries **immutable once published** | `connector`, `obs` |
| [`exec`](internal/exec/) | Fetch, the N-way semi-join cascade, two pluggable `JoinEngine`s, verification filter, local filters, masking, projection | `connector`, `obs`, `plan` |
| [`server`](internal/server/) | HTTP surface, admission, error vocabulary, sync + async + NDJSON paths. **The composition point** | all of the above |

### External dependencies

Five direct, and the build-tag split is the one with consequences.

| Module | Used for | Where |
|---|---|---|
| `vitess.io/vitess` | The SQL parser — ADR-001's Go stand-in for the Calcite sidecar. Gives an AST, not an optimizer, which is the whole of that ADR's trade-off | [`plan`](internal/plan/) |
| `prometheus/client_golang` | `/metrics`, plus the free Go-runtime and process collectors | [`obs`](internal/obs/) |
| `go.opentelemetry.io/otel` (+ sdk, otlptracehttp) | Spans and OTLP export | [`obs`](internal/obs/) |
| `marcboeker/go-duckdb` | **ADR-007 tier 1's engine — cgo, behind `-tags duckdb`** | [`duckjoin.go`](internal/exec/duckjoin.go) |
| `stretchr/testify` | Assertions | tests only |

**The DuckDB dependency is the only one that changes how the binary is built**, so it is isolated
behind a build tag rather than imported unconditionally:

| | Compiles | cgo | Runtime image | Size |
|---|---|---|---|---|
| default | [`duckjoin_stub.go`](internal/exec/duckjoin_stub.go) — returns an error naming the fix | no | `distroless/static` | 49.5 MB |
| `-tags duckdb` | [`duckjoin.go`](internal/exec/duckjoin.go) — the real engine | **yes** | `distroless/cc` (DuckDB is C++, so libstdc++ as well as glibc) | 148 MB |

Consequences worth knowing before touching it: `go test ./...` does **not compile** the real
engine, so only `go test -tags duckdb ./...` gives it any coverage; the builder is pinned to
`golang:1.26-bookworm` so its glibc matches the `-debian12` runtime; and `go.mod` marks go-duckdb
`// indirect` because `go mod tidy` cannot see an import behind a build tag — harmless, and
verified not to break the tagged build.

### Test doubles

| Package | Notes | Depends on |
|---|---|---|
| [`mocksf`](internal/mocksf/) | Salesforce-shaped. `Capability`, `LieAbout`, `PageSize`, `RateLimit`, `DelayJitter` | `catalog`, `connector` |
| [`mockzd`](internal/mockzd/) | Zendesk-shaped. `MaxInList` is what forces semi-join chunking | `connector` |
| [`harness`](test/acceptance/harness/) | Real `httptest` server; `gw.Query(persona, sql)`, plus `RawAuth` for credentials `Token` cannot express | `server` |

**`LieAbout` is the point of the mocks.** It makes a connector declare `ENFORCED` and then ignore
the predicate — the adversary the whole entitlement model is built against.

---

## The request path, in call order

One `POST /v1/query`, all inside `server`:

| # | Step | Where | What it decides |
|---|---|---|---|
| 1 | **Admission** | `identity.ResolveFromHeader` | Tenant from the verified issuer, never a claim (ADR-011). Caller-fixable credentials are `UNAUTHENTICATED`/401; 503 is reserved for an unreachable attribute source |
| 2 | **Object authz (L1)** | `policy.ObjectDenied` | Rejects a denied table before any catalog or planner work |
| 3 | **Plan (or cache hit)** | `plancache.Resolve` | Six-field key; RLS/CLS residuals injected as plan nodes (L2) |
| 4 | **Bind literals** | `plan.Shape` + `plan.ExtractParams` | Cached plans hold `?` placeholders, so two literals share one plan and each still sees its own value |
| 5 | **Wrap sources** | `freshness.Source{}` | Per-request cache decorator over the shared connector |
| 6 | **Execute** | `exec.Run` → `runJoin` | Single-table scan, or the N-way semi-join cascade over the plan's sides and links |
| 7 | **Fetch + verify (L3)** | `exec.fetchScanRows` | Verification filter, then local filters — **per side, before any join merge** |
| 8 | **Project** | `exec.project` | Positional rows, so `a.id` and `t.id` are two slots rather than one overwritten map key |

**Rate limiting is not in this list on purpose.** It lives one layer deeper, in
[`connector`](internal/connector/httpsource.go)'s page loop, because a vendor bills per HTTP request
and `Fetch` paginates internally. Wired once in `server.New`, never per request — the sources map is
shared across concurrent requests.

---

## Where each ADR lives

| ADR | Code |
|---|---|
| **001** Planner runtime | [`plan`](internal/plan/) — a Go stand-in for the Calcite sidecar the design specifies |
| **002** Entitlements | L1 `policy.ObjectDenied` · L2 [`plan`](internal/plan/) build + invariant · L3 `exec.verifyPushedSecurityPredicates` |
| **003** Caching | [`plancache`](internal/plancache/) and [`freshness`](internal/freshness/) |
| **005** Freshness | [`freshness`](internal/freshness/) + ETag handling in [`connector`](internal/connector/) |
| **006** Rate limits | [`ratelimit`](internal/ratelimit/) + the page gate in [`connector`](internal/connector/) |
| **007** Joins | `exec.runJoin` + [`joinengine.go`](internal/exec/joinengine.go) / [`duckjoin.go`](internal/exec/duckjoin.go) — tier 1 is real (`--join-engine=duckdb`, `-tags duckdb`). **`JoinEngine` deliberately stops at tier 1**: it passes rows in memory, and tiers 2–3 exist because rows don't fit. They need a `Submit`/`Poll` interface, not a wider one |
| **009** Streaming | `server.runStream`, NDJSON terminal frame |
| **011** Identity | [`identity`](internal/identity/) — issuer registry, tenant derivation, and the 401/503 split |

ADR-004, 008 and 010 are design-only — no code. Deliberate, and
[`README.md`](./README.md)'s MVP status says so.

---

## Known gaps — read before reviewing

Real and deliberate. Claiming otherwise would be the actual defect.

| Gap | Where | Status |
|---|---|---|
| **No plan-cache eviction** | `plancache.Cache` | Documented in the type comment. Resident memory grows with distinct keys |
| **No singleflight** | [`freshness`](internal/freshness/) | `TestConcurrentMissesOnOneKeyStampedeToTheConnector` **asserts the stampede**, so the gap stays visible |
| **Per-pod rate-limit buckets** | [`ratelimit`](internal/ratelimit/) | N pods = N× the limit. Design specifies Redis leases |
| **Join order is FROM order** | `plan.buildJoin` | N-way works, but the cascade is left-deep in FROM order; cost-based ordering needs the planner ADR-001 defers |
| **`LIMIT`/`OFFSET` unsupported** | [`plan`](internal/plan/) | Parsed by the grammar, never read by `Build` |
| **`extra` can clobber `pushed`** | `exec.fetchScanRows` | Latent — unreachable until a catalog declares a join key filterable |

---

## Fixtures

```
testdata/catalog/sf.accounts.json    status ENFORCED, region ADVISORY, masking unsupported
testdata/catalog/zd.tickets.json     status ENFORCED, join_key_in_list, max_in_list 200
testdata/policy/support_agent.json   RLS on region, CLS sha256(email), denies sf.opportunities
testdata/policy/admin.json           no RLS, no masks
testdata/tokens/{dana,erin,root}.jwt dana+erin: same role and region, different sub
```

**`dana` and `erin` exist to be identical except for `sub`** — that is what isolates the
principal-keyed cache test from role-driven differences.

`zd.tickets` declares only `status` as a predicate, which is why the `extra`-clobbers-`pushed` gap
above is currently unreachable.
