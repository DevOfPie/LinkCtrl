// The claims. One function per bullet M26.5 says a browser has to settle.
//
import { HEADER_SHIFT } from "./fixture.mjs";

// Everything measured here is measured against the page's own header, not
// against a constant retyped from the template — except where the milestone
// states a constant, in which case the constant is the claim and is asserted as
// one. Where both forms are available the derived one runs first, so a failure
// report names the disagreement rather than the number.

const TOL = 0.5; // px. Sub-pixel layout differs between engines; 0.5 is under
// the smallest difference any of these claims is about (3px of clearance).

const near = (a, b) => Math.abs(a - b) <= TOL;
const px = (n) => `${Math.round(n * 100) / 100}px`;

const IDENTITY = "#linkctrl-identity-menu";
const BELL = "#linkctrl-notification-menu";
const PANELS = [
  { name: "identity menu", panel: IDENTITY },
  { name: "notification bell", panel: BELL },
];

// Widths chosen around the one place the geometry changes behaviour: the
// container stops growing at max-w-6xl (72rem = 1152px), so below it the panel
// sits one gutter from the window and above it it tracks a centred box.
const WIDTHS = [375, 640, 900, 1152, 1153, 1440, 1920];

// Clicking is the faithful way to open one of these, and it costs a step:
// activating a control the browser considers out of view scrolls it into view
// first, which moves every in-flow element under the measurement while leaving
// the fixed panel where it was. That reads exactly like the bug this harness
// exists to catch. At 375px the bar overflows — deliberately, M26.5 keeps mobile
// nav out of scope — so it happens on the narrowest viewport checked here.
// Scroll is therefore returned to the origin after opening, and any check that
// wants a scrolled page scrolls it itself.
async function open(page, panel) {
  await page.click(`button[popovertarget="${panel.slice(1)}"]`);
  await page.waitForFunction((sel) => document.querySelector(sel).matches(":popover-open"), panel, {
    timeout: 5000,
  });
  await page.evaluate(() => window.scrollTo(0, 0));
}

async function closeAll(page) {
  await page.evaluate(() => {
    for (const el of document.querySelectorAll("[popover]")) {
      if (el.matches(":popover-open")) el.hidePopover();
    }
  });
}

// Geometry of one open panel, plus the two things it is claimed to relate to.
async function measure(page, panel) {
  return page.evaluate((sel) => {
    const el = document.querySelector(sel);
    const header = document.querySelector("header");
    const nav = document.querySelector("header nav");
    const invoker = document.querySelector(`button[popovertarget="${sel.slice(1)}"]`);
    const r = el.getBoundingClientRect();
    const h = header.getBoundingClientRect();
    const n = nav.getBoundingClientRect();
    const i = invoker.getBoundingClientRect();
    const cs = getComputedStyle(nav);
    return {
      viewportWidth: document.documentElement.clientWidth,
      panel: { top: r.top, right: r.right, left: r.left, bottom: r.bottom, width: r.width },
      header: { top: h.top, bottom: h.bottom, left: h.left, right: h.right },
      nav: { right: n.right, paddingRight: parseFloat(cs.paddingRight) },
      invoker: { right: i.right, width: i.width },
      openInTopLayer: el.matches(":popover-open"),
    };
  }, panel);
}

// ---------------------------------------------------------------------------

