import { chromium } from '@playwright/test';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';

import { credentials } from './credentials.mjs';

// One sign-in for the whole suite, saved as a storage state every spec starts
// from (F333, D435).
//
// **Why this exists.** Every spec signed in as the same address, the suite is
// twenty tests, and `LINKCTRL_LOGIN_RATE_PER_MIN` is ten per address per minute —
// so the tail of a full run was refused at `/login` and reported *sign-in did not
// reach /dashboard*, which reads as a broken instance rather than as the limiter
// doing exactly what M35 asks of it. Measured at M68: `--workers=1` gave 19
// passed and 1 failed, and the same spec alone passed 75 seconds later; the
// default worker count failed eight, on different specs each time. The gate the
// phase-loop's review obligation names could not be run green in one pass.
//
// **The two declined shapes, on record.** Raising the limit on the test instance
// masks the limiter on the one instance that exercises it — so a defect in the
// rate limiter would first be noticed in production. A per-spec address needs a
// seeder that makes twenty accounts and grows with every spec added.
//
// **What it costs**, and it is real: no spec asserts its own sign-in any more, so
// a regression in the login flow itself shows up here — as twenty failures with
// one cause — rather than being caught by the first spec that tries. That is why
// this file fails loudly and says which of the two likely causes it is.
export default async function globalSetup(config) {
  const { baseURL, storageState } = config.projects[0].use;
  // The real resolution, from the one copy of it: the environment first, then
  // the Address/Password table in docs/dev-notes/instances.md. Not a default
  // written here — a guessed password would charge the lockout counter and
  // report it as a broken login flow.
  const { email, password } = credentials();

  const browser = await chromium.launch();
  const page = await browser.newPage({ baseURL });
  try {
    await page.goto('/login');
    await page.fill('#email', email);
    await page.fill('#password', password);
    await page.click('button[type="submit"]');
    await page.waitForURL('**/dashboard', { timeout: 15000 });
  } catch (err) {
    throw new Error(
      'the suite could not sign in once, so every spec would fail at its first ' +
        'navigation. Either the test instance was rebuilt and the account table ' +
        'in docs/dev-notes/instances.md is stale — export LINKCTRL_UI_EMAIL and ' +
        'LINKCTRL_UI_PASSWORD — or the login flow itself is broken, which this ' +
        'setup is the only thing left that exercises: ' +
        err.message,
    );
  }
  const state = await page.context().storageState();
  // **A state with no cookies is written by nothing and read by everything.**
  // The first run of this setup wrote a 216-byte state holding none, every spec
  // then started signed out, and the failures were about missing seeded data and
  // a save that never navigated — nothing that named a session. That is the
  // silence-looks-like-success shape, so it is refused here rather than
  // discovered three specs later.
  if (!state.cookies || state.cookies.length === 0) {
    await browser.close();
    throw new Error(
      'signed in and captured a storage state holding no cookies, so every spec ' +
        'would run signed out and fail about whatever it happened to touch first. ' +
        'The sign-in reached /dashboard, so this is the context losing the cookie ' +
        'rather than the credentials being wrong',
    );
  }
  await mkdir(dirname(storageState), { recursive: true });
  await writeFile(storageState, JSON.stringify(state));
  await browser.close();
}
