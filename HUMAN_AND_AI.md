# How this was built: the division of labour between me and the models

This was my first take-home built end-to-end with LLMs, and the process is worth recording
because the *shape* of the division turned out to be specific and repeatable — not "AI wrote it,
I reviewed it."

**Gemini** produced the initial design outline: the component inventory, the first cut at
plane separation, the shape of the ADR set. **Claude** took over for everything after that —
TDD cycles, the load harness and benchmarking, comparing design alternatives, and the
documentation set you're reading. I drove review, and that review is where most of the
repo's sharpest findings came from.

---

## What the models were good at

| Task | Why it fit |
|---|---|
| **Volume with structure** | Eleven ADRs in a consistent Context → Options → Decision → Consequences → Revisit-if shape. Tedious to keep uniform by hand, trivial for a model |
| **Arithmetic chains** | Little's Law → fleet → pod size, carried through eight steps with every intermediate shown. It doesn't drop a term |
| **Mechanical consistency** | Renumbering §12→§11 across four documents and ~30 code comments; validating 130 anchor links; keeping three docs in sync |
| **Writing the test before the code** | TDD cycles were near-frictionless. Red→green→refactor is a well-specified loop |
| **Recall of the brief** | Cross-checking every claim against the THP PDF, catching that `Parquet on S3` — one of three named materialization options — appeared nowhere in the design |

---

## Where I drove, and what it changed

### 1. Claims that outran their evidence

| What I pushed on | What it fixed |
|---|---|
| *"this k6 measurement is meaningless — a formula arrives at the same number"* | Correct. `hit = 1 − (N×⌈D∕T⌉)/R` — `N` distinct keys, `D` run duration, `T` the staleness TTL, `R` total requests — reproduced 3 of 4 rows **exactly**. Arithmetic was being presented as measurement |
| *"latency doesn't drop because the source is mocked — try 200 ms–1 s"* | The highest-value intervention of the project. It reversed a retraction Claude had made on bad evidence, and converted a tautology into a real measurement that then found two live defects |
| *"testing 1 principal at 500 req/s is unrealistic"* | A 112× stampede figure was being reported as a production number. The honest figure at design scale is ~1.1× |
| *"is this load test using any result cache?"* | Every cached entry was **0 rows / 2 bytes** — the query matched nothing. That caveat had existed and was silently lost in a rewrite |
| *"are we still under reading time budgets?"* | Two of five stated read times were wrong, in **both** directions. Nobody had measured them |

### 2. Reasoning that didn't hold

| What I pushed on | What it fixed |
|---|---|
| *"nowhere in the THP is pure in-memory a requirement"* | An internal constraint was masquerading as a brief requirement — and it surfaced that one of THP's three named materialization options was never addressed |
| *"fine to reject disk spill on SLO grounds — but there is no SLO for async"* | A real non sequitur: a latency argument reused in the one place latency doesn't apply. Produced the four-tier join ladder |
| *"what are your assumptions — always-on, or created on demand?"* | An **unstated** assumption — and the first answer was wrong too: it flipped to on-demand-per-job, which a later arrival-rate check overturned. Two rounds to reach a warm pool. The question was right both times; the answers weren't |
| *"isn't spinning up Spark as easy as maintaining ClickHouse?"* | The ops-cost argument compared self-managed to self-managed and ignored serverless. It mostly evaporated |
| *"this measures the plan cache — but working set is the majority of memory"* | **Two errors in a table just written**: planner sidecars attributed to the wrong cache, pod-size dependency overstated 5× |
| *"even if only 5% of jobs go async, spinning up 50 ClickHouse a second is unrealistic — this is a shared infra problem"* | I had chosen on-demand-per-job **without ever checking the arrival rate**. At the design's own trigger that is ~7.5 escalations/s against a 10–30 s startup — Little's Law puts **75–1,500 instances permanently mid-provision** |
| *"this is an MVP limitation, not the planner"* | The 2-table cap was blamed on Calcite — which was chosen partly *for* cost-based join ordering. It actually lives in a hand-rolled Go executor and one parse restriction; decomposition, OPA residuals, over-projection and verification are all already per-scan, in a loop that never counts tables |

### 3. Supplying the frame that was missing

- **"capacity planning is in-flight × working set; cache planning is TTL × object size"** — this
  reframing is what exposed that the cache sizing formula's TTL term is *fictional*, because
  nothing ever evicts.
- **"add 10 query varieties — a dashboard has 2–3 widgets"** — made the workload realistic *and*
  produced a finding neither of us had: **distinct keys is the variable; how they arise is
  irrelevant.** 10 users × 10 widgets behaves identically to 100 users × 1 query.
- **"keep tier 2 strictly in-memory — it still gives orders of magnitude more room than DuckDB's
  256 MB"** — the reframing that collapsed an entire design branch. I had been defending tier 2's
  *disk*, and rejected "just disable spill" as gutting it. Wrong axis: tier 2's value is the
  **~1,000× memory jump**, not the disk beneath it. Disabling spill deleted a scratch-volume
  scheme, per-tenant CMKs, an orphaned-volume reaper and an open crypto-shred residual — and left
  the ladder with one rule: fit in memory or escalate.
- **"run one job at a time per ClickHouse node"** — better than what I had proposed (single-tenant
  deployments), and it fixed something I hadn't raised at all: concurrent tenants co-mingle rows in
  one **process heap**, which no storage prefix or key separates.
- **"why isn't sort in the key? is it in memory even after a hit?"** — two questions that found
  the **no-eviction defect** and the undisclosed `ORDER BY` gap.
