# MVP Implementation Plan - TDD

Companion to `DESIGN.md`. ~7 h across 14 cycles. Every cycle is red -> green -> refactor.

## Ground rules

1. **No production code without a failing test.** If you can't express it as a test, you
 don't yet know what you're building.
2. **Outside-in.** Each cycle opens with an *acceptance* test through the HTTP boundary,
 then unit tests drive the pieces it needs. This keeps the response contract honest and
 stops you from building beautifully-tested components that don't compose.
3. **Cycles are risk-ordered, not layer-ordered.** The tests that could falsify a claim in
 `DESIGN.md` come first. If cycle 6 fails, you want to know at hour 3, not hour 6.
4. **Two things are not TDD'd**, and that's deliberate: the mock SaaS services (they *are*
 test fixtures - driven by the tests that consume them) and Docker Compose wiring. Say so
 in the README rather than pretending otherwise.
5. **Commit per green.** The commit log becomes evidence of process, which is itself
 assessed under "Communication".

## Stack

Go for everything - gateway *and* mocks. One toolchain, one `go test ./...`, one thing for a
reviewer to install. Mocks are ~80 lines each; FastAPI would be marginally faster to write
and materially worse to review.

## Layout

```
cmd/gateway/main.go
cmd/mocksf/main.go # Salesforce-ish
cmd/mockzd/main.go # Zendesk-ish
internal/
 identity/ # token -> principal; issuer -> tenant
 catalog/ # tables, columns, capability model
 policy/ # PolicyProvider -> residuals + masks
 plan/ # SQL -> tree; injection; capability walk; verdicts; invariant
 plancache/ # composite key
 exec/ # fanout, semi-join, residual filter, verification filter, masking
 ratelimit/ # token bucket + async reroute
 freshness/ # TTL cache, ETag, max_staleness
 connector/ # SDK: capability, pagination, ETag, error mapping
 obs/ # metrics, trace_id
test/acceptance/ # black-box, through HTTP
testdata/{catalog,policy,tokens}/*.json
```

---

## Test inventory

The full set, in the order you'll write them. **Bold = the tests that carry the submission.**

| # | Test | Proves |
|---|---|---|
| 1 | `TestQueryEnvelope` | Response contract: rows, columns, `freshness_ms`, `rate_limit_status`, `trace_id` |
| 2 | **`TestTenantDerivedFromIssuerNotClaim`** | ADR-011 - a forged `tenant_id` is ignored |
| 3 | `TestCapabilityClassification` | ENFORCED -> push; ADVISORY -> retain |
| 4 | `TestRLSInjectedAsFilter` | Policy compiles into plan nodes |
| 5 | **`TestInvariantFailsClosedOnDroppedPredicate`** | ADR-002 plan-time check |
| 6 | `TestCLSMaskApplied` / `TestResidualColumnStripped` / `TestOverProjectionAppliesToEnforcedToo` / `TestMissingPredicateColumnFailsClosed` | Masking; column trimming; over-projection applies to `ENFORCED` too; FLS-hidden predicate column fails closed |
| 7 | `TestHonestConnectorZeroViolations` | The check isn't always-on |
| 8 | **`TestLyingConnectorFailsClosed`** | ADR-002 runtime verification filter |
| 9 | **`TestPlanCacheDoesNotLeakAcrossRoles`** | ADR-003 pre-policy caching is a vulnerability |
| 10 | `TestRateLimitExhausted` / `TestAsyncReroute` | 429 + `Retry-After`; 202 + poll URL |
| 11 | `TestMaxStalenessCacheHit` / `TestETagRevalidation` | ADR-005 freshness |
| 12 | **`TestSemiJoinReducesProbeCalls`** | ADR-007 + pushdown-creativity bonus |
| 13 | `TestSourceTimeoutPartialResults` | ADR-009 terminal frame |
| 14 | `TestTraceIDPropagates` / `TestConnectorDurationMetric` | Observability |
| 15 | `TestCacheRatioByPrincipalCount` (k6) | **Section 5.3 - the doc's stated riskiest assumption** |
| 16 | **`TestFreshnessCacheIsolatedByPrincipal`** | ADR-002's result-cache key addendum - layer 3 can differ per user, so the cache must too |
| 17 | `TestResultCacheHitRatioMetric` | `result_cache_requests_total{outcome}` - the counter Section 9 says the ratio derives from |

