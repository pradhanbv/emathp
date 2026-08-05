# Rejected Alternatives

Companion to `DESIGN_FULL.md`. For every option that was genuinely in contention across the eleven
ADRs, this records the strongest real case *for* it, the specific reason it lost anyway, and a
one-line summary of the trade-off. Full reasoning for the alternative that *was* chosen lives in
`DESIGN_FULL.md`; this document exists so the road not taken is on record too, not just asserted away.

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
| **DataFusion (Rust)** | Extensible rule-based optimizer; `datafusion-substrait` covers logical and physical plans; no JVM, no GC, no hop. | Team capability, not merit - no Rust depth on the team shape (Section 10). | The closest call, which is why ADR-001 is held weakly (Section 11). Revisit if Rust depth is hired or the plan cache underperforms. |
| **Steampipe / Postgres FDW** | Literally this product category, and it already exists. | Single-tenant by construction. No per-tenant key, quota, or fairness model. FDW pushdown is coarse - no per-predicate `ENFORCED`/`ADVISORY` contract, the primitive ADR-002 depends on. | Good evidence the product category is real; not a foundation for a multi-tenant product. |
| **Go-native parser** (vitess, pg_query_go) | One runtime, no hop, and it handles the v1 SQL surface fine - the prototype proves it. | Gives an AST, not an optimizer. Ownership of capability-aware pushdown and residual-predicate correctness - the security-critical part - would fall to hand-written code. | Fine at n=2 (the prototype's own scale). The real question is n=50 connectors and multi-way joins, where owning rewrite-rule correctness for security predicates is the wrong trade. |
| **GraalVM native-image Calcite** | No hop, no JVM warmup. | Native-image needs closed-world compilation, which fights Calcite's runtime code generation and reflection - but only if the *executor* needs native-image too, and only the *planner* is needed here. | An open question, not a settled rejection - this is exactly why ADR-001 is Proposed rather than Accepted. If an M1 spike shows planner-only compilation is reachable, this could still be selected. |
| **Spark** | Mature distributed execution, and the only option here that scales a join past one node - which is exactly why it *is* selected, as ADR-007's tier 3. | Wrong decision to bring it to: this is the *planner* choice, on a 500 ms P50 sync budget, and Spark is a batch scheduler whose job-submission overhead alone exceeds it. | Rejected for this ADR, not from the design. The latency budget is the whole objection, so it evaporates on the async path where ADR-007 uses it. |
| **Velox** | Genuinely state-of-the-art vectorized execution, and the engine behind Presto/Spark acceleration - if execution speed were the constraint, this would be the answer. | Execution speed is not the constraint: a query here spends ~250 ms waiting on a SaaS API and single-digit ms planning. Velox also has no planner, so it would sit *underneath* a decision this ADR still has to make. | Out of scope rather than beaten - it answers a question this design doesn't ask. |

---

## ADR-002 - Entitlements

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Post-filter in Go** | Simple, connector-agnostic, always correct. | Rows leave the source and enter our memory before being discarded - that is the leak, plus wasted quota. The `ADVISORY` residual path *is* this, bounded - it is rejected as the default, not as a fallback. | Rejected as a strategy, kept as a bounded fallback - a difference of degree, not kind, which is why `residual_filter_rows_dropped` is monitored rather than the exception being pretended away. |
| **Inject into compiled Substrait in Go** | Keeps policy logic in one language, no planner round-trip. | Substrait uses positional field references; names survive only as root-level hints. Rewriting a compiled binary plan by position fails silently under column reordering. | The right idea at the wrong stage - injection belongs in the logical plan, while columns still have names, not after compilation to a positional form. |
| **OPA as a policy blob store** | Simple, familiar, no Compile API dependency. | Wastes OPA - the planner would then have to interpret Rego semantics itself, duplicating the engine. | The right tool used the wrong way; partial evaluation via the Compile API is the actual value OPA provides here. |
| **Zanzibar (OpenFGA / SpiceDB)** | Genuinely better for *mirroring* source permission graphs - Drive ACLs, Salesforce sharing rules. It would also buy back the result cache: with the source's visibility graph you could key on visibility-equivalence classes instead of on individual principals, which is the one thing that would lift ADR-003's per-principal hit ratio. | Deferred, not dead: under assumption A1 (source ACLs enforce themselves via delegated OAuth) there is nothing to mirror for the connectors that support delegation. That is narrower than "nothing to mirror" - connectors falling back to service-account auth carry explicit mirrored policy already, and for those a tuple store is the right shape. The reason to wait is that the fallback set is small and unmeasured, not that the need is absent. | Held as the fallback if delegated OAuth doesn't hold across enough connectors - see ADR-002's revisit trigger and Section 11. **Deferring it also defers the cache win**, which ADR-003's hit-ratio problem should be read against. |
| **Cedar** | Comparable expressiveness, a stronger formal-verification story, and Cedar's own RFC 0095 uses residual-to-SQL translation as its motivating example - conceptually the same approach. | Maturity, not concept: both partial evaluators sit behind experimental crate features (`partial-eval`, superseded by `tpe`), and the untyped evaluator's own RFC states it can return ill-typed residuals it cannot safely simplify - exactly what would be fed into a plan. OPA's Compile API is GA and documented for this. | Conceptually validated by Cedar's own design docs; rejected on maturity. Worth revisiting once `tpe` stabilizes. |
| **Sampling the runtime verification filter** | Cheaper than checking every request. | A security control that runs some fraction of the time lets a lying connector through the rest of the time. | Not a control if it's probabilistic. |
| **Pushing security predicates to `ADVISORY` connectors as an optimization** | Would cut fetch volume while the local filter preserves correctness. | Safe against *under*-filtering (the common failure mode) but not *over*-filtering: an `ADVISORY` source silently dropping entitled rows produces an incomplete result that's undetectable without a control fetch. That risk exists for any predicate pushed to an `ADVISORY` source, including ordinary user predicates we do push - what separates the two is the **symptom**, not the mechanism. An over-filtered user predicate is a visibly wrong answer someone reports; an over-filtered *security* predicate looks exactly like correct entitlement filtering and nobody ever reports it. | Under-filtering is caught by the verification filter; over-filtering would not be. We accept it where the failure is legible and refuse it where it is silent - which is why the bandwidth cost lands only on security predicates. |
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
| **Buy all** (Merge / Nango / Paragon / Airbyte) | Instant breadth; the vendor owns API drift. And the chosen design already buys the long tail, so most connectors live with the vendor abstraction regardless - "buy all" is the same trade taken further, not a different one. | Unified-API vendors normalize to a lowest-common-denominator schema and rarely expose per-field pushdown or per-user delegated auth. That is survivable on the long tail and fatal on the ten to twenty connectors where pushdown determines the SLO - so the objection is to buying *those*, not to buying. Bought connectors are exactly the ones that declare no `ENFORCED` predicates and fall back to `ADVISORY` local filtering, which is why the capability vocabulary is what makes a hybrid expressible at all. | Not "the vendor discards what we depend on" - we tolerate that everywhere it is cheap. The line is where fetch volume drives quota and latency. |

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
| **Envoy ratelimit service** | Battle-tested, off-the-shelf, and its descriptors are arbitrary key-value tuples - so `(connector, tenant)` models fine, called from either execution path. An earlier version of this row claimed it couldn't; that was wrong about the descriptor model. | The real objection is the one directly above: it is a remote call on every decision, which is what leased local buckets exist to avoid. Envoy also gates at the *proxy*, and the budget has to be spent per outbound HTTP request deep inside a paginating connector - a place no sidecar sees. | Right mechanism, wrong altitude. Rejected on where the spend happens, not on what it can express. |

---

## ADR-007 - Joins

| Option | Strongest case for it | Why rejected | Bottom line |
|---|---|---|---|
| **Naive dual full fetch** | Simplest possible implementation. | 501 calls vs. 4 on the reference fixture. | Only competitive when selectivity is poor - and at poor selectivity the semi-join rewrite loses too, for the same reason. |
| **Container per join (DuckDB)** | Hard process isolation per query. | Container cold start alone can consume the entire 1.5 s P95 budget. | The isolation is achievable with a memory limit; the latency cost of a container per query is not recoverable. |
| **SQLite for tier 1** (incl. pure-Go `modernc.org/sqlite`) | Pure-Go means no cgo: every byte stays in the Go heap where `GOGC` and `GOMEMLIMIT` can see it, and no OS thread is blocked per join - precisely the property Section 6.3's chain assumes and DuckDB denies. The semi-join rewrite also argues its own engine down: if joins reach tier 1 already small, SQLite is adequate. | **Structural, about shape not capacity.** Its heap limit is a process-wide singleton, so the same 2 GB arrives as one shared pool rather than eight isolated ceilings - and `RESULT_TOO_LARGE` has to name *which* join exceeded. In a shared pool a 5 MB join fails because a 1.9 GB one is holding it. **Predicted, not measured:** single-threaded joins at the ceiling should exceed the 4 s budget; neither engine is a dependency here and nothing was benchmarked. | **Safe under uncertainty, not the better engine** - DuckDB degrades acceptably at both ends of the join-size distribution, SQLite only at the small end. Two things would defeat that argument: metering bytes in Go before the engine sees them, or running one join at a time - which tier 2 does and the sync path cannot afford. **Reverses if** Section 6.4 measures joins averaging ~25 MB, or a benchmark narrows the gap. |
| **ClickHouse for tier 1** (the in-request join engine) | Scales past single-node; a real, mature server. | It's a server with a lifecycle and a footprint; the sync path needs a join engine creatable and destroyable in milliseconds inside a request. | Rejected for tier 1 only - **ClickHouse is tier 2 of the accepted design.** DuckDB in-process took tier 1. |
| **On-demand ClickHouse instance per job** (tier 2) | Perfect isolation: a fresh single-tenant instance per job, destroyed after, so no idle cost and no multi-tenant contention. | Never checked the arrival rate. At the risk register's own trigger escalations run ~7.5/s (5% of all traffic would be 50/s); against a 10-30 s startup, Little's Law puts **75-1,500 instances permanently mid-provision**. It is a fleet either way - just one paying a cold start on every job already escalated for being too big. | An early-draft conclusion, corrected. Tier 2 is a warm pool; isolation comes from serialization plus per-tenant S3 prefixes under the tenant's KEK. |
| **Concurrent multi-tenant jobs on one tier-2 node** | Obvious utilization win - pack several jobs onto a node instead of dedicating it to one. | Concurrent tenants co-mingle rows in a single process heap, which no storage prefix or key separates. Serialization plus a restart between jobs makes the node single-tenant for the job's duration. | Costs less than it appears: a tier-2 job is *defined* as one exceeding the pod ceiling, so it monopolizes a node regardless. What is forfeited is packing the jobs just above the tier-1 line. |
| **Spark-only from day one** (no tier 2 at all) | Spark can do everything ClickHouse can; tier 2 adds no *capability*, only cost efficiency. One async engine instead of two is less to build, operate and reason about. | Inside its band a warm pool of dedicated nodes beats per-job serverless overhead, and tier 2's ceiling is a number the plan-time estimate can compare against. Neither claim is measured. | **The rejection this design weakened most**, and the only one here resting on an unquantified cost comparison. Tier 2 is a cost optimization inside a utilization window, not a capability rung - and serialization pins node count to concurrent jobs, so above ~1/s escalations its economics fail and tier 3 is the answer anyway. **Reverses if** measured serverless per-job overhead is under roughly a warm node-hour at the observed escalation rate, or if that rate is ever seen above ~1/s - at which point tier 2 should never be built. |
| **Spill to disk on memory exhaustion** (tiers 0-2) | Standard database behavior; avoids a failed query outright. | Two different reasons, because one argument does not cover both. **Tiers 0-1:** trades a fast failure for a slow one inside a 1.5 s budget, and creates an encrypted-temp-storage obligation for data that shouldn't persist at all. **Tier 2:** the latency argument does *not* carry over - async has no completion SLO - so the reason is that a hard memory ceiling is a number the plan-time estimate can compare against, where "node RAM plus however much disk" is not; letting tier 2 spill would blur the tier-2/tier-3 boundary the design routes on. | Rejected in favor of `RESULT_TOO_LARGE` with a cardinality estimate. Tier 2 loses little: its value is the ~1,000x memory jump from tier 1's 256 MB `memory_limit`, not the disk beneath it. **Tier 3 is the only spilling tier**, and there the disk-handling is the vendor engine's problem. |
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
16. An earlier draft of Section 6.3 summed DuckDB's `K x 256 MB` (K = max concurrent joins per pod, 8) into a single "peak live heap"
    and applied Go's `GOGC=100` 2x multiplier to all of it. DuckDB is reached through cgo and its
    buffer manager allocates outside the Go heap, so the multiplier was being applied to memory
    the collector cannot see. The chain now splits by allocator and derives a 4.8-8 GB range.

---

## Weakest rejections

Ranked by how likely each one is to be reversed on new evidence - the same spirit as
`DESIGN_FULL.md` Section 11, applied to the alternatives that lost rather than the decisions that won.

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
5. **Spark-only (ADR-007).** The newest entry and the least quantified: with tier 2 reduced to a
   cost optimization rather than a capability, its whole rejection rests on a cost comparison
   nobody has measured - no serverless per-job pricing, no observed escalation rate. It is also
   the one rejection this design's own arithmetic strengthened rather than weakened.
6. **SQLite for tier 1 (ADR-007).** Ranked here rather than higher because one leg of the
   rejection is structural - a process-wide heap limit cannot express a per-join ceiling - and
   structural arguments don't move on measurement. The other leg is an unmeasured prediction: if joins arrive small,
   the ceiling rarely binds and SQLite's pure-Go footprint wins on everything else. That
   measurement is Section 6.4's first priority, so this is the rejection most likely to be
   *revisited soonest*, even if it survives.
7. **Cedar (ADR-002).** Corrected once already (see "Design evolution," item 15) - the original
   objection was wrong on the facts, not just weak. The current rejection rests on maturity
   (both partial evaluators are behind experimental feature flags), which is a real but
   time-bound objection, not a structural one.

**The general pattern.** Every item on this list names the metric or event that would flip it -
consistent with `DESIGN_FULL.md`'s own convention for its accepted decisions (Section 11). A
rejection with no stated reversal condition is the kind worth re-examining first.
