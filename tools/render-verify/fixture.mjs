// The page under test, assembled from the product's own templates.
//
// Nothing here is a copy of the header. Every class string that decides where a
// popover lands — the header's, the nav container's, both invokers', both
// panels' — is read out of internal/ui/templates at run time, and the stylesheet
// the page loads is the same internal/ui/static/css/app.css the server serves.
// A committed HTML fixture would have been a second copy of the markup, free to
// stay green while the template it claims to describe drifts away from it; see
// README.md for the whole argument.
//
// What this file does invent is the panels' *contents* — a few links, a few list
// items. No claim in M26.5 is about them: `w-56`, `w-80` and the top/right pair
// fix the panel box whatever is inside it.

import { readFile } from "node:fs/promises";
import path from "node:path";

export const LAYOUT = "internal/ui/templates/layout.html";
export const PARTIALS = "internal/ui/templates/partials/nav.html";
export const STYLESHEET = "internal/ui/static/css/app.css";

// A template fragment carries Go template actions. Only the two invoker buttons
// are lifted verbatim, and between them they use four constructs, so this
// resolves those four rather than pretending to be text/template:
//
//   {{/* … */}}          dropped
//   {{if}}…{{else}}…{{end}}  the else branch wins, so a nav-active conditional
//                        renders its inactive form — the state the header is in
//                        on every page but one
//   {{if}}…{{end}}       the branch is kept, so the unread badge draws and the
//                        invoker has the width it has when there is something
//                        to see
//   {{.Field}}           substituted from VALUES, or dropped
//
// Neither button nests an if inside an if, which is what makes the innermost
// -first loop below sufficient. If one ever does, the assertion that the fixture
// still holds no `{{` catches it rather than letting a half-resolved action
// through as text.
const VALUES = {
  ".Unread": "3",
};

function resolveActions(fragment, values) {
  let out = fragment.replace(/\{\{\/\*[\s\S]*?\*\/\}\}/g, "");

  const conditional = /\{\{if [^{}]*\}\}((?:(?!\{\{if )[\s\S])*?)\{\{end\}\}/;
  for (let guard = 0; conditional.test(out); guard++) {
    if (guard > 50) throw new Error("runaway while resolving template actions");
    out = out.replace(conditional, (_, body) => {
      const split = body.indexOf("{{else}}");
      return split === -1 ? body : body.slice(split + "{{else}}".length);
    });
  }

  out = out.replace(/\{\{([^{}]*)\}\}/g, (_, action) => values[action.trim()] ?? "");

  if (out.includes("{{")) {
    throw new Error(`unresolved template action in fragment: ${out.slice(0, 120)}`);
  }
  return out;
}

// Extraction. Every one of these throws by name, because a template edit that
// moves a control is the case this harness has to report as "the header changed"
// rather than as a null dereference three frames down.
function must(source, file, what, re) {
  const m = source.match(re);
  if (!m) {
    throw new Error(
      `${file} no longer holds ${what} in the shape this harness reads.\n` +
        `  looked for: ${re}\n` +
        `  If the header was deliberately restructured, update tools/render-verify/fixture.mjs\n` +
        `  to match, and re-read M26.5's geometry claims while you are there.`,
    );
  }
  return m;
}

function attr(tag, name) {
  const m = tag.match(new RegExp(`${name}="([^"]*)"`));
  if (!m) throw new Error(`attribute ${name} missing from: ${tag.slice(0, 120)}`);
  return m[1];
}

export async function readHeaderShape(repoRoot) {
  const layout = await readFile(path.join(repoRoot, LAYOUT), "utf8");
  const partials = await readFile(path.join(repoRoot, PARTIALS), "utf8");

  const headerTag = must(layout, LAYOUT, "the <header> element", /<header\b[^>]*>/)[0];
  const navTag = must(layout, LAYOUT, "the header's <nav> container", /<header\b[^>]*>\s*<nav\b[^>]*>/)[0]
    .match(/<nav\b[^>]*>/)[0];
  const barTag = must(layout, LAYOUT, "the identity/bell wrapper", /<div class="ml-auto[^"]*">/)[0];

  const panel = (id) => {
    const tag = must(partials, PARTIALS, `the ${id} panel`, new RegExp(`<div id="${id}"[^>]*>`))[0];
    return { id, popover: attr(tag, "popover"), class: attr(tag, "class") };
  };

  const invoker = (id, values) => {
    const raw = must(
      partials,
      PARTIALS,
      `the invoker for ${id}`,
      new RegExp(`<button\\b[^>]*popovertarget="${id}"[^>]*>[\\s\\S]*?</button>`),
    )[0];
    return resolveActions(raw, values);
  };

  // The email is the one value the fixture varies rather than fixes: M26.5 gives
  // "the email address in the identity button has no fixed width" as the whole
  // reason both panels right-align to the container instead of to their invoker,
  // so the harness renders two widths of it and asserts the panels do not care.
  const emailSpan = must(
    partials,
    PARTIALS,
    "the signed-in address inside the identity invoker",
    /<span class="([^"]*)">\{\{\.Identity\.Email\}\}<\/span>/,
  );

  return {
    headerTag,
    navTag,
    barTag,
    emailSpanClass: emailSpan[1],
    identity: {
      panel: panel("linkctrl-identity-menu"),
      invoker: invoker("linkctrl-identity-menu", { ...VALUES, ".Identity.Email": "@@EMAIL@@" }),
    },
    notification: {
      panel: panel("linkctrl-notification-menu"),
      invoker: invoker("linkctrl-notification-menu", VALUES),
    },
  };
}

