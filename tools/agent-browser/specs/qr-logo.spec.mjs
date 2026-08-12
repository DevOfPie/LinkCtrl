import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M50.5's reopening: the logo upload applies itself, and both of the things
// that makes true are invisible to a template test.
//
// What the Go suites already prove is the markup — `hx-trigger="change"`,
// `hx-post`, `hx-encoding`, `hx-target`, `hx-select` on the form — and the
// handler's answer to a request that carried `HX-Request: true` because a test
// set the header by hand. Neither of those is htmx *binding* a `change` on a
// `<form>` to a multipart post, which is the whole mechanism the two-step
// button was traded for. So it is driven here.
//
// Two runs through it, because the two answers take different routes out of the
// handler and only one of them is a swap:
//
//   accepted — `seeOther` answers an htmx request with `HX-Redirect`, so the
//              browser navigates and the notice arrives on a fresh page;
//   refused  — `finishQRAction` answers **200** rather than 422 (F214c/F218),
//              because htmx's default `responseHandling` does not swap a 4xx at
//              all. That claim about htmx is the reason the status is what it
//              is, and nothing in this repository exercised it until this spec:
//              a refusal that answered 422 would leave the page silent, which
//              is exactly what is asserted against here.
//
// It also settles the pressed state's keyboard limb. `input.css` narrows the
// busy style to `:focus:not(:focus-visible)` so that tabbing onto the control
// does not paint a permanent pressed look, and that rests on the platform
// matching `:focus-visible` for keyboard focus and not for a pointer click —
// a behaviour with no markup to scan. Both halves are checked below, against
// the element's own `matches()` rather than against a computed colour, because
// the selector is the claim.
//
// Credentials follow the other signed-in specs exactly: LINKCTRL_UI_EMAIL /
// LINKCTRL_UI_PASSWORD, else the account table in docs/dev-notes/instances.md.
// One sign-in attempt (retries are 0), so a stale table costs one charge
// against the lockout counter and a red run pointing at the file to fix.
//
// The spec removes the logo it uploaded before it ends, so a second run starts
// where the first one did and the other specs see the instance they expect.

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

// A 64×64 checkerboard, 199 bytes, encoded by Go's image/png and pasted here.
// Bytes rather than a file in the repository, for the reason the demo seeder
// carries its own: a fixture on disk is one more thing that can go missing
// between the spec and the thing it tests.
const logoPNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAjklEQVR4nOzXsQ3CMBgGUYKYgoGg' +
    'hFFpGQjWMAv81JdI75WOm1MkS99lrXWaPO/f8fz1vu7q/nk8PRABNQE1AbXtcfuMH/b23v+7f/g/' +
    'IKAmoCagttkDMQE1ATUBNXugJqAmoCagZg/UBNQE1ATU7IGagJqAmoCaPVATUBNQE1CzB2oCagJq' +
    'Amq/AAAA///2pmZYbkF8vgAAAABJRU5ErkJggg==',
  'base64',
);

// Not an image at all, under a name that says it is: the decoder is chosen by
// the file's own bytes, so this is refused as a format rather than as a
// filename, and the sentence it is refused with says so.
const notAnImage = Buffer.from('this is not a PNG, whatever it is called\n');

