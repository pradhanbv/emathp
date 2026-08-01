# Universal SQL Across Enterprise Apps

Federated SQL over Salesforce + Zendesk, generalizing to 1,000s of SaaS app types. One cross-app
query end-to-end: **auth → entitlement checks → rate-limit handling → freshness control**.

| Doc | For | Read time |
|---|---|---|
| **This file** | Orientation, quickstart, what's proven and what isn't | ~7 min |
| [`DESIGN_FINAL.md`](./DESIGN_FINAL.md) | All 11 ADRs, both capability ladders, capacity math, six-month plan | ~25 min |
| [`DESIGN_FULL.md`](./DESIGN_FULL.md) | Canonical — worked derivations, full rejection reasoning, 16 diagrams | ~90 min |
| [`REJECTED_ALTERNATIVES.md`](./REJECTED_ALTERNATIVES.md) | Every option considered and turned down, with its steelman | ~15 min |
| [`IMPLEMENTATION_PLAN.md`](./IMPLEMENTATION_PLAN.md) | The TDD build log, cycle by cycle | ~20 min |

---

## Quickstart

```bash
docker compose --profile core --profile mocks up -d   # gateway + 2 mock SaaS sources
go test ./...                                          # 30 tests, ~1s
docker compose --profile testing run --rm k6           # load test: 500 req/s for 60s
```

**The demo that matters** — same SQL, two identities:

```bash
# Support agent: RLS restricts to their region, email is masked
curl -s localhost:8080/v1/query \
 -H "Authorization: Bearer $(cat testdata/tokens/dana.jwt)" \
 -d '{"sql":"SELECT id, name, email, region FROM sf.accounts","max_staleness":"60s"}' | jq

# Admin: no region filter, email in clear
curl -s localhost:8080/v1/query \
 -H "Authorization: Bearer $(cat testdata/tokens/root.jwt)" \
 -d '{"sql":"SELECT id, name, email, region FROM sf.accounts","max_staleness":"60s"}' | jq
```

Every response carries `freshness_ms`, `rate_limit_status`, `trace_id`.

---

## Four tests carry the submission

Each exists because it proves a design claim a reviewer would otherwise take on faith.

| Test | Claim it proves | ADR |
|---|---|---|
| `TestLyingConnectorFailsClosed` | A connector that declares a predicate `ENFORCED` then ignores it is **caught at runtime and fails closed** — RLS survives a connector whose behaviour diverges from its declaration | 002 |
| `TestPlanCacheDoesNotLeakAcrossRoles` | Caching a plan *before* policy injection is a privilege-escalation vector; the composite cache key prevents it. Built even though the in-process planner is cheap — the bug class covers the result cache too | 003 |
| `TestSemiJoinReducesProbeCalls` | Cross-app join rewritten as a semi-join: **505 → 17 connector calls (29.7×)** at 2.4% join-key selectivity. The ratio *is* selectivity | 007 |
| `TestTenantDerivedFromIssuerNotClaim` | A token asserting `tenant_id: t_evilcorp` resolves to `t_acme` — tenant comes from the verified issuer, the claim is never read | 011 |

```bash
go test ./... -run 'LyingConnector|PlanCacheDoesNotLeak|SemiJoin|TenantDerived' -v
```

**Why the lying-connector test is the important one.** The realistic failure isn't vendor
dishonesty — it's *our own* connector sending `?region=EMEA` where the API expects
`?filter[region]=EMEA`. Most REST frameworks **silently ignore unknown query parameters** and
return the unfiltered set: an RLS filter that appears pushed, does nothing, and leaks the full
table with a `200`. So every `PUSHED_ENFORCED` security predicate is re-applied locally after
fetch, with two metrics carrying opposite expectations:

| Metric | Expected | Meaning |
|---|---|---|
| `residual_filter_rows_dropped` | Non-zero | Normal — the cost of the `ADVISORY` path |
| `enforced_predicate_violations_total` | **Zero** | Non-zero ⇒ a connector diverged from its declaration. **Page someone.** |

---

## MVP status at a glance

Eleven ADRs, grouped by *how real* the prototype's version of each is rather than by ADR number —
because several ADRs land in more than one lane at once. ADR-007 alone is fully built (semi-join),
entirely absent (DuckDB), and undecided (`RESULT_TOO_LARGE`) in three different places, which a
single per-ADR verdict would flatten into one misleading word.