---

## Cycle 0 - Walking skeleton (30 min)

Red first, and make it the *contract*:

```go
// test/acceptance/envelope_test.go
func TestQueryEnvelope(t *testing.T) {
 gw := harness.Start(t)
 res := gw.POST("/v1/query", `{"sql":"SELECT id FROM sf.accounts"}`,
 harness.Token("support"))

 require.Equal(t, 200, res.Code)
 require.NotEmpty(t, res.Body.Columns)
 require.NotEmpty(t, res.Body.TraceID)
 require.NotNil(t, res.Body.FreshnessMS)
 require.Contains(t, res.Body.RateLimitStatus, "sf")
}
```

Green with hardcoded rows. **Done when** the envelope required by the brief is locked in
before any logic exists - every later cycle fills it in rather than reshaping it.

---

## Cycle 1 - Identity (20 min)

```go
func TestTenantDerivedFromIssuerNotClaim(t *testing.T) {
 raw := `{"iss":"https://acme-corp.okta.example","sub":"u_8f31c2",
 "tenant_id":"t_evilcorp","groups":["8f3c-4d21"]}`

 p, err := identity.Resolve(raw, testdata.IssuerRegistry())

 require.NoError(t, err)
 require.Equal(t, "t_acme", p.Tenant) // from iss
 require.NotEqual(t, "t_evilcorp", p.Tenant) // claim ignored
 require.Equal(t, []string{"support_agent"}, p.Roles)
}
```

Implement: parse-unverified -> `registry.ByIssuer(iss)` -> group->role map -> attribute lookup.
Signature verification is mocked; **derivation is real** (see `DESIGN.md` ADR-011).

**Done when** the hostile fixture resolves to the correct tenant.

---

## Cycle 2 - Plan tree + capability classification (45 min)

The largest cycle. Parse with `vitess/go/vt/sqlparser`, convert to your own tree - don't
hand-roll a parser.

```go
func TestCapabilityClassification(t *testing.T) {
 cat := catalog.Load("testdata/catalog") // sf.status=ENFORCED, sf.region=ADVISORY
 p := plan.Build("SELECT id,region FROM sf.accounts WHERE status='open'", cat,
 policy.Residuals{{Table: "sf.accounts", Expr: "region = $principal.region"}})

 require.Equal(t, plan.PushedEnforced, p.VerdictFor("status = 'open'").Disposition)
 require.Equal(t, plan.Residual, p.VerdictFor("region = $principal.region").Disposition)
 require.Contains(t, p.Scan("sf.accounts").Pushed, "status")
 require.NotContains(t, p.Scan("sf.accounts").Pushed, "region")
}
```

Types:

```go
type Node interface{ Children() []Node }
type Scan struct{ Table string; Project []string; Pushed []Predicate }
type Filter struct{ Child Node; Pred Predicate; Site Site } // Origin: USER | SECURITY
type Project struct{ Child Node; Cols []Col } // Col.Mask != nil => CLS
type Join struct{ Left, Right Node; On Equi }
```

**Done when** the same predicate classifies differently against two capability profiles.

**Deferred optimization, noted not fixed.** ADR-002's capability table says `ADVISORY`
predicates should still be pushed as a volume optimization when they're not security-origin -
only *security* predicates are barred from pushing to `ADVISORY` (that's the over-filtering risk
ADR-002 discusses separately). The classifier here doesn't check `Origin` at all: it treats
`ADVISORY` as never-pushed for every predicate. Safe (over-conservative - costs bandwidth, not
correctness) and untested either way, since no fixture has a user-origin predicate against an
`ADVISORY` column. Revisit if a fixture ever needs that distinction to matter.

