# Phase 2 — the milestones

Released as **0.2.0**, 2026-08-06. History, kept because these are the
definitions of done the implementations are still held to, and because a
`done` row is the only durable record that a milestone's claim was ever
accepted.

Thirty-three milestones: twenty-five integers, M21 through M45, and eight
fractional insertions. That count is the baseline the size target in
[planning.md](../planning.md) was set against — a phase stays under sixteen —
and it is counted from the table below rather than recalled.

Every milestone file named here is still in this directory. Only the status
table moved: [README.md](README.md) carries the **live** phase. These
thirty-three rows sat on the resume path, read on every `/work phase` for a
phase that had ended — and appending Phase 3 beneath them would have made
forty-eight and growing, which is the shape the doc-cost audit exists to catch.

| # | Milestone | Depends on | Status |
| --- | --- | --- | --- |
| [M21](m21.md) | Audit log: behavior, retention, growth alerting | — | done |
| [M22](m22.md) | Notifications: in-app behavior | — | done |
| [M23](m23.md) | Cross-replica cache invalidation (pub/sub) | — | done |
| [M24](m24.md) | Shared rate limits (credentials and API) | — | done |
| [M24.5](m24.5.md) | Dark mode: theme tokens, system default, override | — (before M25) | done |
| [M25](m25.md) | Workspace and organization switcher | — | done |
| [M26](m26.md) | Mailer: optional SMTP delivery | — | done |
| [M26.5](m26.5.md) | Dashboard header: identity menu and notification bell | — (before M27) | done |
| [M26.6](m26.6.md) | Bounded Redis failure, when the server never answers | — (before M32.5, M34, M40) | done |
| [M27](m27.md) | Organizations: invitations and joining | M21 M22 M25 M26 | done |
| [M28](m28.md) | Team management, workspaces, org creation | M27 | done |
| [M28.5](m28.5.md) | Organization deletion and tenancy teardown | M28 | done |
| [M29](m29.md) | Self-serve signup, configured by the operator | M26 M27 | done |
| [M30](m30.md) | Destination blocking: tiers and logging | M21 | done |
| [M31](m31.md) | Blocked-attempt disputes and owner review | M30 M22 | done |
| [M32](m32.md) | Opt-in reputation and malware feeds | M30 M31 | done |
| [M32.5](m32.5.md) | Bot blocking, per domain and per link | — (before M33, M34) | done |
| [M32.9](m32.9.md) | **Mid-phase adversarial review** | M21–M32.5 | done |
| [M33](m33.md) | Deep-link path forwarding | — (before M34) | done |
| [M33.5](m33.5.md) | A demo that shows the phase, not just its links | M32.9 | done |
| [M34](m34.md) | Routing rules: conditions, first-match evaluation | M23 M30 M33 | done |
| [M35](m35.md) | Gated links: password, signed, one-time, max-click | M34 (ordering) | done |
| [M36](m36.md) | Split testing: weighted, sequential, fallback, flags | M34 M35 M30 | done |
| [M37](m37.md) | Dimension visualizations, rollup cadence first | — | done |
| [M38](m38.md) | Folders: API and tree UI | — | done |
| [M39](m39.md) | Per-domain ownership | M21 | done |
| [M40](m40.md) | Custom domains: verification and serving | M39 M23 | done |
| [M41](m41.md) | QR codes and campaigns | — | done |
| [M42](m42.md) | Webhooks | M30 | done |
| [M43](m43.md) | Automation rules | M22 M35 M42 | done |
| [M44](m44.md) | API keys: rotation and scope choice | M21 | done |
| [M44.9](m44.9.md) | **Pre-release adversarial review** | M21–M44 | done |
| [M45](m45.md) | Deferred findings, documentation pass, 0.2.0 | all | done |

The inherited rules these milestones were built under are in
[README.md](README.md#what-every-milestone-inherits), which is where they still
live: most are product invariants that outlast the phase, and which of them
Phase 3 inherits is confirmed when Phase 3 is planned.
