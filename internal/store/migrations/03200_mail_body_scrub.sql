-- +goose Up
--
-- The outbox stops holding a credential once it has delivered one (finding F32).
--
-- `01100` said a rendered body is what survives a restart, and that is still
-- true. What it did not say is that two of the four templates this phase shipped
-- carry a single-use token in that body — `invitation.txt` (M27) and
-- `verification.txt` (M29) — so between delivery and the 30-day purge the table
-- held a redeemable credential in clear. A token read out of `body` by SQL alone
-- hashes to `invitations.token_hash` and redeems, up to owner. That falsified
-- `01200`'s *"a database leak hands over no redeemable invites"* and `01400`'s
-- counterpart, both of which are corrected in place by this milestone.
--
-- The fix is that a row stops carrying the message the moment it stops needing
-- it: `MarkMailSent` and `MarkMailFailed` blank `body` in the same statement
-- that sets the terminal status, so the window is the delivery rather than the
-- retention window. Why that option and not the other two — a send-time
-- reference, or shorter retention for credential-bearing kinds — is in
-- decisions.md under M45.
--
-- **Not additive, and that is named rather than hidden.** The inherited rule is
-- that DDL is additive within a minor version. Both statements below break it:
-- one erases data, the other narrows what the column may hold. It is allowed
-- here for the reason `02700`'s `DROP COLUMN` was — it lands before 0.2.0 — and
-- for one `02700` did not have: `mail_outbox` was created by `01100` in this
-- same unreleased phase, so no released instance has a row for this to reach.
--
-- The UPDATE is the point rather than a tidy-up. Fixing the writers protects
-- mail sent after the upgrade; every already-delivered invitation still sitting
-- in the table would keep its token for up to thirty more days, which is longer
-- than the token itself lives.
UPDATE mail_outbox SET body = '' WHERE status <> 'pending';

-- And the guarantee is the database's, not a convention two queries happen to
-- keep. F32 exists because a claim held by convention — *"the first mail this
-- ships contains no secret"* — stayed in the docs while two templates that
-- carry one landed beside it. A constraint cannot rot that way: a future edit
-- that drops the scrub from either query fails on the first send instead of
-- quietly re-opening this.
--
-- Pending rows are excluded because a pending row is a message that has not been
-- delivered. It has to hold its body; that is what the outbox is for. The
-- exposure that leaves is bounded by the credential's own TTL rather than by
-- this table, and is documented in docs/SECURITY.md.
ALTER TABLE mail_outbox
    ADD CONSTRAINT mail_outbox_finished_body_scrubbed
    CHECK (status = 'pending' OR body = '');

-- +goose Down
ALTER TABLE mail_outbox DROP CONSTRAINT IF EXISTS mail_outbox_finished_body_scrubbed;
