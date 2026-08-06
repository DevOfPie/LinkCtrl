#!/usr/bin/env node
//
// Re-verify M26.5's rendered-geometry claims in Blink, Gecko and WebKit.
//
//   make verify-render                 all three engines
//   node tools/render-verify/verify.mjs --engine webkit
//   node tools/render-verify/verify.mjs --headed --engine chromium
//
// Exits non-zero when a claim fails, and non-zero when it cannot check one.
// Nothing here is imported by anything that ships: Plan.md D25 lets tooling that
// only *verifies* the product use Node, on the condition that Node stays out of
// the product. See README.md.

import { createServer } from "node:http";
import { access } from "node:fs/promises";
import path from "node:path";
import process from "node:process";

import { buildFixture, readHeaderShape, readStylesheet, VARIANT_NAMES, LAYOUT, PARTIALS, STYLESHEET } from "./fixture.mjs";
import { CHECKS } from "./checks.mjs";

const HERE = import.meta.dirname;
const REPO_ROOT = path.resolve(HERE, "..", "..");

const ENGINES = [
  ["chromium", "Blink"],
  ["firefox", "Gecko"],
  ["webkit", "WebKit"],
];

// A failure a reader can act on, with no stack behind it.
class Stop extends Error {}

function parseArgs(argv) {
  const opts = { engines: ENGINES.map(([id]) => id), headed: false, debug: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--engine") {
      const want = argv[++i];
      if (!ENGINES.some(([id]) => id === want)) {
        throw new Stop(`Unknown engine "${want}". One of: ${ENGINES.map(([id]) => id).join(", ")}`);
      }
      opts.engines = [want];
    } else if (arg === "--headed") {
      opts.headed = true;
    } else if (arg === "--debug") {
      opts.debug = true;
    } else if (arg === "-h" || arg === "--help") {
      opts.help = true;
    } else {
      throw new Stop(`Unknown argument "${arg}". Try --help.`);
    }
  }
  return opts;
}

const HELP = `render-verify — M26.5's popover geometry, checked in three engines

  --engine <chromium|firefox|webkit>   just one engine
  --headed                             show the browser window
  --debug                              print stack traces instead of advice
  -h, --help                           this

Setup, once:
  npm install --prefix tools/render-verify
  npx --prefix tools/render-verify playwright install chromium firefox webkit
`;

async function present(rel) {
  try {
    await access(path.join(REPO_ROOT, rel));
    return true;
  } catch {
    return false;
  }
}

// Everything that can be wrong before a browser is launched, reported as one
// sentence and an instruction rather than as whatever exception it would
// otherwise have become several frames later.
async function preflight() {
  if (!(await present("tools/render-verify/node_modules/playwright"))) {
    throw new Stop(
      "Playwright is not installed for this harness.\n" + "  npm install --prefix tools/render-verify",
    );
  }
  for (const rel of [LAYOUT, PARTIALS]) {
    if (!(await present(rel))) {
      throw new Stop(
        `${rel} is missing, so there is no header to read the geometry out of.\n` +
          "  Run this from a LinkCtrl checkout.",
      );
    }
  }
  if (!(await present(STYLESHEET))) {
    throw new Stop(
      `${STYLESHEET} has not been built, and it is where every one of these\n` +
        "  measurements comes from. It is generated and deliberately untracked:\n" +
        "  make css",
    );
  }
}

// Static origin for the fixtures. A file:// page would work for two of the three
// engines and is a different security context in the third, which is not a
// difference this harness wants to be explaining later.
async function serveFixtures(shape) {
  const css = await readStylesheet(REPO_ROOT);
  const pages = new Map(VARIANT_NAMES.map((name) => [`/${name}.html`, buildFixture(shape, name)]));

  const server = createServer((req, res) => {
    const url = req.url.split("?")[0];
    if (url === "/app.css") {
      res.writeHead(200, { "content-type": "text/css; charset=utf-8" });
      res.end(css);
      return;
    }
    const page = pages.get(url);
    if (page) {
      res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
      res.end(page);
      return;
    }
    res.writeHead(404, { "content-type": "text/plain" });
    res.end(`no fixture at ${url}`);
  });

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const { port } = server.address();
  return { server, baseURL: `http://127.0.0.1:${port}` };
}

async function launch(playwright, engine, headed) {
  try {
    return await playwright[engine].launch({ headless: !headed });
  } catch (err) {
    if (/Executable doesn't exist|browserType.launch/.test(err.message)) {
      throw new Stop(
        `The ${engine} build Playwright expects is not installed.\n` +
          "  npx --prefix tools/render-verify playwright install chromium firefox webkit\n" +
          "  (add --with-deps, under sudo, on a machine missing the system libraries)",
      );
    }
    throw err;
  }
}

async function runEngine(playwright, engine, label, baseURL, opts) {
  const browser = await launch(playwright, engine, opts.headed);
  const results = [];
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const page = await context.newPage();
    const ctx = { baseURL, engine, engineVersion: `${label} via ${engine} ${browser.version()}` };
    await page.goto(`${baseURL}/default.html`);

    for (const [name, check] of CHECKS) {
      try {
        const { detail, fails } = await check(page, ctx);
        results.push({ name, fails, detail });
      } catch (err) {
        results.push({ name, fails: [`check threw: ${err.message}`], detail: "" });
        if (opts.debug) console.error(err);
      }
    }
    await context.close();
  } finally {
    await browser.close();
  }
  return { engine, label, results };
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  if (opts.help) {
    process.stdout.write(HELP);
    return 0;
  }

  await preflight();

  const { chromium, firefox, webkit } = await import("playwright");
  const playwright = { chromium, firefox, webkit };

  const shape = await readHeaderShape(REPO_ROOT);
  const { server, baseURL } = await serveFixtures(shape);

  console.log("render-verify — M26.5 popover geometry");
  console.log(`  header read from   ${LAYOUT}`);
  console.log(`  panels read from   ${PARTIALS}`);
  console.log(`  stylesheet         ${STYLESHEET}`);
  console.log(`  fixtures served at ${baseURL}`);
  console.log("");

  let failed = 0;
  try {
    for (const [engine, label] of ENGINES) {
      if (!opts.engines.includes(engine)) continue;
      const run = await runEngine(playwright, engine, label, baseURL, opts);
      console.log(`${label} (${engine})`);
      for (const r of run.results) {
        if (r.fails.length === 0) {
          console.log(`  ok    ${r.name}`);
          if (r.detail) console.log(`          ${r.detail}`);
        } else {
          failed += r.fails.length;
          console.log(`  FAIL  ${r.name}`);
          for (const f of r.fails) console.log(`          ${f}`);
        }
      }
      console.log("");
    }
  } finally {
    server.close();
  }

  if (failed > 0) {
    console.log(`render-verify: ${failed} failed assertion${failed === 1 ? "" : "s"}`);
    return 1;
  }
  console.log("render-verify: every claim held in every engine checked");
  return 0;
}

try {
  process.exitCode = await main();
} catch (err) {
  if (err instanceof Stop) {
    console.error(`render-verify: ${err.message}`);
  } else if (process.argv.includes("--debug")) {
    console.error(err);
  } else {
    console.error(`render-verify: ${err.message}`);
    console.error("  Re-run with --debug for the stack.");
  }
  process.exitCode = 1;
}
