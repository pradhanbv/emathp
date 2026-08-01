# Universal SQL Across Enterprise Apps

A federated SQL gateway over Salesforce + Zendesk, generalizing to 1,000s of SaaS app types.
One cross-app query, end-to-end: auth, entitlement checks, rate-limit handling, freshness
control. This file is a 5-minute orientation - depth lives elsewhere:

| File | For |
|---|---|
| [`DESIGN.md`](./DESIGN.md) | 25-minute design read - all 11 ADRs, both capability ladders, capacity math |
| [`DESIGN_FULL.md`](./DESIGN_FULL.md) | Full canonical design - worked derivations, rejected alternatives, sequence diagrams |
| [`DESIGN_LESS.md`](./DESIGN_LESS.md) | Same content as `DESIGN_FULL.md`, denser prose, shorter than it |
| [`REJECTED_ALTERNATIVES.md`](./REJECTED_ALTERNATIVES.md) | Every option seriously considered and turned down |
| [`IMPLEMENTATION_PLAN.md`](./IMPLEMENTATION_PLAN.md) | The TDD build log, cycle by cycle |

---

## Quickstart

```bash
docker compose --profile core --profile mocks up -d # gateway + 2 mock SaaS sources
go test ./... # 30 tests, ~1s
docker compose --profile testing run --rm k6 # load test: 500 req/s for 60s
```

The demo that matters - the same SQL under two identities:

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

Every response carries `freshness_ms`, `rate_limit_status`, `trace_id`. Rate-limit exhaustion,
async reroute, and the freshness cold/warm/revalidated states are all reproducible with one
command each - ask, or see `DESIGN_FULL.md`'s recreate section for the full list.

---

## MVP status at a glance

Same eleven ADRs, grouped by how real the prototype's version of each is - not by ADR number,
since "partial" hides very different kinds of partial.

```mermaid
flowchart TB
    subgraph built["BUILT AND VERIFIED — real code, real tests, real HTTP round trips"]
        direction LR
        PLAN["Go planner + capability<br/>classification<br/><i>spike evidence for ADR-001</i>"]
        RLS["RLS/CLS injection, plan-time<br/>invariant, runtime verification<br/>filter (ADR-002)"]
        CACHE["Parameterized plan cache,<br/>role-isolated (ADR-003)"]
        JOIN["Semi-join rewrite —<br/>505→17 calls, 29.7x (ADR-007)"]
        TENANT["Tenant from verified <code>iss</code>,<br/>never from claim (ADR-011)"]
        OBS["Real Prometheus histogram +<br/>real OTel trace, connector<br/>spans nested under query span"]
        RCACHE["Result cache keyed by principal,<br/>not just table+columns+filters<br/><i>ADR-002 addendum; real<br/>result_cache_requests_total metric</i>"]
    end

    subgraph partial["PARTIAL — real mechanism, deliberately narrowed scope"]
        direction LR
        FRESH["Freshness: rungs 1 &amp; 4 +<br/><code>max_staleness</code> (ADR-005)<br/><i>rungs 2–3 not built</i>"]
        RATE["Rate limits: single-node bucket,<br/>429, async reroute (ADR-006)<br/><i>no Redis lease, no fair queue</i>"]
        STREAM["NDJSON + <code>SOURCE_TIMEOUT</code><br/>terminal frame (ADR-009)<br/><i>thin coverage, at risk</i>"]
        POLICY["Policy: <code>PolicyProvider</code> stub<br/>returns residuals from JSON<br/><i>injection real, OPA mocked</i>"]
        JWT["Identity: issuer→tenant<br/>derivation is real<br/><i>signature verification mocked</i>"]
    end

    subgraph mocked["MOCKED OR NOT BUILT — infrastructure a reviewer can assume"]
        direction LR
        CONN["Salesforce + Zendesk<br/>connectors (ADR-004)<br/><i>mocksf / mockzd, not real APIs</i>"]
        SIDECAR["Calcite sidecar,<br/>Substrait IR (ADR-001)<br/><i>deferred to M1 spike</i>"]
        DUCK["Ephemeral DuckDB<br/>materialization (ADR-007)<br/><i>in-memory Go hash join instead</i>"]
        LIFE["Tenant lifecycle API<br/>(ADR-008) — not implemented"]
        KMS["Per-tenant KMS,<br/>crypto-shred (ADR-010)<br/>— not implemented"]
        AUDIT["Audit trail<br/>(ADR-010) — not implemented<br/><i>no access log exists;<br/>nothing to review post-incident</i>"]
    end

    subgraph open["OPEN QUESTION — not decided, not just unbuilt"]
        direction LR
        LIMITOFFSET["LIMIT / OFFSET<br/>same layer as projection,<br/>less MVP value — gap, not a cut<br/><i>ADR-007: pushdown vs. truncation-only</i>"]
        RTL["RESULT_TOO_LARGE<br/>guardrail specified in ADR-007,<br/>never implemented<br/><i>the sharper risk — a skewed join<br/>can exhaust memory today</i>"]
    end

    classDef builtStyle fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef partialStyle fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef mockedStyle fill:#f3f4f6,stroke:#6b7280,color:#1f2937
    classDef openStyle fill:#dbeafe,stroke:#2563eb,color:#1e3a8a
    class PLAN,RLS,CACHE,JOIN,TENANT,OBS,RCACHE builtStyle
    class FRESH,RATE,STREAM,POLICY,JWT partialStyle
    class CONN,SIDECAR,DUCK,LIFE,KMS,AUDIT mockedStyle
    class LIMITOFFSET,RTL openStyle
```

**What it proves.** Green is exactly the mechanisms a reviewer would otherwise take on faith -
security and correctness, not infrastructure. Grey is every ADR whose unbuilt half is
infrastructure (a JVM sidecar, a KMS call, a Terraform-replacing API) - never a security
mechanism; that split is deliberate, not a shortfall. Blue is a hole in the v1 SQL surface
itself, surfaced by review rather than designed around - see `DESIGN.md`'s Decision register
(ADR-007) and least-confident-decisions section.

---

## One dashboard, one trace

`jaeger_1.png` - a cross-app join trace: fanout to both connectors runs in parallel, and the
merge step is visibly the only serial cost. `prom_1.png` / `prom_2.png` - request-duration
histogram and rate-limit budget remaining, by connector and tenant. What each proves: the SLO is
dominated by external connector latency, not our own overhead - the whole basis for excluding
upstream source faults from the availability SLI (`DESIGN.md`, SLO section).

---

## Rationale in one paragraph

Three enforcement layers for entitlements (object auth, policy-compiled RLS/CLS, source ACLs as
backstop, plus a runtime filter that catches a connector lying about its own capability), a
freshness ladder that never spends rate-limit quota silently, and a four-tier join ladder
(in-memory → DuckDB → on-demand ClickHouse → Spark serverless) sized by a cost estimate, never
by table count. Every "partial" or "not built" item above is a named, deliberate scope cut with
its own reasoning in `DESIGN.md` - not a silent gap. Full trade-off discussion, all rejected
alternatives, and the six-month execution plan: `DESIGN.md` → `DESIGN_FULL.md`.
