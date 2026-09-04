-- asset_registry
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- the last correctness gap in the core.
--
-- an asset is any string matching [A-Z][A-Z0-9_]*(/\d{1,6})?, so "USD/2" and
-- "USD/6" are both valid. they are different assets, they never mix, and
-- nothing anywhere raises an error. a typo in one caller becomes a second
-- currency that quietly accumulates its own balances, and the first sign of it
-- is a conservation check passing on a ledger whose dollars are in two piles.
--
-- fusing the scale into the asset was right and is not what changed. what was
-- missing is a list of the assets a ledger is allowed to handle at all.

create table assets (
    ledger        varchar     not null,
    asset         varchar     not null,
    registered_at timestamptz not null default now(),

    primary key (ledger, asset)
);

-- and the constraint that closes the actual bug: one scale per code, per
-- ledger.
--
-- expressed on the code rather than by storing a scale column, so the design
-- decision that there is no scale anywhere in this system survives. the scale
-- lives in the asset identifier, exactly as before; this index only says that
-- a ledger cannot register two spellings of the same currency.
--
-- registering USD/2 therefore makes USD/6 and bare USD refusable, by the same
-- mechanism and with the same error, rather than by a rule someone has to
-- remember to apply.
create unique index assets_one_scale_per_code
    on assets (ledger, split_part(asset, '/', 1));

-- existing ledgers keep working: whatever they have already used is registered
-- for them. this is the honest migration for a table that is about to become
-- required, and it is why the foreign keys below can be added at all.
insert into assets (ledger, asset)
select distinct ledger, asset from accounts_volumes
on conflict do nothing;

-- the gate itself. every commit materialises an accounts_volumes row for each
-- (account, asset) it touches and inserts a move per posting side, so a
-- foreign key on either is checked on every write, by the database, for every
-- writer.
--
-- there is no foreign key to ledgers, for a reason worth restating here: a
-- foreign key check takes FOR KEY SHARE on the parent row, the commit path
-- takes FOR UPDATE on the same ledgers row to allocate ids, and two concurrent
-- transactions each holding the shared lock and each waiting to upgrade is a
-- deadlock that sorting cannot prevent.
--
-- assets is safe from that because nothing ever updates a row in it. no
-- FOR UPDATE is taken, so there is no upgrade, so there is no cycle. the
-- shared locks concurrent commits take on the same asset row are shared with
-- each other and contend with nothing.
alter table accounts_volumes
    add constraint accounts_volumes_asset_registered
    foreign key (ledger, asset) references assets (ledger, asset);

alter table moves
    add constraint moves_asset_registered
    foreign key (ledger, asset) references assets (ledger, asset);

-- registration is permanent.
--
-- deregistering an asset a ledger holds balances in would leave those balances
-- referring to nothing, and re-registering it at a different scale would
-- reinterpret every amount already recorded in it: 10000 of USD/2 is a hundred
-- dollars, and the same number in USD/6 is a hundredth of one. that is not a
-- correction, it is a silent restatement of the whole book.
--
-- the foreign keys already make a delete fail wherever the asset is in use.
-- this refuses it even for an asset nothing has touched yet, because the
-- alternative is a rule that holds only while nobody has got there first.
create function giro_assets_are_permanent() returns trigger as $$
begin
    raise exception 'giro: asset registration is permanent, % cannot be %',
        coalesce(old.asset, new.asset),
        case tg_op when 'DELETE' then 'deregistered' else 'changed' end
        using errcode = 'restrict_violation';
end;
$$ language plpgsql;

create trigger assets_permanent before update or delete on assets
    for each row execute function giro_assets_are_permanent();
create trigger assets_no_truncate before truncate on assets
    for each statement execute function giro_append_only();

-- the application registers assets and reads them back. it cannot remove one,
-- which the trigger above enforces for every writer and this enforces for
-- this one.
grant select, insert on assets to giro_app;