async function popoverIsSupportedAndUsed(page, ctx) {
  const support = await page.evaluate(() => ({
    api: typeof HTMLElement.prototype.showPopover === "function",
    identity: document.querySelector("#linkctrl-identity-menu")?.getAttribute("popover"),
    bell: document.querySelector("#linkctrl-notification-menu")?.getAttribute("popover"),
    // A closed popover is display:none from the UA stylesheet. Read the
    // computed value rather than checkVisibility(), which is newer than the
    // engine floor this milestone set.
    identityHiddenBeforeInvoking:
      getComputedStyle(document.querySelector("#linkctrl-identity-menu")).display === "none",
    bellHiddenBeforeInvoking:
      getComputedStyle(document.querySelector("#linkctrl-notification-menu")).display === "none",
  }));

  const fails = [];
  if (!support.api) fails.push("HTMLElement.prototype.showPopover is missing — engine below the M26.5 floor");
  for (const [what, value] of [["identity menu", support.identity], ["notification bell", support.bell]]) {
    if (value !== "auto") {
      fails.push(`${what} carries popover="${value}", not "auto" — manual dismisses on neither Escape nor an outside click`);
    }
  }
  if (!support.identityHiddenBeforeInvoking) fails.push("identity panel is visible before its invoker is used");
  if (!support.bellHiddenBeforeInvoking) fails.push("notification panel is visible before its invoker is used");

  return {
    detail: `${ctx.engineVersion}, popover="auto" on both panels, both closed at load`,
    fails,
  };
}

// "An open popover sits in the top layer, whose containing block is the viewport
// rather than an ancestor, so `position: absolute` inside the header does not
// anchor the panel to it." The transformed-header variant is what proves it: a
// transformed ancestor becomes the containing block for a merely-fixed
// descendant, and a top-layer element ignores it.
async function topLayerIgnoresAncestor(page, ctx) {
  const fails = [];
  const detail = [];

  await page.setViewportSize({ width: 1440, height: 900 });

  const plain = {};
  await page.goto(`${ctx.baseURL}/default.html`);
  for (const { name, panel } of PANELS) {
    await open(page, panel);
    plain[name] = await measure(page, panel);
    await closeAll(page);
  }

  await page.goto(`${ctx.baseURL}/transformed-header.html`);
  for (const { name, panel } of PANELS) {
    await open(page, panel);
    const moved = await measure(page, panel);

    const headerShift = moved.header.left - plain[name].header.left;
    if (!near(headerShift, HEADER_SHIFT)) {
      fails.push(
        `the transform did not apply: header moved ${px(headerShift)}, expected ${px(HEADER_SHIFT)} — ` +
          `this check would have passed for the wrong reason`,
      );
    }
    const panelShift = moved.panel.right - plain[name].panel.right;
    if (!near(panelShift, 0)) {
      fails.push(
        `${name} panel moved ${px(panelShift)} when its header ancestor was transformed — ` +
          `it is anchored to the header, not to the viewport, so it is not in the top layer`,
      );
    }
    detail.push(`${name}: header moved ${px(headerShift)}, panel moved ${px(panelShift)}`);
    await closeAll(page);
  }

  await page.goto(`${ctx.baseURL}/default.html`);
  return { detail: detail.join("; "), fails };
}