| Lane | Contents |
|---|---|
| **🟢 Built & verified**<br>real code, real tests, real HTTP round trips | Go planner + capability classification · RLS/CLS injection + plan-time invariant + runtime verification filter (002) · parameterized role-isolated plan cache (003) · semi-join rewrite, 505→17 calls (007) · tenant from verified `iss`, never a claim (011) · real Prometheus histogram + real OTel trace · result cache keyed by principal |
| **🟡 Partial**<br>real mechanism, deliberately narrowed scope | Freshness rungs 1 & 4 + `max_staleness` only (005) · rate limits: single-node bucket, `429`, async reroute — no Redis lease, no fair queue, **no per-tenant dimension** (006) · NDJSON + `SOURCE_TIMEOUT` terminal frame, thin coverage (009) · policy injection real, OPA mocked (002) · identity derivation real, signature verification mocked (011) |
| **⚪ Mocked / not built**<br>infrastructure a reviewer can assume | Salesforce + Zendesk connectors are mocks (004) · Calcite sidecar + Substrait IR deferred to M1 spike (001) · materialization is an in-memory Go hash join, not DuckDB (007) · tenant lifecycle API (008) · per-tenant KMS + crypto-shred (010) · **audit trail (010) — no access log exists; nothing to review post-incident** |
| **🔵 Open question**<br>not decided, not just unbuilt | `LIMIT`/`OFFSET` — same implementation layer as projection but less MVP value, which is likely why the gap wasn't caught; still undecided past the grammar (007) · `RESULT_TOO_LARGE` — guardrail specified in 007, never implemented. **The sharper risk: a skewed join can exhaust memory today** |

**The pattern.** Every unbuilt ADR maps to a requirement about *infrastructure* — a JVM sidecar,
a vendor contract, Terraform, a KMS call. Every built one maps to a requirement about *behaviour
under adversarial conditions*. **We built what a reviewer cannot take on faith.**

---

## Measured: the number the design is least sure about

Per-principal cache hit ratio is *"the single most consequential unknown"* in the design.
Per-user delegated tokens make entitlements correct essentially for free, but force per-principal
cache keys and collapse locality. **The capacity model assumes 30%.** The k6 script is
parameterized by distinct principal count, so this is measured rather than assumed — every row
below is **500 req/s for 60 s (30,001 requests), 0 failures**, gateway restarted between runs so
a warm cache can't inflate the next:

```bash
docker compose --profile testing run --rm -e PRINCIPALS=100 k6   # then 1000, then 10000
```

| Distinct principals | `result_cache_hit_ratio` | Connector calls (of 30,001) | p95 |
|---|---|---|---|
| 1 | 99.99% | 3 | 398 µs |
| 100 | 99.33% | 200 | 420 µs |
| 1,000 | 93.33% | 2,000 | 491 µs |
| **10,000** | **33.33%** | **20,000** | **713 µs** |

- **Connector calls track distinct principals exactly** — `principals × 2` in every row: each
  principal misses once on first fetch, once more when its 30 s `max_staleness` expires midway
  through the 60 s run.
- **Cross-checked three ways** — k6's client-side `meta.cache_hit`, the gateway's
  `result_cache_requests_total{outcome}`, and `connector_request_duration_seconds_count`. All
  three agree exactly on the 10,000-principal run (10,001 hits / 20,000 misses / 20,000 calls).
- **What it means:** hit ratio holds well past the model's 30% assumption — *but only while
  principal count stays small relative to request volume.* At 10,000 principals it falls to 33%,
  and p95 rises in step, because a miss is a real connector call. At 10M users principal count
  dominates request volume by orders of magnitude, so **connector quota — not our fleet — becomes
  the binding constraint**, and quota can't be autoscaled past.
- **Latency caveat:** sub-millisecond p95 against in-process mocks says nothing about the 1.5 s
  SLO, which real SaaS latency dominates. The p95 column is good for its *shape* (it moves with
  miss rate), not its magnitude.

**A real bug, found while building this table.** Two requests sharing a cached plan (same SQL
shape, different `WHERE` literal) were silently serving whichever literal built the plan — the
plan cache's own hit-ratio pressure triggered it on any two same-shaped, differently-valued
queries. `$principal.<attr>` values were already resolved lazily per call; ordinary literals were
not, until this table required varying one. Fixed in
`TestPlanCacheDoesNotStaleBindLiteralValues`.

---

## Observability: one dashboard, one trace

| Artifact | What it proves |
|---|---|
| `jaeger_1.png` | Cross-app join trace — fanout to both connectors runs genuinely **in parallel**, and our own overhead is a small fraction of wall clock. This is the whole basis for excluding upstream source faults from the availability SLI |
| `prom_1.png`, `prom_2.png` | Request-duration histogram and rate-limit budget remaining, by connector and tenant — the load-test run behind the hit-ratio table above |

![Jaeger trace of a cross-app join](./jaeger_1.png)

![Prometheus request-duration and rate-limit budget panels](./prom_2.png)

---

## Recreate every claim yourself

| Claim | How |
|---|---|
| **Entitlement enforcement (RLS/CLS)** | The two-identity demo in Quickstart above |
| **Async reroute** | `Prefer: respond-async` header → returns a `poll_url`; poll it for the result. Doesn't require exhausting a budget first — the header reroutes on request, the same path a client falls back to after a `429` |
| **Rate-limit exhaustion** | Compose runs unlimited by default. Start a second gateway with a budget: `go run ./cmd/gateway --addr :8090 --sf-url http://localhost:8081 --zd-url http://localhost:8082 --sf-limit 3`, then send 4 requests → 3× `200`, then `429` + `Retry-After: 1` |
| **Freshness control** | Vary `max_staleness` across calls and watch `freshness_ms` and the cold/warm/revalidated states in the response |
| **Load / hit ratio** | `docker compose --profile testing run --rm -e PRINCIPALS=10000 k6` |

