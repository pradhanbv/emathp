// k6 load script - DESIGN.md Section 5.3's "single most consequential
// unknown": how result_cache_hit_ratio (the freshness cache, Cycle 9)
// behaves as the number of distinct principals grows. Not TDD'd
// (IMPLEMENTATION_PLAN.md ground rule 4, same as docker-compose.yml).
//
// Run: k6 run -e PRINCIPALS=1 k6/load.js   (then 10, then 100)
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
import { check } from 'k6';
import { Rate } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const PRINCIPALS = parseInt(__ENV.PRINCIPALS || '10', 10);

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
	const principal = __VU % PRINCIPALS;
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
