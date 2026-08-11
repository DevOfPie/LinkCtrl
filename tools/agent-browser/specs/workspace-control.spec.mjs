import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M46.6's kept assertions: the two rendered-appearance claims no template test
// can see. The template suite proves the placeholder option is empty and the
// classes are present; only a browser proves what that *looks like* — a closed
// face showing no text at all, and a header that still fits a 360px viewport
// with the fused control in it.
//
// Unlike the clean-console spec this one needs a session: the workspace pair
// renders only in the signed-in shell, and the switcher half only above one
// membership — which the test instance has because it carries `lctl demo`'s
// data. Credentials are NOT committed here: they come from LINKCTRL_UI_EMAIL /
// LINKCTRL_UI_PASSWORD, else from the account table in
// docs/dev-notes/instances.md — the one place a rebuild is already obliged to
// record the new credential. Exactly one sign-in attempt is made (retries are
// 0), so a stale table costs one charge against the lockout counter and a red
// run pointing at the file to fix.

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

// 360px is the bound that shaped the header (M46): the width the round-two
// walkthrough overflowed, and where m46.6.md says the fused control is measured.
test.use({ viewport: { width: 360, height: 780 } });

test('the workspace pair is one control: a chevron-only face, inside 360px', async ({ page }) => {
  const { email, password } = credentials();
  await page.goto('/login');
  await page.fill('#email', email);
  await page.fill('#password', password);
  await page.click('button[type="submit"]');
  await page
    .waitForURL('**/dashboard', { timeout: 10000 })
    .catch(() => {
      throw new Error(
        'sign-in did not reach /dashboard — if the test instance was rebuilt, ' +
          'update the account table in docs/dev-notes/instances.md (its own ' +
          'instructions cover this) or export LINKCTRL_UI_EMAIL / LINKCTRL_UI_PASSWORD',
      );
    });

  const select = page.locator('header select[name="workspace_id"]');
  await expect(
    select,
    'no switcher in the header — the signed-in account needs a second membership, which `make demo` seeds',
  ).toHaveCount(1);

  // The closed face is the chevron alone. A native select displays its
  // selected option's label, so an empty label is what "no visible text"
  // means mechanically — and it is the one mechanism all three engines render
  // as a bare chevron (color:transparent erases the arrow in Chromium and
  // Firefox, which draw it in the text colour).
  expect(
    await select.evaluate((el) => el.selectedOptions[0]?.label ?? null),
    'the closed face shows text; the owner amended B1 to the chevron alone',
  ).toBe('');

  // The list it opens must stay readable: every real option still names an
  // organization and a workspace. Only the placeholder is allowed to be blank.
  const options = await select.evaluate((el) => [...el.options].map((o) => o.label));
  expect(options.length, 'the switcher lists nowhere to go').toBeGreaterThan(1);
  for (const label of options.slice(1)) {
    expect(label, 'a destination option lost its label').not.toBe('');
  }

  // The fused control must not push the header past the viewport: no sideways
  // scroll on the document, and the header itself contained in 360px.
  const widths = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
    header: document.querySelector('header')?.getBoundingClientRect().width ?? 0,
  }));
  expect(
    widths.scroll,
    'the page scrolls sideways at 360px — the constraint that shaped the label (m46.6.md)',
  ).toBeLessThanOrEqual(widths.client);
  expect(widths.header, 'the header is wider than the viewport').toBeLessThanOrEqual(360);
});
