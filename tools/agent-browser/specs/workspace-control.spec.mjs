import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M46.6's kept assertions: the rendered-appearance claims no template test can
// see. The template suite proves the markup — a chevron-button invoker, a
// popover panel of workspace buttons, the anchor classes; only a browser
// proves what that *looks like* — a closed face showing no text at all, a
// panel that hangs right-aligned off the fused control rather than wherever
// the engine puts it, and a header that still fits a 360px viewport.
//
// The opened state carries most of the weight since the reopening: F209's four
// symptoms all lived there — a native select popup nothing in the product
// could position, style, or purge of its blank placeholder row, and a closed
// face that painted the chosen option's text until the redirect landed. So
// this spec opens the panel, reads its rows, measures its right edge against
// the control's, and drives a real switch there and back — the round trip is
// what proves the invoker's face holds no text after a switch is triggered,
// and it leaves the instance in the workspace it started in, so the other
// signed-in specs see the data they expect.
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

test('the workspace pair is one control: chevron-only face, a panel of its own, inside 360px', async ({ page }) => {
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

  const invoker = page.locator('header button[popovertarget="linkctrl-workspace-menu"]');
  await expect(
    invoker,
    'no switcher in the header — the signed-in account needs a second membership, which `make demo` seeds',
  ).toHaveCount(1);

  // The closed face is the chevron alone. Since the reopening that is true by
  // construction — the invoker is a button whose face is an SVG glyph — so the
  // assertion is that no text node crept in.
  expect(
    (await invoker.textContent()).trim(),
    'the closed face shows text; the owner amended B1 to the chevron alone',
  ).toBe('');

  // The label's title is "Organization · Workspace", and the panel's rows
  // carry the same title — it is how a switch is observed and how the way
  // back is found after switching away.
  const label = page.locator('header p[title]');
  const home = await label.getAttribute('title');

  // The opened state, where all four F209 symptoms lived.
  await invoker.click();
  const panel = page.locator('#linkctrl-workspace-menu');
  await expect(panel, 'clicking the chevron does not open the panel').toBeVisible();

  // No blank row, and every row names its workspace and carries an id to post.
  const rows = panel.locator('button[type="submit"]');
  expect(await rows.count(), 'the panel lists nowhere to go').toBeGreaterThan(0);
  for (const row of await rows.all()) {
    expect((await row.textContent()).trim(), 'a panel row names nothing — the blank placeholder row is back (F209)').not.toBe('');
    expect(await row.getAttribute('value'), 'a panel row posts an empty workspace_id').not.toBe('');
  }

  // Right-aligned with the fused control — the anchor-positioning claim. The
  // control is the invoker's enclosing bordered container, and the panel's
  // right edge must land on its right edge, not on the menus' shared viewport
  // edge and not wherever the engine likes.
  const edges = await page.evaluate(() => {
    const invokerEl = document.querySelector('header button[popovertarget="linkctrl-workspace-menu"]');
    const control = invokerEl.closest('div').getBoundingClientRect();
    const menu = document.getElementById('linkctrl-workspace-menu').getBoundingClientRect();
    return { control: control.right, panel: menu.right, panelLeft: menu.left };
  });
  expect(
    Math.abs(edges.panel - edges.control),
    `the panel's right edge (${edges.panel}) is not the control's (${edges.control}) — F209's first symptom`,
  ).toBeLessThanOrEqual(1);
  expect(edges.panelLeft, 'the open panel overflows the 360px viewport on the left').toBeGreaterThanOrEqual(0);
  expect(edges.panel, 'the open panel overflows the 360px viewport on the right').toBeLessThanOrEqual(360);

  // Drive a real switch through the panel: one POST, and the invoker's face
  // must carry no text once a switch is triggered — the native select painted
  // the chosen option into the w-8 face here (F209's third symptom).
  await rows.first().click();
  await expect(label, 'switching did not change the named workspace').not.toHaveAttribute('title', home);
  expect(
    (await page.locator('header button[popovertarget="linkctrl-workspace-menu"]').textContent()).trim(),
    'the invoker paints text after a switch is triggered (F209)',
  ).toBe('');

  // And back — by the row wearing the home workspace's own title, not by
  // position — so the run leaves the instance where it found it.
  await page.locator('header button[popovertarget="linkctrl-workspace-menu"]').click();
  await page.locator('#linkctrl-workspace-menu').getByTitle(home, { exact: true }).click();
  await expect(label, 'switching back did not restore the original workspace').toHaveAttribute('title', home);

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
