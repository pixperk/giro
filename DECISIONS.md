# Decisions

Every non-obvious choice in giro, with the reasoning and what was turned down.

This is the file to read when you are wondering "why is it like this" — before
changing something that looks wrong, and before assuming a constraint is
accidental. Most of them are load-bearing, and several were arrived at by being
wrong first.

Read [README.md](README.md) to use giro. This is why it is shaped that way.

**Roughly grouped:** D1–D13 the model, D14–D24 the write path and concurrency,
D25–D31 reads and performance, D32–D36 the library surface and the guards,
D37–D44 account policy, D45–D50 reconciliation, D51 the operator surface.

---

### D1. Postings name both sides, and amounts are always positive

A posting carries source, destination, asset and a positive amount. There is no
debit or credit flag and no sign.

Naming both sides in one record makes an unbalanced entry impossible to write
down, rather than an error to be detected later. Direction is already carried by
which field an account sits in, so a sign would be a second way of saying the
same thing, and two ways of saying one thing is two ways for them to disagree.

### D2. Volumes rather than balances

Store `input` and `output`, both monotonically increasing. Derive balance.

Three reasons, in increasing weight. Gross flow is information a balance
destroys. Additive updates compose under concurrency while absolute writes lose
each other. And a frozen snapshot on each move turns historical balance into an
index seek instead of a replay.

### D3. The asset string carries its own scale

`USD/2` means two decimal places, so `100` is one dollar. There is no scale
column anywhere.

A scale stored beside an amount can be lost in a serialisation round trip or
defaulted to zero by a migration, and then `10000` silently becomes ten thousand
dollars instead of a hundred. Fusing scale to identity makes an amount
uninterpretable without it.

### D4. Arbitrary precision integers everywhere

`*big.Int` in Go, `numeric` with no precision in PostgreSQL.

`int64` overflows at 9.2e18, which two units of an 18 decimal token reach. A
fixed width is a ceiling, and the cost of a ceiling here is silently wrong
money. A size guard of about 100 digits lives in validation to reject absurd
input, which is a denial of service concern rather than a business rule.

### D5. `timestamptz` for every timestamp

PostgreSQL stores an instant in UTC and converts on the way out.
`timestamp without time zone` stores wall clock with no zone, which puts the
burden of normalising on every write path, and one missed conversion is silent.

### D6. Gapless ids from a counter column, not a sequence

Transaction and log ids come from `UPDATE ledgers SET last_tx_id = last_tx_id + 1
RETURNING`, inside the transaction.

A sequence takes no row lock and is faster, but it is non transactional: a
rolled back transaction burns an id and leaves a gap. The counter serialises
writes per ledger, which we already pay for, because synchronous hash chaining
has to read the previous hash before writing the next. The lock needed for the
chain is the same lock that allocates the id, so gaplessness is free.

The cost is real: this lock is held until commit, so writes to a single ledger
are serialised. Writes to different ledgers are not, which is also how write
throughput scales.

### D7. No foreign key referencing `ledgers`

A foreign key check takes `FOR KEY SHARE` on the parent row. The commit path
takes `FOR UPDATE` on the same `ledgers` row to allocate an id. Two concurrent
transactions would each hold the shared lock and each wait to upgrade to the
exclusive one, which deadlocks. Sorting cannot help, because there is only one
lock and the problem is the change in strength rather than the order.

The `moves` to `transactions` foreign key is kept, because that parent row is
inserted by the same transaction and is invisible to anyone else, so there is
nothing to share and no upgrade.

### D8. `text[]` for exploded addresses, not `jsonb`

PostgreSQL arrays are ordered and indexable by position, which is exactly what
address matching needs: `address_array[1] = 'users'` for the first segment,
`array_length` for depth. Storing this as `jsonb` would mean encoding position
into the keys to get the same thing back.

### D9. Two address indexes, because there are two query shapes

A plain prefix such as `users:42:*` is a range scan on the address text using a
`varchar_pattern_ops` index. Measured on 150k accounts, that is an index only
scan costing 8 against 1297 for the GIN path.

A wildcard in the middle, `users:*:wallet`, cannot be expressed as a range, so
it uses GIN containment to narrow and a positional predicate to filter.
Containment alone is wrong because it is position independent: searching for
`users` also matches `fees:users:refunds`.

A positional predicate never drives an index. It is there for correctness, not
speed.

### D10. Forward only migrations, with checksums

No down migrations. You cannot un-drop a column that had data, so every real
rollback is a new migration anyway, and a down half is a file nobody tests that
lies under pressure.

Every applied migration records a SHA-256 of its body. Editing a migration that
already ran is a hard error, because otherwise development and production
diverge silently and only one of them is right.

### D11. Migrations hold a session advisory lock on a dedicated connection

Two instances booting at once must not both apply the same migration. A session
scoped advisory lock is released automatically when the connection drops, so a
crashed migration cannot strand it, which a lock row would.

It must be a dedicated `*pgx.Conn` and never a pool. Releasing on a different
pooled connection returns false and does nothing, leaving the lock held until
that connection happens to close. The signature takes `*pgx.Conn` so the type
system enforces this.

The lock is acquired by polling `pg_try_advisory_lock` rather than by blocking
on `pg_advisory_lock`. A session parked in a lock wait still holds a virtual
transaction id, and `create index concurrently` waits for every virtual
transaction that was live when it started. A runner holding the lock and
building an index therefore waits for a runner waiting for the lock, and
Postgres breaks the cycle by killing the waiter. That is a boot failure in the
exact case the lock exists for. Polling has no such edge: between attempts the
waiting session holds nothing, at a cost of one poll interval of latency.

### D12. A nil amount panics instead of being read as zero

A nil counter in `Volumes` means nothing has flowed, so it is safely treated as
zero. A nil `Amount` on a posting means the request was malformed. Coercing it
would turn a broken payment into a no-op that commits and returns success.

