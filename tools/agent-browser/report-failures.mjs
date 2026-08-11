// Reads `playwright test --reporter=json` on stdin and prints only what
// failed, so a green run costs one line and a red one costs exactly the
// assertion that broke. The reader is an agent whose context window is the
// budget — M46.5's spec bullet says so — and a green run's full report is
// spend with no information in it.
import process from 'node:process';

let raw = '';
process.stdin.setEncoding('utf8');
for await (const chunk of process.stdin) raw += chunk;

let report;
try {
  report = JSON.parse(raw);
} catch {
  // The runner died before producing a report — a config error, a missing
  // engine. Its own words are the diagnosis; pass them through and fail.
  process.stdout.write(raw);
  process.exit(1);
}

const strip = (s) => String(s).replace(/\u001b\[[0-9;]*m/g, '');
const failures = [];

const walk = (suite) => {
  for (const child of suite.suites ?? []) walk(child);
  for (const spec of suite.specs ?? []) {
    for (const t of spec.tests ?? []) {
      if (t.status === 'expected' || t.status === 'skipped') continue;
      const errs = (t.results ?? [])
        .flatMap((r) => (r.errors?.length ? r.errors : r.error ? [r.error] : []))
        .map((e) => strip(e.message ?? e));
      failures.push({ where: `${spec.file}:${spec.line}`, title: spec.title, errs });
    }
  }
};
for (const s of report.suites ?? []) walk(s);
for (const e of report.errors ?? []) {
  failures.push({ where: '(runner)', title: 'error before any test', errs: [strip(e.message ?? e)] });
}

const passed = report.stats?.expected ?? 0;

if (failures.length === 0 && passed > 0) {
  console.log(`verify-ui: green — ${passed} passed`);
  process.exit(0);
}
if (failures.length === 0) {
  console.log('verify-ui: red — no test ran, which is not the same as no test failing');
  process.exit(1);
}
for (const f of failures) {
  console.log(`FAIL ${f.where} ${f.title}`);
  for (const e of f.errs) console.log(e.replace(/^/gm, '    '));
}
process.exit(1);
