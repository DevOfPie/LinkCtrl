import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M50.8: the five claims on the QR tab that only a browser can make.
//
// The Go suite asserts markup — a class, an attribute, an id — and every one of
// these is something else:
//
//   the two inputs agree     `qr-size.js` copies whichever of the slider and the
//                            number moved into the other. Nothing in the markup
//                            says that happens; the script either runs under
//                            `script-src 'self'` or it does not, and a template
//                            test cannot tell the two apart. This is the one
//                            claim in the milestone that *only* a browser can
//                            make, which is why the script exists at all.
//   the tooltip appears      D192 chose a tooltip this page owns precisely so
//                            that something could watch it appear — the native
//                            one has no DOM presence at all. Both triggers are
//                            driven: a pointer over the button but *outside the
//                            glyph*, which is the geometry the owner reported,
//                            and focus on a disabled control, which the native
//                            tooltip could never reach.
//   the hover is visible     F238(j) is a colour resolving to the colour behind
//                            it on the selected row. That is computed style, and
//                            no class string tells you two tokens came out the
//                            same.
//   the save comes back      the scroll position after a redirect is a property
//                            of the load, not of the markup.
//   and comes back once      added at this milestone's reopening (F244a). Where
//                            the page *settles* was already true; what the
//                            owner saw was a second position on the way there,
//                            and only a browser can observe the interval
//                            between a load and the frame that follows it.
//   and does not come back   the same offset, stored for a write whose response
//   twice                    swapped instead of loading, must not survive to be
//                            applied to some unrelated later load. Storage
//                            outliving a navigation is invisible to every other
//                            kind of test there is.
//
// **Each case asserts its own precondition**, which m50.8.md's risk section
// asks for in as many words: a spec that silently stopped exercising the thing
// it drives looks exactly like a spec that passes, and F237 is that failure one
// level down. So every case fails loudly when the fixture it needs is not there
// rather than skipping quietly.
//
// The specs restore what they moved before they end, so a second run starts
// where the first one did and the other specs see the instance they expect.
//
// Credentials follow the other signed-in specs exactly: LINKCTRL_UI_EMAIL /
// LINKCTRL_UI_PASSWORD, else the account table in docs/dev-notes/instances.md.
//
// **This file signs in twice and not once per case, which the other specs do,
// and the reason is a limit rather than a preference.** `LOGIN_RATE_PER_MIN`
// defaults to **10** and the whole kept suite runs in well under a minute from
// one address, so a file signing in per case puts the run over it — every spec
// then fails at the sign-in step, saying the credentials are wrong when they are
// not. Found the hard way, on this file's first green-except-for-that run. The
// scripted cases therefore share one signed-in page in a serial block, and the
// script-blocked case takes the second because it needs a context configured
// differently. The cost is that a failure early in the block stops the rest,
// which serial mode makes visible rather than confusing.
//
// **The current number, because "it fits" is not something a later author can
// act on**: `make verify-ui` signs in **8** times against a limit of 10 — one
// each from link-tabs, workspace-control and qr-logo, three from qr-codes-list
// (one per case) and two from here. **Two spare.** A third sign-in added
// anywhere, by any spec, is one away from red, and the failure will name the
// wrong cause on every file rather than on the one that added it. Counted from
// the tree rather than remembered; F242 is the row for the headroom itself.

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

async function signIn(page) {
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
}

// The first link the instance lists, opened on its QR tab. Enough for the size
// control and the scroll position, which are properties of the tab rather than
// of a list with more than one row in it.
async function openQRTab(page) {
  await page.goto('/links');
  const row = page.locator('main a[href^="/links/0"]').first();
  await expect(row, 'the links table lists no link to open').toHaveCount(1);
  const linkPage = (await row.getAttribute('href')).split('?')[0];
  await page.goto(`${linkPage}?tab=qr`);
  await expect(
    page.locator('#qr_size'),
    'the QR tab draws no size control, so nothing below is exercising it',
  ).toHaveCount(1);
  return linkPage;
}