Validation rejects nil, so the panic is unreachable through any correct path. It
exists to make a skipped validation loud rather than silent.



`jsonb` parses into a binary tree. It reorders keys, strips whitespace, and
silently keeps only the last of any duplicate key. The bytes that come back are
not the bytes that went in.

That is fatal for a hash chain. A hash taken at write time would never match one
recomputed during verification, so the integrity check would fail on a perfectly
healthy ledger, which is worse than not having one: a real problem becomes
indistinguishable from the false alarm.

`json` is validated on write and stored verbatim, and round trips byte for byte.
The indexing `jsonb` would buy is irrelevant here, because a log entry is
appended, read whole for replay, and looked up by `idempotency_key`, which is
its own column. Nothing ever queries inside one.

This applies only to `logs.data`. `transactions.postings` and the metadata
columns stay `jsonb`, since nothing hashes them and querying into them is
useful.

### D13. Multi ledger from the start, in one schema

Every table carries `ledger` first in its key. Retrofitting tenancy means
touching every table, index, query and test while carrying live data, whereas
adding the column now costs one column and gives the composite key that keyset
pagination wants anyway.

Since the hash chain serialises writes per ledger, separate ledgers are also how
write throughput scales.

### D14. The log entry is stored as `json`, not `jsonb`

`jsonb` parses into a binary tree. It reorders keys, strips whitespace, and
silently keeps only the last of any duplicate key. The bytes that come back are
not the bytes that went in.

That is fatal for a hash chain. A hash taken at write time would never match one
recomputed during verification, so the integrity check would fail on a perfectly
healthy ledger, which is worse than not having one: a real problem becomes
indistinguishable from the false alarm.

`json` is validated on write and stored verbatim, and round trips byte for byte.
The indexing `jsonb` would buy is irrelevant here, because a log entry is
appended, read whole for replay, and looked up by `idempotency_key`, which is
its own column. Nothing ever queries inside one.

This applies only to `logs.data`. `transactions.postings` and the metadata
columns stay `jsonb`, since nothing hashes them and querying into them is
useful.

---

### D15. The contract is written first, and only models are generated

`api/openapi.yaml` is the source of truth for the http surface, and the Go
types in `internal/api/gen.go` are generated from it. Changing the request shape
therefore shows up as a diff in the contract, deliberately made, rather than
falling out of somebody editing a struct.

Only models are generated. The same tool will also emit routing and query
parameter binding, but that pulls three modules into the build to parse query
strings, for a surface of eight endpoints on the standard library router. So
routing is hand written, and what the compiler no longer checks a test does:
every path in the contract must have a route, and every route must be in the
contract.

The generator itself is not a dependency. It runs from `just generate` with a
pinned version, so it never enters `go.mod`. The only thing this service links
against is a postgres driver.

### D16. Amounts are json numbers, and the Go type is a pointer

An amount goes over the wire as a json number rather than a string, matching how
it is stored: an arbitrary precision integer in the asset's smallest unit.

That has one consequence worth stating loudly, because it is a client side
hazard rather than a server one. Amounts can exceed 2^53, and JavaScript's
`JSON.parse` silently loses precision above that. A browser client must use a
big number aware parser. The contract says so in its description.

On the Go side the generated type is `*big.Int` and not `big.Int`, which is not
a stylistic preference. `big.Int` implements `MarshalJSON` on the pointer
receiver, and `encoding/json` can take the address of a struct field inside a
slice but not of a map value. With a value type, amounts inside slices marshal
correctly and every amount inside a map silently marshals as `{}`. Balances are
maps.

### D17. Errors carry a stable code, and 422 is not 400

Every error response has a `code` from a fixed set, alongside the http status.
The message is for humans and may change, the code is what a client branches on.

400 and 422 are different answers. A 400 means the request was malformed: a
negative amount, an address with a trailing space, a lowercase asset. A 422
means the request was well formed and could not be applied, which in practice
means insufficient funds. A client can fix the first by changing its code and
the second only by changing the world, so they should not share a status.

Insufficient funds carries the numbers in `details`, because an error a client
can act on programmatically should not require parsing a sentence.

Internal errors return a generic message. The real one goes to the log, since it
can carry table names, queries and account addresses.

### D18. Timestamps are truncated to microseconds

Postgres stores `timestamptz` at microsecond precision. A timestamp with
nanoseconds would be silently rounded on the way in, so the response to a create
would disagree with every later read of the same transaction.

Truncating at the boundary means the value returned is the value stored.

### D19. Malformed query strings are rejected rather than ignored

Go's `r.URL.Query()` discards any parameter it cannot parse, so a corrupted
query string arrives at a handler as absent parameters. For a cursor that is
dangerous: the client would silently receive the first page again and reprocess
transactions it has already seen.

Query strings are parsed explicitly, and anything unparseable is a 400.

### D20. Expansions are opt in, and an unknown one is an error

`GET /accounts/{address}` returns the account without its volumes, because most
reads of an account want its metadata rather than its money, and volumes cost a
second query. `?expand=volumes` asks for them.

An unrecognised expand value is a 400 rather than being ignored, so a typo does
not look like an account that happens to have no volumes.


### D21. Metadata history lives in the log, not in a revision table

Metadata is stored on the `transactions` and `accounts` rows as a jsonb column,
and every change appends a `SET_METADATA` or `DELETE_METADATA` entry to the log.

The obvious alternative is a table keyed by `(target, revision)` holding every
version of the document. That answers "what was the metadata at revision 3",
which nothing has asked for, and it would be a second append only history of
the same events with weaker guarantees than the one that is hash chained.

The split instead is: the column says what it is now, the log says how it got
that way, and a historical value is a replay of the log up to a date. If an
indexed historical lookup is ever needed, it can be built from the log, which
is what having a source of truth is for.

