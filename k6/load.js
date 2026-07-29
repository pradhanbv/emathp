// k6 load script - DESIGN.md Section 5.3's "single most consequential
// unknown": how result_cache_hit_ratio (the freshness cache, Cycle 9)
// behaves as the number of distinct principals grows. Not TDD'd
// (IMPLEMENTATION_PLAN.md ground rule 4, same as docker-compose.yml).
//
// Run: docker compose --profile testing run --rm -e PRINCIPALS=100 k6
// (then 1000, then 10000 - at 30k requests per run, principal counts below
// ~100 are all indistinguishable from 100% hit ratio, so the degradation
// only becomes visible once principal count approaches request volume).
//
// Why status='open-<n>' rather than a real query. The freshness cache key
// is the fully-bound outbound fetch (table + columns + pushed filter
// values - see internal/freshness/freshness.go's cacheKey). sf.accounts'
// status column is ENFORCED (pushed), so giving each simulated principal
// its own literal makes their fetches genuinely distinct cache entries -
// a stand-in for the real design's per-principal delegated OAuth token
// (ADR-002), which this prototype doesn't model at the connector layer at
// all. It returns zero matching rows against the mock's fixture data; that
// doesn't matter here; only the fetch-and-cache mechanics do.
import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Rate } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const PRINCIPALS = parseInt(__ENV.PRINCIPALS || '10', 10);
const RATE = parseInt(__ENV.RATE || '500', 10);
const DURATION = __ENV.DURATION || '60s';
const VUS = parseInt(__ENV.VUS || '100', 10);

// constant-arrival-rate, not vus+duration: the brief asks to "reach ~500-1k
// QPS for 60s", which is a target *rate*. vus+duration would instead run
// as fast as the system allows and report whatever throughput fell out -
// a different (and unfalsifiable) claim. Without this block at all, k6
// defaults to 1 VU x 1 iteration: the script ran, sent one request, and
// exited in ~0s, which is what it did before this was added.
export const options = {
	scenarios: {
		load: {
			executor: 'constant-arrival-rate',
			rate: RATE,
			timeUnit: '1s',
			duration: DURATION,
			preAllocatedVUs: VUS,
			maxVUs: VUS * 3,
		},
	},
};

const resultCacheHitRatio = new Rate('result_cache_hit_ratio');

function tokenFor(n) {
	// Same issuer/group as testdata/tokens/dana.jwt (support_agent, tenant
	// t_acme) - only sub varies, since role/tenant derivation is keyed by
	// iss and group, not by sub (see internal/identity).
	return JSON.stringify({
		iss: 'https://acme-corp.okta.example',
		sub: `u_synthetic_${n}`,
		groups: ['8f3c-4d21'],
	});
}

export default function () {
	// Global iteration counter, NOT __VU. Under constant-arrival-rate k6
	// only allocates as many VUs as the latency-bandwidth product needs -
	// at ~1 ms responses, 500 req/s needs well under one concurrent VU, so
	// __VU would be 1 or 2 forever and PRINCIPALS would silently collapse
	// to one or two distinct cache keys no matter what it was set to.
	const principal = exec.scenario.iterationInTest % PRINCIPALS;
	const payload = JSON.stringify({
		sql: `SELECT id FROM sf.accounts WHERE status = 'open-${principal}'`,
		max_staleness: '30s',
	});

	const res = http.post(`${BASE}/v1/query`, payload, {
		headers: {
			Authorization: `Bearer ${tokenFor(principal)}`,
			'Content-Type': 'application/json',
		},
	});

	const ok = check(res, { 'status is 200': (r) => r.status === 200 });
	if (ok) {
		const body = JSON.parse(res.body);
		resultCacheHitRatio.add(Boolean(body.meta && body.meta.cache_hit));
	}
}