// A link carrying more than one code, for the claims that are about a *list*.
async function openMultiCodeQRTab(page) {
  await page.goto('/links');
  const rows = page.locator('main a[href^="/links/0"]');
  await expect(rows.first(), 'the links table lists no link to open').toHaveCount(1);
  const hrefs = await rows.evaluateAll((els) => els.map((e) => e.getAttribute('href')));
  const seen = new Set();
  for (const href of hrefs) {
    const linkPage = href.split('?')[0];
    if (seen.has(linkPage)) continue;
    seen.add(linkPage);
    await page.goto(`${linkPage}?tab=qr`);
    if ((await page.locator('main ul li button[popovertarget^="qr-download-"]').count()) > 1) {
      return linkPage;
    }
  }
  throw new Error(
    'no seeded link carries more than one QR code, so the codes list cannot be ' +
      'driven as a list. `make demo` reseeds the test instance; M50 is the ' +
      'milestone whose seeder row puts a second code on a link',
  );
}

test.describe.configure({ mode: 'serial' });

let page;

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage();
  await signIn(page);
});

test.afterAll(async () => {
  await page.close();
});

test('the slider and the number follow each other while they move', async () => {
  await openQRTab(page);

  const slider = page.locator('#qr_size_slider');
  const number = page.locator('#qr_size');
  const min = Number(await slider.getAttribute('min'));
  const max = Number(await slider.getAttribute('max'));
  expect(max, 'the slider has no usable range').toBeGreaterThan(min);

  // The precondition, stated: the script has to be the thing that loaded, not
  // an inline handler and not htmx. A page that failed to fetch it would fail
  // every assertion below for a reason nobody could see from the failure.
  const loaded = await page.evaluate(() =>
    Array.from(document.querySelectorAll('script[src]')).some((s) =>
      s.getAttribute('src').includes('qr-size.js'),
    ),
  );
  expect(loaded, 'the page does not load static/js/qr-size.js at all').toBe(true);

  // Slider → number. Dragged rather than assigned: `fill()` on a range input
  // sets the value and fires the event, which is the same path a drag takes,
  // and it is the only way to move a range input deterministically.
  const target = String(Math.round((min + max) / 2));
  await slider.fill(target);
  await expect(
    number,
    'dragging the slider left the number where it was, which is exactly what the ' +
      'owner reported: the number lies about what will be saved until you save it',
  ).toHaveValue(target);

  // Number → slider.
  const typed = String(Math.min(max, Number(target) + 37));
  await number.fill(typed);
  await expect(
    slider,
    'typing a size left the slider where it was; the owner asked for both ' +
      'directions in the same sentence',
  ).toHaveValue(typed);

  // And the one place the mirror deliberately does not run. A number above the
  // range must not move the slider: a range input clamps, and a clamped slider
  // is a value nobody typed being posted as if somebody had.
  //
  // **This case stops here on purpose.** The slider has already been dragged
  // above, so it is off `size_shown` and would win the server's arbitration
  // whatever the box holds — saving now would store the dragged size and refuse
  // nothing. That is F240, it is D182's rule and not this milestone's, and
  // driving it to the save would be writing an assertion around the defect.
  // What is asserted is the mirror's own behaviour, which is what M50.8 owns.
  const tooBig = String(max + 1000);
  await number.fill(tooBig);
  await expect(
    slider,
    'a size above the range moved the slider, which clamps it — the box would then ' +
      'lose the arbitration to a number nobody typed even on a form nobody dragged',
  ).toHaveValue(typed);
});

