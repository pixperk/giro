-- database_enforced_invariants
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- until now every rule lived in go, which holds exactly as long as the
-- application is the only thing that writes. it is not, and will not be: an
-- engineer in psql, a backfill script, a migration nobody thought through, or
-- credentials that got out. none of those run the go code, and all of them can
-- write whatever they like.
--
-- these move the rules into the database, where they apply to every writer
-- including the ones nobody anticipated.
--
-- what this does not do on its own is make the book unforgeable. the role that
-- owns these tables outranks their triggers and can switch them off, so the
-- guarantee is bounded by the role the application connects as. the next
-- migration is what closes that, by giving the application a role that cannot.

-- ---------------------------------------------------------------------------
-- append only
-- ---------------------------------------------------------------------------

-- a mistake is corrected by appending a compensating transaction, never by
-- editing a row.
--
-- "append only" turned out to be too strong for two of these tables, which is
-- worth recording because the design document said otherwise and this is what
-- proved it wrong. transactions has reverted_at stamped on it and metadata
-- merged into it; moves has its effective volumes rewritten when something
-- lands behind them in effective order. both are real, both are load bearing,
-- and neither is an edit of what was recorded: a reverted transaction still
-- says exactly what it said, and pcv, the frozen snapshot, is never touched.
--
-- so the guards below name the columns that may change and refuse every other
-- difference. that direction matters. a guard listing what is forbidden is
-- only as good as the imagination of whoever wrote the list, and a column
-- added by a future migration arrives unprotected. listing what is allowed
-- means a new column is guarded from the moment it exists, without anyone
-- remembering to guard it.
create function giro_append_only() returns trigger as $$
begin
    raise exception 'giro: % is append only, correct by appending', tg_table_name
        using errcode = 'restrict_violation';
end;
$$ language plpgsql;

-- refuses an update that changes any column outside the allowed list, which is
-- passed as the trigger argument. deletes are refused outright.
--
-- comparing whole rows as jsonb with the allowed keys removed catches a change
-- to any other column, including ones that do not exist yet.
create function giro_only_change() returns trigger as $$
declare
    allowed text[] := tg_argv[0]::text[];
begin
    if tg_op = 'DELETE' then
        raise exception 'giro: % rows are never deleted', tg_table_name
            using errcode = 'restrict_violation';
    end if;
    if (to_jsonb(old) - allowed) <> (to_jsonb(new) - allowed) then
        raise exception 'giro: % may only change %, and this changes more',
            tg_table_name, array_to_string(allowed, ', ')
            using errcode = 'restrict_violation';
    end if;
    return new;
end;
$$ language plpgsql;

-- two triggers per table, and the second is the one that is easy to miss.
--
-- truncate does not visit rows. it discards the table's files and gives it
-- empty ones, so there are no row events and a "for each row" trigger is not
-- bypassed so much as never called. update and delete guards look complete and
-- the whole table can still be erased by one statement that raises nothing.
--
-- the statement level form is the only shape "before truncate" accepts, for
-- the same reason: there are no rows to hand it.
--
-- every table carries its own, rather than relying on a cascade reaching it.
-- truncating a parent with cascade does fire the child's trigger, but guarding
-- only the parent leaves the child open to being truncated directly.

-- logs really is append only. nothing writes to it after the insert, and the
-- hash chain would not survive it if anything did.
create trigger logs_append_only before update or delete on logs
    for each row execute function giro_append_only();
create trigger logs_no_truncate before truncate on logs
    for each statement execute function giro_append_only();

-- a transaction is stamped when reverted and carries mutable metadata. what it
-- recorded -- its postings, timestamp, reference and post commit volumes --
-- cannot move.
create trigger transactions_limited_change before update or delete on transactions
    for each row execute function giro_only_change('{reverted_at,metadata}');
create trigger transactions_no_truncate before truncate on transactions
    for each statement execute function giro_append_only();

-- a move's effective volumes are rewritten when a backdated transaction lands
-- behind it, which is the whole reason there are two snapshots. the frozen one
-- and the movement itself do not move.
create trigger moves_limited_change before update or delete on moves
    for each row execute function giro_only_change('{pcev_input,pcev_output}');
create trigger moves_no_truncate before truncate on moves
    for each statement execute function giro_append_only();

-- an account's metadata is mutable, and first_usage moves earlier when a
-- backdated transaction turns out to predate what we thought was the first.
-- its address and insertion date do not change.
create trigger accounts_limited_change before update or delete on accounts
    for each row execute function giro_only_change('{metadata,first_usage,updated_at}');
create trigger accounts_no_truncate before truncate on accounts
    for each statement execute function giro_append_only();
create trigger accounts_volumes_no_truncate before truncate on accounts_volumes
    for each statement execute function giro_append_only();
create trigger ledgers_no_truncate before truncate on ledgers
    for each statement execute function giro_append_only();

-- ---------------------------------------------------------------------------
-- volumes only ever increase
-- ---------------------------------------------------------------------------

-- input is everything that ever arrived and output is everything that ever
-- left, so neither can fall. the commit path only ever writes
-- "input = input + n" with a validated positive amount, so this refuses
-- nothing it does.
--
-- what it catches is the cheapest way to fake a balance: quietly lowering an
-- account's output. that leaves gross flow looking plausible while the balance
-- rises out of nowhere.
create function giro_volumes_monotonic() returns trigger as $$
begin
    if new.input < old.input then
        raise exception 'giro: % % input fell from % to %, volumes only increase',
            new.address, new.asset, old.input, new.input
            using errcode = 'restrict_violation';
    end if;
    if new.output < old.output then
        raise exception 'giro: % % output fell from % to %, volumes only increase',
            new.address, new.asset, old.output, new.output
            using errcode = 'restrict_violation';
    end if;
    return new;