---

## Cycle 3 - RLS injection + invariant (30 min)

```go
func TestRLSInjectedAsFilter(t *testing.T) {
 p := buildFor(t, "support", "SELECT id FROM sf.accounts")
 f := plan.FindFilter(p, "region")
 require.NotNil(t, f, "RLS must appear as a Filter node")
 require.Equal(t, plan.SecurityOrigin, f.Pred.Origin)
 require.Equal(t, plan.Local, f.Site)
}

func TestInvariantFailsClosedOnDroppedPredicate(t *testing.T) {
 p := buildFor(t, "support", "SELECT id FROM sf.accounts")
 plan.RemoveFilter(p, "region") // simulate an optimizer eliminating it
 require.ErrorIs(t, plan.AssertInvariant(p), plan.ErrEntitlementDenied)
}
```

The invariant: every `SECURITY` predicate is `PUSHED_ENFORCED` **or** present in the residual
set. Runs after parameter binding, **including on plan-cache hits**.

**Why `RemoveFilter` is fault injection, not a workaround.** The bug ADR-002 says this invariant
catches is a *later* stage eliminating a correctly-injected security filter - a rule-based
optimizer rewrite (filter merging, constant folding, join reordering) removing a node between
injection and execution. The v1 planner has no such stage: `Build` classifies and injects a
predicate atomically, so it is invariant-preserving by construction - no catalog or policy
fixture can make it emit a plan missing a security filter. That means the failure mode can't be
reached through `Build`'s own inputs; the only way to test that `AssertInvariant` actually
catches a missing predicate is to construct the bad state directly, i.e. mutate an already-built
tree. `RemoveFilter` stands in for the optimizer bug this codebase hasn't built yet. The cost:
the invariant needs a pre-mutation snapshot of what *should* be there (`Plan.security`) to check
the post-mutation tree against, since deriving expectations from the tree after mutating it would
just be checking the tree against itself. Revisit once a real rewrite stage exists (post-M1,
ADR-001) - test against a deliberately-buggy rewrite rule instead, and drop both `RemoveFilter`
and the snapshot.

---

## Cycle 4 - CLS + residual column hygiene (30 min)

```go
func TestCLSMaskApplied(t *testing.T) {
 rows := runAs(t, "support", "SELECT email FROM sf.accounts")
 require.Equal(t, sha256hex("dana@acme-corp.example"), rows[0]["email"])
}

func TestResidualColumnStripped(t *testing.T) {
 // region is fetched to satisfy the local filter, then trimmed from output
 res := runAs(t, "support", "SELECT id FROM sf.accounts")
 require.NotContains(t, res.Columns, "region")
 require.Contains(t, res.Debug.FetchedColumns, "region")
}

func TestOverProjectionAppliesToEnforcedToo(t *testing.T) {
 // ENFORCED => predicate is pushed, but the verification filter still needs the column
 sf := mocksf.Start(t, mocksf.Capability("region", catalog.Enforced))
 res := runAgainst(t, sf, "support", "SELECT id FROM sf.accounts")
 require.Contains(t, res.Debug.FetchedColumns, "region")
 require.NotContains(t, res.Columns, "region")
}

func TestMissingPredicateColumnFailsClosed(t *testing.T) {
 // source hides the column the RLS rule depends on (e.g. Salesforce FLS)
 sf := mocksf.Start(t, mocksf.HideColumn("region"))
 res := runAgainst(t, sf, "support", "SELECT id FROM sf.accounts")
 require.Equal(t, 403, res.Code) // not: silently return zero rows
 require.Equal(t, "ENTITLEMENT_DENIED", res.Body.Error.Code)
}
```