- **"why DuckDB and not SQLite?"** — SQLite appeared in **no document**, despite being the most
  obvious alternative to an embedded engine. `REJECTED_ALTERNATIVES.md` opens by promising the
  strongest case for *"every option that was genuinely in contention"* — but a register can only
  hold options someone thought of, so it looks complete precisely because omissions leave no
  trace. Answering it meant putting both engines against [§6](./DESIGN.md#6-capacity-and-performance)'s
  own load, which is where the argument stopped being about speed:

  At 1k QPS with a 15% join share that is **150 joins/s, 90 concurrent fleet-wide, 8 per pod** —
  and the *concurrency* is what decides it. DuckDB gives each of those 8 its own buffer manager
  and its own 256 MB ceiling. SQLite's heap limit is process-wide, so the same 2 GB arrives as
  **one pool the 8 divide**. Same total memory, different shape — and in a shared pool the query
  that fails is whichever is allocating when the pool runs out, so a 5 MB join fails because a
  1.9 GB one is holding it and `RESULT_TOO_LARGE` names the wrong query. At one join at a time
  the distinction would not exist; it exists because joins are concurrent.

  The answer that survived was not the one I first gave. Not *"DuckDB is faster"* but
  **"DuckDB degrades acceptably at both ends of the join-size distribution, SQLite only at the
  small end"** — safe under uncertainty rather than better. Which end the traffic actually sits at
  is the same unmeasured number [§6.4](./DESIGN.md#64-the-sensitivity-that-actually-matters) already
  ranks first.
- **Choosing "behind an interface, opt-in" when asked how far DuckDB should go.** The alternative
  on offer was to make DuckDB *the* tier-1 executor, and that option's own description said
  `delete hashJoin`. Taking it would have removed the Go join entirely — so "does the Go
  implementation do N-way joins?" could not have been asked, let alone answered yes. Keeping both
  forced the N-way contract to be *written down* as an interface (`sides`, `links`) instead of
  living inside whichever implementation happened to survive. **The consequence only surfaced
  three prompts later**, which makes it a different shape from the rest of this list: not a wrong
  number caught, but a scope decision whose value was invisible when it was made.

### 4. Process and presentation

- **"use tables and charts instead of prose"** — not cosmetic. Prose reads at ~200 wpm; a table
  scans at roughly twice that, so the same content costs half the time. It also makes redundancy
  *visible*: converting one section to a table exposed that the same point had been stated
  **three times** in three paragraphs, which nobody had noticed while it was prose. The Measured
  section went from nine bold-led paragraphs to two three-column tables and got shorter, clearer,
  and easier to attack.
- **"do these belong to 'consequences we accept'?"** — three of four bullets didn't. One was a
  requirement interpretation, one a rejected option, and one argued the 2-table cap *isn't* a real
  limitation — filed under costs we accept.
- **"keep `DESIGN_FULL.md` alongside the read-time-optimised versions"** — the most useful
  structural decision of the project, for a reason specific to working with LLMs. Context windows
  compact. Across two or three compactions and several sessions, a model working from its own
  summary is working from a summary *of a summary*, and detail degrades silently — which is
  precisely how confident, wrong claims get generated. A canonical full document meant Claude
  could always re-read ground truth instead of trusting its own recollection, and meant every
  condensed claim was checkable against a source rather than against memory. Several corrections
  in this repo were caught exactly that way.

---

## The pattern worth naming

Three of the most significant findings in this repo came from questions, not from generated
output:

| Finding | Consequence |
|---|---|
| **No eviction policy** | Resident memory grows unbounded with distinct keys; §6.2's sizing assumed a bound that does not exist |
| **No singleflight** | While the runbook explicitly promised it prevents cache stampedes |
| **The harness was latency-blind** | Structurally incapable of detecting *any* concurrency defect whose window is the duration of a downstream call |

And the failure modes being corrected were consistent rather than random:

1. **Overclaiming from weak evidence** — a single anomalous data point treated as a finding; a
   retraction issued on a measurement that was incapable of testing the claim.
2. **Reusing an argument outside its domain** — the SLO justification for rejecting disk spill,
   applied to a tier that has no SLO.
3. **Conflating similar-sounding quantities** — two caches, two memory pools, two kinds of
   "biggest," the plan-cache hit ratio versus the result-cache hit ratio.
4. **Elaborating a mechanism instead of questioning the requirement** — faced with
   tenant-mingled disk spill, the response was encrypted scratch volumes, per-tenant CMKs and a
   volume reaper. The answer was to not spill.

Knowing those four in advance is what made review efficient. The productive question was almost
never "is this well written" — it was **"is that specific number actually true, and how would we
know?"** Nearly every correction above came from asking that of one figure, not from knowing the
answer in advance. That is the part that generalises.

---

## What I'd change about how I ran the review loop

These are mine, not the models'. Each is a thing I could have demanded up front and didn't —
and each would have caught a defect earlier than my eventual question did.

- **Demand realistic latency before accepting any benchmark.** I let the mocks answer in ~1 ms for
  most of the project. That made every latency and concurrency number meaningless and hid two real
  defects, and the fix was a single flag. I should have asked "what does a miss actually cost here?"
  the first time a latency figure appeared, not several sessions later.
- **Ask for the "what this test cannot show" list before reading the result.** Nearly all the
  overclaiming I had to unwind came from results arriving without their limits attached. Those
  limits were knowable in advance — the harness returns zero rows, runs no joins, and answers
  instantly, all of which were true from the day it was written. Requiring the caveats *with* the
  number would have made most of my later corrections unnecessary.
- **Treat every superlative as a review trigger.** "The single most important number" appeared
  twice, for two different numbers, in one document — and I only caught it because I went looking
  for something else. Superlatives are where inconsistency hides, and they are cheap to grep for.