test('a style save comes back to where the reader was', async () => {
  const linkPage = await openQRTab(page);

  // The shared page outlives this case, so the viewport is put back at the end
  // rather than left short for whatever runs next.
  const was = page.viewportSize();
  await page.setViewportSize({ width: 1000, height: 400 });

  // **Where a save lands with nothing remembered**, which is what this case has
  // to be told apart from: `qrReturn`'s link-page branch carries `#qr`, so the
  // browser performs a fragment scroll of its own to the top of the QR card.
  // Asserting only that the control is *on screen* afterwards would pass on that
  // alone — it did, on this case's first draft, with the restoration deleted.
  await page.goto(`${linkPage}?tab=qr#qr`);
  const atFragment = await page.evaluate(() => Math.round(window.scrollY));

  const save = page.locator('#qr button[type="submit"]:not([name])');
  await expect(save, 'the QR tab draws no Save control').toHaveCount(1);
  await save.scrollIntoViewIfNeeded();
  const before = await page.evaluate(() => Math.round(window.scrollY));

  expect(
    Math.abs(before - atFragment),
    'the fragment already lands within a screen of where the reader is, so this ' +
      'case cannot tell a remembered position from `#qr` and is asserting nothing',
  ).toBeGreaterThan(150);

  await save.click();
  await page.waitForURL('**qr=styled**', { timeout: 15000 });
  const after = await page.evaluate(() => Math.round(window.scrollY));

  // Back where the reader was. The tolerance is the notice the save renders,
  // which is real content arriving above the section and shifts everything below
  // it — the offset is restored, not the pixel row.
  expect(
    Math.abs(after - before),
    `after a save the page is at ${after} and the reader was at ${before}. The ` +
      'position is remembered across the write and put back; a save that lands ' +
      'anywhere else is the reset the owner called jarring and bad UX',
  ).toBeLessThan(150);
  expect(
    Math.abs(after - atFragment),
    'the save landed where the bare fragment lands, so the remembered position ' +
      'is not what put the page there',
  ).toBeGreaterThan(100);
  await expect(
    page.locator('#qr button[type="submit"]:not([name])'),
    'the Save control is off screen after a save',
  ).toBeInViewport();

  await page.setViewportSize(was);
});

