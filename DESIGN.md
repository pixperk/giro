# giro — Design

A double-entry financial ledger. Go + Postgres. Modelled on
[formancehq/ledger](https://github.com/formancehq/ledger), reduced to the parts
that make it a ledger rather than a product.

This document explains what the system is and why it is shaped this way. Each
concept gets an explanation, the decision we took, the reasoning, and what we
turned down. If you ever wonder "why is this like this", the answer should be
here. If it isn't, that's a gap worth filling.

**Contents**

- [Part 1 — In brief](#part-1--in-brief)
- [Part 2 — The primitives](#part-2--the-primitives)
- [Part 3 — The write path](#part-3--the-write-path)
- [Part 4 — Concurrency](#part-4--concurrency)
- [Part 5 — Security](#part-5--security)
- [Part 6 — The queries that matter](#part-6--the-queries-that-matter)
- [Part 7 — Storage, API, tenancy](#part-7--storage-api-tenancy)
- [Part 8 — Deliberately out of scope](#part-8--deliberately-out-of-scope)
- [Part 9 — The invariants](#part-9--the-invariants)

---

# Part 1 — In brief

A ledger records **movements of value between accounts**, and its only real job
is to never lose track of any of it.

The atomic unit is a **posting**: an amount of an asset moving from one account
to another.

```json
{ "source": "world", "destination": "users:alice", "asset": "USD/2", "amount": 10000 }
```

A **transaction** is an ordered list of postings applied atomically — all of
them commit, or none do. That is the only write operation in the system.

Four ideas carry everything else:

**1. Money is conserved.** Value enters from a special account called `world`
and leaves to it. `world` is the only account permitted a negative balance. For
any asset, the balances of every account summed together always equal exactly
zero. This is checkable with one SQL query and it is the master invariant of
the whole system.

**2. We store volumes, not balances.** Each account holds two counters per
asset — `input` (everything that ever arrived) and `output` (everything that
ever left). Both only increase. Balance is `input - output`, computed at read
time and stored nowhere.

**3. The log is the source of truth.** Every mutation appends an entry to an
append-only, SHA-256 hash-chained log. The `transactions` and
`accounts_volumes` tables are a *projection* of that log — a cache we keep
because replaying from zero on every read would be absurd. If they ever
disagree with the log, the log is right.

**4. Nothing is ever mutated or deleted.** A mistake isn't corrected by editing
a row; it's corrected by appending a compensating transaction. Metadata is
versioned rather than overwritten. The history is the product.

---

# Part 2 — The primitives

## 2.1 Assets

An asset is **what kind of thing is moving**. The amount says how much; the
asset says how much *of what*.

```
{ source: "world", destination: "users:alice", asset: "USD/2", amount: 10000 }
                                                       ^^^^^         ^^^^^
                                                       what          how much
```

`10000` alone is meaningless. `10000` of `USD/2` is **$100.00**.

These are all equally valid and the engine treats them identically:

| Asset | Meaning |
|---|---|
| `USD/2` | US dollars, 2 decimals — `10000` = $100.00 |
| `BTC/8` | Bitcoin, 8 decimals — `100000000` = 1 BTC |
| `POINTS` | Loyalty points, no decimals |
| `SEATS` | Licence seats |

**Decision.** Scale is part of the asset identifier. There is no `scale` or
`currency_decimals` column anywhere in the system. `USD` and `USD/2` are
different assets and cannot be mixed. Pattern: `[A-Z][A-Z0-9_]*(/\d{1,6})?`.

**Why.** A scale stored *beside* an amount is a scale that can be lost —
dropped in a serialisation round trip, mismatched between two rows, defaulted
to zero by a migration. Then `10000` silently becomes $10,000 instead of
$100.00. Fusing scale to identity means an amount is uninterpretable without
it, because it is right there in the same record.

It also keeps the core indifferent to what an asset *is*. The moment the engine
knows what a dollar means, it has opinions, and those opinions become the thing
you work around when you want to track something that isn't money.

**Assets never mix.** Every volume row is keyed by `(account, asset)`. Alice
with three assets has three independent balances that are never added,
compared, or netted. Conservation is checked **per asset**.

Which means there is no exchange rate anywhere in this system. Converting $100
to €92 is one transaction with two postings:

```
users:alice → treasury     10000  USD/2
treasury → users:alice      9200  EUR/2
```

The ledger records that both happened atomically. It has no idea they are
related and no opinion on whether 9200 was fair. That is a pricing question and
it belongs upstream.

**Rejected: floats** (`0.1 + 0.2`, and precision loss is silent).
**Rejected: a currency table** (a join on the hot path that buys nothing the
string doesn't already give you).

## 2.2 Addresses

An address is a name, with one rule: segments joined by colons.

```
world
users:alice
users:alice:wallet
fees:platform
liabilities:pending_payouts
```

Each segment matches `[a-zA-Z0-9_-]+`. One segment is fine. Ten is fine.

**Decision.** Colon-segmented paths, validated per segment. Accounts are not
pre-registered — an account exists because a posting referenced it.

**Why the segmentation.** It encodes a hierarchy the ledger can query without
knowing what the hierarchy *means*. "All user wallets" is `users:*`. That's a
product question answered by a storage-layer index, with no schema change and
no table of account types. Restructure your naming later and the ledger doesn't
care, because it has no concepts to migrate.

**Why no registration.** The alternative is an account-creation step before
every first payment: an extra round trip, an extra failure mode, an extra state
to reconcile. An account that has never been touched and one that doesn't exist
answer every question identically — balance zero — so they are the same thing.
This removes "account not found" on a first payment and the create-then-fund
race entirely.

**Implementation.** Store the exploded segments as a JSONB array beside the
string form and GIN-index it. Prefix queries then hit an index instead of
`LIKE 'users:%'` scanning the table.

**Rejected: opaque UUIDs with a separate metadata table.** More normalised, but
it forces a join for every question a human would ask, and moves the hierarchy
into application code where the ledger can't help.

Exactly one address is special-cased by name.

## 2.3 The `world` account

Here is the problem it solves.

Every posting needs two sides — that's what makes imbalance unrepresentable.
Now write down the first thing that ever happens in your ledger: **Alice
deposits $100.**

```
{ source: ???, destination: "users:alice", amount: 10000 }
```

What goes in `source`? Every account is empty; the ledger is brand new. And you
cannot leave it blank, because a one-sided posting is exactly the thing the
design makes impossible.

You are stuck — not because of a missing feature, but because your ledger is a
**closed system** and the money is coming from **outside** it.

### `world` is the outside

`world` means *everything not tracked in this ledger*. Alice's bank. Stripe. A
card network. Someone's pocket. The ledger doesn't know which — only that value
crossed the boundary.

```
world → users:alice      10000     $100 entered the ledger
users:alice → world       4000     $40 left the ledger
```

Deposits come **from** `world`. Withdrawals go **to** `world`. Internal
transfers never touch it. It is not a place money sits — it is the doorway.

### The exemption, and why it is forced

`world` may go negative. Nothing else may.

That is not a convenience — it is arithmetic. `world` starts at zero like
everything else, and the first deposit sends $100 out of it. If `world` had to
stay non-negative, your first transaction would fail and so would every one
after it. You could never start.

So exactly one account gets the exemption, and it's the one representing "not
here". Everything *inside* obeys the rule that you cannot spend what you do not
have.

### Its negative balance is a measurement

```
world           −15000
users:alice       7000
users:bob         5000
fees:platform     3000
                 ──────
                      0    ← always
```

`world` at −$150 reads as: **$150 has entered this ledger and not yet left.**
It is not a debt `world` owes. It is a mirror of everything held inside, sign
flipped.

That number is useful. For a wallet product, `−world` is total customer
liability — what you would owe if everyone withdrew at once. It should match
your actual bank balance, and when it doesn't you have found something.

**The analogy that holds up.** A poker table. Someone buys in for $100 and
chips come from the **rack**. The rack isn't a player and has no stack; it's
where chips come from and return to. Count it and it goes negative by exactly
the number of chips in play. `world` is the rack. The −$150 is not the rack's
poverty; it is your count of chips on the table.

### Two things it is not

**Not a loophole.** Every crossing is a normal posting, logged and hash-chained
like any other. You can query exactly how much entered, when, and to whom.
`world` doesn't skip the audit trail — it *is* the audit trail for external
flow.

**Not real money.** `world → alice` says value crossed *this ledger's*
boundary. It does not say cash arrived in a bank account. Confirming that is
reconciliation, and being able to compare `−world` against a bank statement is
precisely the point.

**Rejected: an `is_issuance` flag on transactions.** Same effect, but it
creates a second write path that bypasses the balance check — and the dangerous
operation is exactly the one you least want a second version of. Modelling
issuance as an ordinary posting keeps a single write path for everything.

**Practical note.** The exemption is by literal name: `account != "world"`, one
string comparison. If you later want to distinguish *where* external money came
from, don't add more exempt accounts — that widens the hole. Use metadata
(`{"rail": "stripe"}`) or route through a funded internal account, so the
exemption stays exactly one account wide.

## 2.4 Postings

A posting is one movement of value — the smallest thing this ledger records.

```go
type Posting struct {
    Source      string   // where it leaves
    Destination string   // where it arrives
    Asset       string   // what kind of thing
    Amount      *big.Int // how much — always positive
}
```

Read one aloud and it is a complete sentence:

```
{ source: "users:alice", destination: "users:bob", asset: "USD/2", amount: 3000 }
```

> *$30.00 moved from Alice to Bob.*

Nothing is missing. Not who lost it, not who gained it, not the denomination.

### Both sides in one record

Traditional bookkeeping writes that as **two** rows:

```
DEBIT   bob     30.00
CREDIT  alice   30.00
```

Two rows that must agree. If they ever don't — a crash between writes, a bad
migration, a bug in one path — money came from nowhere, and you find out at
month-end when the trial balance won't close.

**Decision.** Name both sides in a single record, amount always positive, no
debit/credit flag.

**Why.** An unbalanced entry stops being an error you *detect* and becomes a
thing you **cannot write down**. There is no arrangement of those four fields
that creates or destroys value. The books balance not because a process checks
them but because the shape of the data cannot express imbalance.

That is the trade the whole model is built on: push correctness into the
structure so runtime checks become unnecessary rather than merely reliable.

The positive-amount rule follows. Direction is fully determined by which field
an account sits in, so a sign would be a *second* way of saying the same thing
— and two ways of saying one thing is two ways for them to disagree.
`Validate()` rejects negatives outright. `-30 from Bob to Alice` has exactly
one legal spelling: `30 from Alice to Bob`.

### What it does to the ledger

A posting touches two rows and bumps one counter on each:

```
destination.input  += amount
source.output      += amount
```

That is the entire write. Watch $30 move:

```
                  input    output   balance
alice  before     10000        0     10000     ($100.00)
bob    before         0        0         0

  { alice → bob, USD/2, 3000 }

alice  after      10000     3000      7000     ($70.00)
bob    after       3000        0      3000     ($30.00)
```

The same `3000` added twice, once to each side. Whatever leaves one place
arrives somewhere else, so the sum of all balances does not move. Do it a
million times and it still does not move. That is conservation, and it falls
out of the shape of a posting rather than being enforced on top of it.

**Rejected: signed amounts, one account per row** — the conventional journal
shape. It permits an entry that doesn't balance, so every read path has to be
defensive about data every write path promised was fine. It also makes
"reverse this" a matter of flipping signs correctly across N rows instead of
swapping two fields.

## 2.5 Transactions

A single posting is rarely the whole story. Alice pays Bob $30 and the platform
takes $2.50:

```json
[
  { "source": "users:alice", "destination": "users:bob",    "asset": "USD/2", "amount": 2750 },
  { "source": "users:alice", "destination": "fees:platform","asset": "USD/2", "amount":  250 }
]
```

One **transaction**: both commit or neither does. There is no state of the
world where Alice paid the fee but Bob was never paid.

**Postings are ordered, and the order is real.** Money can flow *through* an
account:

```
world    → treasury    10000
treasury → alice       10000
```

Both run in sequence inside one atomic commit. The balance check runs on the
final state, so `treasury` netting to zero is fine — but run the second posting
first and `treasury` is spending money it hasn't received.

### Which is why reversing is subtle

To reverse a transaction, swap source and destination on every posting **and
reverse their order**:

```
original:   A → B  100     then    B → C  100
reversed:   C → B  100     then    B → A  100        ✓
wrong:      B → A  100     then    C → B  100        ✗
```

The wrong version pays A back out of B before C has returned anything, so B
briefly goes negative and the reversal fails a check it should have passed.
Same postings, different order, different outcome.

This is easy to get wrong and easy to miss, because with a single posting both
versions are identical — and a single posting is what you'll write your first
test with. **Write the three-posting chain test.**

## 2.6 Volumes

```sql
create table accounts_volumes (
  ledger  varchar not null,
  address varchar not null,
  asset   varchar not null,
  input   numeric not null default 0,
  output  numeric not null default 0,
  primary key (ledger, address, asset)
);
```

This is the entire mutable state of the ledger. Everything else — transactions,
moves, logs — is append-only history. This one table is the only place a row is
ever updated, which is why all the locking discipline concentrates here.

```go
type Volumes struct { Input, Output *big.Int }   // both only ever increase
func (v Volumes) Balance() *big.Int { return new(big.Int).Sub(v.Input, v.Output) }
```

**Decision.** Store two monotonically increasing counters per `(account,
asset)`. Derive balance on read. Never persist a balance.

### Why — three reasons, increasing in weight

**Gross flow is information a balance destroys.** An account that has settled
$4M lifetime and currently holds nothing, and an account that has never been
used, both show a balance of zero. They are not remotely the same account.
Volumes distinguish them for free, and fraud rules, fee tiers, and
reconciliation all care about throughput rather than position.

**Additive updates compose; absolute writes don't.** Every update is
`input = input + $1`, a **relative** operation performed by the database. Two
concurrent credits apply cleanly in either order and reach the same result.
With a balance column you write `balance = 120`, an **absolute** operation
computed from a value you read earlier, and whichever writer commits second
silently discards the first. That is money vanishing with no error anywhere.

We still take a row lock (Part 4) — but the lock makes the *balance check*
correct, while relative updates make the *write* correct, independently. Two
defences with different failure modes: without the lock you get a wrongly
accepted transaction; without relative updates you get corrupted volumes. One
is an error, the other is a lie that persists forever.

**Historical balance becomes an index seek.** Each `moves` row carries a frozen
snapshot of the account's volumes at that instant (§2.7), so "what was Alice's
balance on 3 March" is one indexed lookup rather than a replay of her entire
history.

### Volumes are computed per transaction, not per posting

This part is not obvious. A transaction with four postings does **not** produce
four volume updates. It produces one per distinct `(account, asset)`, with the
postings aggregated into it.

A payout run:

```
1.  world     → treasury        10000
2.  treasury  → users:alice      6000
3.  treasury  → users:bob        3000
4.  treasury  → fees:platform    1000
```

`treasury` appears in all four — receiving in one, sending in three. It gets
**one** update:

| account | input | output | balance change |
|---|---|---|---|
| `fees:platform` | +1000 | 0 | +1000 |
| `treasury` | **+10000** | **+10000** | **0** |
| `users:alice` | +6000 | 0 | +6000 |
| `users:bob` | +3000 | 0 | +3000 |
| `world` | 0 | +10000 | −10000 |

Note `treasury`: net zero, but both counters jump by 10000. It genuinely passed
$100 through itself, and the row remembers even though the balance says nothing
happened.

The update list is then **sorted by (account, asset)** — and note where that
happens: in the domain layer, in `VolumeUpdates()`, before storage sees
anything. The lock order is a deterministic property of the transaction, not
something the SQL layer improvises. See §4.4.

### The self-posting case

`alice → alice, 500` adds **500 to both** of Alice's counters. Balance
unchanged, gross flow up by 500 each way. Conservation holds, because the same
amount was added to the same account's two counters and cancels in
`sum(input) - sum(output)`.

The plumbing detail that matters: the account enters the update set **once** (as
source), and the accumulation loop then checks `account == posting.Source` and
`account == posting.Destination` as two independent `if`s, not an if/else. Both
fire. Write it as if/else and a self-posting silently increments only one side,
breaking conservation on a transaction nobody thought to test.

### Monotonic means diffs are meaningful

Because neither counter ever decreases, any two observations subtract cleanly:

```
volumes at T2  −  volumes at T1  =  exactly what flowed between T1 and T2
```

Gross in, gross out, both directions, no history scan. Balances cannot do this
— a balance that reads 100 at both ends could mean nothing happened or that
$10M churned through. This property is what makes the frozen snapshots work.

### What revert does to volumes

Alice pays Bob $30, then it's reverted:

```
                    alice                    bob
after payment    (10000, 3000) = 7000     (3000,    0) = 3000
after revert     (13000, 3000) = 10000    (3000, 3000) = 0
```

Balances return exactly where they started, and **nothing decreased** — the
reversal added 3000 to Alice's *input* and 3000 to Bob's *output*. The rows now
say: $30 went to Bob, and $30 came back.

A balance column would show Alice at 10000 and Bob at 0, indistinguishable from
a payment that never happened. Volumes preserve the fact that it happened and
was undone — the same reason the original transaction isn't deleted.

## 2.7 Moves, and the two snapshots

### Why store a snapshot at all

Question: **what was Alice's balance on 3 March?**

The naive answer replays her history — every move she's ever been in, add
inputs, subtract outputs, stop at the date. Correct, and it gets slower every
day she uses the product. For 50,000 moves you scan 50,000 rows to answer one
question.

So instead: **every time we touch Alice, write down her balance right then.**

```
move 1:  +100  →  note: "Alice is now at 100"
move 2:  +50   →  note: "Alice is now at 150"
move 3:  −30   →  note: "Alice is now at 120"
```

Now "balance on 3 March" is: find the last note before 3 March, read it. One
indexed lookup, same cost whether she has 3 moves or 3 million.

That note is **post-commit volumes**. Decode the name literally:

> **post** (after) **commit** (this move was written) **volumes** (her
> input/output pair)

A sticky note on every move saying "here is where the account stood immediately
after this". `pcv_input` and `pcv_output` are two columns on `moves` holding
it.

**Decision.** Write one `moves` row per account per posting — two rows per
posting — each carrying that frozen snapshot.

**Why a separate table.** `transactions` is organised by transaction; almost
every question a human asks is organised by *account*. "Alice's statement",
"Alice's balance last March", "everything that touched the fee account this
quarter" are all account-shaped, and answering them from JSONB postings means
scanning. `moves` is the same data indexed the way it is actually read. The
snapshot is denormalised on purpose: derivable, stored anyway, and safe
precisely because nothing is ever mutated.

**If transactions always arrived in the order they happened, we would be done
here.** One set of notes, in order. Everything below exists because they don't.

## 2.8 Two clocks

A settlement file arrives Tuesday describing Friday's movements. A card capture
settles days after authorisation. News arrives late — that is what dealing with
the outside world is like.

**Decision.** Every transaction carries `timestamp` (when it happened
economically) and `inserted_at` (when this database found out). They may
disagree, and transactions may be inserted with a past effective date.

**Why we can't avoid it.** Collapsing the two clocks forces a choice between a
ledger that misreports *when* things happened and one that refuses to record
them at all.

### What it costs

Alice, in the order we *learned* about things:

```
     effective    learned      amount    running balance
m1   Mar 1        Mar 1        +100      100
m2   Mar 3        Mar 3        +50       150
m3   Mar 5        Mar 5        −30       120
```

Three moves, three notes: 100, 150, 120. Both clocks agree so far.

Now on **8 March a settlement file arrives** about a $50 payment that actually
happened on **2 March**:

```
m4   Mar 2        Mar 8        +50       ???
```

By insertion order it is fourth: 120 + 50 = **170**.
By effective order it belongs *second*, between m1 and m2.

Re-sort by effective date and recompute:

```
     effective    amount    running balance (effective order)
m1   Mar 1        +100      100
m4   Mar 2        +50       150
m2   Mar 3        +50       200     ← its note said 150
m3   Mar 5        −30       170     ← its note said 120
```

m2's and m3's notes are now wrong for effective-date purposes. They were
computed before we knew about m4.

### So we keep two sets of notes

```
     effective  learned   amount    pcv      pcev
m1   Mar 1      Mar 1     +100      100      100
m2   Mar 3      Mar 3     +50       150      200
m3   Mar 5      Mar 5     −30       120      170
m4   Mar 2      Mar 8     +50       170      150
```

| | ordered by | mutable? | answers |
|---|---|---|---|
| **`pcv`** post-commit volumes | insertion | **never** — written once | "what did the ledger believe at the time?" |
| **`pcev`** post-commit *effective* volumes | effective date | **rewritten** on backdating | "what was actually true on 3 March?" |

### Why keep the immutable one at all

Because these answer genuinely different questions and both get asked.

Alice phones support on **6 March**. The agent looks and says **"$120"** — the
honest, correct answer given everything known at that moment. Then the
settlement file lands, and now the system says she had $170 on 6 March.

| Question | Notes | Answer |
|---|---|---|
| "What did we tell Alice on 6 March, and was it right?" | `pcv` | **$120** — yes, correct given what we knew |
| "How much did Alice economically have on 6 March?" | `pcev` | **$170** |

Keep only `pcev` and the first question becomes unanswerable: the record would
claim the agent said $120 when the balance was $170, which reads as the agent
being wrong. `pcv` is what defends you — *the system was correct, the
information arrived late*. That is the difference between a reconciliation
discrepancy and a bug, and only immutable notes can tell them apart.

`pcv` is the **audit** view. `pcev` is the **reporting** view. Neither is more
real.

### Maintaining `pcev`

Two strategies. **Compute on read** — sum moves up to the requested date;
correct, simple, slow on hot accounts. **Fix up on write** — walk forward and
apply the delta to every later move; what Formance's PL/pgSQL triggers do,
makes reads a single seek.

**We start with compute-on-read, behind an interface.** Correct first, fast
when a benchmark hurts. This is the most complex area of the system and the one
most likely to harbour subtle bugs; several of Formance's 54 migrations exist
to fix exactly this.

### Two traps, stated loudly

**The balance check uses the current balance, never the effective-date
balance.** It is tempting to check a backdated transaction against what the
balance was on its effective date. Don't. The money either exists now or it
does not; the effective-date view is a reporting concern and must never gate a
write. Getting this backwards lets a backdated transaction spend money that has
since been spent.

**An effective-date question must read effective-date notes.** Filtering by
`effective_date` while reading `pcv` returns wrong answers the moment any
backdated transaction exists — and passes every test until then, because the
two are identical until the first backdate. See §6.4.

### One more subtlety

A transaction's `pc_volumes` field holds the *final* per-account state after
the whole transaction, while each `moves` row holds the state after *that
specific move*. For `treasury` in the §2.6 payout, the transaction records
`(10000, 10000)` but its four moves record four different intermediate
snapshots. Formance computes these by walking the postings in reverse from the
final state — cheaper than replaying forward, and why `CommitTransaction`
reverses the slice before building moves.

---

# Part 3 — The write path

## 3.1 The commit sequence

One Postgres transaction, in exactly this order:

```
1. collect every distinct (address, asset) the postings touch — both sides
2. SORT that set by address, then asset
3. INSERT zero-volume rows ON CONFLICT DO NOTHING,
   then SELECT ... FOR UPDATE the same set, ORDER BY address, asset
4. compute resulting balances in Go; reject if any non-world account goes negative
5. UPDATE ... SET input = input + $n / output = output + $n   (relative!)
6. allocate tx id: UPDATE ledgers SET last_tx_id = last_tx_id + 1 ... RETURNING
7. INSERT the transaction and its moves
8. append the log entry; COMMIT
```

Steps 2–5 are the heart of the system and are explained in Part 4.

## 3.2 The append-only, hash-chained log

**Decision.** Every mutation appends one row to `logs`. Each row's `hash` is
`SHA256(previous.hash || canonical_json(entry))`. Five entry types cover the
whole system: `NEW_TRANSACTION`, `REVERTED_TRANSACTION`, `SET_METADATA`,
`DELETE_METADATA`, `INSERTED_SCHEMA`.

**Why — two distinct properties.**

*Tamper evidence.* Editing or removing any historical entry invalidates every
hash after it. You cannot quietly rewrite the past; you can only append. For a
system whose entire purpose is being believed about money, that is the point.

*Rebuildability.* Because the log is complete and ordered, `transactions` and
`accounts_volumes` can be reconstructed from it. That reframes them as a
**projection** — a cache maintained for read performance, not the truth. It
also gives a clean seam for shipping the ledger elsewhere: an analytics replica
is a consumer of the log, not a second writer.

**The cost, stated plainly.** Hash-chaining is a serialization point. Each
write must read the previous hash, so writes *to a single ledger* are strictly
serial. We accept this: the `ledgers` row lock that allocates transaction ids
(step 6) doubles as the chain guard, so it costs nothing extra structurally.
Different ledgers still write concurrently. Formance hit this ceiling in
production and moved hashing to an async background procedure. We are not
building that now, and we should not be surprised when we need it.

**Canonical encoding is forever.** The moment a hash is written, that entry's
exact byte encoding is frozen. Change field order, add a field, alter how a
timestamp formats, and every subsequent verification fails. Write the encoder
deliberately, with a test pinning known input to a known hash.

## 3.3 Idempotency: a key *and* a hash

**Decision.** Two columns. `idempotency_key` comes from the client.
`idempotency_hash` is SHA-256 over the request inputs. On a replayed key,
compare hashes: identical → return the original result and create nothing;
different → error.

**Why.** A network timeout after the server committed is indistinguishable,
client-side, from a request that never arrived. So clients retry, which means
**every write endpoint will be called twice**. That is not an edge case.

The key alone gives at-most-once delivery — the easy half, and the one everyone
implements. The **hash** catches the failure that actually hurts: a client
reusing a key for a *different* payment, from a buggy retry wrapper or a UUID
generated once at process start. With key-only idempotency the ledger returns
payment #1's success and payment #2 never happens: no error, no record,
discovered weeks later. The hash converts a silent wrong answer into a loud
one, which is the only trade in this document that is unambiguously free.

## 3.4 Nothing is mutated: revert and metadata

**Decision.** A transaction is never edited or deleted. Reverting stamps
`reverted_at` on the original and commits a **new** transaction holding the
reversed postings, with metadata pointing back. Metadata lives in separate
tables keyed by `(target, revision)`; the current value is the highest
revision, and deleting a key writes a new revision without it.

**Why.** The history *is* the product. An auditor's question is never "what is
the balance" — they can see that — it is "how did it come to be this". Editing
a row answers that with a lie by omission. Appending a compensating entry
answers it truthfully: the mistake happened, then it was corrected, and both
facts are visible with their timestamps.

It also keeps the write path singular. A revert is an ordinary transaction
going through the same locking, checks, and log append. No second path, no
`UPDATE` on the hot table, nothing that can corrupt volumes.

**The check that must be there.** Before committing the reversal, verify no
account ends negative. If Alice was paid and has already spent it, the reversal
must **fail** — the money is genuinely not there. Provide a `force` flag for an
operator who means it, and make them ask.

**The race to close.** Set `reverted_at` inside the same locked transaction and
treat "already reverted" as a hard error. Otherwise two concurrent revert
requests both pass the check and you refund twice.

---

# Part 4 — Concurrency

## 4.1 The race we are defending against

Alice has $100. Two requests arrive at the same instant, each moving $100 out.

```
time   request A                      request B                    alice
────────────────────────────────────────────────────────────────────────
 1     read balance → 100                                          100
 2                                    read balance → 100           100
 3     check 100 >= 100  ✓                                         100
 4                                    check 100 >= 100  ✓          100
 5     write output += 100                                         0
 6                                    write output += 100          −100
```

Both checks passed, because both read *before* either wrote. Alice ends at
**−$100**, having spent the same money twice, and no error was raised anywhere
— every individual step did exactly what it was told.

This is **check-then-act**, the fundamental hazard in every ledger ever built.
Everything below closes it.

## 4.2 Defence 1 — the lock

`SELECT ... FOR UPDATE` takes an exclusive row lock:

```
time   request A                      request B                    alice
────────────────────────────────────────────────────────────────────────
 1     LOCK alice ✓                                                100
 2     read → 100                     LOCK alice … blocked         100
 3     check ✓                        ⏳                            100
 4     output += 100                  ⏳                            0
 5     COMMIT (lock released)         ⏳                            0
 6                                    LOCK alice ✓                 0
 7                                    read → 0                     0
 8                                    check 0 >= 100  ✗            0
 9                                    REJECT: insufficient funds   0
```

**The critical detail:** this works at `READ COMMITTED`, Postgres's default.
Normally a statement sees a snapshot from when it started, so you would expect
B at step 7 to see the stale 100. But when `FOR UPDATE` blocks and then
acquires, Postgres **re-reads the row at the moment the lock is granted**. You
get the fresh value, not your snapshot's. That is the entire reason this
pattern is safe without `SERIALIZABLE`.

### Why not SERIALIZABLE

| | `READ COMMITTED` + `FOR UPDATE` | `SERIALIZABLE` |
|---|---|---|
| Correctness here | Explicit — you can point at the lock | Automatic — engine detects conflicts |
| Under contention | Blocks, then proceeds | **Aborts** with `40001`, redo all work |
| Cost of a conflict | Waiting | Wasted work + retry |
| Failure mode | Slow | Livelock on hot rows |

`SERIALIZABLE` is optimistic underneath: it lets transactions run and aborts one
when it detects a conflict. On a hot row like `world` — touched by most
transactions — that means constant aborts, each discarding completed work.
Locking makes writers queue instead: slower per request, and it finishes.

We take the explicit lock, and handle `40001` in the retry loop anyway since
Postgres can raise it from other causes.

## 4.3 Defence 2 — relative updates

The lock makes the *check* correct. Relative updates make the *write* correct,
independently.

```sql
update accounts_volumes set output = output + 100   -- ✓ database does the arithmetic
update accounts_volumes set output = 100            -- ✗ computed from a stale read
```

With the absolute form, a value read at time 1 becomes a value written at time
5; anything that changed in between is silently overwritten — **lost update**,
money gone, no error.

Why bother if the lock already prevents this? **Because defences fail.** Miss an
account in the lock set, add a path that forgets to lock, hit a bug. With
relative updates the worst case is an incorrect balance check — a transaction
wrongly accepted or rejected. With absolute writes the worst case is corrupted
volumes. One is an error; the other is a lie that persists forever.

## 4.4 Defence 3 — sorted lock order

A transaction touching multiple accounts takes multiple locks, one at a time.

```
request A:  alice → bob      request B:  bob → alice
```

Unsorted, each locking in posting order:

```
time   request A              request B
──────────────────────────────────────────────
 1     LOCK alice ✓
 2                            LOCK bob ✓
 3     LOCK bob … blocked
 4                            LOCK alice … blocked
 5     ⏳ A waits for B                    ⏳ B waits for A
```

**Deadlock.** Postgres detects the cycle after `deadlock_timeout` (default 1s)
and kills one with `40P01`.

Sort both lock sets by address first — `alice` before `bob`, for everyone,
always:

```
time   request A              request B
──────────────────────────────────────────────
 1     LOCK alice ✓           LOCK alice … blocked
 2     LOCK bob ✓             ⏳
 3     … work …               ⏳
 4     COMMIT                 LOCK alice ✓
 5                            LOCK bob ✓
```

No cycle is possible. Deadlock **requires** two parties acquiring shared
resources in opposite orders; a globally consistent order removes the
precondition rather than reducing the odds. This is the difference between "we
rarely deadlock" and "we cannot deadlock this way".

Hence the sort at the end of `VolumeUpdates()`, and `ORDER BY address, asset`
inside the `SELECT` too — the planner is not obliged to lock in the order you
listed rows.

## 4.5 Defence 4 — the zero-row insert

`bob` has never been touched, so no row exists. `SELECT ... FOR UPDATE` on a
non-existent row locks **nothing** — it doesn't error, it returns zero rows.

```
time   request A                      request B
─────────────────────────────────────────────────────────────
 1     SELECT bob FOR UPDATE → ∅      SELECT bob FOR UPDATE → ∅
 2     (no lock held)                 (no lock held)
 3     INSERT bob (0,0)               INSERT bob (0,0)
 4     ...                            ✗ duplicate key, or worse
```

You cannot lock what does not exist. So make it exist, in the same statement:

```sql
with ins as (
  insert into accounts_volumes (ledger, address, asset, input, output)
  values ($1, $2, $3, 0, 0)
  on conflict do nothing
)
select address, asset, input, output from accounts_volumes
 where (ledger, address, asset) in (...)
 order by address, asset
   for update;
```

The CTE materialises `(0, 0)` rows for anything missing; `on conflict do
nothing` makes it harmless if they exist. Then `FOR UPDATE` has something real
to grab. If the outer transaction rolls back, the zero rows roll back with it —
free on the failure path.

This is a **phantom** in textbook terms: a row appearing between your read and
your write. `SERIALIZABLE` would catch it; at `READ COMMITTED` you handle it by
refusing to have phantoms.

## 4.6 Defence 5 — retry anyway

Consistent ordering removes *our* deadlocks. Postgres can still deadlock
through index pages, foreign keys, and autovacuum interactions we don't
control.

```go
for attempt := 0; attempt < 10; attempt++ {
    err := commit(ctx)
    if isRetryable(err) {   // 40P01 deadlock, 40001 serialization failure
        backoff(attempt)
        continue
    }
    return err
}
```

**Retry the whole transaction, from step 1.** Not the failed statement. After
an abort every value read is untrustworthy, including the balances checked.
Restart means re-lock, re-read, re-check.

**Cap it.** Formance retries unbounded. A cap of ~10 with backoff turns a
pathological case into a visible error rather than a request that hangs forever
holding a connection.

Retryable: `40P01`, `40001`. **Not** retryable: insufficient funds, validation
errors, idempotency conflicts — retrying a business rejection just fails ten
times slower.

## 4.7 What still serialises

| Bottleneck | Scope | Escape hatch |
|---|---|---|
| Hash chain — each log reads the previous hash | **All writes to one ledger** | Async hashing (Formance did this) |
| `ledgers` row lock for id allocation | All writes to one ledger | Same lock; free given the above |
| Hot account rows (`world`, treasury, fees) | Transactions touching that account | Shard: `fees:platform:00…15`, sum the subtree |

Writes to *different* ledgers are fully concurrent. Within one ledger you are
serial. At MVP scale that is hundreds of transactions per second, which is
plenty — just know it is a wall and not a slope.

The sharding hatch is worth recognising the symptom for: lock wait times
climbing on a handful of addresses while everything else is fine. The prefix
index (§2.2) makes summing a sharded subtree cheap.

---

# Part 5 — Security

The threat is not someone stealing data. It is someone **changing what the
ledger believes about money**, or the ledger being wrong on its own.

## 5.1 What is explicitly not the ledger's job

This matters as much as what is. The ledger executes movements; it does not
decide whether they are allowed.

- **Authentication and authorisation.** Whether *this user* may move funds
  *from that account* is an upstream decision. The ledger sees a valid posting
  and applies it.
- **Fraud, AML, sanctions, limits.** Policy. Upstream.
- **Exchange rates.** It records two postings and has no opinion on whether
  9200 EUR was fair for 10000 USD.
- **Whether real money moved.** `world → alice` says value crossed *this
  ledger's* boundary. Confirming cash landed is reconciliation, against a bank
  statement.

The posture follows: **the ledger is an internal service.** It sits behind your
API layer, on a private network, and trusts its caller. Its job is to be
*correct*, not *guarded* — it earns trust by making incorrect states
unrepresentable rather than by checking credentials.

Which is also why the API must never be exposed to end users directly. Anyone
who can reach it can post `world → themselves`.

Locally we run Postgres with trust auth on `127.0.0.1`, which is fine for
exactly this reason and must never leave the laptop.

## 5.2 The tenant boundary is the biggest practical risk

Every table has a `ledger` column. That column is a **security boundary**, and
it is enforced by you remembering to write `where ledger = $1`. Forget it once,
in one query, and you have leaked or corrupted another tenant's money.

This is the most likely serious bug in the system, because it fails silently —
the query returns *more* rows, not an error.

**Don't rely on discipline. Structure it away.** Never let a raw connection
reach a query builder; wrap it so the scope is baked in at construction:

```go
type Store struct { db *pgxpool.Pool; ledger string }

func (s *Store) accounts() *Query { return s.q("accounts").Where("ledger = ?", s.ledger) }
```

Every query starts from a scoped builder. There is no path that constructs one
without the predicate, because the unscoped constructor isn't exported.

**Then verify it.** Formance has `store_scoped_select_test.go` purely to catch
this. Write yours: create two ledgers, put data in both, run every read path
against ledger A, assert nothing from B appears. Add a case every time you add
an endpoint. Postgres Row-Level Security is the defence-in-depth version if you
want it later.

## 5.3 Input validation

| Input | Attack | Defence |
|---|---|---|
| **Negative amount** | `-100` from A to B is a *withdrawal from B*, bypassing B's balance check entirely | `Validate()` rejects `< 0`. The most important line in the codebase. |
| **Huge amount** | `numeric` is unbounded; a 10,000-digit amount is valid and slow | Cap digits at parse time (~40 is generous for money) |
| **Malformed address** | `"world "` with a trailing space isn't `world`, but reads identically in logs | Strict regex, no trimming, no normalisation |
| **Address spoofing** | Client posts `source: "world"` and mints money | Authorisation, upstream (§5.1) |
| **JSON float coercion** | `10000000000000000000` through `float64` silently becomes `1e19` | `json.Decoder.UseNumber()`. Corrupts value with no error — the worst kind of bug. |
| **Metadata** | Unbounded keys/values, injection into downstream consumers | Cap size and count; opaque to the ledger, not to what reads it |
| **Cursor tampering** | Base64 cursor decoded and trusted | Cursors carry an opaque position only; validate after decode |

SQL injection is handled by parameterised queries (`$1`) throughout. The one
place to watch is any dynamically built `IN` clause in the locking query: build
placeholders, never string-concatenate values.

## 5.4 What the hash chain actually proves

Be precise, because it is easy to overclaim.

**It proves:** nobody edited history *without* recomputing every subsequent
hash. Casual tampering — a support tool `UPDATE`, a bad migration, a corrupted
row — is detected immediately by the verifier.

**It does not prove:** that someone with full database write access didn't
rewrite the chain. They have every hash and the algorithm; they can rebuild it
consistently.

So it defends against *tampering*, not a *compromised database*. The stronger
property needs an anchor outside the database — periodically publishing the
latest hash somewhere append-only. Then rewriting history requires rewriting
the anchor too, which you don't control. Not needed for the MVP; worth knowing
what you have.

## 5.5 Availability

| Risk | Mitigation |
|---|---|
| Unbounded retry loop holds connections | Cap at 10 + backoff (§4.6) |
| Lock queue on a hot account | `lock_timeout` / `statement_timeout` so requests fail fast |
| Unbounded page size | Cap `limit` server-side (~100), ignore larger |
| Expensive `LIKE` scans | Index them, or cap the query surface |
| Connection exhaustion | Pool limits below Postgres `max_connections` (default 100) |

`lock_timeout` deserves emphasis: without it, one slow transaction holding
`world` backs up every request behind it until the pool is exhausted and the
service is down. With it, requests fail fast and recover.

---

# Part 6 — The queries that matter

`$1` is a **parameter placeholder** — values are passed separately, never
pasted into the SQL string, so nothing can be injected. Here `$1` is always the
ledger name.

## 6.1 Conservation

```sql
select asset, sum(input) - sum(output) as drift
  from accounts_volumes
 where ledger = $1
 group by asset
having sum(input) <> sum(output);
```

**`where ledger = $1`** — this ledger only. On every query, always (§5.2).

**`group by asset`** — pile rows up by asset. Every `USD/2` row from every
account in one pile. Aggregates run per pile.

**`sum(input) - sum(output)`** — within the pile, add every account's input,
add every account's output, subtract.

**Why it must be zero.** Every posting adds the same amount to exactly one
account's input and one account's output, contributing `+X` and `−X`. Summed
across all of them it cancels, always. If it doesn't, value was created or
destroyed.

```
world          input 0      output 15000
users:alice    input 7000   output 0
users:bob      input 5000   output 0
fees:platform  input 3000   output 0
               ─────────────────────────
               sum   15000  sum    15000     →  drift 0   ✓
```

**`having ...`** — `having` is `where` for groups: it filters *after*
aggregation, keeping only piles whose sums disagree. (`where` cannot do this;
it runs before `sum()` exists.)

So **a healthy ledger returns zero rows.** It is a test assertion, not a
report. `assert rowCount == 0` after every test.

## 6.2 Total across a subtree

```sql
select asset, sum(input) - sum(output) as balance
  from accounts_volumes
 where ledger = $1 and address like 'users:%'
 group by asset;
```

`%` matches any characters, so this catches `users:alice`, `users:bob`,
`users:alice:savings` — everything under `users:`. The result is the combined
balance of every user wallet, one row per asset. No join, no account-type
table: the naming convention did the work.

`LIKE` is the readable version and it is a table scan. The fast version queries
`address_array` through the GIN index (§2.2). Same answer either way — start
with `LIKE`, switch when it matters.

## 6.3 Outstanding liability

```sql
select asset, output - input as outstanding
  from accounts_volumes
 where ledger = $1 and address = 'world';
```

No aggregate — `world` has one row per asset, so this reads a row.

The subtraction is **flipped**: `output - input`. `world`'s balance is normally
negative (§2.3), and flipping the sign gives a positive figure meaning "value
currently inside the ledger".

```
world: input 0, output 15000   →  outstanding = 15000  ($150.00)
```

For a wallet product that is total customer liability — what you would owe if
everyone withdrew at once. It should match your bank balance.

## 6.4 Historical balance

```sql
select pcev_input - pcev_output from moves
 where ledger = $1 and address = 'users:alice' and asset = 'USD/2'
   and effective_date <= '2026-03-03'
 order by effective_date desc, seq desc
 limit 1;
```

Find Alice's most recent move at or before the date, read its snapshot. One
index seek on `(ledger, address, asset, effective_date desc, seq desc)`.

**It reads `pcev`, not `pcv`.** An effective-date question must be answered
with effective-date snapshots (§2.8). Filtering by `effective_date` while
reading `pcv` returns wrong answers the moment any backdated transaction exists
— and passes every test until then, because the two are identical until the
first backdate.

`order by effective_date desc, seq desc` — `seq` breaks ties when two moves
share an effective date, falling back to insertion order.

---

# Part 7 — Storage, API, tenancy

## 7.1 Arbitrary-precision integers everywhere

**Decision.** `*big.Int` in Go, `numeric` in Postgres. Never `int64`, never
`float64`, never `bigint`.

**Why.** `int64` overflows at roughly 9.2 × 10^18 — more money than exists,
until someone denominates in an 18-decimal token where a single unit is 10^18
and two of them overflow. That is the standard shape of on-chain assets.
Choosing a fixed width means choosing a ceiling, and the cost of a ceiling here
is silently wrong money.

**The cost we accept.** `pgx` will not map `numeric` to `*big.Int`
automatically. Scan into a `string` and `SetString(s, 10)`, or write a
`pgtype` codec once, in one place.

**The JSON situation.** `math/big.Int` implements `json.Marshaler` and
`json.Unmarshaler`, and its `UnmarshalJSON` goes through `SetString` — so a
**typed** `*big.Int` field round-trips exactly, with no extra work:

```go
type Posting struct { Amount *big.Int `json:"amount"` }
// {"amount":100000000000000000000000000000001} round-trips byte-identical
```

The float64 coercion trap is real but narrower than it looks: it applies when
decoding into `any` / `map[string]any`, where the same input becomes `1e+32`.
So the rule is **never decode request bodies into `any`** — always into typed
structs. Watch for it in metadata handling and anywhere generic JSON is
inspected, which is exactly where `any` tends to creep in.

## 7.2 Multi-ledger from day one, one schema

**Decision.** Every table carries `ledger` as the first element of its primary
key. The ledger name appears in the API path. One Postgres schema, no
per-tenant schemas or databases.

**Why now.** Retrofitting tenancy means touching every table, index, query, and
test — while carrying live data. Adding the column up front costs one column
and a slightly wider primary key, and the composite key is what you want anyway:
`(ledger, id)` is the natural key for keyset pagination.

Separate ledgers are useful even for one product — production and sandbox, or
per-region for regulatory separation. And since the hash chain serialises
writes *per ledger* (§3.2), multiple ledgers is also how you scale writes.

**Rejected: a schema per tenant** (Formance calls these buckets). Real
isolation and independent migration, at the cost of migration tooling that runs
across N schemas and connection routing that knows which to use. Operational
machinery that teaches us nothing about ledgers.

## 7.3 Keyset pagination, never OFFSET

**Decision.** List endpoints paginate on `(ledger, id)` with an opaque base64
cursor.

**Why.** `OFFSET` on an append-only table gives wrong answers as rows land
between requests — a client walking pages skips and repeats rows. In most
systems that is cosmetic. In a ledger it reads as **missing money**, and the
client cannot distinguish it from real missing money. The correct behaviour is
also cheaper, since `OFFSET n` makes Postgres walk and discard `n` rows.

## 7.4 Dry run

**Decision.** `?dryRun=true` runs the entire commit path — locks, balance
checks, volume updates — then rolls back and returns what would have happened.

**Why.** Nearly free: the commit path already runs in a transaction, so this is
`ROLLBACK` instead of `COMMIT`. And it is honest in a way a simulation cannot
be, because it *is* the real path rather than a second implementation that
might drift.

## 7.5 The HTTP surface

Go 1.26's `net/http` `ServeMux` handles method-and-path patterns natively — no
router library needed.

```
POST   /v1/ledgers/{ledger}                       create ledger
POST   /v1/ledgers/{ledger}/transactions          commit (Idempotency-Key header)
GET    /v1/ledgers/{ledger}/transactions          list, cursor-paginated
GET    /v1/ledgers/{ledger}/transactions/{id}
GET    /v1/ledgers/{ledger}/accounts/{address}
GET    /v1/ledgers/{ledger}/accounts/{address}/balances
GET    /v1/ledgers/{ledger}/logs                  the audit trail
```

---

# Part 8 — Deliberately out of scope

Formance is ~729 Go files. Most of that is production scar tissue rather than
ledger. Each of these is a choice, not an oversight.

| Omitted | What it buys | Why not now |
|---|---|---|
| **Numscript** (the DSL) | Declarative multi-source sourcing, proportional splits | A whole project on its own. Its output is just `[]Posting`, so a clean seam keeps the door open. |
| **Buckets** (schema-per-tenant) | Hard isolation, independent migration | Operational machinery; teaches nothing about ledgers. |
| **Replication pipelines** | Streaming the log to an OLAP replica | `GET /logs` is the seam this attaches to later. |
| **OpenTelemetry** | Traces and histograms on every storage call | One `slog` line per commit is enough at this scale. |
| **Schema enforcement** | Constraining which transaction shapes a ledger accepts | Pure policy. Nothing in the core depends on it. |
| **Async hashing** | Removes per-ledger write serialization | A throughput optimisation with real complexity. Synchronous until measured. |
| **External hash anchoring** | Tamper evidence against a compromised database | Meaningful only with an operational process behind it (§5.4). |

---

# Part 9 — The invariants

These are the deliverable. A ledger that is fast and wrong is worth nothing.

1. **Conservation.** For every asset, `sum(input) - sum(output)` across all
   accounts including `world` is exactly zero. One query (§6.1). Run it after
   every test.
2. **No negatives.** No account except `world` ever has `input < output`, at
   any point, under any concurrency.
3. **Chain integrity.** Recomputing every log hash from the first reproduces
   the stored hash of the last.
4. **Projection fidelity.** Replaying all transaction logs into an empty
   database reproduces `accounts_volumes` exactly. This is the one that proves
   the log really is the source of truth and not merely a nice audit trail.

Tests run against a real Postgres (`giro_test`), never a mock. Every
interesting bug in this system lives in lock ordering, and a mock cannot have
that bug. Run with `-race`.

**The test that justifies the whole design:** fund an account with 100, fire 50
concurrent goroutines each trying to move 10 out. Exactly 10 must succeed, 40
must fail with insufficient funds, the final balance must be exactly 0, and no
goroutine may observe a negative balance at any point.

---

# Reference

`github.com/formancehq/ledger`, in reading order:

- `internal/storage/ledger/balances.go` — the sorted `FOR UPDATE` with the
  zero-row insert CTE. Ten lines, three ideas (§4.2–4.5).
- `internal/transaction.go` — `VolumeUpdates()`: per-transaction aggregation,
  the self-posting case, and the lock-order sort (§2.6).
- `internal/controller/ledger/log_process.go` — idempotency and deadlock retry
  (§3.3, §4.6).
- `internal/log.go` — the hash chain (§3.2).
- `internal/storage/bucket/migrations/0-init-schema/up.sql` — the shape of it all.
