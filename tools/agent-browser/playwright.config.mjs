import { defineConfig } from '@playwright/test';

// The kept spec drives a running instance — a real page, a real CSP, real
// htmx — never a committed copy of the markup. tools/render-verify/README.md
// refuses the committed-fixture route in as many words, and the refusal holds
// here for the same reason: a harness that verifies its own copy of the markup
// verifies nothing.
//
// The default target is the test instance (`make up`); LINKCTRL_BASE_URL
// overrides it. Chromium only: the spec's claims are about what the product
// serves, not about engine differences — cross-engine geometry is
// tools/render-verify's job, on its own pin.
export default defineConfig({
  testDir: './specs',
  retries: 0,
  use: {
    baseURL: process.env.LINKCTRL_BASE_URL ?? 'http://127.0.0.1:8081',
    browserName: 'chromium',
  },
});
