// Decodes the corpus internal/qr wrote, at several simulated viewing distances.
//
// M50.6's reopening asks for the logo box to grow "to the largest size the code
// still reliably decodes at", and for readability to be "measured by simulated
// scanning ... the raster scaled down so few pixels per module remain, at
// several scales". This is that measurement, and it is kept rather than run
// once: `make verify-scan` renders the corpus off the shipping path and hands it
// here, and a non-zero exit is the gate on the fraction.
//
// **Two decoders, and both must read every picture.** They are independent
// engines rather than two names for one — zxing-wasm is zxing-cpp compiled to
// WebAssembly, the algorithm most commercial scanners descend from, and jsQR is
// a separate implementation with its own detector. A fraction that only one of
// them reads is a fraction that depends on a decoder, and the whole point of
// measuring is to not depend on one. The two disagree in practice: jsQR alone
// found the failure that put the cap where it is, at a box zxing-wasm reads
// without complaint.
//
// **What is not here, and why.** `@zxing/library` — the JavaScript port rather
// than the wasm build — fails plain unoccluded codes at versions 14, 16, 20 and
// 34, so it cannot be evidence about a logo. `zbarimg` is a system package, not
// an npm one, so it cannot be pinned or run in CI; `--zbar` runs it anyway when
// it is on PATH, reports what it says and never gates, because it is the
// strictest engine to hand and its dissent is worth seeing.
//
// What none of them is: a poster. Downscaling approximates the one thing that
// dominates at distance — a camera sampling fewer pixels per module — and none
// of the rest, so a pass here is evidence of relative safety between fractions
// rather than of absolute field performance. m50.6.md says so in its risks and
// this comment says so where somebody reading the harness will see it.

import { readFileSync, readdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PNG } from "pngjs";
import jsQR from "jsqr";
import { readBarcodes, prepareZXingModule } from "zxing-wasm/reader";

const args = parseArgs(process.argv.slice(2));
if (args.help) {
  process.stdout.write(usage());
  process.exit(0);
}
if (!args.corpus) {
  process.stderr.write("--corpus <dir> is required\n\n" + usage());
  process.exit(2);
}

// The pixels-per-module a scanner is assumed to have left by the time the code
// is far enough away to be interesting. Two is the aggressive end: below it a
// module boundary lands inside a sensor pixel and no amount of error correction
// helps, so a failure there says nothing about the logo.
const DEFAULT_TARGETS = [8, 6, 4, 3, 2];
const targets = args.ppm
  ? args.ppm.split(",").map((s) => Number(s.trim()))
  : DEFAULT_TARGETS;
for (const t of targets) {
  if (!Number.isFinite(t) || t < 1) {
    process.stderr.write(`--ppm takes pixels per module, and ${t} is not one\n`);
    process.exit(2);
  }
}

// The wasm is loaded off disk rather than fetched. zxing-wasm resolves its
// binary from a CDN by default, and a verification harness that needs the
// network is a harness that fails for a reason unrelated to what it verifies.
const wasm = readFileSync(
  new URL("./node_modules/zxing-wasm/dist/reader/zxing_reader.wasm", import.meta.url),
);
await prepareZXingModule({
  overrides: {
    wasmBinary: wasm.buffer.slice(wasm.byteOffset, wasm.byteOffset + wasm.byteLength),
  },
  fireImmediately: true,
});

const decoders = [
  [
    "zxing-wasm",
    async (img) => {
      const found = await readBarcodes(
        { data: img.data, width: img.width, height: img.height },
        { formats: ["QRCode"], tryHarder: true },
      );
      return found.length > 0 ? found[0].text : null;
    },
  ],
  [
    "jsqr",
    async (img) => {
      const found = jsQR(img.data, img.width, img.height, {
        inversionAttempts: "dontInvert",
      });
      return found === null ? null : found.data;
    },
  ],
];

const rows = readManifest(join(args.corpus, "manifest.tsv"));
checkCorpusMatchesManifest(args.corpus, rows);

let attempts = 0;
const failures = [];

for (const row of rows) {
  const png = PNG.sync.read(readFileSync(join(args.corpus, row.file)));
  for (const target of targets) {
    if (target > row.scale) continue; // Upscaling invents nothing.
    const shrunk = resample(png, target / row.scale);
    for (const [name, decode] of decoders) {
      attempts++;
      const got = await decode(shrunk);
      if (got === null) {
        failures.push({ ...row, target, decoder: name, why: "no code found" });
      } else if (got !== row.payload) {
        failures.push({
          ...row, target, decoder: name, why: "decoded to a different payload",
        });
      }
    }
  }
}

report(rows, attempts, failures);
if (args.zbar) crossCheckWithZbar(rows);
process.exit(failures.length === 0 ? 0 : 1);

// ---------------------------------------------------------------------------

