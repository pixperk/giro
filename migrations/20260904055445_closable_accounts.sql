-- closable_accounts
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- a client offboards and their account carries on working. nothing records
-- that the relationship ended, and a posting made by mistake a year later is
-- accepted as readily as one made on the first day.
--
-- closed is on accounts rather than accounts_volumes, unlike allow_negative,
-- and the difference is not arbitrary. permission to go below zero is a fact
-- about an account in one asset: a cost line in USD is an ordinary balance in
-- USDT. closure is a fact about the relationship, and per asset it would have
-- a hole in it -- an account closed in USD/2 would still accept USDT/6, which
-- is not what anybody closing an account means.
alter table accounts
    add column closed boolean not null default false;

-- the guard on this table lists the columns that may change, so a new column
-- is refused until it is named. that is the allow list working as intended:
-- closure had to be declared here rather than arriving unprotected.
drop trigger accounts_limited_change on accounts;
create trigger accounts_limited_change before update or delete on accounts
    for each row execute function giro_only_change('{metadata,first_usage,updated_at,closed}');

grant update (closed) on accounts to giro_app;

-- finding a closed account that holds something.
--
-- closing requires a zero balance, checked when it happens. a commit that was
-- already in flight can still land afterwards, because refusing that would
-- mean taking a lock on this row in every commit -- and a foreign key style
-- lock on a row the write path also updates is the shape that deadlocks.
--
-- so the check is where the other invariant checks are, and the index is what
-- makes it a seek rather than a scan of every account ever opened.
create index accounts_closed on accounts (ledger, address) where closed;
