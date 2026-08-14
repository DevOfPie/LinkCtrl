import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M50.7: the QR tab's codes list, and the four claims about it that no template
// scan can make.
//
// Each of these is a class string in the Go suite and a *behaviour* here, and
// the split is D24's precedent — *"a top-layer element ignores its ancestor's
// containing block, so positioning is verified in a browser rather than
// asserted from markup"* — applied to a list rather than to a header menu:
//
//   selects on its whole area  the row paints a selected background across its
//                              full width while only the name was clickable
//                              (F224f). `after:absolute after:inset-0` on the
//                              anchor is what stretches the target over the
//                              row; whether a click on blank space actually
//                              lands on it is layout, not markup.
//   a real gap                 `ml-4` is a declaration. The owner reported a
//                              destructive control 8px from a download button
//                              on a 22px row, which is a measurement, so it is
//                              measured — in the rendered boxes, both at the
//                              seam where selecting stops and between the two
//                              controls that were adjacent.
//   twenty menus, twenty rows  the part of the popover pattern nav.html leaves
//                              least exercised. One shared `anchor-name`,
//                              scoped per row by `anchor-scope` on the `<li>`;
//                              without the scope every menu in the list
//                              resolves to the last button that declares the
//                              name, and every template test still passes.
//   the fill moves             the default icons are drawn from the row that
//                              holds the flag and the whole list re-renders on
//                              a post, so clicking an empty one has to leave
//                              exactly one filled and it has to be the row that
//                              was clicked. No swap and no script do this
//                              (D188), which is precisely why it is worth
//                              driving: the claim is that a plain
//                              post-and-redirect is enough.
//
// The spec restores the default it moved before it ends, so a second run starts
// where the first one did and the other specs see the instance they expect.
//
// Credentials follow the other signed-in specs exactly: LINKCTRL_UI_EMAIL /
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

// The seeded link that carries more than one code. Every claim below is about a
// *list*, so a link with one row would leave three of the four unexercised —
// and silently, which is the failure mode this looks for rather than accepts.
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

test('a row selects on its whole area, and the action cluster is held off it', async ({ page }) => {
  await signIn(page);
  await openMultiCodeQRTab(page);

  const rows = page.locator('main ul li:has(button[popovertarget^="qr-download-"])');
  const count = await rows.count();
  expect(count, 'the codes list is not a list').toBeGreaterThan(1);

  // The row that is *not* selected, so a click on it has somewhere to go. The
  // selected row carries bg-sunken; picking by the absence of the download
  // menu's own selected marker would be circular, so it is read off the panel
  // link each row points at against the URL the page is on.
  let target = -1;
  for (let i = 0; i < count; i += 1) {
    const cls = await rows.nth(i).getAttribute('class');
    if (!cls.includes('bg-sunken')) {
      target = i;
      break;
    }
  }
  expect(target, 'every row reads as selected, so there is nothing to select').toBeGreaterThan(-1);

  const row = rows.nth(target);
  const before = page.url();

  // --- the gap, measured rather than declared -------------------------------
  const box = await row.boundingBox();
  const cluster = await row.locator('div.relative.z-10').boundingBox();
  expect(cluster, 'the row draws no action cluster').not.toBeNull();
  const nameBox = await row.locator('a[href*="/qr"]').first().boundingBox();
  expect(
    cluster.x - (nameBox.x + nameBox.width),
    'there is no blank space between the selecting area and the acting controls; the ' +
      'owner reported a misclick, which is a distance rather than a class',
  ).toBeGreaterThan(8);

  const menuButton = row.locator('button[popovertarget^="qr-download-"]');
  const removeButton = row.locator('button[name="remove"]');
  if ((await removeButton.count()) === 1) {
    const menuBox = await menuButton.boundingBox();
    const removeBox = await removeButton.boundingBox();
    expect(
      removeBox.x - (menuBox.x + menuBox.width),
      'the destructive control still sits at the cluster gap from a download button, ' +
        'which is the 8px the owner reported (F224f)',
    ).toBeGreaterThan(8);
  }

  // --- the click, on blank space --------------------------------------------
  //
  // A point inside the row, past the name's own text and short of the cluster:
  // the strip that painted as selectable and was not.
  const blankX = Math.round((nameBox.x + nameBox.width + cluster.x) / 2);
  const blankY = Math.round(box.y + box.height / 2);
  await page.mouse.click(blankX, blankY);
  await page.waitForURL((url) => url.toString() !== before, { timeout: 10000 });
  expect(
    page.url(),
    'clicking a row\'s blank area did not change the selected code; the row paints ' +
      'as selectable across its whole width and only the name was the target (F224f)',
  ).toMatch(/[?&]code=/);
});