const IDENTITY_ITEMS = ["Members", "Invitations", "Workspaces", "Domains", "Reputation feeds", "Account"];

function panelHTML(panel, body) {
  return `<div id="${panel.id}" popover="${panel.popover}" class="${panel.class}">${body}</div>`;
}

// `transform` on the header is the discriminator for the top-layer claim. A
// transformed ancestor becomes the containing block for `position: fixed`
// descendants — so a panel that is merely fixed moves with the header, and a
// panel that is genuinely in the top layer does not. Nothing else in the page
// tells those two apart.
//
// The shift is leftwards on purpose. Overflow past the right edge of the initial
// containing block is scrollable and overflow past the left edge is not, so a
// negative translate moves the header without giving the document a horizontal
// scroll position for the measurement to have to account for.
export const HEADER_SHIFT = -200;

const VARIANTS = {
  default: { email: "dev@killerofpie.com", headerStyle: "" },
  "long-email": {
    email: "an.extremely.long.address.that.stretches.the.invoker@example-organisation.test",
    headerStyle: "",
  },
  "transformed-header": {
    email: "dev@killerofpie.com",
    headerStyle: `transform: translateX(${HEADER_SHIFT}px);`,
  },
};

export const VARIANT_NAMES = Object.keys(VARIANTS);

export function buildFixture(shape, variantName) {
  const variant = VARIANTS[variantName];
  if (!variant) throw new Error(`unknown fixture variant: ${variantName}`);

  const headerTag = variant.headerStyle
    ? shape.headerTag.replace(/^<header\b/, `<header style="${variant.headerStyle}"`)
    : shape.headerTag;

  const identityInvoker = shape.identity.invoker.replaceAll("@@EMAIL@@", variant.email);

  const identityBody = IDENTITY_ITEMS.map(
    (label) => `<a href="#" class="block px-3 py-2 text-sm text-ink hover:bg-sunken">${label}</a>`,
  ).join("") +
    `<form method="post" action="#"><button type="submit" class="block w-full px-3 py-2 text-left text-sm text-ink hover:bg-sunken">Sign out</button></form>`;

  const notificationBody =
    `<p class="border-b border-line px-3 py-2 text-xs font-medium uppercase tracking-wide text-subtle">Notifications</p>` +
    `<ul class="max-h-80 overflow-y-auto">` +
    [1, 2, 3]
      .map(
        (n) =>
          `<li class="border-b border-line px-3 py-2 last:border-b-0"><p class="text-sm font-medium text-ink">Notification ${n}</p><p class="mt-1 text-xs text-subtle">1 Jan 2026, 00:00 UTC</p></li>`,
      )
      .join("") +
    `</ul>` +
    `<a href="#" class="block border-t border-line px-3 py-2 text-sm font-medium text-accent-ink hover:bg-sunken">View all</a>`;

  // The page scrolls on purpose. The template's own note says the right offset
  // uses `%` rather than `vw` so a classic scrollbar cannot throw it off, and a
  // fixture short enough to have no scrollbar would never exercise that.
  const filler = Array.from(
    { length: 40 },
    (_, i) => `<p class="py-4 text-sm text-muted">Row ${i + 1}, so the page is taller than the viewport.</p>`,
  ).join("");

  return `<!doctype html>
<html lang="en" class="h-full">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LinkCtrl render-verify — ${variantName}</title>
<link rel="stylesheet" href="/app.css">
</head>
<body class="min-h-full bg-surface text-ink antialiased">
${headerTag}
  ${shape.navTag}
    <a href="#" class="flex items-center gap-2 font-semibold tracking-tight text-ink">LinkCtrl</a>
    <a href="#" class="text-sm font-medium text-muted hover:text-ink">Dashboard</a>
    <a href="#" class="text-sm font-medium text-muted hover:text-ink">Links</a>
    <a href="#" class="text-sm font-medium text-muted hover:text-ink">API keys</a>
    ${shape.barTag}
      <div class="flex items-center">
        ${shape.notification.invoker}
        ${panelHTML(shape.notification.panel, notificationBody)}
      </div>
      <div class="flex items-center">
        ${identityInvoker}
        ${panelHTML(shape.identity.panel, identityBody)}
      </div>
    </div>
  </nav>
</header>
<main class="mx-auto max-w-6xl px-4 py-8">${filler}</main>
</body>
</html>
`;
}

export async function readStylesheet(repoRoot) {
  return readFile(path.join(repoRoot, STYLESHEET), "utf8");
}
