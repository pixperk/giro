-- reconcile_against_moves
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- a statement line is one account, one asset, one amount, one direction. that
-- is a move, not a transaction, and pairing it with a transaction was the
-- wrong grain.
--
-- a conversion makes it obvious: selling stablecoin for dollars is one
-- transaction that moved 100,000 of one asset and 99,960 of another. a line on
-- kraken's dollar statement is talking about exactly one of those, and "the
-- transaction" has no single amount to compare it against. the move on
-- external:lp:kraken:USD does.
--
-- the transaction is still reachable -- a move names its own -- so nothing is
-- lost by linking to the finer thing.

-- the foreign key wants a ledger scoped target, and the moves primary key is
-- seq alone because a bigserial is unique on its own. this adds the scoped
-- uniqueness the reference needs, and keeps the discipline every other table
-- here follows: ledger first, always.
create unique index moves_ledger_seq on moves (ledger, seq);

alter table recon_matches
    drop constraint recon_matches_ledger_transaction_id_fkey,
    drop column transaction_id,
    add column move_seq bigint not null,
    add constraint recon_matches_move_fkey
        foreign key (ledger, move_seq) references moves (ledger, seq);

-- one move is matched at most once per source. two sources may each have their
-- own view of the same movement, and should: the chain and the exchange both
-- saw the same deposit and both are worth recording.
-- dropping the column took its index with it, so this is only a create.
drop index if exists recon_matches_one_per_source;
create unique index recon_matches_one_per_source
    on recon_matches (ledger, source, move_seq);