// F244(a), and it is the case the one above could not be: *"immediately upon
// changing the default the screen reloads at the top of the page then jumps
// down to match the previous scroll position."*
//
// **The endpoint was never the complaint.** The case above asserts where the
// page settles, which was true before this reopening and is still true — the
// reader does arrive where they were. What they are shown on the way is a
// second position, because `qrReturn`'s `#qr` fragment and the remembered
// offset were both aiming at the same scroll and the browser applies the
// fragment after `DOMContentLoaded`. So this case asserts the **interval**: the
// document's scroll offset at every point it can be observed, from the first
// script through a second of frames, is the one the reader stood at.
//
// **The observation that catches it is the `load` event**, and it is recorded
// here because it is not obvious: measured against the build this reopening
// corrects, the trail reads `DCL 230, load 409, frames 230` — the fragment
// scroll lands between the two events and the second restoration undoes it
// before the first frame is painted. A rAF sampler alone therefore sees one
// value in headless and concludes the page is fine, which is the third probe
// that missed this. The event handlers run inside the window the frames cannot
// see.
//
// **Three preconditions, because three probes failed on exactly these.** The
// control is addressed by `name="make_default"` and asserted present — the
// first probe searched for `name="default"` and skipped in silence, which is
// F237's shape. The reader's position is set explicitly and the control is
// asserted **already in the viewport**, so Playwright's click does not scroll
// it into view and store a position no reader held — the second probe's
// failure. And that position is asserted far enough from where the bare
// fragment lands that the two are distinguishable at all.
test('a save is never shown a position the reader did not stand at', async () => {
  const linkPage = await openMultiCodeQRTab(page);

  const was = page.viewportSize();
  await page.setViewportSize({ width: 1000, height: 400 });

  // Every offset this document ever holds, recorded from before its own
  // scripts run. `addInitScript` re-runs per document, so what is read after
  // the write is the loaded page's own trail and not the previous one's.
  //
  // **Scoped to this case, and that is not tidiness.** `page` is the file's
  // shared serial one, so instrumentation left installed rides every document
  // the four cases sharing it after this load: a `__qrTrail`, a capture-phase
  // scroll listener and a frame loop, none of which any of them asked for and
  // none of which would fail if they broke something — which is the whole
  // hazard. Five cases follow this one; the scripts-disabled case runs in its
  // own context and is the one that would not have inherited it anyway.
  // `addInitScript` returns a disposable, and it is disposed as soon as the
  // trail has been read. The frame loop is bounded as well, so the one document
  // still open at that point stops sampling on its own rather than running for
  // the rest of the file.
  const trailProbe = await page.addInitScript(() => {
    window.__qrTrail = [];
    const at = (why) =>
      window.__qrTrail.push({ why, y: Math.round(window.scrollY || window.pageYOffset || 0) });
    addEventListener('scroll', () => at('scroll'), { capture: true, passive: true });
    document.addEventListener('DOMContentLoaded', () => at('DOMContentLoaded'));
    addEventListener('load', () => at('load'));
    let frames = 0;
    const frame = () => {
      at('frame');
      if (frames++ < 120) requestAnimationFrame(frame);
    };
    requestAnimationFrame(frame);
  });

  // Where the bare fragment lands, which is the position this case has to be
  // able to tell the reader's from.
  await page.goto(`${linkPage}?tab=qr#qr`);
  const atFragment = await page.evaluate(() => Math.round(window.scrollY));

  await page.goto(`${linkPage}?tab=qr`);
  const make = page.locator('main ul li button[name="make_default"]').first();
  await expect(
    make,
    'the codes list draws no enabled default control. The control is ' +
      '`name="make_default"`; a probe that looked for `name="default"` found ' +
      'nothing and reported success, which is what this assertion exists to stop',
  ).toHaveCount(1);

  // Which row holds the flag now, so it can be given back at the end. This
  // case is the only one in the file that moves a link's default, and the
  // file's rule is that a second run starts where the first one did.
  const wasDefault = await page
    .locator('main ul li:has(button[aria-pressed="true"]) a[href*="/qr?code="]')
    .first()
    .textContent();

  // The reader's own position: the control near the bottom of the viewport,
  // set by scrolling the document rather than by anything that touches the
  // control.
  await make.evaluate((el) => {
    window.scrollTo(0, Math.round(el.getBoundingClientRect().top + window.scrollY - 300));
  });
  const before = await page.evaluate(() => Math.round(window.scrollY));
  await expect(
    make,
    'the default control is outside the viewport, so clicking it would scroll ' +
      'the page first and the position stored would be the driver\'s rather ' +
      'than the reader\'s',
  ).toBeInViewport();
  expect(
    Math.abs(before - atFragment),
    `the reader is at ${before} and the bare fragment lands at ${atFragment}; the ` +
      'two are within a screen of each other, so a jump between them would be ' +
      'invisible and this case is asserting nothing',
  ).toBeGreaterThan(100);

  await make.click();
  await page.waitForURL(/[?&]qr=(defaulted|promoted)/, { timeout: 15000 });
  await page.waitForLoadState('load');
  const trail = await page.evaluate(() => window.__qrTrail);
  // Off the shared page before anything else, including before the navigation
  // that gives the default back — every document from here on is uninstrumented.
  await trailProbe.dispose();

  expect(
    trail.length,
    'nothing was recorded on the loaded page, so the trail this case reads is ' +
      'not being produced and every assertion below is vacuous',
  ).toBeGreaterThan(2);
  expect(
    trail.some((s) => s.why === 'load'),
    'the load event was never observed, and it is the one point in the interval ' +
      'where the fragment scroll was visible',
  ).toBe(true);

  const strayed = trail.filter((s) => Math.abs(s.y - before) > 20);
  expect(
    strayed,
    `the reader stood at ${before} and the page they came back to held ` +
      `${JSON.stringify(strayed)} on the way. A save returns you to where you ` +
      'were; it does not show you somewhere else first (F244a)',
  ).toEqual([]);

  // The flag back where it was found.
  const restore = page
    .locator('main ul li', { hasText: wasDefault.trim() })
    .locator('button[name="make_default"]')
    .first();
  await expect(
    restore,
    `the row that held the default ("${wasDefault.trim()}") no longer offers the ` +
      'control that would give it back, so this case cannot leave the instance ' +
      'as it found it',
  ).toHaveCount(1);
  await restore.click();
  await page.waitForURL(/[?&]qr=(defaulted|promoted)/, { timeout: 15000 });

  await page.setViewportSize(was);
});

// Not an image at all, under a name that says it is — the same file qr-logo's
// spec is refused with, and refused for the same reason: the decoder is chosen
// by the bytes rather than by the filename.
const notAnImage = Buffer.from('this is not a PNG, whatever it is called\n');