Postgres does the merge and the delete natively, `metadata || $new` and
`metadata - $key`, so there is no read modify write and two callers touching
different keys do not overwrite each other.

### D22. A metadata write that changes nothing writes nothing

Setting metadata that is already present is a no-op: no row is touched and no
log entry is appended.

Clients retry, so an identical write is normal traffic rather than a mistake.
Recording it would fill the hash chain with entries describing nothing
happening.

The cost is that the trail records changes rather than attempts, so "someone
tried to set this at 14:03" is not answerable when the value was already set.
For a ledger, what changed is the more useful record.

### D23. Tagging an account creates it

Accounts are never registered, so setting metadata on an address that has never
been used creates the row rather than returning a 404.

That is the moment a caller most wants it: attaching a user id to a wallet
before any money has moved through it. The account still holds nothing, since
tagging is not funding.


### D24. A revert is a new transaction, and it can fail

Reverting does not edit or delete anything. It commits a new transaction
holding the original's postings with both sides swapped, and stamps
`revertedAt` on the original as a mark that a correction exists.

Balances return to where they were. Volumes do not: both counters go up, so the
rows record that money moved and then came back, rather than looking like it
never moved.

The reversal goes through the same locking, balance check and log append as any
other transaction, which means it can be **rejected**. If the money has since
been spent it is not there to give back, and forcing it would manufacture a
negative balance. `force` exists for an operator who has decided that is the
lesser problem, and it is a deliberate act rather than a default.

Two guards worth knowing about. `revertedAt` is set inside the same database
transaction as the reversal, so two concurrent reverts cannot both pass the
check and refund twice. And the reversal reverses the *order* of the postings as
well as their direction, because keeping the order would pay the first account
back before the last had returned anything, and an intermediate account would
dip below zero.

### D25. A reversal is dated now, unless asked otherwise

By default the reversal carries the current time, not the original's effective
date. A reversal happens when it happens, and backdating one rewrites what
historical balances say about a period that has probably already been reported
on.

`atEffectiveDate` asks for the other behaviour, for the case where the reversal
is correcting a data entry error rather than undoing a real movement.


### D26. Effective volumes are maintained on write and verified by replay

Every move carries two snapshots. `pcv` follows insertion order and is written
once, recording what the ledger believed at the time. `pcev` follows effective
date order, which is a different sequence whenever a transaction is backdated,
and answers what was actually true on a date.

`pcev` is maintained on write: a new move reads the effective balance as of its
own timestamp, accumulates from there, and shifts every move already sitting
later in effective order by its delta. Historical balance is then an index seek
rather than a sum over an account's whole history.

The alternative is computing it on read, which is trivially correct and grows
with the account's age. Maintaining a cache is faster and can be wrong, so the
slow version exists too, as `VerifyEffectiveVolumes`: it walks every account in
effective order, accumulates, and compares against what is stored. Randomised
sequences of backdated transactions are checked against it.

That is the trade. The optimisation is only defensible because something
independent checks it, and an optimisation nothing checks is a guess.

The fix up is written in Go rather than as a database trigger, for the same
reason: logic in PL/pgSQL is hard to test and invisible to the type system.

### D27. The log is verified against the projection, not just for tamper evidence

`VerifyProjection` replays the log and requires that `accounts_volumes` is
exactly what it produces, that every transaction was logged, and that the log
describes no transaction the table lacks.

This is what makes "the log is the source of truth" a fact rather than a
statement of intent. It is also the only check that would catch a commit path
writing one thing and logging another: every other assertion reads the
projection, so a consistent lie passes all of them. Reversing the postings in
the logged copy leaves conservation, every balance and the hash chain intact,
and only this notices.

### D28. Dry run is the real path, rolled back

`?dryRun=true` takes the locks, checks the balances against live data, writes
the volumes and moves, and then rolls back instead of committing.

It is not a simulation, so it cannot drift from what a commit does. A
transaction that would be rejected is rejected here with the same status, which
is the entire point of previewing.

Nothing is consumed: no id is allocated, no idempotency key is claimed, no log
entry survives, and the account rows materialised to take locks disappear with
everything else. The id on the response is what it would have been, not a
reservation. It answers 200 rather than 201, because nothing was created.


### D29. Batches are atomic, and only atomic

`POST /transactions/bulk` applies every transaction or none of them. There is no
best effort mode and no per item result list.

A best effort batch is several requests with fewer round trips, which a caller
can do in a loop. All or nothing is the thing that cannot be built out of single
commits, so it is the one worth offering. Items are applied in order and see
each other's effects, so an item may spend what an earlier item provided.

Every lock the batch needs is taken up front, deduplicated and sorted, before
any item runs. Sorting within an item is not enough here: two batches touching
the same accounts in different item orders would each hold half of what the
other needs. Measured, a batch of fifty is roughly twice the throughput of fifty
single commits, because it pays for the ledger row lock once.

At most 100 items, because an unbounded batch holds locks on an unbounded number
of rows on tables other requests are queueing for.

### D30. Writes to one ledger are serialised, and that is measured

Every commit takes an exclusive lock on its ledger's row to allocate ids and
read the chain tip, and holds it until commit. So writes to a single ledger do
not benefit from more callers.

Measured on a laptop against local postgres:

| | |
|---|---|
| one caller, one ledger | about 1,200 transactions per second |
| sixteen callers, one ledger | about 1,200 per second, and zero retries |
| sixteen callers, eight ledgers | about 3,100 per second |
| batch of fifty against fifty singles | roughly twice the throughput |

The flat line from one caller to sixteen is the design working as described,
not a bottleneck to be tuned away. Throughput scales by adding ledgers, which
is one reason multiple ledgers exist. Zero retries under contention means the
lock ordering is holding: transactions queue rather than deadlock.

### D31. The write path reads a snapshot rather than summing history

