import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M68's browser claim: **the table does not shift when Remove is pressed.**
//
// The owner's amendment on 2026-08-18 chose select-mode over per-row removal, and
// the sentence it chose it with is a measurement: pressing Remove turns each row's
// trailing chevron into a checkbox *in the same column*, so nothing else on the
// page moves. m68.md names the browser harness as what asserts it, and this is
// that assertion — no Go test can make it, because what is being compared is a
// laid-out column's geometry in two states rather than the markup that produced
// it. A pair of hard-coded widths that happened to differ by a pixel would pass
// every template scan in this repository and fail here.
//
// It also carries the console. Two of the pages this milestone adds are the first
// in the product to render a stylesheet-classed badge built in Go and a script
// that relabels a button, and the one thing a markup test cannot see is what the
// browser refused.
//
// **This spec needs an instance with an add-on installed, and since M68's fifth
// attempt the test instance is one.** Every image carries the sample add-on at
// `/addons`; `LINKCTRL_ADDONS_DIR` is what turns it into a running one, and
// `scripts/instance.sh` now sets it for the test instance as well as the demo.
// Before that it was unset here, the manager's routes were not mounted, and both
// add-on specs skipped on a 404 — which is the same string as a pass, and is why
// the bullet this file exists for had never been asserted by anything.
//
// The skip below is kept all the same, for the instance that legitimately has no
// add-on host: an operator who installs nothing is in exactly that state, and it
// is what `LINKCTRL_BASE_URL` pointed anywhere else will find. It says why rather
// than passing quietly, and on the way past it asserts that such an instance
// offers no link to a page that would 404.
//
// Credentials follow every other signed-in spec: LINKCTRL_UI_EMAIL /
// LINKCTRL_UI_PASSWORD, else the account table in docs/dev-notes/instances.md.

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

// The manager's nav entry, addressed by where it lives rather than by its words.
//
// It sits inside the identity menu, which is a `popover="auto"` element and is
// therefore out of the accessibility tree until somebody opens it — so
// `getByRole('link')` finds nothing in either state and would pass the absence
// assertion below on an instance that draws the entry perfectly well. The
// container's id and the href are both stable and neither depends on the menu
// being open. The href alone would not do either: the manager's own pages carry
// Cancel links to the same address.
const navEntry = '#linkctrl-identity-menu a[href="/instance/addons"]';

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

