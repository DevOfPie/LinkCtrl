import { test, expect } from '@playwright/test';

// M46.5's kept spec: the assertion no template scan can make. The scans read
// markup and the integration suite reads HTTP; what the browser *refused* —
// a CSP violation, a script error — only a console shows. F206 is the proof:
// htmx's injected indicator stylesheet was blocked on every page, in every
// browser, and no gate saw it.
//
// It runs against the sign-in page deliberately. The layout, the stylesheet,
// the CSP and htmx are all live there, and it needs no session — a committed
// spec must not carry credentials, and a failed sign-in charges a real
// lockout counter.
test('the sign-in page renders with a clean console', async ({ page }) => {
  const errors = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') errors.push(msg.text());
  });
  page.on('pageerror', (err) => errors.push(`pageerror: ${err.message}`));

  // networkidle so late arrivals are caught: htmx runs at DOMContentLoaded
  // (the script is deferred) and F206's error fired at 13ms.
  const response = await page.goto('/login', { waitUntil: 'networkidle' });
  expect(response.status(), 'is the test instance up? (make up)').toBe(200);

  // A page that failed to run htmx at all would also report a clean console.
  // Assert the library is live and read the config the layout's meta tag set,
  // so green means "htmx ran, told not to inject" and not "nothing happened".
  const htmx = await page.evaluate(() =>
    window.htmx
      ? { version: window.htmx.version, includeIndicatorStyles: window.htmx.config.includeIndicatorStyles }
      : null,
  );
  expect(htmx, 'window.htmx is missing — the layout no longer loads it').not.toBeNull();
  expect(
    htmx.includeIndicatorStyles,
    'the htmx-config meta tag must turn indicator-style injection off, or the CSP blocks it on every page (F206)',
  ).toBe(false);

  expect(errors, 'the console must be clean — every entry here is something the browser refused or a script threw').toEqual([]);
});
