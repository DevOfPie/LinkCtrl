import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// The account the suite signs in as, from the environment or from the instance
// table this repository keeps.
//
// **One copy, since M70.** Six specs each carried an identical private copy of
// this, because each of them signed in for itself — which is the defect F333 is
// about, seen from the other side. The suite signs in once now
// (`global-setup.mjs`), so there is one caller and there is no reason for the
// function to exist seven times.
//
// LINKCTRL_UI_EMAIL / LINKCTRL_UI_PASSWORD win; otherwise the Address and
// Password rows of docs/dev-notes/instances.md, which is where this repository
// records what the test instance was seeded with. One attempt (retries are 0),
// so a stale table costs one charge against the lockout counter and a red run
// pointing at the file to fix.
const instancesDoc = fileURLToPath(
  new URL('../../docs/dev-notes/instances.md', import.meta.url),
);

export function credentials() {
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