**Required columns at the scan** = output projection + local-predicate refs + join keys +
mask refs, computed bottom-up, then trimmed by the top `Project`. The naive implementation
derives them from the output projection alone; the residual filter then evaluates against a
missing field, and in Go the zero value makes `"" != "EMEA"` drop **every** row - an empty
result with no error, indistinguishable from a user who legitimately has no accounts. The
fail-open variant of the same bug leaks everything. This is the highest ratio of
severity-to-obviousness in the whole build, which is why it gets three tests.

---

## Cycle 5 - Connector SDK + Salesforce mock (40 min)

Mock behaviours: capability-declared filtering, pagination, `ETag`/`If-None-Match` -> 304,
`429` + `Retry-After`, configurable delay, and a **`--lie-about=region`** flag.

```go
func TestConnectorPaginationAndETag(t *testing.T) {
 sf := mocksf.Start(t, mocksf.Rows(250), mocksf.PageSize(100))
 rows, meta, _ := connector.Fetch(ctx, sf.URL, connector.Req{Table: "accounts"})
 require.Len(t, rows, 250)
 require.Equal(t, 3, sf.CallCount())

 _, meta2, _ := connector.Fetch(ctx, sf.URL, connector.Req{Table: "accounts", ETag: meta.ETag})
 require.True(t, meta2.NotModified)
}
```

---

## Cycle 6 - Verification filter (30 min) [KEY]

The headline. Two tests, and you need both - the negative one proves the check isn't
trivially always-firing.

```go
func TestHonestConnectorZeroViolations(t *testing.T) {
 sf := mocksf.Start(t, mocksf.Capability("region", catalog.Enforced))
 res := runAgainst(t, sf, "support", "SELECT id,region FROM sf.accounts")
 require.Equal(t, 200, res.Code)
 require.Zero(t, obs.EnforcedPredicateViolations.Value())
}

func TestLyingConnectorFailsClosed(t *testing.T) {
 sf := mocksf.Start(t,
 mocksf.Capability("region", catalog.Enforced), // declares it enforces
 mocksf.LieAbout("region")) // ...and ignores the filter

 res := runAgainst(t, sf, "support", "SELECT id,region FROM sf.accounts")

 require.Equal(t, 403, res.Code)
 require.Equal(t, "ENTITLEMENT_DENIED", res.Body.Error.Code)
 require.Empty(t, res.Body.Rows, "must not serve rows from a connector that lied")
 require.Positive(t, obs.EnforcedPredicateViolations.Value())
}
```

Implement: after fetch, re-apply every `PUSHED_ENFORCED` **security** predicate locally. Any
row dropped => the connector's behaviour diverged from its declaration => fail closed.

**Note the plan-time invariant passes in the lying case** - the predicate *was* legitimately
pushed to a target that claimed enforcement. Different bug class, different mechanism.

**Two gaps folding in here, both because this is the first cycle with real end-to-end wiring.**
`runAgainst(t, sf, "support", sql)` is the first test that goes through a real connector and a
real HTTP-shaped response - everything before it (Cycles 3-4) used in-process Go helpers
(`buildFor`, `runQuery`) precisely to avoid wiring the gateway handler before there was a real
connector to wire it to. Two things were deferred waiting for exactly this point:

1. **`testdata/tokens/{dana,root}.jwt` don't exist yet.** `harness.Token` has been returning
 placeholder strings since Cycle 0. This is the cycle real Bearer tokens (and real
 `identity.Resolve` wiring into the handler) become necessary - create them here.