// "the bar is h-14 (3.5rem) plus a 1px border from `sm` up, so 3.75rem clears it
// by 3px and the panel reads as hanging off the header."
//
// At 1440px, which is the `sm`-and-up half of that sentence. M46 made the bar
// wrap to two lines below `sm` and gave the panels `top-[5.25rem]` to match; the
// narrow half is **not** checked here, and saying so is the point of this
// paragraph. It was measured by hand at 360px on the day — bar 81px, panel top
// 84px, the same 3px — and nothing in this file re-measures it.
async function verticalPosition(page, ctx) {
  const fails = [];
  const detail = [];

  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${ctx.baseURL}/default.html`);

  for (const { name, panel } of PANELS) {
    await open(page, panel);
    const m = await measure(page, panel);

    const clearance = m.panel.top - m.header.bottom;
    if (!near(clearance, 3)) {
      fails.push(`${name} panel clears the bar by ${px(clearance)}, not 3px (bar bottom ${px(m.header.bottom)}, panel top ${px(m.panel.top)})`);
    }
    if (!near(m.panel.top, 60)) {
      fails.push(`${name} panel top is ${px(m.panel.top)}, not the 60px that top-[3.75rem] resolves to`);
    }
    if (!near(m.header.bottom, 57)) {
      fails.push(`the bar is ${px(m.header.bottom)} tall, not the 57px (h-14 + 1px border) the 3.75rem was chosen against`);
    }
    detail.push(`${name}: bar ${px(m.header.bottom)}, panel top ${px(m.panel.top)}, clearance ${px(clearance)}`);
    await closeAll(page);
  }

  return { detail: detail.join("; "), fails };
}

// The panel is fixed to the viewport, not scrolled away with the document. If
// `fixed` were dropped the panel would still be top-layer and still look right
// until somebody scrolled.
async function staysPutWhenScrolled(page, ctx) {
  const fails = [];
  const detail = [];

  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${ctx.baseURL}/default.html`);

  for (const { name, panel } of PANELS) {
    await open(page, panel);
    const before = await measure(page, panel);
    await page.evaluate(() => window.scrollTo(0, 400));
    await page.waitForFunction(() => window.scrollY > 0, null, { timeout: 5000 });
    const after = await measure(page, panel);

    if (!near(after.panel.top, before.panel.top)) {
      fails.push(`${name} panel top moved from ${px(before.panel.top)} to ${px(after.panel.top)} over a 400px scroll`);
    }
    detail.push(`${name}: top ${px(before.panel.top)} → ${px(after.panel.top)} after scrolling 400px`);
    await page.evaluate(() => window.scrollTo(0, 0));
    await closeAll(page);
  }

  return { detail: detail.join("; "), fails };
}

// "right-[max(1rem,calc(50%-35rem))] — the container is `mx-auto max-w-6xl px-4`,
// so its content edge is 1rem from the window until the window passes 72rem and
// (50% - 36rem) + 1rem after that. `max()` is the whole expression: one value,
// correct at every width, no media query."
//
// Asserted against the container the sentence names, at widths either side of
// the 72rem hinge — not against a re-typed formula.
async function tracksContainerEdgeAtEveryWidth(page, ctx) {
  const fails = [];
  const detail = [];

  await page.goto(`${ctx.baseURL}/default.html`);

  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    for (const { name, panel } of PANELS) {
      await open(page, panel);
      const m = await measure(page, panel);

      const panelOffset = m.viewportWidth - m.panel.right;
      const containerOffset = m.viewportWidth - (m.nav.right - m.nav.paddingRight);

      if (!near(panelOffset, containerOffset)) {
        fails.push(
          `at ${width}px the ${name} panel sits ${px(panelOffset)} from the window edge, ` +
            `the header container's content edge sits ${px(containerOffset)} — they must be the same edge`,
        );
      }
      if (width <= 1152 && !near(panelOffset, m.nav.paddingRight)) {
        fails.push(
          `at ${width}px (below the 72rem hinge) the ${name} panel sits ${px(panelOffset)} from the window edge, ` +
            `not the container's ${px(m.nav.paddingRight)} gutter`,
        );
      }
      // max-w-[calc(100vw-2rem)]: the panel is never wider than the window.
      if (m.panel.left < -TOL || m.panel.right > m.viewportWidth + TOL) {
        fails.push(
          `at ${width}px the ${name} panel overflows the window: left ${px(m.panel.left)}, ` +
            `right ${px(m.panel.right)}, window ${px(m.viewportWidth)}`,
        );
      }
      if (name === "identity menu") {
        detail.push(`${width}→${px(panelOffset)}`);
      }
      await closeAll(page);
    }
  }

  return { detail: `offset from window edge, by viewport width: ${detail.join(" ")}`, fails };
}

