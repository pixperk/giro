-- reconciliation
--
-- forward only. to undo something, write a new migration.
-- add "-- giro:no-transaction" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

-- every check so far proves the book is consistent with itself. none of them
-- can tell you the money is actually in the bank.
--
-- reconciliation is the other half: the ledger records what we believe
-- happened, and this compares that against what the chain, the exchange and
-- the bank each say happened. when they disagree, one of two things is true
-- and somebody needs to know which. both happen and neither announces itself.
--
-- the ledger stays the source of truth. nothing here writes a posting or
-- changes a balance: a reconciler that could correct the book would be a
-- second way for money to move, and the whole point of it is to have an
-- independent opinion.

-- where statement lines come from. a row per exchange, bank or chain.
--
-- per ledger, like everything else, because two entities reconcile against
-- their own counterparties and a source is not shared between them.
create table recon_sources (
    ledger        varchar     not null,
    id            varchar     not null,
    name          varchar     not null,
    registered_at timestamptz not null default now(),

    primary key (ledger, id)
);

-- one normalised statement line, as the source reported it.
create table recon_records (
    ledger    varchar     not null,
    source    varchar     not null,

    -- the source's own line id. staging is idempotent on it, so ingesting the
    -- same file twice stages nothing new -- which is what makes an ingest safe
    -- to retry after a timeout that may or may not have landed.
    record_id varchar     not null,

    -- the match key: a wire reference, a trade id, a transaction hash. null
    -- when the source gives none, in which case the line can never match and
    -- is there to be looked at by a person.
    reference varchar,

    asset     varchar     not null,
    -- a positive magnitude. which way it went is direction, not a sign,
    -- for the same reason a posting has no sign: one way of saying a thing
    -- cannot disagree with itself.
    amount    numeric     not null check (amount > 0),

    -- 'in' or 'out' from our side, or null when the source does not say.
    -- supplying it is what stops an outbound wire reconciling against an
    -- inbound movement of the same size and reference, which is a real
    -- mistake and an easy one.
    direction varchar     check (direction in ('in', 'out')),

    occurred_at timestamptz,
    raw         json,
    ingested_at timestamptz not null default now(),

    -- filled in by matching. a record matched to several transactions is a
    -- consolidated payment, so this counts rather than naming one.
    matched_count int         not null default 0,
    matched_at    timestamptz,

    primary key (ledger, source, record_id),
    foreign key (ledger, source) references recon_sources (ledger, id),
    foreign key (ledger, asset)  references assets (ledger, asset)
);

-- the work queue: lines nobody has paired yet, oldest first.
create index recon_records_unmatched
    on recon_records (ledger, ingested_at)
    where matched_count = 0;

-- matching joins on (asset, reference), so that is the index.
create index recon_records_by_reference
    on recon_records (ledger, asset, reference)
    where reference is not null and matched_count = 0;

-- what was paired with what, and why. evidence rather than state.
create table recon_matches (
    ledger         varchar     not null,
    id             bigserial   primary key,
    source         varchar     not null,
    record_id      varchar     not null,
    transaction_id bigint      not null,

    -- the source said this much and we recorded that much. zero for an exact
    -- pairing, and a figure worth looking at otherwise: it is the difference
    -- between what somebody else thinks happened and what we think happened.
    variance       numeric     not null,

    -- how many of our transactions this one line paid. more than one is a
    -- consolidated payment, and every row of such a set carries the set's
    -- size so a partial one is visible from any of them.
    set_size       int         not null default 1,

    rule           varchar     not null,
    matched_at     timestamptz not null default now(),

    foreign key (ledger, source, record_id) references recon_records (ledger, source, record_id),
    foreign key (ledger, transaction_id)    references transactions (ledger, id)
);

-- one transaction is matched at most once per source. two sources may each
-- have their own view of the same movement, and should.
create unique index recon_matches_one_per_source
    on recon_matches (ledger, source, transaction_id);

-- ---------------------------------------------------------------------------
-- guards
-- ---------------------------------------------------------------------------

-- evidence is append only, and this is the guard that matters most here.
--
-- deleting these rows moves no money, which is exactly why it is dangerous:
-- the postings are untouched, the chain still verifies, conservation still
-- holds, and the book now reconciles because the rows that did not reconcile
-- are gone. a clean report obtained by deleting the mess is the one failure
-- this whole layer exists to prevent.
create trigger recon_matches_append_only before update or delete on recon_matches
    for each row execute function giro_append_only();
create trigger recon_matches_no_truncate before truncate on recon_matches
    for each statement execute function giro_append_only();

-- a staged record may be marked matched and nothing else. an allow list rather
-- than a deny list, so a column added by a later migration is protected from
-- the moment it exists rather than when somebody remembers.
create trigger recon_records_limited_change before update or delete on recon_records
    for each row execute function giro_only_change('{matched_count,matched_at}');
create trigger recon_records_no_truncate before truncate on recon_records
    for each statement execute function giro_append_only();

create trigger recon_sources_no_truncate before truncate on recon_sources
    for each statement execute function giro_append_only();

-- the application ingests lines, records what it matched, and reads both back.
-- it cannot remove either, and it cannot revise a line to make it match.
grant select, insert on recon_sources, recon_records, recon_matches to giro_app;
grant update (matched_count, matched_at) on recon_records to giro_app;
grant usage on sequence recon_matches_id_seq to giro_app;