// resample shrinks an image by averaging every source pixel a destination pixel
// covers — which is what a sensor does, and the reason this is a distance
// simulation rather than a resize. Dropping pixels instead would make a module
// boundary land on whichever sample happened to be taken, and would fail codes
// for a reason no camera has.
function resample(png, factor) {
  const width = Math.max(1, Math.round(png.width * factor));
  const height = Math.max(1, Math.round(png.height * factor));
  if (width === png.width && height === png.height) {
    return { data: new Uint8ClampedArray(png.data), width, height };
  }
  const out = new Uint8ClampedArray(width * height * 4);
  for (let y = 0; y < height; y++) {
    const y0 = Math.floor((y * png.height) / height);
    const y1 = Math.max(y0 + 1, Math.floor(((y + 1) * png.height) / height));
    for (let x = 0; x < width; x++) {
      const x0 = Math.floor((x * png.width) / width);
      const x1 = Math.max(x0 + 1, Math.floor(((x + 1) * png.width) / width));
      let r = 0, g = 0, b = 0, a = 0, n = 0;
      for (let sy = y0; sy < y1; sy++) {
        let i = (sy * png.width + x0) * 4;
        for (let sx = x0; sx < x1; sx++, i += 4) {
          r += png.data[i];
          g += png.data[i + 1];
          b += png.data[i + 2];
          a += png.data[i + 3];
          n++;
        }
      }
      const o = (y * width + x) * 4;
      out[o] = r / n;
      out[o + 1] = g / n;
      out[o + 2] = b / n;
      out[o + 3] = a / n;
    }
  }
  return { data: out, width, height };
}

function readManifest(path) {
  const lines = readFileSync(path, "utf8").trim().split("\n");
  const head = lines.shift().split("\t");
  const want = [
    "file", "payload", "version", "modules", "scale", "margin", "logo", "level",
  ];
  if (head.join("\t") !== want.join("\t")) {
    throw new Error(
      `the manifest's columns are ${head.join(", ")} and this reads ${want.join(", ")}`,
    );
  }
  return lines.map((line) => {
    const [file, payload, version, modules, scale, margin, logo, level] =
      line.split("\t");
    return {
      file, payload, logo, level,
      version: Number(version),
      modules: Number(modules),
      scale: Number(scale),
      margin: Number(margin),
    };
  });
}

// checkCorpusMatchesManifest refuses a corpus and a manifest that have drifted
// apart in either direction. A picture nobody claims an expected payload for is
// a picture nothing checks, and a corpus that grows one silently is how this
// stops gating.
function checkCorpusMatchesManifest(dir, rows) {
  const onDisk = new Set(readdirSync(dir).filter((f) => f.endsWith(".png")));
  for (const row of rows) {
    if (!onDisk.delete(row.file)) {
      process.stderr.write(`${row.file} is in the manifest and not on disk\n`);
      process.exit(2);
    }
  }
  if (onDisk.size > 0) {
    process.stderr.write(
      `${onDisk.size} picture(s) on disk are not in the manifest: ` +
        `${[...onDisk].slice(0, 5).join(", ")}\n`,
    );
    process.exit(2);
  }
}

// report says what was decoded and how much of it came back exact.
//
// **The two populations are counted apart**, because one sentence over both was
// false of each. The logo'd half is drawn at level H only; the control carries
// no logo and is drawn at every level, so its payloads encode to *smaller*
// symbols and its version range starts below the product's own — a 22-byte
// payload is version 3 at H and version 2 at L. Counting them together reported
// "versions 2-36" for a corpus whose logo'd half starts at 3, and "5 logo
// shapes" for four, because `none` is a row in the logo column and not a shape.
function report(rows, attempts, failures) {
  const logod = rows.filter((r) => r.logo !== "none");
  const control = rows.filter((r) => r.logo === "none");
  const parts = [];
  if (logod.length > 0) {
    parts.push(
      `${logod.length} with a logo, ${span(logod)}, ` +
        `${new Set(logod.map((r) => r.logo)).size} shapes at level H`,
    );
  }
  if (control.length > 0) {
    parts.push(
      `${control.length} with none as the control, ${span(control)}, ` +
        `${new Set(control.map((r) => r.level)).size} levels`,
    );
  }
  process.stdout.write(
    `${rows.length} pictures — ${parts.join("; ")} — ` +
      `at ${targets.join(", ")} pixels per module, ` +
      `through ${decoders.map(([n]) => n).join(" and ")}: ` +
      `${attempts - failures.length} of ${attempts} decodes exact\n`,
  );
  if (failures.length === 0) return;

  // Grouped by what a reader would change: which decoder, which logo shape, and
  // how far away it was. A flat list of two hundred filenames says the same
  // thing and does not say which knob moved.
  const groups = new Map();
  for (const f of failures) {
    const key = `${f.decoder}: ${f.logo} at ${f.target}px/module — ${f.why}`;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(f);
  }
  process.stdout.write(`\n${failures.length} decode(s) did not match:\n`);
  for (const [key, group] of [...groups].sort()) {
    const vs = [...new Set(group.map((f) => f.version))].sort((a, b) => a - b);
    process.stdout.write(
      `  ${key}: ${group.length}, versions ${vs.join(", ")}\n` +
        `    e.g. ${group[0].file}\n`,
    );
  }
}