test('the manager table does not shift when Remove turns chevrons into checkboxes', async ({ page }) => {
  const errors = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') errors.push(msg.text());
  });
  page.on('pageerror', (err) => errors.push(`pageerror: ${err.message}`));

  await signIn(page);

  const response = await page.goto('/instance/addons', { waitUntil: 'networkidle' });
  // **404 is a state, not a failure.** An instance with no `LINKCTRL_ADDONS_DIR`
  // has no add-on host, so the manager's routes are not registered at all — which
  // is m60.md's *no route is mounted* still holding, and is exactly what the test
  // instance is. Skip with the reason, because a skip that said nothing would be
  // the same string as a pass.
  //
  // **And on the way past, assert that nothing offered the reader this address.**
  // `addons.manage` is conferred on the instance principal whether or not a host
  // is configured, so a nav entry gated on the permission alone was drawn on
  // exactly the instances where this 404 lives — which is what this spec saw and
  // did not say. The entry is gated on the shell's `AddonManager` as well now, and
  // the state below is the one where that matters.
  if (response.status() === 404) {
    await expect(
      page.locator(navEntry),
      'this instance has no add-on host and the manager 404s, yet the chrome ' +
        'still offers a link to it — internal/httpx/web.go carries AddonManager ' +
        'onto the shell so that partials/nav.html cannot draw one',
    ).toHaveCount(0);
  }
  test.skip(
    response.status() === 404,
    'this instance has no add-on host, so the manager is not mounted, and the ' +
      'nav offers no entry to it. The test instance has one: if this skipped ' +
      'against :8081, LINKCTRL_ADDONS_DIR has gone out of .env.test — the core ' +
      'SLO column takes it out on purpose (docs/slo.md) and puts it back.',
  );
  expect(response.status(), 'the Add-on manager answered unexpectedly').toBe(200);

  // The other direction: where the manager *is* mounted, the entry is there. Both
  // halves in one spec, because a gate that draws nothing anywhere would pass the
  // assertion above and be a worse defect than the one it was added for.
  await expect(
    page.locator(navEntry),
    'the manager is mounted and the chrome offers no way to reach it',
  ).toHaveCount(1);

  const rows = page.locator('table tbody tr');
  const installed = await page.locator('table tbody th[scope="row"]').count();
  test.skip(
    installed === 0,
    'this instance has an add-on host and nothing installed, so there is no row ' +
      'to measure.',
  );

  // The geometry, in the state a reader arrives in: the trailing cell of the
  // first row, and the row's own height.
  const trailing = rows.first().locator('td').last();
  const before = await trailing.boundingBox();
  const firstRowBefore = await rows.first().boundingBox();
  expect(before, 'the first row has no trailing cell').not.toBeNull();
  await expect(
    trailing.locator('a[aria-label^="Open"]'),
    'the resting state draws a chevron that opens the detail page',
  ).toHaveCount(1);

  // Press Remove. It is a link rather than a button — select mode is a state of
  // the page, so it survives a reload and works with scripting off.
  await page.click('a[href="/instance/addons?select=1"]');
  await page.waitForURL('**/instance/addons?select=1');

  const selecting = page.locator('table tbody tr').first().locator('td').last();
  await expect(
    selecting.locator('input[type="checkbox"]'),
    'pressing Remove did not turn the trailing cell into a checkbox',
  ).toHaveCount(1);

  const after = await selecting.boundingBox();
  const firstRowAfter = await page.locator('table tbody tr').first().boundingBox();

  // The measurement m68.md asks for. One pixel of tolerance, because a checkbox
  // and an anchor are different boxes inside a cell whose width the shared
  // template fixes; what is refused is a column that resizes to its content.
  expect(
    Math.abs(after.width - before.width),
    `the trailing column is ${before.width}px at rest and ${after.width}px in ` +
      'select mode. The two states share one template precisely so the table ' +
      'does not shift under the reader (m68.md, owner-amended 2026-08-18)',
  ).toBeLessThanOrEqual(1);
  expect(
    Math.abs(after.x - before.x),
    'the trailing column moved sideways between the two states',
  ).toBeLessThanOrEqual(1);
  expect(
    Math.abs(firstRowAfter.height - firstRowBefore.height),
    'the row changed height between the two states, so every row below it moved',
  ).toBeLessThanOrEqual(1);

  // The counter, which is the one thing on this page that needs script. Without
  // addon-select.js the label stands as the template wrote it; with it, ticking a
  // box puts the count in. Both are correct pages, and this asserts the enhanced
  // one because that is the one the wireframe drew.
  const button = page.locator('[data-addon-remove-button]');
  expect(
    (await button.textContent()).trim(),
    'the button should read plainly before anything is selected',
  ).toBe('Remove selected');
  await selecting.locator('input[type="checkbox"]').check();
  await expect(
    button,
    'ticking a row did not put the count in the button — static/js/addon-select.js ' +
      'is what does that, and a CSP refusal would look exactly like this',
  ).toHaveText('Remove selected (1)');

  expect(
    errors,
    'the console must be clean — every entry here is something the browser refused or a script threw',
  ).toEqual([]);
});

test('nothing on this page destroys anything in one step', async ({ page }) => {
  await signIn(page);
  const response = await page.goto('/instance/addons', { waitUntil: 'networkidle' });
  test.skip(response.status() === 404, 'this instance has no add-on host');

  // **Removal takes three steps and the first two destroy nothing.** `Remove…` is
  // a link into select mode — a state of the page, so it survives a reload and
  // works with scripting off — and the button there opens the confirmation. That
  // shape is the owner's amendment of 2026-08-18, and the property worth asserting
  // is that neither of the first two steps is the act.
  const remove = page.getByRole('link', { name: 'Remove…' });
  if ((await remove.count()) > 0) {
    await remove.first().click();
    await page.waitForURL('**/instance/addons?select=1');
    await expect(
      page.getByRole('button', { name: /^Remove \d+ add-on/ }),
      'pressing Remove… went straight to a confirmation; it opens select mode, ' +
        'because one confirmation covers however many rows are ticked',
    ).toHaveCount(0);

    await page.locator('[data-addon-select]').first().check();
    await page.locator('[data-addon-remove-button]').click();
    const confirm = page.getByRole('button', { name: /^Remove \d+ add-on/ });
    await expect(
      confirm,
      'the removal did not open a confirmation',
    ).toHaveCount(1);
    // Every purge box on it is unticked. This is the assertion the wireframe's
    // amendment turns on: one dialog for many modules means several irreversible
    // decisions in one breath, and a default-on box is where a mis-tick lands.
    const boxes = page.locator('input[type="checkbox"][name^="purge_"]');
    for (let i = 0; i < (await boxes.count()); i++) {
      await expect(
        boxes.nth(i),
        'a purge box on the confirmation is ticked by default',
      ).not.toBeChecked();
    }
    await page.goto('/instance/addons');
  }

  // The orphan list's own purge does open its confirmation directly, because
  // there is nothing to select: one schema, one decision.
  const purge = page.getByRole('button', { name: 'Purge…' });
  if ((await purge.count()) > 0) {
    await purge.first().click();
    await expect(
      page.getByRole('button', { name: 'Delete this data' }),
      'pressing Purge… deleted the schema without asking',
    ).toHaveCount(1);
  }
});