Computing what an account held as of a date is the same question on the read
path and the write path: a read answers it for a caller, and a commit needs it
to place a new move in effective order.

The read path always read the latest snapshot. The write path summed the whole
history, which is O(n) in the account's age on every write. On 20,000 moves that
is a sequential scan taking 2 ms against an index seek taking 0.03 ms, roughly
sixty times slower and growing.

Both now read the snapshot. This was found by benchmarking, not by any
correctness test: the results were identical either way.

### D32. Addresses and assets are named types

`ledger.Address` and `ledger.Asset` rather than `string`. Both are strings
underneath, so nothing changes at runtime or in the database.

The value is in application code above this library, where an address is
carried through several layers before it reaches a posting. That is where a
transposed argument survives review, and where the compiler now refuses it.

It does not catch a transposed literal, because an untyped constant converts to
any string type. `SetAllowNegative(ctx, "USD/2", "cost:peg", true)` still
compiles and is refused at runtime by validation instead. And nothing catches a
transposed source and destination, since both are addresses. No type system
can.

The wire format is unaffected. The generated types in `internal/api/gen.go` use
`string`, because the contract describes JSON, and the domain types stop at
that boundary.

Done immediately after the packages became importable, while nothing outside
the module depended on them. It is a breaking change, and every day it waits
is another consumer to coordinate with.

### D33. The server refuses to start against a schema it does not match

`giro serve` checks the applied migrations against the ones it carries, before
it listens.

Without it a binary needing a migration nobody ran starts cleanly, answers its
health check, and fails on the first commit with a raw SQL error, in a money
path, in front of a caller. A deploy can act on a process that will not start.
It cannot act on one that started and lies.

Three outcomes rather than two, because only two are faults:

| State | Result |
|---|---|
| Schema behind the binary | Refuse. Something the code expects does not exist. |
| A migration applied with a different body | Refuse. The schema and the code disagree about what ran. |
| Schema ahead of the binary | Warn and serve. |

The third is the one worth explaining. The usual deploy order is migrate first,
then roll the binaries, so between those two steps every instance still on the
old build is running against migrations it does not carry. That is the deploy
working. Refusing there would mean an old instance could not be restarted
during a rollout, turning a routine deploy into an outage.

It is still reported, because the same state is a real fault if it outlives the
rollout, and nothing else would mention it.

The check takes no lock and applies nothing. A process that serves traffic
should not be able to change the schema.

### D34. Assets are declared, accounts are not

An account exists because a posting named it (D2). An asset has to be
registered first. Those look inconsistent and are not, and the difference is
the point.

An asset carries its own scale, so `USD/2` and `USD/6` are both well formed,
are different assets, and never mix. Without a registry a mistyped scale is not
an error, it is a second currency that accumulates its own balances, and
**conservation still passes** because each pile balances on its own. Nothing
anywhere would raise a word.

There are a handful of assets in a system and thousands of accounts, so
requiring the handful to be declared costs almost nothing and closes a failure
mode that is otherwise silent and permanent.

**One scale per currency, per ledger.** Registering `USD/2` makes `USD/6` and
bare `USD` refusable. It is expressed as a unique index on the code rather than
by storing a scale column, so the decision that no scale is stored anywhere
(D1) survives intact: the scale still lives in the asset identifier and the
engine still never branches on it. The index only says a ledger cannot hold two
spellings of one currency.

**Registration is permanent**, and the reason is worth stating. Re-registering
a currency at a different scale would reinterpret every amount already recorded
in it: 10000 of `USD/2` is a hundred dollars and the same number in `USD/6` is
a hundredth of one. That is not a correction, it is a silent restatement of the
whole book.

**Enforced by foreign key from `accounts_volumes` and `moves`,** so it holds
for raw SQL too. There is deliberately no foreign key to `ledgers` (D14),
because the commit path takes `FOR UPDATE` on that row and two transactions
each holding a shared lock and each waiting to upgrade is a deadlock sorting
cannot prevent. `assets` is safe from that because nothing ever updates a row
in it: no `FOR UPDATE` is taken, so there is no upgrade and no cycle.

The Go check exists for the error rather than the enforcement. A constraint
violation says `accounts_volumes_asset_registered`; a caller who mistyped a
scale needs to be told that `USD/2` exists and `USD/6` does not.

**Cost:** roughly 4% on the hot-account path, within noise elsewhere.

### D35. The invariants are enforced in the database, not only in Go

Every rule used to live in Go, which holds exactly as long as the application
is the only writer. It is not, and will not be: an engineer in psql, a backfill
script, a migration nobody thought through, credentials that got out. None of
those run the Go code.

| Guard | Shape | Catches |
|---|---|---|
| Append only on `logs` | row `before update or delete` | rewriting history |
| Named columns on `transactions`, `moves`, `accounts` | row, allow list | changing what was recorded |
| No truncate, every table | **statement** `before truncate` | erasing a table |
| Volumes only increase | row `before update` | faking a balance by lowering what was spent |
| No unpermitted overdraw | row `before insert or update` | spending what is not there |
| Conservation | **deferred constraint** | creating or destroying value |
| Counters never fall | row `before update` | reusing a transaction id |

Three of those are worth explaining.

**Conservation cannot be a row trigger.** `input = input + 500000` is an
increase, on one row, and nothing about that row is invalid. Conservation is a
property of the whole table. It also cannot be checked per statement, because a
commit writes one volume row at a time and passes through unbalanced
intermediate states. So it is a constraint trigger deferred to commit, which
asks the only question that matters: is the book balanced by the time this is
over.

**Truncate needs a different trigger shape.** It discards the table's files
rather than visiting rows, so no row trigger fires and update and delete guards
that look complete let the whole table go without raising anything. `before
truncate` exists only in the statement form, for the same reason.

**The lists say what is allowed, not what is forbidden.** A forbid list is only
as good as the imagination of whoever wrote it, and a column added by a future
migration arrives unprotected. Comparing whole rows with the permitted keys
removed means a new column is guarded from the moment it exists.