2. **L1 object-level authorization has no test anywhere in this plan**, despite `DESIGN.md`'s
 "What the prototype enforces (not deferred)" section explicitly claiming "object-level authz
 rejects an out-of-scope table at admission" as one of three layers proven end-to-end. It
 doesn't belong in Cycle 4 (CLS/column hygiene is a Layer 2 concern; L1 is pre-plan admission,
 a different layer entirely per ADR-002's sequence diagram) - it belongs here, where a real
 admission path first exists. Add: `policy.Provider.ObjectDenied(role, table) bool` reading
 `RolePolicy.Objects.Deny`, checked before `plan.Build` is even called, failing
 `ENTITLEMENT_DENIED` before any planning happens. Without this, the design doc's own claim
 about what the prototype proves is unbacked by the test suite.

---

## Cycle 7 - Plan cache (20 min)

```go
func TestPlanCacheDoesNotLeakAcrossRoles(t *testing.T) {
 const sql = "SELECT id,email FROM sf.accounts"
 admin := planFor(t, "admin", sql)
 support := planFor(t, "support", sql)

 require.NotEqual(t, admin.CacheKey, support.CacheKey)
 require.False(t, support.CacheHit, "support must not receive admin's plan")
 require.Contains(t, support.Masks, "email")
 require.NotContains(t, admin.Masks, "email")
}

func TestPlanCacheHitsOnSameShapeDifferentValue(t *testing.T) {
 a := planFor(t, "support", "SELECT id FROM sf.accounts WHERE status='open'")
 b := planFor(t, "support", "SELECT id FROM sf.accounts WHERE status='closed'")
 require.Equal(t, a.CacheKey, b.CacheKey) // parameterized
 require.True(t, b.CacheHit)
}
```

Key: `(sql_shape, tenant, policy_version, policy_shape_hash, cap_version, role_set_hash)`.
The pair of tests demonstrates both halves - safe *and* actually useful.

**Note (Cycle 13).** `TestPlanCacheHitsOnSameShapeDifferentValue` proves two things, not one:
same-shape queries share a cache entry, and each query's own WHERE-clause literal still reaches
the connector on a cache hit. The cached `Plan` never carries a resolved value for either kind
of predicate - `$principal.<attr>` (this cycle) and ordinary literals (`$param.N`, resolved via
`plan.ExtractParams` against each request's own SQL) are both bound fresh per call.

---

## Cycle 8 - Rate limits + async (30 min)

```go
func TestRateLimitExhausted(t *testing.T) {
 gw := harness.Start(t, harness.Bucket("sf", 3))
 for i := 0; i < 3; i++ { gw.Query("support", simpleSQL) }
 res := gw.Query("support", simpleSQL)

 require.Equal(t, 429, res.Code)
 require.Equal(t, "RATE_LIMIT_EXHAUSTED", res.Body.Error.Code)
 require.NotEmpty(t, res.Header("Retry-After"))
 require.Contains(t, res.Body.Error.Message, "sf") // names the connector
}

func TestAsyncReroute(t *testing.T) {
 res := gw.QueryWithHeader("support", simpleSQL, "Prefer", "respond-async")
 require.Equal(t, 202, res.Code)
 require.NotEmpty(t, res.Body.PollURL)
 require.Eventually(t, func() bool { return gw.Poll(res.Body.PollURL).Done }, 5*time.Second, 100*time.Millisecond)
}
```

Async is an in-memory map. The rubric point is the reroute path existing, not a queue.

---

## Cycle 9 - Freshness (30 min)

```go
func TestMaxStalenessServesCache(t *testing.T) {
 gw.Query("support", sql, freshness.MaxStaleness("60s"))
 before := sf.CallCount()
 res := gw.Query("support", sql, freshness.MaxStaleness("60s"))

 require.Equal(t, before, sf.CallCount(), "within TTL => no live fetch")
 require.True(t, res.Body.Meta.CacheHit)
 require.Less(t, res.Body.FreshnessMS, int64(60_000))
}

func TestETagRevalidationSpendsBudget(t *testing.T) {
 clock.Advance(90 * time.Second)
 before := gw.RateLimitRemaining("sf")
 res := gw.Query("support", sql, freshness.MaxStaleness("60s"))

 require.True(t, res.Body.Meta.Revalidated)
 require.Less(t, gw.RateLimitRemaining("sf"), before,
 "ADR-005: a freshness probe spends a token")
}
```

That second assertion is the one worth having - freshness that silently consumes quota is a
quota leak with good PR, and the doc says so.

---

## Cycle 10 - Zendesk mock + semi-join (40 min) [KEY]

**Interface debt due here.** `connector.FetchRequest.Filters` is `map[string]string` -
single-value equality only, built for the v1 conjunctive-`WHERE` surface. The semi-join rewrite
needs to push the build side's join keys into the probe side as an `IN (val1, val2, ...)`
predicate - a shape the current type can't express. This needs to grow (a `map[string][]string`,
or a small predicate type) before `mockzd.MaxInList` and the probe-side fetch can be implemented,
not after.