test('a refused upload leaves no position behind for a later load', async () => {
  const linkPage = await openQRTab(page);

  const was = page.viewportSize();
  await page.setViewportSize({ width: 1000, height: 400 });

  // The logo upload is the one write on this tab that posts over htmx, so it is
  // the one that can store a position and then never produce the load that
  // reads it back: a refusal answers 200 and swaps `#qr` in place. Without the
  // swap listener the offset survives in `sessionStorage`, keyed to this
  // pathname, until *some* later load of it — which is this case.
  const logo = page.locator('#qr_logo');
  await expect(
    logo,
    'the QR tab draws no logo file input, so no htmx write on it can be driven',
  ).toHaveCount(1);

  const [chooser] = await Promise.all([
    page.waitForEvent('filechooser'),
    logo.click(),
  ]);
  const before = await page.evaluate(() => Math.round(window.scrollY));
  expect(
    before,
    'the page is still at the top with the logo control reached, so a restored ' +
      'position and a fresh load would be the same number and this case asserts nothing',
  ).toBeGreaterThan(200);

  const urlBefore = page.url();
  await chooser.setFiles({ name: 'logo.png', mimeType: 'image/png', buffer: notAnImage });

  await expect(
    page.locator('#qr').getByText('a logo is a PNG or a JPEG, decided by what is in the file'),
    'the refusal never arrived, so no swap happened and the thing this case is ' +
      'about — a stored position with no load coming — was never set up',
  ).toBeVisible({ timeout: 15000 });
  expect(
    page.url(),
    'the refusal navigated instead of swapping, which is the one shape that would ' +
      'consume the stored position legitimately',
  ).toBe(urlBefore);

  // A fresh load of the **same pathname**, which is what the stored offset is
  // keyed to. The cache-buster keeps the path identical while guaranteeing a new
  // navigation rather than a reload the browser might restore scroll for; going
  // via another page instead would consume the offset on that page's own load
  // and prove nothing.
  await page.goto(`${linkPage}?tab=qr&cb=${Date.now()}`);
  const after = await page.evaluate(() => Math.round(window.scrollY));
  expect(
    after,
    `an unrelated later load of this page landed at ${after}, where the reader ` +
      `stood at ${before} during an upload that was refused. The position outlived ` +
      'the load it was stored for',
  ).toBeLessThan(100);

  await page.setViewportSize(was);
});

test('the tooltip shows anywhere on the button, and on focus where the button refuses it', async () => {
  await openMultiCodeQRTab(page);

  // --- a pointer, on the button but outside the glyph -----------------------
  //
  // This is the geometry the owner reported: the tooltip was a <title> inside a
  // 12px <svg> in a ~30px control, so most of the button showed nothing.
  const make = page.locator('main ul li button[name="make_default"]').first();
  await expect(make, 'no enabled default control to hover').toHaveCount(1);
  const tipID = await make.getAttribute('aria-describedby');
  expect(tipID, 'the default control names no tooltip').toBeTruthy();
  const tip = page.locator(`[id="${tipID}"]`);
  await expect(tip, 'the tooltip is visible before anything hovered it').toBeHidden();

  const box = await make.boundingBox();
  const glyph = await make.locator('svg').boundingBox();
  expect(
    glyph.width + 8,
    'the glyph fills the button, so hovering "outside the glyph, inside the ' +
      'button" is not a distinction this page can make and the case proves nothing',
  ).toBeLessThan(box.width);

  // Two pixels inside the button's left edge — inside the padding, well clear
  // of the glyph.
  await page.mouse.move(Math.round(box.x + 2), Math.round(box.y + box.height / 2));
  await expect(
    tip,
    'hovering the button outside its glyph shows no tooltip. The owner asked that ' +
      '"anywhere a click on the button would be accepted should also show the ' +
      'tooltip when hovered" (F238i)',
  ).toBeVisible({ timeout: 5000 });
  expect(
    (await tip.textContent()).trim(),
    'the tooltip says something other than the owner\'s wording',
  ).toBe('Make Default QR Code');

  await page.mouse.move(0, 0);
  await expect(tip, 'the tooltip stayed up after the pointer left').toBeHidden();

  // --- a keyboard, on a control that refuses focus --------------------------
  //
  // The default code's own icon is `disabled`, so it cannot be focused at all —
  // which is why the native tooltip could never have reached a keyboard reader
  // and why D192 put the tooltip on a focusable host instead.
  const filled = page.locator('main ul li button[aria-pressed="true"]').first();
  await expect(filled, 'no disabled default control on the list').toHaveCount(1);
  expect(
    await filled.isDisabled(),
    'the filled default control is not disabled, so this case is not exercising ' +
      'the state the native tooltip could not reach',
  ).toBe(true);

  const filledTipID = await filled.getAttribute('aria-describedby');
  const filledTip = page.locator(`[id="${filledTipID}"]`);
  // Addressed by the control's own state rather than by a `has:` filter: a
  // filter's inner locator resolves against the host, so a page-rooted selector
  // inside one matches nothing.
  const host = page.locator('main ul li .qr-tip-host:has(button[aria-pressed="true"])').first();
  await host.focus();
  await expect(
    filledTip,
    'focusing the host of a disabled control shows no tooltip; a keyboard reader ' +
      'therefore never meets the explanation, which is the half of D192 the cheap ' +
      'option could not buy',
  ).toBeVisible({ timeout: 5000 });
});

