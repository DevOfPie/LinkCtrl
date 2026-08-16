import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// M50.5's reopening: the logo upload applies itself, and both of the things
// that makes true are invisible to a template test.
//
// What the Go suites already prove is the markup — `hx-trigger="change"`,
// `hx-post`, `hx-encoding`, `hx-target`, `hx-select`, and since M50.8's second
// reopening `form=` as well, all of them on the input — and the
// handler's answer to a request that carried `HX-Request: true` because a test
// set the header by hand. Neither of those is htmx *binding* a `change` to a
// multipart post, which is the whole mechanism the two-step button was traded
// for, and neither is htmx serializing a control that submits to a form it does
// not sit inside, which is what F246(b) moved the control into the style form's
// grid to do. Both are driven here, and the second is read off the request body
// rather than inferred from the answer.
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
  // **What htmx serializes for the upload, taken from htmx rather than from the
  // wire.** M50.8's second reopening moved the file input into the style form's
  // grid on a `form="…"` attribute, and *that htmx serializes a control which
  // submits to a form it is not inside* is the claim the milestone said would be
  // demonstrated on the instance instead of argued.
  //
  // It cannot be read off the request: Chromium hands a multipart upload to the
  // network stack as a stream, and Playwright's `postDataBuffer()` is null for
  // one — measured here, not assumed. `htmx:configRequest` carries the FormData
  // htmx is about to send, which is one step earlier and is the step the claim
  // is about. What the *server* did with it is the rest of this case: the
  // redirect it answers with is built from the `next` and `code` in that body.
  //
  // Installed before the first navigation, because an init script reaches the
  // documents created after it and the upload happens two pages later.
  let sent = null;
  await page.exposeFunction('__qrLogoRequest', (names) => {
    sent = names;
  });
  await page.addInitScript(() => {
    document.addEventListener('htmx:configRequest', (event) => {
      const detail = event.detail ?? {};
      if (!String(detail.path ?? '').includes('/qr/logo')) return;
      const names =
        detail.formData && typeof detail.formData.keys === 'function'
          ? [...detail.formData.keys()]
          : Object.keys(detail.parameters ?? {});
      window.__qrLogoRequest(names.sort());
    });
  });

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
  const uploadForm = page.locator('form#qr-logo-upload');
  await expect(uploadForm, 'the logo upload form is not on the page').toHaveCount(1);
  expect(
    await uploadForm.locator('button, input[type="submit"]').count(),
    'the logo form grew a submit control back',
  ).toBe(0);

  // **The control is form-associated since M50.8's second reopening** (F246b):
  // it renders inside the style form's grid, between the colours and the size,
  // and submits to a form it is not inside. Both halves are asserted, because
  // each alone is satisfied by the arrangement this milestone replaced — the
  // old one sat inside its own form, and a control that lost `form=` would sit
  // in the right place posting to the wrong route.
  const association = await input.evaluate((el) => ({
    owner: el.form && el.form.id,
    ancestor: el.closest('form') && el.closest('form').getAttribute('action'),
  }));
  expect(
    association.owner,
    'the file input does not belong to the upload form, so the file travels in ' +
      'whatever body the style form is posting',
  ).toBe('qr-logo-upload');
  expect(
    association.ancestor,
    'the file input is not inside the style form, so it has not moved between ' +
      'the colours and the size at all and this case is proving nothing about ' +
      'the arrangement F246(b) asked for',
  ).toMatch(/\/qr$/);

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

  // And what it sent to get here. Three names, each for a reason: `logo` is the
  // file, and it is in the body only because htmx serialized a control that
  // names its form rather than sits in it; `next` and `code` are what
  // LinkQRLogo reads to decide where the reader comes back to and which code is
  // written. **Anything more would be the style form**, which the input sits
  // inside — htmx takes `elt.form || closest(elt, 'form')`, so the association
  // wins and the ancestor is never serialized. That is a claim about htmx and
  // this assertion is where it is kept: the first reading of it was the other
  // way round, and an `hx-params` filter was written against a body that never
  // existed. Anything less is a body the handler answers by sending the reader
  // somewhere they did not ask to go.
  expect(
    sent,
    'no htmx request to the logo route was ever configured, so this case saw ' +
      'nothing of what was sent and the assertion below would pass on silence',
  ).not.toBeNull();
  expect(sent, 'the upload was serialized with these fields').toEqual([
    'code',
    'logo',
    'next',
  ]);

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