end;
$$ language plpgsql;

create trigger accounts_volumes_monotonic before update on accounts_volumes
    for each row execute function giro_volumes_monotonic();

-- ---------------------------------------------------------------------------
-- no unpermitted overdraw
-- ---------------------------------------------------------------------------

-- the same rule checkBalances applies in go, restated where raw sql also has
-- to obey it. the go version stays: it produces a typed error naming the
-- account and the shortfall, which is what an api caller needs, and it runs
-- before any write rather than after.
--
-- one carve out, found by the test suite rather than by reasoning about it.
--
-- it fires when the balance moves, not when the permission changes. an
-- operator revoking the permission on an account that is already negative is
-- doing something reasonable, and often urgent: stopping further drawdown now
-- rather than after the balance is back. refusing that would leave the only
-- way to stop the bleeding blocked by the bleeding. the account stays negative
-- and unpermitted afterwards, which is a state VerifyBalancePermissions
-- exists to surface.
--
-- there is deliberately no escape hatch. an earlier version honoured a
-- transaction local flag so that a forced revert could overdraw, and any role
-- able to set a custom setting could then overdraw anything -- including the
-- application role, which made this the one guard the application could walk
-- past. an operator who needs a reversal that drives an account negative
-- grants the account the permission, reverts, and revokes: three steps that
-- leave a trail, instead of one flag that leaves none.
create function giro_no_unpermitted_overdraw() returns trigger as $$
begin
    if tg_op = 'UPDATE'
       and new.input = old.input and new.output = old.output then
        return new;
    end if;

    if new.input < new.output and not new.allow_negative then
        raise exception 'giro: % would hold % in %, and is not permitted a negative balance',
            new.address, new.input - new.output, new.asset
            using errcode = 'restrict_violation';
    end if;
    return new;
end;
$$ language plpgsql;

create trigger accounts_volumes_no_overdraw before insert or update on accounts_volumes
    for each row execute function giro_no_unpermitted_overdraw();

-- ---------------------------------------------------------------------------
-- conservation
-- ---------------------------------------------------------------------------

-- the master invariant: for any asset, every balance summed together is
-- exactly zero. it is the one that catches value being created, and it is the
-- one a row level guard cannot see.
--
-- "input = input + 500000" on one row is an increase, on a single row, and
-- nothing about that row is invalid. conservation is a property of the whole
-- table, so a trigger that is handed one row has nothing to compare it
-- against.
--
-- hence a constraint trigger, deferred to commit. a legitimate transaction
-- passes through unbalanced intermediate states, because the commit path
-- writes one volume row per statement, so checking after each statement would
-- reject honest work. deferring asks the only question that matters: is the
-- book balanced by the time this is over.
--
-- scoped to the asset of the row that changed rather than the whole ledger.
-- assets never mix, so no other asset can have been disturbed by this row, and
-- the narrower check is what keeps the cost proportional to one asset's
-- accounts rather than all of them.
create function giro_conservation() returns trigger as $$
declare
    subject   record;
    drift     numeric;
begin
    subject := coalesce(new, old);

    select sum(input) - sum(output) into drift
      from accounts_volumes
     where ledger = subject.ledger and asset = subject.asset;

    if drift <> 0 then
        raise exception 'giro: ledger % asset % drifted by %, value was created or destroyed',
            subject.ledger, subject.asset, drift
            using errcode = 'restrict_violation';
    end if;
    return null;
end;
$$ language plpgsql;

create constraint trigger accounts_volumes_conservation
    after insert or update or delete on accounts_volumes
    deferrable initially deferred
    for each row execute function giro_conservation();

-- the conservation check groups by (ledger, asset), which the primary key
-- (ledger, address, asset) cannot serve: address sits between the two columns
-- being filtered on. include the counters so the check is an index only scan
-- rather than a heap visit per account.
create index accounts_volumes_by_asset
    on accounts_volumes (ledger, asset) include (input, output);

-- ---------------------------------------------------------------------------
-- ids are never reused
-- ---------------------------------------------------------------------------

-- ids come from a counter on this row rather than a sequence, so that a gap
-- means a missing entry rather than a rolled back transaction. lowering the
-- counter would hand the next transaction an id that already exists, and the
-- unique constraint would then reject an honest commit while the tampering
-- that caused it went unrecorded.
create function giro_ledger_counters_monotonic() returns trigger as $$
begin
    if new.last_tx_id < old.last_tx_id then
        raise exception 'giro: ledger % last_tx_id fell from % to %, ids would be reused',
            new.name, old.last_tx_id, new.last_tx_id
            using errcode = 'restrict_violation';
    end if;
    if new.last_log_id < old.last_log_id then
        raise exception 'giro: ledger % last_log_id fell from % to %, ids would be reused',
            new.name, old.last_log_id, new.last_log_id
            using errcode = 'restrict_violation';
    end if;
    return new;
end;
$$ language plpgsql;

create trigger ledgers_counters_monotonic before update on ledgers
    for each row execute function giro_ledger_counters_monotonic();