test('the row controls answer the pointer on the selected row', async () => {
  await openMultiCodeQRTab(page);

  // **A row has to be selected first, and that is not the landing state.** The
  // link page selects a code only when the URL names one, and on a link whose
  // codes all carry slugs no row is selected until the reader picks one — which
  // is exactly what picking one does: it follows that row's own panel URL. So
  // the spec picks one, the way a reader does.
  const first = page.locator('main ul li a[href*="/qr?code="]').first();
  await expect(first, 'no row links to its own panel, so nothing can be selected').toHaveCount(1);
  await first.click();
  await page.waitForURL(/[?&]code=/, { timeout: 10000 });

  const rows = page.locator('main ul li:has(button[popovertarget^="qr-download-"])');
  const count = await rows.count();
  expect(count, 'the codes list is not a list').toBeGreaterThan(1);

  // The selected row is the one this defect is about: it paints `bg-sunken`,
  // and until M50.8 the controls on it hovered to the same token.
  let selected = -1;
  for (let i = 0; i < count; i += 1) {
    if ((await rows.nth(i).getAttribute('class')).includes('bg-sunken')) {
      selected = i;
      break;
    }
  }
  expect(
    selected,
    'no row reads as selected, so the row this defect is about is not on screen',
  ).toBeGreaterThan(-1);

  const row = rows.nth(selected);
  const rowBG = await row.evaluate((el) => getComputedStyle(el).backgroundColor);
  const control = row.locator('button[popovertarget^="qr-download-"]');

  await control.hover();
  const hovered = await control.evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(
    hovered,
    'hovering a control on the selected row paints it the row\'s own background, ' +
      'so the affordance is written and draws nothing — which is exactly what was ' +
      'reported (F238j)',
  ).not.toBe(rowBG);
  expect(
    hovered,
    'hovering a control on the selected row paints nothing at all',
  ).not.toBe('rgba(0, 0, 0, 0)');
});

