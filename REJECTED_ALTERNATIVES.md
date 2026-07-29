# Rejected Alternatives

Companion to `DESIGN.md`. For every option that was genuinely in contention across the eleven
ADRs, this records the strongest real case *for* it, the specific reason it lost anyway, and a
one-line summary of the trade-off. Full reasoning for the alternative that *was* chosen lives in
`DESIGN.md`; this document exists so the road not taken is on record too, not just asserted away.

**Method.** Every rejection here names what the option is genuinely good at before the
constraint that rules it out. A rejection that only lists weaknesses reads as unfamiliarity with
the option; a rejection that concedes the strength and still holds reads as a considered
trade-off. Where a rejection is weak rather than solid, that's stated directly rather than
smoothed over - see "Weakest rejections" at the end.

---

## ADR-001 - Planner runtime

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Trino / Starburst** | Mature federation, huge connector library, real CBO. `SystemAccessControl` has had `getRowFilters`/`getColumnMask` since release 331 (2020), and there is a first-class OPA plugin returning mask expressions from Rego - this is not a naive objection. | Connector model targets JDBC and object storage, not SaaS REST with pagination / rate limits / ETags / OAuth refresh. Credentials are catalog-scoped, cutting against per-principal delegated OAuth. Shared worker pool means no per-tenant isolation without a cluster each. No rate-limit governance concept. | Solves federation, not multi-tenant SaaS federation - the connector layer would need to be written regardless, and the credential and isolation models work against this design. |
| **DataFusion (Rust)** | Extensible rule-based optimizer; `datafusion-substrait` covers logical and physical plans; no JVM, no GC, no hop. | Team capability, not merit - no Rust depth on the team shape (Section 10). | The closest call, which is why ADR-001 is held weakly (Section 12). Revisit if Rust depth is hired or the plan cache underperforms. |
| **Steampipe / Postgres FDW** | Literally this product category, and it already exists. | Single-tenant by construction. No per-tenant key, quota, or fairness model. FDW pushdown is coarse - no per-predicate `ENFORCED`/`ADVISORY` contract, the primitive ADR-002 depends on. | Good evidence the product category is real; not a foundation for a multi-tenant product. |
| **Go-native parser** (vitess, pg_query_go) | One runtime, no hop, and it handles the v1 SQL surface fine - the prototype proves it. | Gives an AST, not an optimizer. Ownership of capability-aware pushdown and residual-predicate correctness - the security-critical part - would fall to hand-written code. | Fine at n=2 (the prototype's own scale). The real question is n=50 connectors and multi-way joins, where owning rewrite-rule correctness for security predicates is the wrong trade. |
| **GraalVM native-image Calcite** | No hop, no JVM warmup. | Native-image needs closed-world compilation, which fights Calcite's runtime code generation and reflection - but only if the *executor* needs native-image too, and only the *planner* is needed here. | An open question, not a settled rejection - this is exactly why ADR-001 is Proposed rather than Accepted. If an M1 spike shows planner-only compilation is reachable, this could still be selected. |
| **Spark** | - | Batch scheduler; never a candidate at a 500 ms P50 budget. | Listed for completeness. Never seriously in contention given the latency budget. |
| **Velox** | High-performance C++ execution engine. | It's an execution engine, not a planner - solves a problem this decision doesn't have. | Out of scope for this decision; not evaluated further. |

---

## ADR-002 - Entitlements

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Post-filter in Go** | Simple, connector-agnostic, always correct. | Rows leave the source and enter our memory before being discarded - that is the leak, plus wasted quota. The `ADVISORY` residual path *is* this, bounded - it is rejected as the default, not as a fallback. | Rejected as a strategy, kept as a bounded fallback - a difference of degree, not kind, which is why `residual_filter_rows_dropped` is monitored rather than the exception being pretended away. |
| **Inject into compiled Substrait in Go** | Keeps policy logic in one language, no planner round-trip. | Substrait uses positional field references; names survive only as root-level hints. Rewriting a compiled binary plan by position fails silently under column reordering. | The right idea at the wrong stage - injection belongs in the logical plan, while columns still have names, not after compilation to a positional form. |
| **OPA as a policy blob store** | Simple, familiar, no Compile API dependency. | Wastes OPA - the planner would then have to interpret Rego semantics itself, duplicating the engine. | The right tool used the wrong way; partial evaluation via the Compile API is the actual value OPA provides here. |
| **Zanzibar (OpenFGA / SpiceDB)** | Genuinely better for *mirroring* source permission graphs - Drive ACLs, Salesforce sharing rules. | Deferred, not dead: assumption A1 (source ACLs enforce themselves via delegated OAuth) means there's nothing to mirror, so no tuple store is needed - for now. | Held as the fallback if delegated OAuth doesn't hold across enough connectors - see ADR-002's revisit trigger and Section 12. |
| **Cedar** | Comparable expressiveness, a stronger formal-verification story, and Cedar's own RFC 0095 uses residual-to-SQL translation as its motivating example - conceptually the same approach. | Maturity, not concept: both partial evaluators sit behind experimental crate features (`partial-eval`, superseded by `tpe`), and the untyped evaluator's own RFC states it can return ill-typed residuals it cannot safely simplify - exactly what would be fed into a plan. OPA's Compile API is GA and documented for this. | Conceptually validated by Cedar's own design docs; rejected on maturity. Worth revisiting once `tpe` stabilizes. |
| **Sampling the runtime verification filter** | Cheaper than checking every request. | A security control that runs some fraction of the time lets a lying connector through the rest of the time. | Not a control if it's probabilistic. |
| **Pushing security predicates to `ADVISORY` connectors as an optimization** | Would cut fetch volume while the local filter preserves correctness. | Safe against *under*-filtering (the common failure mode) but not *over*-filtering: an `ADVISORY` source silently dropping entitled rows produces an incomplete result that's undetectable without a control fetch. | Under-filtering is caught by the verification filter; over-filtering would not be. The bandwidth cost of not pushing is accepted in exchange for that guarantee. |
| **Conformance tests instead of runtime verification** | Catches our-code bugs at introduction, no per-query cost. | Blind to vendor-side behavioral drift between test runs. | Complementary to runtime verification, not a substitute for it - both run. |

---

## ADR-003 - Plan cache

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **No cache** | Zero invalidation risk; a stale plan carrying superseded policy is a class of bug caching itself introduces. | Makes an expensive planner indefensible - the JVM sidecar's latency case collapses without it. | With the prototype's ~1 ms Go planner, close to the right answer on latency grounds alone. Built anyway for the correctness lesson - the same key-design problem also covers the result cache and the attribute cache. |
| **Key on SQL text alone** | Best possible hit ratio. | Security defect - a plan built under a privileged policy would be served to an unprivileged user. | A privilege-escalation vector, directly covered by `TestPlanCacheDoesNotLeakAcrossRoles`. |
| **Key on `(sql, user)`** | Safe. | Hit ratio approaches zero at 10M users. | Safe, but not useful at the target scale. |

---

## ADR-004 - Build vs. buy

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Build all** | Full control over pushdown, auth, and rate-limit semantics. | 1,000s of connectors against unversioned vendor APIs is not a six-month program - it's the whole company. Schema drift alone would consume the team. | Build where pushdown determines the SLO - on the order of ten to twenty connectors, not a thousand. |
| **Buy all** (Merge / Nango / Paragon / Airbyte) | Instant breadth; the vendor owns API drift. | Unified-API vendors normalize to a lowest-common-denominator schema and rarely expose per-field pushdown or per-user delegated auth - both load-bearing here. Buying everything would mean fetching wide and filtering locally, breaking quota and the entitlement model at once. | The vendor's abstraction discards exactly the two properties this design depends on. |

---

## ADR-005 - Freshness

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Centralized CDC / data lake** | Fast, cheap reads; solves freshness by pre-materializing. | Violates on-demand federated execution, and centralizing tenant data makes crypto-shredding (ADR-010) far harder. The brief does list "incremental snapshots" - the cross-tenant centralized lake is what's rejected, not per-tenant bounded snapshots. | Tenant-scoped snapshots stay available for quota-hostile connectors, under the same key and shred path as everything else. |
| **`SELECT MAX(updated_at)` probes** | Simple, reuses an existing indexed column. | SaaS REST APIs don't accept SQL. Update-timestamp watermarks are also blind to hard deletes, so a deleted record stays visible in cache indefinitely. | Present in an early draft; withdrawn once the delete-blindness problem was identified - see "Design evolution" below. |

---

## ADR-006 - Rate limits

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **In-memory per-pod buckets** | No round-trip latency, no shared-fate dependency on Redis. | N autoscaled pods means the effective limit is N times the configured one - the failure mode is the connector banning the API key, a cross-tenant outage. | Rejected for the production design, but the prototype uses exactly this as a stated single-node simplification (see `README.md`'s divergences table) - the risk this rejection describes is why it's called out explicitly rather than left implicit. |
| **Redis on every decision** | Exactly correct accounting. | Adds a round trip to every request and creates shared fate with Redis. | Correct and slow - leased local buckets get most of the accuracy at none of the added latency. |
| **Envoy ratelimit service** | Battle-tested, off-the-shelf. | Doesn't model per-connector budgets shared across both the synchronous and async-reroute execution paths. | The wrong shape for a budget that has to span two execution paths. |

---

## ADR-007 - Joins

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Naive dual full fetch** | Simplest possible implementation. | 505 calls vs. 17 on the reference fixture. | Only competitive when selectivity is poor - and at poor selectivity the semi-join rewrite loses too, for the same reason. |
| **Container per join (DuckDB)** | Hard process isolation per query. | Container cold start alone can consume the entire 1.5 s P95 budget. | The isolation is achievable with a memory limit; the latency cost of a container per query is not recoverable. |
| **ClickHouse** | Scales past single-node; a real, mature server. | It's a server with a lifecycle and a footprint; a join engine that can be created and destroyed in milliseconds inside a request is what's needed instead. | Revisit if materialized volumes grow past what one node can hold. |
| **Spill to disk on memory exhaustion** | Standard database behavior; avoids a failed query outright. | Trades a fast failure for a slow one inside a 1.5 s budget, and creates an encrypted-temp-storage obligation for data that shouldn't persist at all. | Rejected in favor of `RESULT_TOO_LARGE` with a cardinality estimate - a clear, actionable failure is a better outcome than a slow timeout. |
| **Native join pushdown for same-source joins** (e.g. Salesforce SOQL relationship subqueries) | Genuinely faster and cheaper when both tables share a connector - no semi-join, no materialization, one round trip at the source. | Would add a second, per-connector capability dimension ("which join shapes can this source push down") on top of per-predicate pushdown, for every connector - real scope against 1,000s of app types, most without an equivalent to SOQL subqueries. | Left out deliberately: a same-source join gets the same treatment as a cross-app one. Revisit if same-source joins come to dominate query volume enough to justify a connector-specific fast path. |

---

## ADR-008 through ADR-011

| ADR | Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|---|
| 008 | **`terraform apply` per tenant** | Consistent with how every other piece of infrastructure is provisioned - one tool, one mental model, a full plan/apply audit trail. | Doesn't scale to hundreds of customers; makes off-boarding latency a function of a plan/apply cycle; crypto-shredding cannot be gated on Terraform state. | Terraform owns static infrastructure; a Control Plane API owns tenant lifecycle - shredding can't wait on a plan. |
| 009 | **Chunked transfer + status code** | Streams rows to the client as they arrive, with no format change from a normal HTTP response. | The HTTP status is committed before a mid-stream failure is known - `SOURCE_TIMEOUT` has nowhere to surface. | A response status already sent to the client cannot be revoked mid-stream. |
| 009 | **HTTP trailers** | The HTTP-spec-correct way to attach trailing metadata, like a final status, after a chunked body. | Inconsistently supported by intermediaries in practice. | The technically correct answer, unreliable in the deployment reality. |
| 010 | **"Instantly destroy the KMS key"** | The simplest possible mental model for crypto-shredding: destroy the key, the data becomes permanently unreadable, done. | Cloud KMS enforces mandatory destruction waiting periods (AWS 7-30 days, GCP a comparable minimum) - the key cannot actually be destroyed on demand. | An early-draft assumption, corrected: disable the key and revoke grants immediately (rendering data unreadable within seconds); destruction completes on the provider's schedule. |
| 010 | **Shred audit records under the tenant key** | Consistent - every artifact belonging to a tenant is destroyed the same way, with no separate key-management surface to maintain. | Destroys the record proving the tenant's data was handled correctly - the compliance obligation that specifically has to survive off-boarding. | Audit lives in a separate key domain with tokenized identifiers - the trail survives; re-identification of who it was about does not. |
| 011 | **Trust token claims directly** | Simple, no attribute-resolution hop. | A trusted `tenant_id` claim is a cross-tenant read - whoever mints the token chooses the tenant. Group claims also break down at scale (Entra emits GUIDs and drops the list past roughly 200 groups). | Tenant is derived from inside the signature envelope (the verified issuer), never from the payload. |
| 011 | **Direct federation** | Simpler; no new issuer to operate, no signing-key blast radius. | Pushes per-IdP quirks into OPA, the planner, and audit indefinitely. | Kept as the fallback for compliance regimes that forbid re-issuing identity - viable at a handful of customers, not at the target scale, where one normalized claim contract matters more. |
| 011 | **mTLS / client certificates** | Strong authentication. | Wrong ergonomics for end users; carries no attributes. | Not evaluated further given both objections apply regardless of scale. |

---

## Design evolution

These are not rejected alternatives - they are positions earlier drafts of the design took and
later corrected, kept on record so the reasoning behind the current version isn't mistaken for
having been obvious from the start.

1. The Calcite planner was initially labeled control plane, then moved to data plane - it runs per-request and sits in the latency path.
2. `SELECT MAX(updated_at)` probes were replaced by the four-rung freshness capability ladder (ADR-005) - SaaS REST APIs don't accept SQL, and update-timestamp watermarks are blind to hard deletes.
3. OPA was initially scoped as a policy blob store, then moved to Compile API partial evaluation - the residual predicates it returns translate directly into plan `Filter`/`Project` nodes.
4. "Instantly destroy the KMS key" was corrected to disable-and-revoke-now, destroy-on-schedule, matching how cloud KMS destruction actually works.
5. In-memory per-pod rate-limit buckets were replaced by Redis-backed leases in the design (ADR-006) - N pods otherwise means N times the configured limit.
6. "Stream results as they arrive" was narrowed to two explicit execution paths (ADR-009) - joins and sorts are blocking operators and cannot stream through.
7. Chunked transfer encoding was replaced by NDJSON with a terminal metadata frame, for the reason in ADR-009's table above.
8. Container-per-join was replaced by in-process DuckDB, for the reason in ADR-007's table above.
9. "Plan-time injection guarantees zero leakage" was corrected by adding the `ENFORCED`/residual runtime invariant (ADR-002) - injection alone doesn't catch a connector that ignores a predicate it claimed to enforce.
10. Terraform-per-tenant was replaced by API-driven tenant lifecycle (ADR-008).
11. Deriving `tenant_id` from token claims was replaced by deriving it from the verified issuer (ADR-011).
12. An earlier estimate of the plan cache saving roughly 25x on planner fleet size was corrected to roughly 2-3x, after finding concurrent operations had been conflated with pod count.
13. The plan-cache hit-ratio target was raised from 90% to 95%, since at 90% the miss population is itself the P95 - planner latency would land in the SLO undiminished.
14. An earlier draft rejected Trino partly for having no policy-injection hook; this was inaccurate - Trino's `SystemAccessControl` SPI does provide row filters and column masks. The rejection now rests on the connector model, credential scoping, and tenant isolation instead.
15. An earlier draft rejected Cedar partly for lacking partial evaluation; this was also inaccurate - Cedar has partial evaluation, and its own RFC 0095 uses residual-to-SQL translation as its motivating example. The rejection now rests on maturity.

---

## Weakest rejections

Ranked by how likely each one is to be reversed on new evidence - the same spirit as
`DESIGN.md` Section 12, applied to the alternatives that lost rather than the decisions that won.

1. **DataFusion (ADR-001).** Not really a settled rejection - ADR-001 itself is Proposed, not
   Accepted, specifically because this is unresolved. The current position is to run Calcite in
   a pre-warmed JVM sidecar over gRPC, instrument it, and move only if measurement says to.
   "Switch to DataFusion" is actually two separate migrations: swapping the sidecar's runtime
   removes GC exposure but keeps the network hop; removing the hop entirely requires a Rust data
   plane. The hop itself is sub-millisecond on loopback against a roughly 250 ms connector call,
   making it the least likely bottleneck of the two - the more probable finding, if the M1 spike
   runs, is that neither runtime is the actual constraint and plan-cache hit ratio is.
2. **Trino (ADR-001).** Rejected on accurate grounds after an earlier, weaker version of the
   rejection was corrected (see "Design evolution," item 14) - the SPI does what a first
   pass assumed it didn't. The rejection now rests on the connector model and tenant isolation,
   not on a missing policy hook.
3. **No plan cache (ADR-003).** With a roughly 1 ms in-process planner, this is close to the
   right answer on latency grounds alone. It isn't defended on latency - it's kept for the
   cache-key correctness lesson, which also covers the result cache and the attribute cache.
4. **Buy-all connectors (ADR-004).** The 10-20/long-tail build split is a judgment call with no
   measured data behind it yet. The specific objection to buying everything is what unified-API
   vendors discard - per-field pushdown and per-user delegated auth, both load-bearing here.
5. **Cedar (ADR-002).** Corrected once already (see "Design evolution," item 15) - the original
   objection was wrong on the facts, not just weak. The current rejection rests on maturity
   (both partial evaluators are behind experimental feature flags), which is a real but
   time-bound objection, not a structural one.

**The general pattern.** Every item on this list names the metric or event that would flip it -
consistent with `DESIGN.md`'s own convention for its accepted decisions (Section 12). A
rejection with no stated reversal condition is the kind worth re-examining first.