One carve out, found by the test suite rather than by reasoning. The overdraw
guard fires when the balance moves, not when the permission changes, so an
operator can stop further drawdown on an account that is already negative.

**There is no escape hatch, and there was.** An earlier version of this let a
revert declare itself forced through a transaction local setting, so that a
reversal could commit against an account that had since spent the money. Any
role can set a custom setting, so the application role could set it too, which
made the overdraw guard the only one of the seven the application could walk
past. `RevertOptions.Force` is gone with it.

An operator who needs a reversal that drives an account negative grants the
account the permission, reverts, and revokes. Three steps that leave a trail in
the permission state, and one of them is already the mechanism for exactly
this.

**The cost.** The conservation check is one aggregate over the accounts holding
that asset, per touched row, at commit. Measured: 0.045 ms at a thousand
accounts, 10.7 ms at two hundred thousand. Throughput fell about 8% across
ledgers and is unchanged sequentially. It grows with the book, and if that ever
matters the answer is a maintained total per asset rather than an aggregate.

**What this does not do on its own.** A table's owner outranks its own triggers
and can disable them with one statement. D36 is what closes that.

### D36. The application connects as a role that cannot disable the guards

`alter table logs disable trigger user` needs no privilege beyond owning the
table. So every guard in D34 was advisory while the application owned what it
was guarded by.

Two roles. The migration tool owns everything and runs only during a rollout.
`giro_app` gets `select`, `insert`, and `update` on named columns. No `delete`,
no `truncate`, no `alter`, no ownership, and no write access to
`schema_migrations`, so a serving process cannot claim it migrated something.

The column scoping is the load bearing half. A table level `update` grant would
re-open the money columns and leave the trigger as the only thing standing,
which is exactly the hole an external audit found in the system this borrows
from: the guard covered insert and delete, left update open, and ordinary table
privileges were enough to write a spendable balance.

The grants are the same allow list as the triggers, expressed in a second
mechanism. The trigger says what a row may become; the grant says what the role
may reach for. Neither is redundant, because they fail differently: a trigger
can be switched off by the table's owner, and a missing grant cannot be given
by the role that lacks it.

**It is tested rather than asserted.** `just test-restricted` runs the whole
suite as `giro_app`. Every application path passes, including reverts,
backdating, metadata and all four verifiers. Six tests skip, and they are
exactly the six that have to damage the book to prove the damage is noticed.
Under this role they cannot stage their attack.

Two things worth knowing. The role is `nologin`: nothing authenticates as it
directly, so the credential story stays out of the schema. And a missing grant
surfaces as `relation does not exist` rather than a permission error when the
schema `usage` grant is the one missing, which reads like a missing table and
is not.

### D37. Money in flight lives in a holding account, not in a status column

A wire is submitted at two and confirmed at six. Posting it at submission means
the books are wrong for four hours if it bounces; posting it at settlement
means the payer's balance shows money that has already gone, and they can spend
it twice. Both are the same mistake: one event recording two.

So it moves into a holding account and out again.

```
submitted   client:acme                 -> pending:wire:WR-2026-0142
settled     pending:wire:WR-2026-0142   -> external:bank:northwind:USD
returned    pending:wire:WR-2026-0142   -> client:acme
```

This needs nothing from the engine. It is a naming convention, and three
properties fall out of what is already here:

- **The payer cannot spend it.** It genuinely left their account, so the
  balance guard is what stops them. No new mechanism.
- **Total value in transit is one prefix read.** A status would be something to
  filter; a balance is something to read.
- **It cannot be settled twice.** The holding account holds the amount exactly
  once, so a second settlement overdraws an account nobody permitted and is
  refused. A partial settlement works naturally: move part, the rest stays.

**Rejected: a status column on the transaction.** It is what most ledgers do,
and the trap in it is that a status is easy to add and easy to leave
disconnected from the arithmetic. A `pending` flag that does not affect the
balance is a label rather than a mechanism: the money is already spendable and
the flag only says somebody meant it not to be. Making it real means the
balance calculation has to branch on it, everywhere, for ever.

A status would also need a mutable money table, and after D35 there is no such
thing.

**Rejected: pending balances in the engine.** TigerBeetle carries
`debits_pending` separately from `debits_posted`, with post, void and an
expiry. It is the stronger model and it is the right one for card
authorisation, where the money has not moved and merely must not be spendable.
That is a different problem from money in flight, where the money genuinely has
left. Adopting it would mean a schema change, a second conservation story and a
second verifier, for a capability a ledger only needs once it is authorising
against funds rather than moving them.

**What the convention does not give you is a timeout**, and that is the one
thing worth building. A wire that neither settles nor returns leaves money in
the holding account indefinitely, and every check passes while it does:
conservation holds, the chain is intact, the projection agrees. Nothing is
wrong with the arithmetic, which is exactly why nothing notices.

`StaleBalances(ctx, prefix, olderThan)` is the answer, and it is deliberately
not named after holds. It reports money sitting still under a prefix, so the
same call finds dormant client accounts and an operating account that should
have returned to zero. The engine keeps its position of having no opinion about
what an address means.

It reads insertion order rather than effective dates: the question is when this
ledger last recorded something happening, not when it happened. Measured at
2.8 ms over 200,500 moves, on the index the read path already has.

### D38. giro runs behind a transaction pooler, with two conditions

Tested against PgBouncer 1.25 in transaction mode rather than reasoned about,
because the deployment target pools that way and the failures are all silent.

**The runtime is fine.** 200 concurrent commits through the pooler: no errors,
zero retries, conservation intact. It takes no session advisory locks, no temp
tables and no `LISTEN`/`NOTIFY`, which is what a transaction pooler punishes.