// span is the symbol versions a group of pictures covers, read off the manifest
// rather than assumed — the manifest records the version each picture actually
// encoded to, which for the control is not the version its filename carries.
function span(rows) {
  const vs = rows.map((r) => r.version);
  return `versions ${Math.min(...vs)}-${Math.max(...vs)}`;
}

// crossCheckWithZbar reports what the strictest engine to hand says, and gates
// nothing. ZBar is a system package: it cannot be pinned in this file's
// package.json, CI has no such binary, and its version differs per machine — so
// its numbers are a measurement to read, on the same terms as docs/slo.md's,
// and never a pass or a fail.
//
// It reads files rather than buffers, so each shrunk picture is written out and
// removed again.
function crossCheckWithZbar(rows) {
  let version;
  try {
    version = execFileSync("zbarimg", ["--version"], { encoding: "utf8" }).trim();
  } catch {
    process.stdout.write("\nzbarimg is not on PATH — nothing to cross-check\n");
    return;
  }
  const dir = mkdtempSync(join(tmpdir(), "linkctrl-zbar-"));
  const missed = [];
  const attempted = [];
  try {
    for (const row of rows) {
      const png = PNG.sync.read(readFileSync(join(args.corpus, row.file)));
      for (const target of targets) {
        if (target > row.scale) continue;
        attempted.push({ ...row, target });
        const shrunk = resample(png, target / row.scale);
        const out = new PNG({ width: shrunk.width, height: shrunk.height });
        out.data = Buffer.from(shrunk.data.buffer, 0, shrunk.data.length);
        const path = join(dir, "shrunk.png");
        writeFileSync(path, PNG.sync.write(out));
        let got = "";
        try {
          got = execFileSync(
            "zbarimg",
            ["--raw", "-q", "-Sdisable", "-Sqrcode.enable", path],
            { encoding: "utf8" },
          ).replace(/\n$/, "");
        } catch {
          got = "";
        }
        if (got !== row.payload) missed.push({ ...row, target });
      }
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  // **Split by whether there is a logo in the picture at all**, because that is
  // the only split its numbers can be read against. A miss on a logo'd code is
  // the cap's doing or ZBar's; a miss on a control picture is ZBar's alone, and
  // the sentence "it reads the whole control, so this is the logo's doing" is
  // true or false on this line and nowhere else.
  const share = (pred) => {
    const tried = attempted.filter(pred).length;
    const lost = missed.filter(pred).length;
    return `${tried - lost} of ${tried}`;
  };
  const hasLogo = (r) => r.logo !== "none";
  const noLogo = (r) => r.logo === "none";
  process.stdout.write(
    `\n${version}, reporting only: ` +
      `${attempted.length - missed.length} of ${attempted.length} exact` +
      ` — ${share(hasLogo)} with a logo, ${share(noLogo)} on the no-logo control\n`,
  );
  if (missed.length === 0) return;
  const byScale = new Map();
  for (const m of missed) {
    const key = `${m.logo === "none" ? "control" : "logo"}, ` +
      `stored at ${m.scale}px/module, read at ${m.target}`;
    byScale.set(key, (byScale.get(key) ?? 0) + 1);
  }
  for (const [key, n] of [...byScale].sort()) {
    process.stdout.write(`  ${key}: ${n}\n`);
  }
  const controlMisses = missed.filter((m) => m.logo === "none");
  if (controlMisses.length > 0) {
    const vs = [...new Set(controlMisses.map((m) => m.version))].sort((a, b) => a - b);
    process.stdout.write(
      `  the control is not clean under this engine — versions ${vs.join(", ")};` +
        ` its misses on logo'd codes are therefore not all the logo's doing\n`,
    );
  }
}

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--help" || a === "-h") out.help = true;
    else if (a === "--zbar") out.zbar = true;
    else if (a === "--corpus") out.corpus = argv[++i];
    else if (a === "--ppm") out.ppm = argv[++i];
    else {
      process.stderr.write(`unknown argument ${a}\n\n` + usage());
      process.exit(2);
    }
  }
  return out;
}

function usage() {
  return [
    "scan.mjs --corpus <dir> [--ppm 8,6,4,3,2] [--zbar]",
    "",
    "  --corpus  a directory internal/qr's TestWriteScanCorpus wrote",
    "  --ppm     the pixels per module to shrink each picture to, comma separated",
    "  --zbar    also report what a system zbarimg says, which gates nothing",
    "",
    "Exits non-zero if any picture fails to decode to its manifest payload",
    "through either pinned decoder. make verify-scan is the way to run it.",
    "",
  ].join("\n");
}
