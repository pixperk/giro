-- init_schema
--
-- six tables. every one carries ledger as the first element of its key, so
-- the tenant boundary is structural rather than something each query has to
-- remember.
--
-- amounts are numeric with no precision anywhere. a precision is a ceiling,
-- and int64 overflows at 9.2e18, which an 18 decimal token reaches with two
-- units.
--
-- timestamps are timestamptz. postgres stores an instant in utc and converts
-- on the way out. timestamp without time zone stores wall clock with no zone,
-- which is a footgun for a system whose whole job is knowing when things
-- happened.


-- one row per ledger. the counters are why this table exists.
--
-- the obvious alternative is a postgres sequence per ledger. sequences take no
-- row lock so they are faster, but they are non transactional: a rolled back
-- transaction burns an id and leaves a gap. a counter column updated inside the
-- transaction is gapless.
--
-- the cost is that every write to a ledger serialises on this row. we pay that
-- already, because synchronous hash chaining requires reading the previous hash
-- before writing the next one. the lock we need for the chain is the same lock
-- that allocates the id, so gaplessness is free.
create table ledgers (
    name          varchar     primary key,
    metadata      jsonb       not null default '{}',
    added_at      timestamptz not null default now(),

    last_tx_id    bigint      not null default 0,
    last_log_id   bigint      not null default 0,
    last_log_hash bytea
);


-- accounts are never registered, they appear because a posting named one.
-- this table exists for metadata and for prefix queries, not to authorise
-- anything: an address absent from here still has a balance of zero.
create table accounts (
    ledger         varchar     not null,
    address        varchar     not null,
    address_array  text[]      not null,
    metadata       jsonb       not null default '{}',
    first_usage    timestamptz not null,
    insertion_date timestamptz not null,
    updated_at     timestamptz not null,

    primary key (ledger, address)
);

-- text[] rather than jsonb because postgres arrays are ordered and indexable
-- by position, which is what address matching actually needs:
--
--   address_array[1] = 'users'                    first segment
--   address_array[1:2] = array['users','alice']   prefix at any depth
--   array_length(address_array, 1) = 2            exactly two segments
--
-- storing this as jsonb would mean encoding position into the keys, something
-- like {"0":"users","1":"alice"}, because jsonb arrays offer no positional
-- access. the array type gives it for free.
-- two indexes because there are two shapes of address query.
--
-- a plain prefix, users:42:*, is a range scan on the address text. measured on
-- 150k accounts this is an index only scan costing 8, against 1297 for the gin
-- path, so prefixes should always take this route.
create index accounts_address_prefix on accounts (ledger, address varchar_pattern_ops);

-- a wildcard in the middle, users:*:wallet, cannot be expressed as a range, so
-- it uses gin containment to narrow and a positional predicate to filter.
-- containment alone is wrong: it is position independent, so users matches
-- fees:users:refunds too.
create index accounts_address_array on accounts using gin (address_array);
create index accounts_address_depth on accounts (ledger, array_length(address_array, 1));


-- the only mutable table in the system. everything else is append only.
--
-- both counters only ever increase, so every write is input = input + $n,
-- a relative update the database performs. balance is input - output, derived
-- on read and stored nowhere.
--
-- this is also the contention point of the whole ledger. rows for world, and
-- for any treasury or fee account, are touched by most transactions.
create table accounts_volumes (
    ledger  varchar not null,
    address varchar not null,
    asset   varchar not null,
    input   numeric not null default 0,
    output  numeric not null default 0,

    primary key (ledger, address, asset)
);


-- no foreign key to ledgers on this table or any other, and the reason is
-- specific rather than a preference.
--
-- a foreign key check takes FOR KEY SHARE on the referenced ledgers row. the
-- commit path later takes FOR UPDATE on that same row to allocate an id. two
-- concurrent transactions would each hold KEY SHARE and each wait to upgrade to
-- UPDATE, which is a deadlock neither lock ordering nor sorting can prevent.
-- the ledger name is validated in application code instead.
create table transactions (
    ledger       varchar     not null,
    id           bigint      not null,

    -- when it happened economically, vs when this database learned of it.
    -- they disagree whenever news arrives late, which is most of the time.
    timestamp    timestamptz not null,
    inserted_at  timestamptz not null default now(),
    reverted_at  timestamptz,

    reference    varchar,
    postings     jsonb       not null,
    metadata     jsonb       not null default '{}',

    -- denormalised from postings so "every transaction touching users:alice"
    -- is an index lookup rather than a scan over the postings jsonb.
    -- text[] so a gin index answers  sources @> array['users:alice'].
    sources      text[]      not null,
    destinations text[]      not null,

    -- per account state after this transaction, frozen at commit. never
    -- rewritten, even when a backdated transaction lands later.
    pc_volumes   jsonb,

    primary key (ledger, id)
);

-- a reference is the caller's own identifier for a transaction, unique per
-- ledger when present. partial, because most transactions have none.
create unique index transactions_reference on transactions (ledger, reference)
    where reference is not null;

create index transactions_timestamp on transactions (ledger, timestamp desc);


-- the per account half entries. two rows per posting, one for each side.
--
-- transactions are organised by transaction; almost every question a human asks
-- is organised by account. this is the same data indexed the way it is read.
create table moves (
    seq             bigserial primary key,
    ledger          varchar     not null,
    tx_id           bigint      not null,

    address         varchar     not null,
    asset           varchar     not null,
    amount          numeric     not null,
    is_source       boolean     not null,

    effective_date  timestamptz not null,
    insertion_date  timestamptz not null,

    -- the account's volumes immediately after this move, in insertion order.
    -- written once, never updated. answers "what did the ledger believe then".
    pcv_input       numeric     not null,
    pcv_output      numeric     not null,

    -- the same in effective date order. rewritten when a transaction is
    -- inserted with an earlier effective date. answers "what was actually true
    -- on that date". null until computed.
    pcev_input      numeric,
    pcev_output     numeric,

    -- safe here, unlike the ledgers reference above: the transactions row is
    -- inserted by this same transaction, so the key share lock is taken on a
    -- row we already own and no lock upgrade is involved.
    foreign key (ledger, tx_id) references transactions (ledger, id)
);

-- the index that turns historical balance from a replay into a seek. seq
-- breaks ties when two moves share an effective date.
create index moves_account_history on moves (ledger, address, asset, effective_date desc, seq desc);

-- fetching every move belonging to one transaction.
create index moves_tx on moves (ledger, tx_id);


-- the source of truth. transactions and accounts_volumes are a projection of
-- this that could be rebuilt by replay.
--
-- type is text with a check rather than an enum. extending an enum needs
-- ALTER TYPE ADD VALUE, which cannot be used in the same transaction that adds
-- it; a check constraint is replaced by an ordinary migration.
create table logs (
    ledger           varchar     not null,
    id               bigint      not null,
    type             text        not null,
    data             jsonb       not null,
    date             timestamptz not null default now(),

    -- sha256 over the previous hash and this entry. editing any historical
    -- entry invalidates every hash after it.
    hash             bytea       not null,

    -- the key comes from the client. the hash is over the request inputs, so a
    -- replayed key carrying different inputs is an error rather than a silent
    -- success for a payment that never happened.
    idempotency_key  varchar(256),
    idempotency_hash text,

    primary key (ledger, id),

    constraint logs_type_known check (type in (
        'NEW_TRANSACTION',
        'REVERTED_TRANSACTION',
        'SET_METADATA',
        'DELETE_METADATA'
    ))
);

create unique index logs_idempotency_key on logs (ledger, idempotency_key)
    where idempotency_key is not null;