**Condition one: migrations must connect directly.** Not "should". The
migration advisory lock is session scoped, and through a transaction pooler two
different clients both acquired it, because they shared one server connection
and Postgres saw one session. A third client then released a lock it had never
taken. Every call returned success.

That lock is the only thing stopping two deploys migrating at once, so through
a pooler that protection is not weakened, it is absent. `GIRO_MIGRATE_DATABASE_URL`
exists for this, and the release check now names pooling as the likely cause
when it fires.

**Condition two: the pooler must support prepared statements.** With
`max_prepared_statements = 0`, giro fails in the middle of the money path with
`prepared statement "stmtcache_..." already exists`.

No pgx setting rescues it. `simple_protocol` and `exec` send parameters as
text, and the `json` columns then fail to parse. `describe_exec` still uses an
unnamed prepared statement, which the pooler can move a connection out from
under. So this is a requirement on the pooler and not something the client can
work around.

PgBouncer has supported it since 1.21 and defaults to 200. The deployment
target documents the same default, so the condition is met there rather than
merely likely to be.

**And never `SET ROLE` behind one.** PgBouncer runs its reset query only in
session mode by default, so session state survives and leaks: a client that set
a role left it behind, and the next unrelated client on that connection
inherited it. The restricted role therefore comes from **membership at login**
(D36), which is a property of the authenticated role rather than session state,
and survives multiplexing because there is nothing to survive.

### D39. A conversion derives its amounts from its rate, in a package of its own

**Where it lives is the decision.** The first version of this put `Conversion`
in the `ledger` package, next to postings and volumes, which contradicted D1 in
as many words: the ledger has no idea the two sides of a trade are related and
no opinion on whether a price was fair. A type whose whole job is knowing they
are related does not belong in the core of a general ledger.

So `fx` is its own package. It imports `ledger`, and nothing imports it except
the command that composes the layers. The engine keeps its position, metadata
stays opaque to the engine, and a consumer that does no trading never sees it.
That the arrangement holds is checked by the compiler rather than by review: if
`ledger` or `storage` imported `fx`, the test package would be an import cycle
and would not build.

A conversion is two postings in one transaction, one asset out and another in.
Conservation is checked per asset, so each side balances on its own and nothing
compares them. The rate lived in metadata as free text nothing read, which left
the number the margin depends on as the number nothing checked.

`ledger.Conversion` takes the rate and the amount sold, and **computes** what
arrives. Two numbers that must agree become one number that cannot disagree.

```go
sale := ledger.Conversion{
    From: "USDT/6", Seller: "treasury:usdt", SoldTo: "external:lp:kraken:USDT",
    To:   "USD/2",  BoughtFrom: "external:lp:kraken:USD", Buyer: "ops:usd",
    Amount: usdt(100_000), Rate: "0.99960",
}
postings, _ := sale.Postings()          // 100,000.000000 USDT out, 99,960.00 USD in
metadata := sale.Metadata(nil)          // the rate, recorded for checking later
```

**The scales are half the arithmetic.** An amount is in minor units, so
`out = in × rate × 10^(toScale − fromScale)`. The factor of 10⁻⁴ between USDT/6
and USD/2 does as much work as the rate does.

**Exact rationals, never floats.** A rate is a decimal quantity and binary
floating point cannot hold most decimals. Fractions truncate rather than round
up, so a conversion can never manufacture a unit that was not there; what is
left over is dust, and dust belongs in an account rather than in the difference
between two numbers.

`fx.Verify` recomputes every transaction that states a rate. `giro verify` runs
it alongside the engine's own checks, contributed at the composition root
rather than built in, so it appears as a sixth line without the engine
learning what a trade is.

Like `VerifyProjection` it catches something no other check would: restate the
rate without touching the money and both sides still conserve, the chain is
still intact, the projection still agrees.

**What it does not do**, and this is the part worth being plain about. It has
no opinion on whether 0.99960 was a *fair* price. That is a pricing question,
it belongs upstream (D1), and the ledger has no way to know. So a rate that is
simply wrong, recorded consistently in both the rate and the amounts, passes
every check here while the trade is nine thousand dollars light. Only comparing
against the venue's own statement finds that, which is reconciliation.

### D40. Property tests get a new seed every run, and print it

Three tests generate random transactions and assert the book survives. All
three were seeded with a constant, so they generated the same two thousand
cases on every run, for ever. They found bugs once and could never find
another, while continuing to look like they were searching.

A property test earns its cost by going looking. Fixing the seed turns it into
a very elaborate unit test.

The seed is now the clock, printed on every run, and `GIRO_TEST_SEED` replays
one:

```
seed 1788499781666073000: replay with GIRO_TEST_SEED=1788499781666073000
```

That keeps the reason fixed seeds are tempting. A failure nobody can reproduce
is not much use at three in the morning, and a random test that prints nothing
is exactly that. Printing the seed unconditionally costs nothing, because `go
test` only shows output for tests that fail or run with `-v`.

`just replay <seed>` runs them against a given one.

### D41. A posting can move whatever is there

Sweeping a client's sub-wallet into the treasury needs an amount the caller
cannot know. Reading the balance and then posting it is two operations with a
gap: the balance grows in between and the sweep is short, or it shrinks and the
commit is refused.

Neither corrupts anything. The overdraw guard sees to that, so the book is
never wrong either way. But "move whatever is there" did not exist as an
operation, so every caller reimplemented it with the same race.

`Posting.UpTo` makes `Amount` a ceiling rather than a figure, and a nil amount
means no ceiling at all:

```json
{ "source": "client:acme:wallet", "destination": "treasury:usd",
  "asset": "USD/2", "upTo": true }
```

**Resolved after the lock and before the balance check.** That is the only
window where the answer is both known and pinned, and it is the whole point:
nothing can change the balance between deciding the amount and moving it. Eight
concurrent sweeps of one account move exactly its balance in total, no more and
no less.