test('choosing a file uploads it, and a refusal comes back through the swap', async ({ page }) => {
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

  // Any link's QR tab does; the test instance carries `lctl demo` data, so the
  // links table has rows. The tab is reached by its URL rather than by the
  // strip, which link-tabs.spec.mjs already drives.
  await page.goto('/links');
  const row = page.locator('main a[href^="/links/0"]').first();
  await expect(row, 'the links table lists no link to open').toHaveCount(1);
  const href = await row.getAttribute('href');
  const linkPage = href.split('?')[0];
  await page.goto(`${linkPage}?tab=qr`);

  const input = page.locator('#qr_logo');
  await expect(input, 'the QR tab draws no logo file input').toHaveCount(1);

  // There is no submit button in that form any more: choosing the file is the
  // whole interaction. Asserted here as well as in the template suite because
  // the rest of this test is meaningless if a button is doing the work.
  const uploadForm = page.locator('form[hx-post$="/qr/logo"]');
  await expect(uploadForm, 'the logo form is not the htmx one').toHaveCount(1);
  expect(
    await uploadForm.locator('button, input[type="submit"]').count(),
    'the logo form grew a submit control back',
  ).toBe(0);

  // --- accepted, and the pressed state's pointer path -----------------------
  //
  // Both in one gesture, because they are one gesture: clicking the input is
  // what opens the OS dialog, and it is that click the control has to look
  // pressed for while the dialog is up.
  //
  // **Nothing has been focused on this page yet, and that is load-bearing.**
  // Chromium arms its `:focus-visible` heuristic when focus *arrives*, so a
  // click on an element that already holds keyboard focus leaves the flag as
  // the keyboard set it — measured here, not assumed. The keyboard half below
  // therefore runs second, after a navigation has cleared the page.
  const [chooser] = await Promise.all([
    page.waitForEvent('filechooser'),
    input.click(),
  ]);
  expect(
    await input.evaluate((el) => el.matches(':focus') && !el.matches(':focus-visible')),
    'a pointer click leaves the file input outside `:focus:not(:focus-visible)`, so the ' +
      'busy state does not survive the second the dialog takes to open — which is what it is for',
  ).toBe(true);

  await chooser.setFiles({ name: 'logo.png', mimeType: 'image/png', buffer: logoPNG });

  // No submit was pressed and no navigation was asked for: htmx's `change`
  // binding is the only thing that can post this, and `HX-Redirect` is the
  // only thing that can move the page.
  await page.waitForURL('**qr=logo**', { timeout: 15000 });
  await expect(
    page.locator('main', { hasText: 'Logo stored' }),
    'the upload applied but the page does not say the logo was stored',
  ).toHaveCount(1);
  await expect(
    page.locator('#qr_logo'),
    'the QR panel did not come back with its logo control',
  ).toHaveCount(1);
  await expect(
    page.getByRole('button', { name: 'Remove the logo' }),
    'the panel does not offer removal, so it does not believe a logo was stored',
  ).toHaveCount(1);

  // --- the pressed state's keyboard path ------------------------------------
  //
  // On the page the redirect just delivered, so nothing has been focused here
  // either. Focus is moved *with the keyboard* — a programmatic focus() would
  // settle nothing, since the question is how focus arrived — and Shift+Tab
  // then Tab returns to the same element by keystroke whatever precedes it.
  const reloaded = page.locator('#qr_logo');
  await reloaded.focus();
  await page.keyboard.press('Shift+Tab');
  await page.keyboard.press('Tab');
  expect(
    await reloaded.evaluate((el) => el === document.activeElement),
    'Shift+Tab / Tab did not land back on the file input, so the check below proves nothing',
  ).toBe(true);
  expect(
    await reloaded.evaluate((el) => el.matches(':focus-visible')),
    'keyboard focus does not match :focus-visible here, so `:focus:not(:focus-visible)` ' +
      'would paint a pressed state that never ends for anybody tabbing through',
  ).toBe(true);

  // --- refused --------------------------------------------------------------
  //
  // Same gesture, a file that is not an image. This one must NOT navigate: the
  // handler answers 200 and htmx swaps `#qr` out of the response, and the
  // refusal has to arrive inside the panel the reader is looking at.
  const urlBefore = page.url();
  const [chooser2] = await Promise.all([
    page.waitForEvent('filechooser'),
    page.locator('#qr_logo').click(),
  ]);
  await chooser2.setFiles({ name: 'logo.png', mimeType: 'image/png', buffer: notAnImage });

  const panel = page.locator('#qr');
  await expect(
    panel.getByText('a logo is a PNG or a JPEG, decided by what is in the file'),
    'the refusal never reached the page — which is what a 422 would do here, because ' +
      'htmx does not swap a 4xx at all',
  ).toBeVisible({ timeout: 15000 });
  expect(page.url(), 'a refusal navigated the page instead of swapping the panel').toBe(urlBefore);

  // The logo it already had is still there. A refused replacement must not cost
  // the reader the picture they had stored.
  await expect(
    panel.getByRole('button', { name: 'Remove the logo' }),
    'the refused upload took the previously stored logo with it',
  ).toHaveCount(1);

  // --- leave it as it was found --------------------------------------------
  await panel.getByRole('button', { name: 'Remove the logo' }).click();
  await page.waitForURL('**qr=logo_removed**', { timeout: 15000 });
  await expect(
    page.getByRole('button', { name: 'Remove the logo' }),
    'the logo this spec uploaded is still on the code it borrowed',
  ).toHaveCount(0);
});
