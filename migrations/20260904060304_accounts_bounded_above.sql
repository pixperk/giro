-- accounts_bounded_above
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- allow_negative could turn the balance guard off and never turn it around,
-- and some accounts need it the other way.
--
-- a cost account is a tally of what something has cost. every loss pushes
-- cost:peg_absorption further negative and nothing ever pushes it back, so a
-- positive balance there means a loss was recorded as a gain. the books still
-- balance -- the money went somewhere real -- and the profit figure is wrong
-- by twice the amount, silently, because conservation has no opinion about
-- which direction is which.
--
-- today such an account is set allow_negative, which means "no rule", so
-- nothing notices. the mirror of the flag we have is what says "this one only
-- leans the other way".
--
-- default true, so nothing changes for any account that exists: an ordinary
-- account is bounded below and unbounded above, which is what it always was.
alter table accounts_volumes
    add column allow_positive boolean not null default true;

grant update (allow_positive) on accounts_volumes to giro_app;

-- both bounds in one guard, because they are one question asked twice. the
-- carve out for a policy change that moves no money applies to both: an
-- operator narrowing a bound on an account that already sits outside it is
-- stopping the bleeding, and refusing that would leave the only remedy blocked
-- by the thing it remedies.
create or replace function giro_no_unpermitted_overdraw() returns trigger as $$
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

    if new.input > new.output and not new.allow_positive then
        raise exception 'giro: % would hold % in %, and is not permitted a positive balance',
            new.address, new.input - new.output, new.asset
            using errcode = 'restrict_violation';
    end if;
    return new;
end;
$$ language plpgsql;

-- and the detector that looks for balances sitting outside a bound they are no
-- longer permitted, which now has two sides for the same reason.
create index accounts_volumes_bounded_above
    on accounts_volumes (ledger, address, asset) where not allow_positive;