Lock order is untouched, because locks are taken per `(account, asset)` and an
amount does not name a row. The volume deltas do carry amounts, so they are
recomputed once the figure is known.

**The transaction records what moved, never the ceiling.** `upTo` is not echoed
back on a committed transaction: it is resolved by then, and returning it would
suggest the figure is still provisional.

Three cases worth stating, because each could plausibly have gone the other
way.

*Sweeping an empty account commits a zero posting rather than failing.* A job
that errors when there is simply no work makes "nothing to do"
indistinguishable from "broken", and giro permits zero amounts already.

*Two sweeps of one account in one transaction see each other.* The first drains
it and the second finds nothing, which is the same rule every posting follows:
money flows through an account within a transaction and order decides what is
there when each one runs.

*Sweeping an account permitted a negative balance is refused.* `world` is the
whole outside world and a contra account is a running total of a cost. Neither
holds a determinate amount, so "everything it has" is not a number, and picking
one and calling it the balance would be worse than refusing.

### D42. An account can be closed, and must be empty to be

A client offboards and their account carries on working. A payment made by
mistake a year later is as welcome as one made on the first day, and nothing in
the ledger records that the relationship ended.

`CloseAccount` refuses further movement **in both directions**, and requires
the account to hold nothing first.

**The emptiness rule is what stops closure stranding money.** An account closed
with a balance would hold it somewhere nothing accepts postings, and getting it
out would need exactly the thing closure forbids. Requiring it empty removes
that by construction rather than by a special case in the guard. A bank does
the same.

**Both directions, for the same reason.** A closed account holds nothing, so
the only thing an incoming posting could do is give it a balance nobody is
watching.

Money that arrives afterwards is handled by reopening. A wire that bounces back
after the client left is refused, stays in its holding account, and an operator
reopens, pays it out, and closes again: three deliberate acts that leave a
trail, rather than a hole that quietly makes closure mean less than it says.

**On `accounts`, not `accounts_volumes`,** unlike the negative-balance
permission, and the difference is not arbitrary. Permission to go below zero is
a fact about an account in one asset, since a cost line in USD is an ordinary
balance in USDT. Closure is a fact about the relationship, and per asset it
would have a hole: an account closed in `USD/2` would still accept `USDT/6`.

**The closure check is not locked, on purpose.** Closing locks the balance rows
it can see, which stops a commit already in flight and not one starting a
moment later. Refusing that would mean every commit taking a lock on the
account row, and a lock on a row the write path also updates is the shape that
deadlocks — the same reason there is no foreign key to `ledgers` (D14). So the
race is left open deliberately and `VerifyClosedAccounts` looks for it, which
is the same trade as D37.

The guard on `accounts` names the columns that may change, so this column had
to be declared there before it could be written. That is the allow list working
as intended: a new column arrives protected rather than unprotected.

### D43. The balance bound has two sides

`allow_negative` could turn the guard off and never turn it around, and some
accounts need it the other way.

A cost account is a tally of what something has cost. Every loss pushes
`cost:peg_absorption` further negative and nothing pushes it back, so a
positive balance there means a loss was recorded as a gain. **Conservation has
no opinion about which direction is which**, so the book balances, both sides
of the posting are real, and the profit figure is wrong by twice the amount.

Such an account was set `allow_negative`, which means "no rule at all", so
nothing noticed. `allow_positive` is the mirror, and the three states cover
what accounts actually are:

| | Bounded below | Bounded above |
|---|---|---|
| `users:alice` | yes | no |
| `world`, `external:*` | no | no |
| `cost:peg_absorption` | no | yes |

Default true, so nothing changes for anything that existed before it: an
ordinary account is bounded below and unbounded above, which is what it always
was.

Both bounds share the carve out from D37. An operator narrowing a bound on an
account already outside it is stopping the bleeding, and refusing that would
leave the only remedy blocked by the thing it remedies. So the state stays
reachable and `VerifyBalancePermissions` reports it, now from either side and
naming which.

### D44. Going below zero is a permission on a row, not a name

An account that spends money it does not have is the failure a ledger exists to
prevent, so the balance guard refuses it. Two kinds of account have to be
exempt, for different reasons.

A boundary account stands for everything outside the ledger. Value entering has
to come from somewhere, so the outside runs negative by exactly what is inside.
A contra account is entirely internal and is a running total of a cost rather
than a pot of money: it only ever emits, and it cannot run out.

Both are the same shape to the guard, which cannot tell either apart from a
client account about to be drained. So the permission is a column on
`accounts_volumes`, default false, and `world` is created carrying it rather
than being named in the source. There is no account name anywhere in the check.

Per asset, not per account: one account can be a cost line in one asset and an
ordinary balance in another.

It lives on `accounts_volumes` rather than `accounts` because that is the row
the commit path already takes `FOR UPDATE`. The permission is read under the
same lock as the balance it governs, so it needs no second lock and adds no
lock ordering to get wrong.

A naming convention was the alternative and is what most ledgers do. It was
turned down because a typo joins the exempt set silently. `externa1:bank:chase`
is not a rejected address, it is a new account permitted to create money, and
nothing about it looks wrong until a balance exists that should not.

The cost is that a new account needs setting up before first use, and that is
the right way round: forgetting is a refused transaction rather than an
unnoticed one.

### D45. Reconciliation is a layer, and it never writes a posting

The ledger's own checks prove the book is consistent with itself. None of them
can tell you the money is actually in the bank, and that question can only be
answered by asking the bank.

`recon` asks. It stages what a counterparty says happened, pairs those lines
with movements, and reports what it could not pair.

**It writes no postings and changes no balance.** A reconciler able to correct
the book would be a second way for money to move, and the entire value of the
layer is that its opinion is independent of the thing it is checking. A
discrepancy is reported, never resolved.