```go
func TestSemiJoinReducesProbeCalls(t *testing.T) {
 sf := mocksf.Start(t, mocksf.Accounts(500, "EMEA"))
 zd := mockzd.Start(t, mockzd.Tickets(50_000, "open"), mockzd.MaxInList(200))

 res := runJoin(t, sf, zd, `
 SELECT a.name, t.subject FROM sf.accounts a
 JOIN zd.tickets t ON t.organization_id = a.external_id
 WHERE t.status = 'open'`)

 require.Equal(t, 200, res.Code)
 require.LessOrEqual(t, zd.CallCount(), 20, "semi-join: 3 chunks not 500 pages")
 require.Equal(t, "semi_join", res.Body.Meta.JoinStrategy)
 require.Greater(t, res.Body.Meta.NaiveCallEstimate, 400)
}
```

`DESIGN.md` gives cross-app joins their own P95 < 4 s SLO. The prototype does not assert it -
at mock-source latencies the number is meaningless. Call counts are the honest proxy here, and
they are what the rewrite actually changes.

Log line to emit - this is the pushdown-creativity evidence, and it needs to be legible in
one line:

```
join.strategy=semi_join build=sf.accounts build_rows=500 keys=500 chunks=3
probe_calls=12 probe_calls_naive=500 total_calls=17 total_calls_naive=505
selectivity=0.024 reduction_total=29.7x
```

Report **total-to-total** (29.7x), not probe-to-total. Rate-limit budget is spent on every
call, and the build side is a fixed cost paid in both plans - which is what dilutes the
probe-side 41.7x down to 29.7x. Log `selectivity` alongside it, because the reduction *is*
the join key's selectivity on the probe side and the ratio is meaningless without it.

**Join grammar restrictions, undocumented until now.** The implementation only accepts:
one equi-join (two tables, not a chain of three+), exactly one column-to-column equality
in the ON clause (no `AND`-composed multi-column join keys), and WHERE conjuncts of the
existing single-table `column op literal` shape, each scoped to one aliased table - a
cross-table WHERE predicate (`WHERE a.x = b.y`) is rejected the same way a literal in the
ON clause is (both fail a type-assert expecting the "other" shape). Build/probe side
selection is also positional, not cost-based: the FROM (left) table is always build, the
JOIN (right) table always probe, since there are no cardinality stats to choose from
otherwise. None of these rejections has a test - `TestSemiJoinReducesProbeCalls` only
exercises the one supported shape. Revisit if a future cycle needs more than a single
two-table equi-join.

---

## Cycle 11 - Timeout + partial results (25 min)

```go
func TestSourceTimeoutPartialResults(t *testing.T) {
 zd := mockzd.Start(t, mockzd.Delay(5*time.Second))
 res := gw.QueryStream("support", singleSourceSQL(zd), gw.Timeout(1*time.Second))

 frames := res.NDJSON()
 last := frames[len(frames)-1]
 require.True(t, last.IsTerminal)
 require.True(t, last.Partial)
 require.Equal(t, "SOURCE_TIMEOUT", last.Sources["zd"].Error)
 require.NotEmpty(t, last.TraceID)
}
```