// "Both panels right-align to that same edge rather than to their own invoker,
// because without CSS anchor positioning the markup cannot know where the
// invoker is, and the email address in the identity button has no fixed width."
async function independentOfInvokerWidth(page, ctx) {
  const fails = [];
  await page.setViewportSize({ width: 1440, height: 900 });

  const read = async (variant) => {
    await page.goto(`${ctx.baseURL}/${variant}.html`);
    const out = {};
    for (const { name, panel } of PANELS) {
      await open(page, panel);
      out[name] = await measure(page, panel);
      await closeAll(page);
    }
    return out;
  };

  const short = await read("default");
  const long = await read("long-email");

  const invokerGrowth = long["identity menu"].invoker.width - short["identity menu"].invoker.width;
  if (invokerGrowth < 100) {
    fails.push(
      `the long-email variant grew the identity invoker by only ${px(invokerGrowth)} — ` +
        `the fixture is not exercising the case this claim is about`,
    );
  }
  for (const { name } of PANELS) {
    const drift = long[name].panel.right - short[name].panel.right;
    if (!near(drift, 0)) {
      fails.push(`${name} panel right edge moved ${px(drift)} when the address in the identity button got longer`);
    }
  }

  await page.goto(`${ctx.baseURL}/default.html`);
  return {
    detail: `identity invoker +${px(invokerGrowth)} wide, both panel right edges unchanged`,
    fails,
  };
}

// D24's reason for the element: "a disclosure cannot close on Escape, and the
// bullet below asks for exactly that."
async function escapeCloses(page, ctx) {
  const fails = [];
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${ctx.baseURL}/default.html`);

  for (const { name, panel } of PANELS) {
    await open(page, panel);
    await page.keyboard.press("Escape");
    try {
      await page.waitForFunction((sel) => !document.querySelector(sel).matches(":popover-open"), panel, {
        timeout: 3000,
      });
    } catch {
      fails.push(`${name} is still open after Escape`);
    }
    await closeAll(page);
  }
  return { detail: "both panels closed on Escape", fails };
}

// "Only one auto popover is open at a time — showing either closes the other —
// which is the exclusive behaviour `<details name="…">` was doing here before."
async function onlyOneOpenAtATime(page, ctx) {
  const fails = [];
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${ctx.baseURL}/default.html`);

  await open(page, IDENTITY);
  await open(page, BELL);
  const state = await page.evaluate(() => ({
    identity: document.querySelector("#linkctrl-identity-menu").matches(":popover-open"),
    bell: document.querySelector("#linkctrl-notification-menu").matches(":popover-open"),
  }));
  if (state.identity) fails.push("opening the bell left the identity menu open — both panels were on screen at once");
  if (!state.bell) fails.push("the bell did not open");
  await closeAll(page);

  return { detail: "opening the bell closed the identity menu", fails };
}

// "reachable by keyboard alone". Focus is set directly rather than tabbed to, so
// this asserts keyboard *activation* without depending on three engines agreeing
// about tab order.
async function keyboardOperable(page, ctx) {
  const fails = [];
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${ctx.baseURL}/default.html`);

  for (const { name, panel } of PANELS) {
    await page.focus(`button[popovertarget="${panel.slice(1)}"]`);
    await page.keyboard.press("Enter");
    try {
      await page.waitForFunction((sel) => document.querySelector(sel).matches(":popover-open"), panel, {
        timeout: 3000,
      });
    } catch {
      fails.push(`${name} did not open when its focused invoker was activated with Enter`);
    }
    await closeAll(page);
  }
  return { detail: "both invokers opened from the keyboard", fails };
}

export const CHECKS = [
  ["popover API present, both panels popover=auto", popoverIsSupportedAndUsed],
  ["top layer ignores a transformed ancestor", topLayerIgnoresAncestor],
  ["panel clears the bar by 3px at top 3.75rem", verticalPosition],
  ["panel stays fixed to the viewport when scrolled", staysPutWhenScrolled],
  ["right edge tracks the container's content edge", tracksContainerEdgeAtEveryWidth],
  ["position independent of the invoker's width", independentOfInvokerWidth],
  ["Escape closes both panels", escapeCloses],
  ["only one panel open at a time", onlyOneOpenAtATime],
  ["invokers operable from the keyboard", keyboardOperable],
];
