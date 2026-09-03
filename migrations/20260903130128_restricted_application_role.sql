-- restricted_application_role
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- the previous migration put seven guards in the database. this is what makes
-- them worth anything.
--
-- a table's owner outranks its own triggers. "alter table logs disable trigger
-- user" is one statement and needs no privilege beyond owning the table, so an
-- application connecting as the role that ran the migrations can switch off
-- every guard protecting it. the guards are advisory until that stops being
-- true.
--
-- so: two roles. the migration tool owns everything and is used only during a
-- rollout. the application gets select, insert, and update on the specific
-- columns it has to change, and nothing else. no delete, no truncate, no
-- alter, no ownership.
--
-- the grants below are the same allow list as the triggers, written a second
-- time in a different mechanism. that repetition is deliberate: the trigger
-- says what a row may become, the grant says what the application may reach
-- for, and an attacker has to get past both.

-- the role is cluster wide rather than per database, so it may already exist
-- from another database or an earlier run. creating it is not idempotent on
-- its own, hence the guard.
--
-- nologin: nothing authenticates as it directly. the application connects as
-- itself and takes this role on, or is granted membership. that keeps the
-- credential story out of the schema, where it does not belong.
do $$
begin
    if not exists (select 1 from pg_roles where rolname = 'giro_app') then
        create role giro_app nologin;
    end if;
end
$$;

-- the schema this is being installed into, not necessarily public. giro is
-- migrated into whatever schema the connection points at, which is how the
-- tests isolate and how a shared database would separate deployments. without
-- usage the role cannot see the tables at all and every statement fails with
-- "relation does not exist", which reads like a missing table rather than a
-- missing grant.
do $$
begin
    execute format('grant usage on schema %I to giro_app', current_schema());
end
$$;

-- reads are unrestricted. every table is readable, because the tenant boundary
-- is the ledger column in every query and not a privilege, and because a
-- reporting query that cannot see the book is useless.
grant select on ledgers, accounts, accounts_volumes, transactions, moves, logs to giro_app;

-- appending is the whole write path. every table takes new rows.
grant insert on ledgers, accounts, accounts_volumes, transactions, moves, logs to giro_app;

-- moves.seq is a bigserial, so inserting one advances a sequence the role has
-- to be allowed to touch. easy to miss: the insert grant alone produces
-- "permission denied for sequence moves_seq_seq" on the first commit.
grant usage on sequence moves_seq_seq to giro_app;

-- and now the narrow part. every update below names its columns.
--
-- granting update on a whole table instead would re-open the money columns and
-- leave the trigger as the only thing standing between a compromised
-- application and minted balances. that is precisely the hole an external
-- audit found in the system this borrows from: the guard covered insert and
-- delete, left update open, and ordinary table privileges were enough to write
-- a spendable balance.

-- id allocation and the hash chain tip.
grant update (last_tx_id, last_log_id, last_log_hash) on ledgers to giro_app;

-- metadata, and first usage moving earlier when a backdated transaction turns
-- out to predate what we thought was the first.
grant update (metadata, first_usage, updated_at) on accounts to giro_app;

-- the counters, and the permission to end below zero. the counters are guarded
-- by the monotonic trigger and by conservation; the flag is policy.
grant update (input, output, allow_negative) on accounts_volumes to giro_app;

-- the revert stamp and metadata. postings, timestamp, reference and the frozen
-- volumes are unreachable.
grant update (reverted_at, metadata) on transactions to giro_app;

-- effective volumes are rewritten when a backdated transaction lands behind a
-- move. the frozen snapshot and the movement itself are unreachable.
grant update (pcev_input, pcev_output) on moves to giro_app;

-- logs takes no update grant at all. it is the source of truth and nothing
-- writes to it after the insert.

-- the schema version is readable so a booting process can refuse to serve
-- against a schema it does not match. it is not writable: applying migrations
-- is the owner's job, and a process that serves traffic should not be able to
-- change the schema or to claim it changed one.
grant select on schema_migrations to giro_app;

-- deliberately absent, and each for a reason:
--
--   delete      corrections are new rows. nothing is ever removed.
--   truncate    the guard trigger refuses it; this means the role cannot ask.
--   references  a foreign key to a ledger table from elsewhere would take
--               locks on the money path.
--   trigger     creating or dropping a trigger on these tables.
--   create      the role can use the schema, not add to it.