test('each row\'s download menu opens on its own row and reaches both formats', async ({ page }) => {
  await signIn(page);
  await openMultiCodeQRTab(page);

  const invokers = page.locator('main ul li button[popovertarget^="qr-download-"]');
  const count = await invokers.count();
  expect(count, 'fewer than two menus, so nothing about twenty of them is exercised')
    .toBeGreaterThan(1);

  // Anchoring, which is the whole reason this claim is here. Every menu shares
  // one `anchor-name` and each `<li>` scopes it; a missing scope resolves all
  // of them to the last invoker in the list and the page still renders.
  const supported = await page.evaluate(() => CSS.supports('anchor-scope', '--x'));
  for (let i = 0; i < count; i += 1) {
    const invoker = invokers.nth(i);
    // An attribute selector rather than `#id`: the default code's row can carry
    // an empty slug, so the id it builds ends in a bare `-`, and this spec has
    // no business knowing which shapes of id are safe to write as a fragment.
    const panel = page.locator(`[id="${await invoker.getAttribute('popovertarget')}"]`);

    await invoker.click();
    await expect(panel, `menu ${i} did not open`).toBeVisible();

    if (supported) {
      const inv = await invoker.boundingBox();
      const men = await panel.boundingBox();
      expect(
        Math.abs(men.y - (inv.y + inv.height)),
        `menu ${i} did not hang off its own row's button (button bottom ${inv.y + inv.height}, ` +
          'menu top ' +
          men.y +
          '). One anchor name serves the whole list and `anchor-scope` on the <li> is ' +
          'what keeps it from resolving to the last button in it',
      ).toBeLessThan(12);
    }

    // Both formats from inside it, which is what makes a third one an entry
    // rather than a third button.
    await expect(panel.getByRole('link', { name: 'PNG' })).toHaveCount(1);
    await expect(panel.getByRole('link', { name: 'SVG' })).toHaveCount(1);

    // Escape closes it, which is the reason this is a popover at all (D24) and
    // not a `<details>`.
    await page.keyboard.press('Escape');
    await expect(panel, `menu ${i} did not close on Escape`).toBeHidden();
  }

  // And one entry is followed, so the menu is a way to the file rather than a
  // decoration over one. The response is a download, so it is taken as one.
  await invokers.first().click();
  const first = page.locator(`[id="${await invokers.first().getAttribute('popovertarget')}"]`);
  const [download] = await Promise.all([
    page.waitForEvent('download', { timeout: 15000 }),
    first.getByRole('link', { name: 'PNG' }).click(),
  ]);
  expect(
    await download.suggestedFilename(),
    'following the PNG entry produced no .png',
  ).toMatch(/\.png$/);
});

// The two accessible names an icon can carry, which are also how this spec
// addresses a row: the filled one names the code and its role, the empty one
// names the action and the code it would act on.
//
// **The name is on the glyph, not on the button**, which is icons.html's own
// convention — *"a define's dot is its accessible name"* — and the buttons carry
// no text at all, so the `<svg>`'s `aria-label` is what a screen reader reads
// off them and what this spec addresses them by.
const isDefault = (code) => `${code} is the default code`;
const makeDefault = (code) => `Make ${code} the default code`;
const codeOf = (label) => label.replace(/ is the default code$/, '');
const nameOf = (button) => button.locator('svg').getAttribute('aria-label');

test('clicking an empty default icon moves the fill, and only one stays filled', async ({ page }) => {
  await signIn(page);
  await openMultiCodeQRTab(page);

  const filled = () => page.locator('main ul li button[aria-pressed="true"]');
  const empty = () => page.locator('main ul li button[aria-pressed="false"][name="make_default"]');

  await expect(filled(), 'the list does not draw exactly one filled default icon').toHaveCount(1);
  const was = codeOf(await nameOf(filled()));
  const others = await empty().count();
  expect(others, 'no empty default icon to click').toBeGreaterThan(0);

  // The owner's words: *"It should update all the icons when any of the icons
  // is changed."* Nothing swaps and no script runs — the control posts, the
  // handler redirects, and the list is drawn again from the row that now holds
  // the flag. That is the claim, and this is what checks it is enough.
  const moved = (await nameOf(empty().first())).replace(/^Make (.*) the default code$/, '$1');
  await empty().first().click();
  await page.waitForURL('**qr=defaulted**', { timeout: 15000 });

  await expect(
    filled(),
    'after moving the default the list draws a number of filled icons other than one; ' +
      'a link always has exactly one default (D183) and the icons are drawn from it',
  ).toHaveCount(1);
  expect(
    await nameOf(filled()),
    'the fill did not move to the row that was clicked',
  ).toBe(isDefault(moved));
  await expect(
    empty(),
    'the icons on the other rows did not all come back empty',
  ).toHaveCount(others);

  // Put it back, so a second run of this file starts where the first one did.
  const restore = page
    .locator('main ul li button[name="make_default"]')
    .filter({ has: page.locator(`svg[aria-label="${makeDefault(was)}"]`) });
  await expect(
    restore,
    'the code the default was moved away from has no icon to move it back with',
  ).toHaveCount(1);
  await restore.click();
  await page.waitForURL('**qr=defaulted**', { timeout: 15000 });
  expect(
    await nameOf(filled()),
    'the default was not restored, so the instance is left different from how it was found',
  ).toBe(isDefault(was));
});
