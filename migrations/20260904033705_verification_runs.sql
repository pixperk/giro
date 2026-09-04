-- verification_runs
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- the checks were all library calls and nothing ran them, which is the failure
-- mode that matters more than any of them individually:
--
--   a detector that stopped running looks exactly like a book with nothing
--   wrong.
--
-- findings above zero is the alert everyone writes. the absence of a run is
-- the one they forget, and it is the one that hides a real problem for as long
-- as nobody notices the cron died. that needs somewhere to record that a check
-- ran, which is this.
create table verification_runs (
    ledger     varchar     not null,
    check_name varchar     not null,
    ran_at     timestamptz not null default now(),

    -- what the check examined, not what it found. this is the column that
    -- distinguishes "looked and found nothing" from "did not look", and
    -- without it a run against an empty ledger is indistinguishable from a run
    -- that never happened.
    checked    bigint      not null,

    ok         boolean     not null,
    -- the findings, or the reason the check could not run. truncated, because
    -- a ledger with ten thousand problems does not need ten thousand lines
    -- stored to tell you something is wrong.
    detail     text,
    took_ms    bigint      not null,

    primary key (ledger, check_name, ran_at)
);

-- the alerting query is "when did each check last run on each ledger", so the
-- index is on the way it is read: most recent first, per check.
create index verification_runs_latest
    on verification_runs (ledger, check_name, ran_at desc);

-- unlike every other table here, this one is not the book. it is an
-- operational record about the book, so it carries no append only guard and no
-- conservation. it does carry a truncate guard, because losing the history of
-- what ran is exactly the state an attacker would want and exactly the state
-- that looks like "nothing has run yet".
create trigger verification_runs_no_truncate before truncate on verification_runs
    for each statement execute function giro_append_only();

-- the application writes its own run records and reads them back. it cannot
-- remove one: a check that found something must not be able to un-find it.
grant select, insert on verification_runs to giro_app;
