-- recovery_is_an_appended_event
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a
-- transaction, for example create index concurrently.

-- A restore is the one failure nothing else here can see.
--
-- ids are allocated from a counter on the ledgers row, and a trigger refuses
-- to let that counter go backwards, because reusing an id means two different
-- transactions answering to the same name. that trigger fires on an update. a
-- restore replaces the table, so it never sees it: the counter goes back to
-- the restore point and the next commit claims an id that has already been
-- issued. every check still passes. the book is internally perfect and no
-- longer means what it meant.
--
-- the repair is to resume above every id ever issued, which leaves a gap where
-- the lost entries were. and a gap is exactly what VerifyLog is built to
-- catch: a missing id means an entry was deleted.
--
-- so the gap is declared rather than tolerated. resuming appends a RECOVERY
-- entry that names the range it skipped, hash chained like everything else,
-- and verification accepts a gap only when the entry after it declares that
-- gap. an undeclared gap is still a broken chain, which is the property worth
-- keeping.
--
-- this is the same rule as the rest of the ledger: nothing recorded is edited,
-- and a correction is something you append. an operator quietly bumping a
-- counter is an edit wearing a different hat.
alter table logs drop constraint logs_type_known;

alter table logs add constraint logs_type_known check (type in (
    'NEW_TRANSACTION',
    'REVERTED_TRANSACTION',
    'SET_METADATA',
    'DELETE_METADATA',
    'RECOVERY'
));
