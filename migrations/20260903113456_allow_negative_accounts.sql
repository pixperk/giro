-- allow_negative_accounts
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- until now exactly one account was permitted a negative balance, and it was
-- identified by its name in go. that is enough for a ledger whose only edge is
-- the outside world, and not enough for a book that carries its own costs.
--
-- a contra account is one value leaves rather than accumulates in: peg
-- absorption, trading fees paid to a venue, anything that is a cost line
-- rather than a pot of money. it runs negative by design, and the overdraw
-- guard is right to refuse it until someone says otherwise, because the guard
-- has no way to tell a cost account from a client account that is about to be
-- drained.
--
-- so the permission becomes a property of the row rather than a name in the
-- source, and world becomes the first account to carry it rather than a
-- special case beside it.
--
-- it lives on accounts_volumes, per (address, asset), for two reasons. an
-- account may be a cost line in one asset and an ordinary balance in another.
-- and this is the row the commit path already takes FOR UPDATE, so the flag is
-- read under the lock that guards the balance it governs, with no second lock
-- and no new ordering to get wrong.
--
-- default false. an account goes negative because someone declared it should,
-- in a statement somebody reviewed, never because a name matched a pattern.
alter table accounts_volumes
    add column allow_negative boolean not null default false;

-- world rows already exist wherever a ledger has seen a single transaction,
-- and they were created before this column did. without this they would be
-- created with the default and the next deposit would be refused for
-- overdrawing an account that is defined by being overdrawn.
update accounts_volumes set allow_negative = true where address = 'world';
