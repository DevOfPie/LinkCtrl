-- The mail outbox. Queued on the request path, drained by the scheduler.

-- name: EnqueueMail :exec
--
-- The message is stored rendered. Nothing here re-renders from a template, so a
-- later template change cannot rewrite a mail somebody is already waiting for.
INSERT INTO mail_outbox (id, recipient, subject, body, kind)
VALUES (@id, @recipient, @subject, @body, @kind);

-- name: ClaimDueMail :many
--
-- One batch of due mail, claimed rather than merely selected.
--
-- The UPDATE is what makes the claim: it spends the attempt and leases the row
-- forward, in one statement, before anything is sent. Two consequences, and
-- both are the point:
--
--   * A process killed between claiming and sending leaves a row that comes
--     back on its own when the lease expires, instead of one stuck pending.
--   * A crash loop is bounded. Counting the attempt at send time would let a
--     process that dies mid-send retry the same message forever.
--
-- FOR UPDATE SKIP LOCKED inside the subquery keeps two drainers from claiming
-- the same row. Leadership already keeps a second replica out of this job, but
-- leadership is an advisory lock released when its holder dies, so a moment of
-- overlap is possible; skip-locked makes that moment cost nothing rather than
-- send a message twice.
--
-- Ordered oldest first, so a backlog drains in the order it was queued.
UPDATE mail_outbox
   SET attempts = attempts + 1,
       next_attempt_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int)
 WHERE id IN (
       SELECT id FROM mail_outbox
        WHERE status = 'pending'
          AND next_attempt_at <= now()
        ORDER BY next_attempt_at
        LIMIT sqlc.arg(batch_size)
          FOR UPDATE SKIP LOCKED
 )
RETURNING id, recipient, subject, body, kind, attempts;

-- name: MarkMailSent :exec
--
-- attempts is not touched: ClaimDueMail already spent it.
--
-- The body is blanked here, in the same statement that marks the row sent
-- (finding F32). Two of the templates this phase ships carry a single-use token
-- in their body, and a delivered message is one whose token has reached the only
-- person entitled to it — keeping a copy afterwards is keeping a redeemable
-- credential in clear for the retention window. Folded into this UPDATE rather
-- than done after it, so there is no crash window in which a row is sent and
-- still carries the token, and so `mail_outbox_finished_body_scrubbed` cannot be
-- passed by a caller that forgets. What an operator reads afterwards —
-- recipient, subject, kind, attempts, last_error — is untouched.
UPDATE mail_outbox
   SET status = 'sent', sent_at = now(), body = ''
 WHERE id = @id;

-- name: MarkMailRetry :exec
--
-- A failure that will be tried again. The error is kept verbatim: it is what an
-- operator reads when somebody reports that mail never arrived.
--
-- Replaces the lease ClaimDueMail set with the real backoff for this attempt,
-- and does not touch attempts — the claim already spent it.
--
-- Seconds rather than an interval parameter, matching the lockout query in
-- auth.sql: an interval maps to pgtype.Interval, which would put a driver type
-- in the service layer's signature for no benefit.
UPDATE mail_outbox
   SET next_attempt_at = now() + make_interval(secs => sqlc.arg(backoff_seconds)::int),
       last_error = sqlc.arg(last_error)
 WHERE id = sqlc.arg(id);

-- name: MarkMailFailed :exec
--
-- Retry exhausted. Terminal, and deliberately not deleted — a row that says
-- what was attempted and why it never arrived is the whole point of an outbox
-- over an in-memory retry loop.
--
-- Blanked for F32's reason, and the failed case is the one that needs it most: a
-- message that never arrived is one whose token is still unspent, and this row
-- would otherwise hold it for thirty days against an invitation that lives seven.
-- The retry path deliberately does not blank — a row still being retried is
-- pending, and it has to keep the message it is going to send.
UPDATE mail_outbox
   SET status = 'failed', last_error = @last_error, body = ''
 WHERE id = @id;

-- name: CountPendingMail :one
SELECT count(*) FROM mail_outbox WHERE status = 'pending';

-- name: PurgeFinishedMail :execrows
--
-- Sent and failed rows past the retention window. The outbox is a record of
-- what was attempted, not an archive: without this the table is the one thing
-- in the schema that grows forever with no window and no metric, which is the
-- shape D5 and M21 exist to avoid repeating.
--
-- It is a retention window and not a secrecy control. The rows it deletes lost
-- their bodies when they finished; this is what stops the record of *that* piling
-- up. Lowering it would not shorten any credential's exposure.
DELETE FROM mail_outbox
 WHERE status <> 'pending'
   AND created_at < now() - make_interval(days => sqlc.arg(max_age_days)::int);

-- name: AbandonUnsendableMail :execrows
--
-- Fails pending rows on an instance that has no relay to send them.
--
-- The outbox has one guard against unbounded growth — PurgeFinishedMail — and it
-- takes only `status <> 'pending'`, which is correct while a mailer exists:
-- Drain claims every pending row and moves it to sent or, after five attempts,
-- failed, and the lease recovers rows a crash interrupted. So nothing stays
-- pending, and nothing needs to.
--
-- Clearing SMTP_HOST on an instance that had one breaks that. The mailer becomes
-- nil, so the drain does not run; the rows enqueued before the change stay
-- pending; and the purge skips them by design. They are then invisible — nothing
-- but CountPendingMail reads them — and permanent (F52).
--
-- Failing rather than deleting, so the record of what was attempted survives its
-- retention window like every other finished row, and reaches PurgeFinishedMail
-- by the normal path rather than by a second delete. The body goes for the same
-- reason MarkMailFailed drops it: a message that will never be sent should not
-- keep holding what it was going to say.
--
-- Bounded by the same window rather than run eagerly, because an operator who
-- clears SMTP_HOST by mistake and puts it back the same afternoon should still
-- get their queue delivered. Only the caller decides when this applies: it runs
-- on the no-mailer path and nowhere else.
UPDATE mail_outbox
   SET status = 'failed',
       last_error = 'no SMTP relay configured; abandoned past the retention window',
       body = ''
 WHERE status = 'pending'
   AND created_at < now() - make_interval(days => sqlc.arg(max_age_days)::int);