Exact commands with expected output: `DESIGN_FULL.md`'s recreate section.

---

## Error vocabulary

Every message names **what to do**, not just what broke.

| Code | HTTP | When |
|---|---|---|
| `RATE_LIMIT_EXHAUSTED` | 429 | Budget spent — carries `Retry-After` + async instructions |
| `STALE_DATA` | 200 | Served outside `max_staleness`; a probe would have exceeded budget |
| `ENTITLEMENT_DENIED` | 403 | Policy or source ACL denied — names the resource, never the policy reason |
| `SOURCE_TIMEOUT` | 200 + terminal frame | A source exceeded its budget; partial results labelled honestly |
| `UNSUPPORTED_PREDICATE` | 400 | Plan would require a full scan of a SaaS API |
| `RESULT_TOO_LARGE` | 400 | Materialization would exceed guardrail *(specified, not implemented)* |
| `CONNECTOR_AUTH_FAILED` | 502 | Token refresh failed / grant revoked |
| `SCHEMA_DRIFT` | 409 | Source schema changed under a pinned connector version |
| `PRINCIPAL_UNRESOLVED` | 503 | Attribute resolution failed and cache expired — fail closed |
| `RESIDENCY_VIOLATION` | 403 | Plan would cross a residency boundary |

---

## Afterthought: connector authoring is the real bottleneck

Building this surfaced something the brief frames as one requirement among many, but which is
actually **the constraint the whole product lives or dies on: writing and validating connectors
at 1,000s-of-app-types scale.** Everything else here — planner, policy compilation, join ladder,
capacity model — is work that gets done once and then serves every connector. Connector authoring
is the only cost that scales *linearly with the catalog*, and ADR-004's build-vs-buy split is
better read as a symptom of that than as a solution to it: **building** everything is "1,000
connectors against unversioned vendor APIs," and **buying** everything normalizes away the
per-field pushdown and per-user delegated auth that ADR-002 and the SLO both depend on. Neither
option removes the bottleneck; they just decide who absorbs it.

**So the thing I'd actually try next is LLM-drafted connectors** — 1,000+ app types each speaking
its own dialect (SOQL, GraphQL, REST, Elasticsearch DSL…) is precisely a translation-and-volume
problem, which is what generation is good at, aimed squarely at the one cost that scales with the
catalog.

**The convenient part is a coincidence, not the reason.** `TestLyingConnectorFailsClosed` was
built for an entirely different purpose — catching a connector that declares a predicate
`ENFORCED` and then ignores it, most likely our *own* connector with a malformed query parameter.
It happens to be **provenance-blind**: it never asks who wrote a connector, only whether it
behaves. So the validation harness that makes generated connectors safe to accept already exists,
having been built for a reason that had nothing to do with LLMs. That removes the obvious
objection to this idea; it isn't what prompted it.

Three places to try it, in the order I'd attempt them:

| # | Where | Why there first |
|---|---|---|
| 1 | **Capability discovery** — draft the `ENFORCED`/`ADVISORY` map from whatever docs exist (OpenAPI in the tidy case; prose and workflow descriptions in the common one) | Declarative output, not code, validated by the gate that already exists. `testdata/catalog/sf.accounts.json` is a handful of hand-written lines for *one* table — the authoring cost at n=1,000 is visible from n=2 |
| 2 | **Schema-drift triage** — diff spec versions, classify breaking vs. additive, draft the patch | Attacks the cost ADR-004 itself calls decisive: *"schema drift alone would consume the team"* |
| 3 | **Request translation** — plan → SOQL/GraphQL/REST/ES DSL | Placed *after* policy injection, so the model only ever **executes** a security decision already made — never makes one |

How connectors get deployed and versioned — independent of who or what authors one — is a
separate question from this suggestion, not a consequence of it: see ADR-004 in
[`DESIGN_FINAL.md`](./DESIGN_FINAL.md).

---

## Layout

| Path | What |
|---|---|
| `cmd/gateway` | The gateway binary |
| `cmd/mocksf`, `cmd/mockzd` | Mock Salesforce / Zendesk sources |
| `internal/{plan,exec}` | Planner, capability classification, semi-join rewrite, join execution |
| `internal/{policy,identity}` | RLS/CLS residuals, issuer→tenant derivation |
| `internal/{ratelimit,freshness}` | Token bucket, freshness ladder + result cache |
| `internal/{connector,server,obs}` | Connector SDK, HTTP surface, OTel/Prometheus |
| `test/acceptance` | End-to-end tests, including the four above |
| `k6/` | Load script, parameterized by principal count |
