import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// The reopened M47's kept assertion: the tab strip on a link's page does not
// wrap at 360px. That property is *why* tabs beat the drawn alternatives — a
// state line wraps, a rail cannot survive one column — so it is asserted in a
// browser rather than assumed from a class name; the template suite proves the
// classes are present, and only a rendered layout proves what they do.
//
// The spec also drives one real tab switch, because the strip's mechanism is
// an htmx swap (hx-select trimming a full response to the #link-tabs
// container) and no template scan can see whether that swap actually replaces
// the panel and pushes the URL.
//
// Since M47.5 the strip carries badges, and the spec asserts the half of that
// no template scan can: the glyphs render at the measured size — 12px, the
// three-engine answer recorded in decisions.md — and every badged tab draws
// its badge, because a chip that failed to render would put the strip back in
// the bare intermediate state M47 shipped and nothing else red.
//
// Credentials follow workspace-control.spec.mjs exactly: LINKCTRL_UI_EMAIL /
// LINKCTRL_UI_PASSWORD, else the account table in docs/dev-notes/instances.md.
// One sign-in attempt (retries are 0), so a stale table costs one charge
// against the lockout counter and a red run pointing at the file to fix.

const instancesDoc = fileURLToPath(
  new URL('../../../docs/dev-notes/instances.md', import.meta.url),
);

function credentials() {
  const { LINKCTRL_UI_EMAIL: email, LINKCTRL_UI_PASSWORD: password } = process.env;
  if (email && password) return { email, password };
  const doc = readFileSync(instancesDoc, 'utf8');
  const address = doc.match(/\|\s*Address\s*\|\s*`([^`]+)`\s*\|/);
  const pass = doc.match(/\|\s*Password\s*\|\s*`([^`]+)`\s*\|/);
  if (!address || !pass) {
    throw new Error(
      'no credentials: set LINKCTRL_UI_EMAIL and LINKCTRL_UI_PASSWORD, or keep ' +
        'the Address/Password table in docs/dev-notes/instances.md current',
    );
  }
  return { email: address[1], password: pass[1] };
}

// 360px is the bound the strip was designed against: the width at which every
// drawn alternative degraded, and the width the horizontal scroll exists for.
test.use({ viewport: { width: 360, height: 780 } });

test('the link page tab strip scrolls sideways at 360px instead of wrapping', async ({ page }) => {
  const { email, password } = credentials();
  await page.goto('/login');
  await page.fill('#email', email);
  await page.fill('#password', password);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard', { timeout: 10000 }).catch(() => {
    throw new Error(
      'sign-in did not reach /dashboard — if the test instance was rebuilt, ' +
        'update the account table in docs/dev-notes/instances.md or export ' +
        'LINKCTRL_UI_EMAIL / LINKCTRL_UI_PASSWORD',
    );
  });

  // Any link's page does; the test instance carries `lctl demo` data, so the
  // links table has rows. The first row's own link is the cheapest way in.
  await page.goto('/links');
  const row = page.locator('main a[href^="/links/0"]').first();
  await expect(row, 'the links table lists no link to open').toHaveCount(1);
  await row.click();
  await page.waitForURL('**/links/0*');

  const strip = page.locator('nav[aria-label="Link sections"]');
  await expect(strip, 'the link page draws no tab strip').toHaveCount(1);

  // Every tab sits on one row. Wrapping is the failure the design rejected,
  // and it is measured rather than inferred: all anchors share the first one's
  // top edge, whatever the strip's overflow state.
  const tabs = strip.locator('a');
  const count = await tabs.count();
  expect(count, 'the strip lost its tabs').toBeGreaterThanOrEqual(2);
  const tops = [];
  for (let i = 0; i < count; i++) {
    tops.push((await tabs.nth(i).boundingBox())?.y ?? NaN);
  }
  for (const y of tops) {
    expect(y, `a tab wrapped to a second row (tops: ${tops.join(', ')})`).toBe(tops[0]);
  }

  // The badges (M47.5). Five of the seven tabs carry one — Danger has no
  // state to carry, and Edit's protections count went at the F211
  // reopening — whatever this link's configuration, because an empty
  // state is a muted 0 or the cross, never a missing badge.
  const chips = strip.locator('a > span');
  expect(await chips.count(), 'a badged tab is missing its badge').toBe(5);

  // The glyphs render at the measured size. 12px is the answer M46.5's
  // three-engine check gave — at 10px the weighted glyph's outlined share
  // collapses into a smear — so a strip glyph rendering at any other height
  // means the class and the recorded answer have come apart.
  const glyphs = strip.locator('svg');
  const glyphCount = await glyphs.count();
  expect(glyphCount, 'the strip draws no glyph badges').toBeGreaterThanOrEqual(2);
  for (let i = 0; i < glyphCount; i++) {
    const box = await glyphs.nth(i).boundingBox();
    expect(box?.height, 'a strip glyph does not render at the measured 12px').toBe(12);
  }

  // The strip scrolls inside itself: the document must not go sideways.
  const widths = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
  }));
  expect(
    widths.scroll,
    'the page scrolls sideways at 360px — the strip is supposed to scroll inside its own box',
  ).toBeLessThanOrEqual(widths.client);

  // One real switch: the QR tab swaps the panel in and pushes its URL.
  //
  // Keyed on the section's own heading. It was the section's opening sentence
  // until M50.7 rewrote that sentence to reach the tab's prose bound (D188) —
  // a marker made of prose is a marker any edit to the prose breaks, and this
  // spec is about swapping panels rather than about wording.
  await strip.locator('a', { hasText: 'QR' }).first().click();
  await page.waitForURL('**tab=qr**', { timeout: 10000 });
  await expect(
    page.locator('main h2', { hasText: 'QR code' }),
    'the QR tab did not swap its section in',
  ).toHaveCount(1);

  // The heading thumbnail opens the QR tab (the F212 reopening's retarget).
  // The template suite proves the anchor carries the strip's own htmx
  // attributes; only a browser proves that clicking it from another tab swaps
  // the QR panel in. From Analytics, because the heading renders outside
  // #link-tabs and must invoke the swap from wherever the reader is standing.
  await strip.locator('a', { hasText: 'Analytics' }).first().click();
  await page.waitForURL('**tab=analytics**', { timeout: 10000 });
  const thumb = page.locator('a[aria-label="QR code: open the QR tab"]');
  await expect(thumb, 'the heading row draws no QR thumbnail anchor').toHaveCount(1);
  await thumb.click();
  await page.waitForURL('**tab=qr**', { timeout: 10000 });
  await expect(
    page.locator('main h2', { hasText: 'QR code' }),
    'clicking the thumbnail did not swap the QR tab in',
  ).toHaveCount(1);
});