Providers live outside giro. The moment a ledger ships a Kraken client it has
stopped being a general ledger, so what ships is the `Source` interface and a
worked example in the tests — which is what proves the interface is sufficient
rather than merely plausible.

### D46. Matching is on an exact reference, and refuses to guess

No fuzzy matching, no matching by amount and date, no subset search. The
reasoning is one asymmetry:

> An unmatched line costs somebody five minutes.
> A falsely matched one costs a restatement.

So everything is deterministic and anything ambiguous is left alone rather than
resolved on a balance of probabilities.

Two rules, cheapest first. **One line, one movement**, on a shared reference.
And **one line, several movements** — a consolidated wire — matched only when
the amounts sum to it exactly.

That exactness is the whole discriminator. Several movements under one reference
is either a real batch or an ambiguous reference, and nothing in the reference
itself tells them apart: a real batch adds up to the line it paid, and two
unrelated movements that happen to share a string do not. A set that does not
add up therefore stays unmatched rather than being recorded as a partial match,
which would assert a pairing nothing justifies and drop it out of the queue a
person still has to work.

**Rejected: many-to-many subset-sum.** Combinatorial, and a set that happens to
add up is not evidence it is the right set.

**Rejected: amount and date matching.** Two payments of the same size on the
same day are common, and telling them apart is exactly what a reference is for.

**Rejected: a default amount tolerance.** A bank that is a penny out is telling
you something. Tolerance is opt in, per source.

Direction is checked against boundary accounts, because without it an outbound
wire reconciles against an inbound movement of the same size and reference —
same number, same reference, opposite direction, and a clean looking match that
is completely wrong.

### D47. Lines are matched against movements, not transactions

A statement line is one account, one asset, one amount, one direction. That is a
move.

A transaction can be two of those at once. Selling stablecoin for dollars moves
100,000 of one asset and 99,960 of another in a single transaction, and a line
on the exchange's dollar statement is talking about exactly one of them — "the
transaction" has no single amount to compare it against.

The first version of this linked matches to transactions and was wrong for that
reason.

### D48. The boundary is a prefix, not a predicate

Matching needs to know which accounts face outward, so a line saying "money in"
pairs with a movement that came in. The ledger has no opinion about what an
address means (D3), so the convention is configuration, defaulting to
`external:`.

A prefix rather than a function, and that is a concession worth naming.
Matching runs in the database, so a Go predicate could not take part in it, and
offering one that silently did not apply would be worse than a narrower knob
that works.

### D49. The reconciliation report cannot be cleaned up

The most dangerous thing about reconciliation is that a clean report is easy to
fake, and faking it moves no money.

Delete the records that did not reconcile and the postings are untouched, the
hash chain still verifies, conservation still holds — and the book now
reconciles. **Every other check in this system stays green.**

So the database refuses it. `recon_matches` is append only. `recon_records`
permits `matched_count` and `matched_at` to change and nothing else, so an
amount or a reference cannot be revised to make a line pair. A line naming an
unregistered asset or source is refused at insert, rather than sitting in the
unmatched queue where a configuration error looks like a break.

These hold against raw SQL and against the role the application connects as.

### D50. A position is compared as well as its lines

Matching can only pair the lines a source actually sent. If a source never
mentions a movement at all, every line it did send matched, the report is clean,
and the money is missing.

`CompareBalance` closes that: the counterparty states its own figure for the net
position across the shared edge, and it is compared against the boundary account
standing for it.

A boundary account per counterparty and asset (D33) is what makes this cheap:
our side of the comparison is already a balance rather than a report to be
assembled. A design that leaves boundary accounts unmaterialised — a reasonable
trade, since a busy edge is otherwise a hot row — cannot do this at all, and
that cost is worth knowing before making it.

The sign convention is stated from our side — what has come to us across this
edge — because "what they hold for us" means something different for a chain, an
exchange and a bank, while the former means one thing for all three. The
original wording said the latter, and writing three adapters against it is what
showed it was ambiguous.

---

### D51. Account policy has a command, not an endpoint

Everything in D37–D44 — which accounts may go negative, which may go positive,
which are closed — was reachable only from Go. That was the right instinct and
the wrong stopping point.

The instinct: an endpoint that lets a caller mark its own account unbounded is
not an API, it is a hole in one. The overdraw guard is what stands between a bug
and minted money, and permission to disable it must not travel over the same
wire as the requests it exists to refuse.

The stopping point: a boundary account (D33) has to be permitted a negative
balance before value can cross inward at it, and nothing over HTTP could permit
one. So the reconciliation story D45–D50 tells was unreachable from the service,
and a deployment that ran giro as a server had no path to set up the accounts
reconciliation is about. The gap was found by writing the documentation and
noticing that a claim it made could not be carried out.

`giro account` closes it, alongside `giro migrate` and `giro verify`:

```
giro account show           <ledger> <address>
giro account allow-negative <ledger> <address> <asset>
giro account close          <ledger> <address>
```

**The privilege is not what separates a command from a route.** `giro_app` is
granted `update` on the policy columns and always was, because the commit path
needs the row; the command reaches the same database with the same role. The
channel is what separates them. A command is reached by someone with a shell or
a deployment step, and there is no listener in front of it accepting requests
from anywhere. That is what makes the setting a decision somebody took rather
than one a request could take for them.

Two consequences, both deliberate. Serving giro and operating it are two jobs,
and a deployment needs a path to the second — a task runner, a job, a migration
step. And a boundary account is declared rather than named into being: the
`external:` prefix is convention, and value cannot cross inward at an edge
nobody opened.

`show` reports the bound beside the balance it governs, because "unbounded
below" reads differently on an account at zero and one at minus four hundred
thousand. Narrowing a bound an account already sits outside is permitted — it is
how an operator stops the bleeding, and refusing it would leave the only remedy
blocked by the thing it remedies — so the command says at the time that the
account now breaks its own rule, and that `giro verify` will report it until
something moves.