**First cut candidate** if you're behind - see the ladder below.

---

## Cycle 12 - Observability (20 min)

```go
func TestConnectorDurationMetric(t *testing.T) {
 gw.Query("support", sql)
 require.Positive(t, obs.Gather("connector_request_duration_seconds",
 map[string]string{"connector": "sf"}).SampleCount)
}

func TestTraceIDPropagates(t *testing.T) {
 res := gw.Query("support", sql)
 require.Equal(t, res.Body.TraceID, sf.LastRequest().Header.Get("X-Trace-Id"))
}
```

---

## Cycle 13 - Compose, k6, README (40 min)

k6 **parameterized by distinct principal count** - this is what turns a load test into
evidence for `DESIGN.md` Section 5.3:

```js
// k6/load.js - run: k6 run -e PRINCIPALS=1 ... then 10, 100
const principals = __ENV.PRINCIPALS || 10;
export default function () {
 const tok = tokens[__VU % principals];
 http.post(`${BASE}/v1/query`, payload, { headers: { Authorization: `Bearer ${tok}` } });
}
```

Record `result_cache_hit_ratio` at each setting. Three data points, one small table in the
README, and the doc's "single most consequential unknown" now has measured evidence behind
it. Almost nobody does this.

Compose profiles: `core`, `mocks`, `testing`. Three commands:

```bash
docker compose --profile core --profile mocks up -d
go test ./... # ~20 tests, incl. the lying-connector test
docker compose --profile testing run k6
```

---

## Cycle 14 - Result cache key correctness (25 min) [KEY]

**Why this cycle exists.** `internal/freshness`'s cache key was `table|columns|filters` -
no principal, no tenant. `DESIGN.md` ADR-002 says the result cache "must be keyed by
principal" and explains why (layer 3 - source-side sharing rules apply under the *calling*
principal's own delegated token, and can differ per user for an identical query, independent
of anything our own RLS computes). The implementation didn't match the design. Nothing caught
it because `mocksf`/`mockzd` don't model per-principal source-side ACLs at all - every caller
gets the same rows for the same query - so the mocks are structurally incapable of exercising
layer 3 divergence. That means the test below can't prove "no cross-principal data leak" by
comparing row *content* (the mocks would return the same content either way); it proves the
*mechanism* instead - that two different principals never share a cache entry - the same way
`TestSemiJoinReducesProbeCalls` proves a call-count property rather than inspecting rows.

```go
func TestFreshnessCacheIsolatedByPrincipal(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	gw.QueryFresh("support", simpleSQL, "60s") // dana - u_8f31c2
	gw.QueryFresh("admin", simpleSQL, "60s")    // root - u_root001

	require.Equal(t, 2, sf.CallCount(),
		"two principals, identical query, must not share one cache entry")
}
```

This is a real red test against the pre-fix code, not a hypothetical: `simpleSQL` (`SELECT id
FROM sf.accounts`) has no `WHERE` clause, so both principals' bound `FetchRequest` has empty
`Filters` - identical `table|columns|filters` signatures - and the old key collapses them into
one entry. `sf.CallCount()` is 1 before the fix, 2 after.

**The regression guard already exists** - Cycle 9's `TestMaxStalenessServesCache` asserts the
*same* principal's second identical query within budget costs zero extra calls. Rerun it after
the fix rather than duplicating it; if the principal-scoping change ever turns the cache into a
no-op for repeat callers, that test catches it.

```go
func TestResultCacheHitRatioMetric(t *testing.T) {
	obs.ResetMetrics()
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	gw.QueryFresh("support", simpleSQL, "60s") // miss - first time this principal asks
	gw.QueryFresh("support", simpleSQL, "60s") // hit - within budget

	require.Equal(t, 1, obs.Gather("result_cache_requests_total",
		map[string]string{"connector": "sf", "outcome": "hit"}).SampleCount)
	require.Equal(t, 1, obs.Gather("result_cache_requests_total",
		map[string]string{"connector": "sf", "outcome": "miss"}).SampleCount)
}
```