test('the add prompt opens beside the counter and adds a code', async () => {
  const linkPage = await openQRTab(page);

  const invoker = page.locator('main button[popovertarget="qr-add"]');
  await expect(
    invoker,
    'the QR tab draws no add control beside the counter (F238h)',
  ).toHaveCount(1);
  const prompt = page.locator('[id="qr-add"]');
  await expect(prompt, 'the add prompt is open before anything invoked it').toBeHidden();

  await invoker.click();
  await expect(prompt, 'the add control opened nothing').toBeVisible();

  // Anchored to the counter rather than centred, where the engine supports it.
  // Same check the row menus get, and for the same reason: a popover with no
  // anchoring still renders and still looks plausible.
  //
  // **Guarded on `position-anchor`, not on `anchor-scope`**, which the row
  // menus' own case uses. The two are different features and this control uses
  // only the first — one counter, one anchor name, nothing scoped — so its
  // stylesheet rule is gated on `position-anchor` too. Guarding here on
  // `anchor-scope` would skip the assertion on exactly the engine where a
  // wrongly-shared gate would have broken the placement.
  if (await page.evaluate(() => CSS.supports('position-anchor', '--x'))) {
    const inv = await invoker.boundingBox();
    const men = await prompt.boundingBox();
    expect(
      Math.abs(men.y - (inv.y + inv.height)),
      'the add prompt does not hang off the counter\'s own button',
    ).toBeLessThan(12);
  }

  const before = await page.locator('main ul li button[popovertarget^="qr-download-"]').count();
  const name = `Spec poster ${Date.now()}`;
  await prompt.locator('#qr_label').fill(name);
  await prompt.getByRole('button', { name: 'Add the code' }).click();
  await page.waitForURL('**qr=added**', { timeout: 15000 });

  const rows = page.locator('main ul li button[popovertarget^="qr-download-"]');
  await expect(
    rows,
    'adding a code through the prompt did not add a row; the prompt posts the ' +
      'field LinkQRStyle reads or it swallows what somebody typed',
  ).toHaveCount(before + 1);
  await expect(
    page.locator('main ul li', { hasText: name }),
    'the code was added under some other name than the one that was typed',
  ).toHaveCount(1);

  // Escape closes it, which is the reason this is a popover (D24).
  await page.goto(`${linkPage}?tab=qr`);
  await page.locator('main button[popovertarget="qr-add"]').click();
  await expect(page.locator('[id="qr-add"]')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('[id="qr-add"]'), 'the add prompt does not close on Escape').toBeHidden();

  // Put the instance back. The row this made is removable because the link now
  // carries more than one code, which is the state that makes the control live.
  const added = page.locator('main ul li', { hasText: name });
  await added.locator('button[name="remove"]').click();
  await page.waitForURL(/qr=(removed|promoted)/, { timeout: 15000 });
  await expect(
    page.locator('main ul li', { hasText: name }),
    'the code this spec added is still there, so the instance is left different ' +
      'from how it was found',
  ).toHaveCount(0);
});

// The script-blocked path, which is the milestone's degradation bullet: with
// `qr-size.js` never running, the number keeps its own value and the form still
// saves it. htmx is off too, so every control on the tab is an ordinary form —
// which is what this product shipped before M50.8 and what it must go on being
// for a reader with JavaScript off.
test.describe('with scripts disabled', () => {
  test.use({ javaScriptEnabled: false });

  test('the size control still saves', async ({ page: plain }) => {
    // Deliberately shadowing the shared, signed-in page above: this case needs a
    // context with scripts disabled, which is a fixture rather than a setting,
    // so it takes Playwright's own page and signs in for itself.
    const page = plain;
    const { email, password } = credentials();
    await page.goto('/login');
    await page.fill('#email', email);
    await page.fill('#password', password);
    await page.click('button[type="submit"]');
    await expect(
      page.locator('main'),
      'sign-in did not work without scripts, so nothing below is reachable',
    ).toBeVisible();

    await page.goto('/links');
    const row = page.locator('main a[href^="/links/0"]').first();
    const linkPage = (await row.getAttribute('href')).split('?')[0];
    await page.goto(`${linkPage}?tab=qr`);

    const number = page.locator('#qr_size');
    await expect(number, 'the QR tab draws no size box without scripts').toHaveCount(1);
    const was = await number.inputValue();

    // The precondition: the mirror is genuinely not running. If it were, the
    // slider would move with the box and the test below would be exercising the
    // scripted path under a name that says otherwise.
    const want = String(Number(was) === 512 ? 384 : 512);
    await number.fill(want);
    await expect(
      page.locator('#qr_size_slider'),
      'the slider followed the number with JavaScript disabled, so the script is ' +
        'running and this case is not the degradation path it claims to be',
    ).toHaveValue(was);

    await page.locator('#qr button[type="submit"]:not([name])').click();
    await page.waitForURL('**qr=styled**', { timeout: 15000 });
    await expect(
      page.locator('#qr_size'),
      'the size typed with scripts disabled did not survive the save. The number ' +
        'input carries its own value and the form posts it; the script is an ' +
        'improvement on that, never a requirement for it',
    ).toHaveValue(want);

    // Put it back.
    await page.locator('#qr_size').fill(was);
    await page.locator('#qr button[type="submit"]:not([name])').click();
    await page.waitForURL('**qr=styled**', { timeout: 15000 });
    await expect(page.locator('#qr_size')).toHaveValue(was);
  });
});
