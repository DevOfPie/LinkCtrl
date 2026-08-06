// Load test for the redirect hot path.
//
// This is the generator half of the SLO measurement. The other half is the
// server's own histogram, which scripts/load-test.sh reads before and after the
// run and reports as a delta — a cumulative histogram would otherwise include
// the warm-up and every request that came before it.
//
// Two things about the shape of this test matter more than the numbers.
//
// It runs on the compose network, hitting http://app:8080 rather than a published
// port. The SLO is defined that way deliberately: measuring through a published
// port on Docker Desktop measures the WSL2 bridge, which on this project has
// produced ~13ms for an operation the server completes in microseconds.
//
// Redirects are not followed. k6 follows 302s by default, which would turn every
// iteration into a request to example.com and measure the internet.
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://app:8080';
const PREFIX = __ENV.PREFIX || 'ld';
const MODE = __ENV.MODE || 'cached';
const RATE = parseInt(__ENV.RATE || '2000', 10);
const DURATION = __ENV.DURATION || '2m';

// The in-process cache holds 10,000 entries. A cached measurement must request
// fewer distinct aliases than that, or it measures eviction; an uncached one must
// request many more, or the cache answers anyway.
const HOT = parseInt(__ENV.HOT || '5000', 10);
const TOTAL = parseInt(__ENV.TOTAL || '100000', 10);

const redirectTime = new Trend('redirect_duration', true);

// PHASE picks the scenario, because warm-up and measurement are two separate k6
// invocations rather than two scenarios in one. The server's histogram is
// cumulative, so the only way to report a delta covering exactly the measured
// window is for the run to contain nothing else — the snapshot is taken between
// the two invocations.
const PHASE = __ENV.PHASE || 'slo';

// SUFFIX appends extra path segments to every measured request, so the run can
// exercise deep-link path forwarding (M33) rather than only the bare alias.
// Empty by default, which leaves every earlier measurement's request shape
// exactly as it was — the seeded links must have forward_path on for a
// non-empty value to answer 302 rather than 404.
const SUFFIX = __ENV.SUFFIX || '';

// Arrival-rate executor for the measurement, not a VU loop: the target is a
// request rate, and a fixed VU count would quietly reduce throughput as latency
// rose — exactly when the number matters.
const scenarios = {
  // One VU walking the hot set in order. Sequential is slower than it needs to
  // be and impossible to get wrong: with more than one VU, __ITER is per-VU, so
  // the obvious `__ITER % HOT` warms only the first slice of the set and leaves
  // the rest to be read from the database inside the measured window.
  warm: {
    executor: 'per-vu-iterations',
    vus: 1,
    iterations: HOT,
    maxDuration: '5m',
    exec: 'warm',
    tags: { phase: 'warmup' },
  },
  slo: {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs: Math.max(100, Math.ceil(RATE / 20)),
    maxVUs: Math.max(400, Math.ceil(RATE / 4)),
    exec: 'measure',
    tags: { phase: 'slo' },
  },
};

export const options = {
  discardResponseBodies: true,
  scenarios: { [PHASE]: scenarios[PHASE] },
  thresholds: PHASE === 'warm' ? {} : {
    // The generator's view includes container-to-container network time, so it is
    // an upper bound on the server-side number the SLO names.
    'redirect_duration{phase:slo}': [
      MODE === 'cached' ? 'p(99)<20' : 'p(99)<100',
    ],
    'checks{phase:slo}': ['rate==1.00'],
  },
};

function aliasFor(n) {
  return `${PREFIX}${n}`;
}

// Every alias in the hot set exactly once, in order.
export function warm() {
  http.get(`${BASE}/${aliasFor(__ITER)}`, { redirects: 0, tags: { phase: 'warmup' } });
}

export function measure() {
  // Cached: stay inside the hot set that warm-up populated. Uncached: spread
  // across the whole dataset, which is ten times the in-process cache.
  const n = MODE === 'cached'
    ? Math.floor(Math.random() * HOT)
    : Math.floor(Math.random() * TOTAL);

  const res = http.get(`${BASE}/${aliasFor(n)}${SUFFIX}`, {
    redirects: 0,
    tags: { phase: 'slo' },
  });

  redirectTime.add(res.timings.duration, { phase: 'slo' });

  // A 404 here would mean the dataset is missing or the prefix is wrong, and a
  // 429 would mean the probe limiter is counting hits — either way the latency
  // number would be measuring the wrong thing, so both fail the run.
  check(res, {
    'is 302': (r) => r.status === 302,
    'has Location': (r) => !!r.headers.Location,
  }, { phase: 'slo' });
}