**Implement:**
1. `freshness.Source` gains a `Principal string` field; `internal/server`'s `buildAndRoute`
   sets it to `principal.Tenant + "|" + principal.Sub` when constructing each connector's
   wrapper - the two fields ADR-002's key addendum names, joined into the one string field the
   cache key function takes. (`table` already carries the connector prefix, e.g. `sf.accounts`
   vs `zd.tickets`, from `plan.ParseTables` - no separate connector label needed in the key.)
2. `cacheKey(req, principal)` folds `principal` into the signature it hashes.
3. Two Prometheus counter increments at the point `Fetch` already knows the outcome - the
   `age <= s.MaxStaleness` branch is a `hit`, falling through to a live/conditional fetch is a
   `miss` - fed to both `obs.Observe` (in-memory, what the tests above assert) and a new
   `obs.ResultCacheRequests` `CounterVec{connector, outcome}` (the real `/metrics` series),
   mirroring exactly how `timedFetch` already double-feeds
   `connector_request_duration_seconds`. Recorded only inside the `MaxStaleness > 0` branch -
   when caching is off entirely, there's no cache decision being made to report.

**Done when** `TestFreshnessCacheIsolatedByPrincipal` and `TestResultCacheHitRatioMetric` are
green, `TestMaxStalenessServesCache` and `TestETagRevalidationSpendsBudget` are still green
(the fix must not regress the cases it isn't about), and `curl localhost:8080/metrics | grep
result_cache_requests_total` shows real `hit`/`miss` counts after driving traffic through a
running gateway - the same standard Section 9's `enforced_predicate_violations_total` already
holds itself to: a metric that exists and is queryable, not just a log line.

---

## Fixtures

```json
// testdata/catalog/sf.accounts.json
{ "table": "sf.accounts",
 "predicates": {
 "status": { "ops": ["="], "enforcement": "ENFORCED" },
 "region": { "ops": ["="], "enforcement": "ADVISORY" }
 },
 "masking": "unsupported", "join_key_in_list": true, "max_in_list": 200 }
```

```json
// testdata/policy/support_agent.json
{ "role": "support_agent",
 "rls": [{ "table": "sf.accounts", "expr": "region = $principal.region" }],
 "cls": [{ "table": "sf.accounts", "column": "email", "fn": "sha256" }],
 "objects": { "deny": ["sf.opportunities"] },
 "policy_version": 47 }
```

```json
// testdata/tokens/dana.jwt - hostile on purpose. Extension matches the
// README's quickstart curl command; content is raw claims JSON, not a
// signed JWT - see ADR-011's "signature verification mocked" scope.
{ "iss": "https://acme-corp.okta.example", "sub": "u_8f31c2",
 "tenant_id": "t_evilcorp", "groups": ["8f3c-4d21"] }
```

`sf.region` is declared `ADVISORY` **specifically so the residual path is exercised**. Real
Salesforce would enforce it. Say this in the README or it looks like a modelling error.

---

## Prioritisation if scope tightens

Ordered by what can be dropped with least loss. Everything below the line is load-bearing.

1. Cycle 11 (timeout / NDJSON) - the design covers it; the prototype needn't
2. `TestAsyncReroute` - keep the 429, drop the 202 path
3. ETag revalidation - keep `max_staleness` cache-hit-vs-live
4. Third k6 data point - two still shows the trend
5. Pagination in the mocks - return one page

 - do not cut below this line -

6. Lying-connector test, semi-join call count, plan-cache role isolation, tenant-from-issuer

These four are the only artifacts that *prove* the design's claims rather than restating them.
Everything else demonstrates competence; these demonstrate correctness.
