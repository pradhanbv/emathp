# Code map

Navigation for the implementation. The design lives in [`DESIGN.md`](./DESIGN.md); this is where
each decision actually landed in code, and where the bodies are buried.

**5,140 lines of Go, 2,020 of tests, 57 tests.** Run everything with `go test -race ./...` (~6 s); `-tags duckdb` adds one more that exercises DuckDB directly.

---

## Start here, by what you want

| You want to see… | Go to | Why that file |
|---|---|---|
| **The security argument**, end to end | [`exec.go:375`](internal/exec/exec.go#L375) `verifyPushedSecurityPredicates` | The runtime half of ADR-002. Re-applies every `ENFORCED` predicate after fetch, so a connector that lied about enforcing it fails closed rather than leaking |
| **Whether the design is real** | [`plan/invariant.go:66`](internal/plan/invariant.go#L66) `AssertInvariant` | The plan-time half. Proves no security predicate went missing between policy and plan |
| **One request, beginning to end** | [`server.go:123`](internal/server/server.go#L123) → the table below | ~200 lines covers admission, policy, planning, execution, envelope |
| **The hardest thing here** | [`exec.go`](internal/exec/exec.go) `runJoin` | The N-way semi-join cascade across SaaS APIs, plus the chunking that keeps each probe bounded |

---

## The request path, in call order

Everything below happens inside one `POST /v1/query`. File references are the real call sites.

| # | Step | Where | What it decides |
|---|---|---|---|
| 1 | **Admission** | [`server.go:123`](internal/server/server.go#L123) `identity.ResolveFromHeader` | Verifies the JWT and derives tenant **from the verified issuer**, never a claim (ADR-011) |
| 2 | **Object authz (L1)** | [`server.go:189`](internal/server/server.go#L189) `Policy.ObjectDenied` | Rejects a denied table before any catalog or planner work happens |
| 3 | **Plan (or cache hit)** | [`server.go:199`](internal/server/server.go#L199) → [`plancache.go:77`](internal/plancache/plancache.go#L77) `Resolve` | Six-field key; RLS/CLS residuals injected as plan nodes (L2) |
| 4 | **Bind literals** | [`build.go:641`](internal/plan/build.go#L641) `Shape` + `ExtractParams` | Cached plans hold `?` placeholders, so two literals share one plan and each still sees its own value |
| 5 | **Wrap sources** | [`server.go:239`](internal/server/server.go#L239) `freshness.Source{}` | Per-request cache decorator over the shared connector |
| 6 | **Execute** | [`exec.go`](internal/exec/exec.go) `Run` | Single-table scan, or `runJoin` — an N-way semi-join cascade over the plan's sides and links |
| 7 | **Fetch + verify (L3)** | [`exec.go:245`](internal/exec/exec.go#L245) | Verification filter, then `applyLocalFilters` — **per side, before any join merge** |
| 8 | **Project** | [`exec.go:430`](internal/exec/exec.go#L430) `project` | Positional rows, so `a.id` and `t.id` are two slots rather than one overwritten map key |

**Rate limiting is not in this list on purpose.** It lives one layer deeper, at
[`httpsource.go`](internal/connector/httpsource.go)'s page loop, because a vendor bills per HTTP
request and `Fetch` paginates internally. Wired once in [`server.go:72`](internal/server/server.go#L72)
`New`, never per request — the sources map is shared across concurrent requests.

---

## Packages

Sizes are source lines excluding tests.

### The load-bearing four

| Package | Src | What it owns |
|---|---|---|
| [`plan`](internal/plan/) | **1,227** | Parse → capability-aware pushdown → residual injection → the invariant. `build.go` is ~950 of it and is the densest file in the repo |
| [`server`](internal/server/) | 681 | HTTP surface, admission, error vocabulary, sync + async + NDJSON paths |
| [`exec`](internal/exec/) | 841 | Fetch, N-way semi-join cascade, two pluggable `JoinEngine`s (Go and DuckDB), verification filter, local filters, masking, projection |
| [`connector`](internal/connector/) | 244 | The `Source` contract, HTTP implementation, pagination, ETags, per-page quota gate |

### Supporting

| Package | Src | What it owns |
|---|---|---|
| [`freshness`](internal/freshness/) | 246 | Result cache keyed `principal\|table\|columns\|filters`. Entries are **immutable once published** |
| [`obs`](internal/obs/) | 215 | Prometheus metrics + OTel tracing, dual-emitted so tests and `/metrics` can't drift |
| [`policy`](internal/policy/) | 182 | Role → residuals, masks, version, shape hash. Stands in for OPA's Compile API |
| [`plancache`](internal/plancache/) | 151 | The six-field key and `combinedCapShape` |
| [`catalog`](internal/catalog/) | 131 | Per-`(table, column, op)` capability with `ENFORCED`/`ADVISORY` |
| [`identity`](internal/identity/) | 122 | Issuer registry, tenant derivation |
| [`ratelimit`](internal/ratelimit/) | 95 | Token buckets, `Gate` |

### Test doubles — read these to understand the tests

| Package | Src | Notes |
|---|---|---|
| [`mocksf`](internal/mocksf/) | 334 | Salesforce-shaped. `Capability`, `LieAbout`, `PageSize`, `RateLimit`, `DelayJitter` |
| [`mockzd`](internal/mockzd/) | 226 | Zendesk-shaped. `MaxInList` is what forces semi-join chunking |
| [`harness`](test/acceptance/harness/) | 215 | Real `httptest` server; `gw.Query(persona, sql)` |

**`LieAbout` is the point of the mocks.** It makes a connector declare `ENFORCED` and then ignore
the predicate — the adversary the whole entitlement model is built against.

---

## Where each ADR lives

| ADR | Code |
|---|---|
| **001** Planner runtime | [`plan/build.go`](internal/plan/build.go) — a Go stand-in for the Calcite sidecar the design specifies |
| **002** Entitlements | L1 [`server.go:189`](internal/server/server.go#L189) · L2 [`build.go`](internal/plan/build.go) + [`invariant.go`](internal/plan/invariant.go) · L3 [`exec.go:375`](internal/exec/exec.go#L375) |
| **003** Caching | [`plancache/`](internal/plancache/) and [`freshness/`](internal/freshness/) |
| **005** Freshness | [`freshness.go`](internal/freshness/freshness.go) + ETag handling in [`httpsource.go`](internal/connector/httpsource.go) |
| **006** Rate limits | [`ratelimit/`](internal/ratelimit/) + the page gate in [`httpsource.go`](internal/connector/httpsource.go) |
| **007** Joins | [`exec.go`](internal/exec/exec.go) `runJoin` + [`joinengine.go`](internal/exec/joinengine.go) / [`duckjoin.go`](internal/exec/duckjoin.go) — tier 1 is real (embedded DuckDB, `--join-engine=duckdb`). **`JoinEngine` deliberately stops at tier 1**: it passes rows in memory, and tiers 2–3 exist because rows don't fit. They need a `Submit`/`Poll` interface, not a wider one |
| **009** Streaming | `runStream` in [`server.go`](internal/server/server.go), NDJSON terminal frame |
| **011** Identity | [`identity/`](internal/identity/) |

ADR-004, 008 and 010 are design-only — no code. That is deliberate and
[`README.md`](./README.md)'s MVP status says so.

---

## Tests, by what they prove

`test/acceptance/` (747 lines) is the layer worth reading; unit tests sit beside their packages.

| Test | Claim it defends |
|---|---|
| `TestLyingConnectorFailsClosed` | **The headline.** Connector declares `ENFORCED`, ignores it, request 403s |
| `TestHonestConnectorZeroViolations` | Its pairing — the filter isn't trivially always-firing |
| `TestPlanCacheDoesNotLeakAcrossRoles` | Privilege escalation via a shared plan |
| `TestConcurrentResolvesDoNotBleedAcrossRoles` | The same, **under contention** |
| `TestSemiJoinReducesProbeCalls` | 501 → 4 connector calls (125×) on the reference fixture |
| `TestFourTableJoinEndToEnd` | **N-way works** — 4 tables, 3 links over HTTP, through every engine in the build (`-tags duckdb` adds DuckDB and requires identical output) |
| `TestUnauthenticatedIsNotAServiceFailure` | Every credential the caller can fix is a **401**, not a 503 — missing header, bad prefix, unparseable claims, unregistered issuer |
| `TestJoinRejectsForwardReference` | Every join must reference an earlier table, so a probe's keys are always already fetched |
| `TestSemiJoinReturnsEveryMatchingRow` | Exactly 2,500 rows across 3 chunks — the correctness half |
| `TestJoinKeepsBothSidesOfCollidingColumns` | `a.id` and `t.id` both survive the merge |
| `TestOuterJoinsRejectedNotSilentlyDowngraded` | `LEFT`/`RIGHT`/`CROSS` are refused, not run as inner |
| `TestFreshnessCacheIsolatedByPrincipal` | Two users, same role, must not share a cache entry |
| `TestPaginatedFetchSpendsOneTokenPerPage` | Quota is denominated in HTTP requests |
| `TestProbeChunksAreIndependentOfBuildRowOrder` | Probe-side cache keys survive a source reordering identical rows |
| `TestConcurrentHitAndRevalidateOnOneKey` | Worthless without `-race` — asserts an absence |

---

## Known gaps — read before reviewing

These are real and deliberate. Claiming otherwise would be the actual defect.

| Gap | Where | Status |
|---|---|---|
| **No plan-cache eviction** | [`plancache.go`](internal/plancache/plancache.go) `Cache` | Documented in the type comment. Resident memory grows with distinct keys |
| **No singleflight** | [`freshness.go`](internal/freshness/freshness.go) | `TestConcurrentMissesOnOneKeyStampedeToTheConnector` **asserts the stampede** so the gap stays visible |
| **Per-pod rate limit buckets** | [`ratelimit/`](internal/ratelimit/) | N pods = N× the limit. Design specifies Redis leases |
| **Join order is FROM order** | [`build.go`](internal/plan/build.go) `buildJoin` | N-way works, but the cascade is left-deep in FROM order; cost-based ordering needs the planner ADR-001 defers |
| **`LIMIT`/`OFFSET` unsupported** | [`build.go`](internal/plan/build.go) | Same layer as projection; parsed, not honoured |
| **`extra` can clobber `pushed`** | `fetchScanRows` in [`exec.go`](internal/exec/exec.go) | Latent — unreachable until a catalog declares the join key filterable |

---

## Fixtures

```
testdata/catalog/sf.accounts.json    status ENFORCED, region ADVISORY, masking unsupported
testdata/catalog/zd.tickets.json     status ENFORCED, join_key_in_list, max_in_list 200
testdata/policy/support_agent.json   RLS on region, CLS sha256(email), denies sf.opportunities
testdata/policy/admin.json           no RLS, no masks
testdata/tokens/{dana,erin,root}.jwt dana+erin: same role and region, different sub
```

**`dana` and `erin` exist to be identical except for `sub`** — that's what isolates the
principal-keyed cache test from role-driven differences.

`zd.tickets` declares only `status`, which is why the `extra`-clobbers-`pushed` bug above is
currently unreachable.

---

## Commands

```bash
go test -race ./...                  # 57 tests, ~6s
go test ./... -run 'LyingConnector|PlanCacheDoesNotLeak|SemiJoin|TenantDerived' -v
go run ./cmd/gateway                 # needs mocksf + mockzd, see README Quickstart
docker compose --profile duckdb up -d --build   # ADR-007 tier 1 on :8090 (cgo, 148MB vs 49.5MB)
```

Full reproduction of every claim: [`README.md`](./README.md)'s recreate appendix.
