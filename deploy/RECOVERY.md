# Recovery

What to do when the database is gone, or is wrong.

Read this before you need it. The part that catches people is not the restore —
Postgres is good at restores — it is that **a ledger cannot be re-derived**.

---

## Why a ledger is not like your other services

When you lose an ordinary service you re-derive it: replay the event stream,
re-fetch from the upstream API, rebuild the cache. There is always something
upstream holding the truth.

A ledger *is* the upstream. Nothing else in your system knows the balances.

That makes "restore to 09:00" mean something uncomfortable. Transactions
between 09:00 and 11:00 **actually happened** — the wire left the bank, the
customer got their money. Restoring un-records them from your book while the
world keeps its copy. So a restore is never "we are back". It is:

> get back to a known-consistent state → prove it is consistent → find out what
> the world did in the gap → re-apply it.

---

## The hazard nothing else catches

Transaction and log ids come from a counter on the ledger's row, gapless and
monotonic. A trigger refuses to let that counter go backwards, because reusing
an id means two different transactions answering to the same name.

**That trigger fires on an `UPDATE`. A restore does not update anything** — it
replaces the table, or the whole data directory. The counter goes back to the
restore point and no trigger ever sees it. The next commit then claims an id
that has already been issued, and every system holding *"giro transaction
4291"* is now pointing at a different transaction than it was yesterday.

Every check still passes. Conservation holds, the chain verifies, the
projection agrees. The book is internally perfect and no longer means what it
meant.

There is **no way to detect this from inside the restored database**. Everything
that could remember the higher position — the ledgers row, the log,
`verification_runs` — was restored along with it, consistently, to the same
earlier moment. So the position has to be kept somewhere the restore cannot
reach.

---

## Before anything goes wrong

### 1. Record the position, continuously

```bash
giro verify --record=false && giro recover tip
```

```
main:4291:3f4WLTEnfJp93aSndGTqdTjg547dZIt-5uyF9raDO4g
```

`ledger:logID:hash`. Ship it wherever your logs go, and keep it in the
deployment record. It is small enough to paste into a ticket and specific
enough to prove, later, that a restore landed where you think it did.

**The hash is what makes it worth recording.** A number alone answers "did the
ledger go backwards". A number and a hash answer the question that matters: *is
transaction 4291 still the same transaction it was?*

Run this hourly, alongside `giro verify`. The most recent one you trust is what
you will compare against.

### 2. Take backups Postgres can restore consistently

Physical base backup plus archived WAL, giving you point-in-time recovery. Not
a per-table dump: giro's invariants are cross-table, and a backup that captured
`accounts_volumes` but not `logs` restores a book that fails conservation.

Your RPO is how much money you are willing to look for by hand. For a ledger,
aim at zero and expect to be wrong.

### 3. Rehearse it

A backup you have never restored is not a backup, it is a belief. Restore into
a scratch database and run the checks below. Quarterly, and after any change to
the schema or the backup configuration.

---

## After a restore

Run these **before letting anything write**. The order matters.

### 1. Is the book sound?

```bash
giro verify --record=false
```

Conservation, the hash chain, the projection. This tells you the restore is
internally consistent — that Postgres gave you back a coherent database.

It does **not** tell you it is the right database.

### 2. Is it the database you had?

```bash
giro recover check main:4291:3f4WLTEnfJp93aSndGTqdTjg547dZIt-5uyF9raDO4g
```

Three answers:

| | Means | Do |
|---|---|---|
| `ok` | The recorded entry is present and unchanged. Nothing was lost. | Continue at step 4. |
| `is behind the recorded tip` | The restore lost entries. Their ids are about to be reissued. | Step 3, **before any write.** |
| `has forked from the recorded tip` | Ids have **already** been reused. Something wrote after the restore. | Stop. See below. |

If it has forked, stop and get a person. The database now contains two
different transactions that have held the same id, and which downstream systems
saw which is not a question this tool can answer. You will need the log entries
from both sides — the archived WAL, and whatever your consumers recorded — to
work out what each id meant to whom.

### 3. Resume above every id ever issued

```bash
giro recover resume main:4291:3f4W... --note="incident 41, restored from 09:00 base backup"
```

```
main resumed at log 4292, declaring ids 4288-4291 as issued before the restore.
those ids are never reissued. record the new tip:
  main:4292:mtfqC_IfZWiGW4d_QfC_e1qFXd_lFZO8zP0Kiqr84ws
```

This appends a `RECOVERY` entry to the log declaring the range it skipped, and
moves the counters past it.

**Why an entry and not a counter bump.** Resuming leaves a real gap where the
lost entries were, and a gap is exactly what the chain check exists to catch —
a missing id means somebody deleted an entry. So the gap is *declared*:
verification accepts a gap only when the entry after it names that exact range.
An undeclared gap is still a broken chain, so this does not weaken the check.

It is also the same rule as everywhere else in giro: nothing recorded is
edited, and a correction is something you append. An operator quietly moving a
counter is an edit wearing a different hat.

The skipped ids are never reissued. They belonged to transactions that really
happened, and leaving them unused is what stops a replay colliding with a real
transaction this database no longer remembers.

### 4. Find out what the world did in the gap

This is the part no tool does for you, and it is why boundary accounts and
[`recon`](../recon/) exist.

Ask each counterparty what they saw between the restore point and now, ingest
it, and match. The unmatched queue is your list of what to re-apply:

```go
recon.Pull(ctx, db, "main", bank, restorePoint)
recon.Match(ctx, db, "main", cfg)
```

`reference_not_found` now means *"they recorded it and we do not have it"* —
which during ordinary operation is a timing question and here is your gap,
itemised.

`CompareBalance` is the coarse version and worth running first: if the
counterparty's stated position matches your boundary account, nothing crossed
that edge in the window.

### 5. Re-apply, idempotently

You do not edit anything back into place. You commit the missing transactions
again, with the same `Idempotency-Key` the original caller used.

That key is what makes this safe. A replay of a request that *did* land returns
the original instead of paying twice — the mechanism you built for network
retries is also your gap-filling tool. If your callers do not send idempotency
keys, this step is manual reconciliation instead, so **make them send keys**
long before you need this page.

The re-applied transactions get new ids. That is correct: they are new records
of old events, and their `timestamp` carries when the money actually moved
while their insertion date says when you recorded it. Two clocks, which is what
they are for.

### 6. Record the new position

```bash
giro verify --record=false && giro recover tip
```

You are back. Put the new tip in the incident record next to the old one.

---

## Who may do this

The **owner** role, not `giro_service`. `recover resume` writes to the ledgers
row and appends a log entry; the serving role can do neither by design. See
[The three roles](../README.md#the-three-roles).

That is deliberate. Resuming a ledger is not something a running service should
be able to do to itself.

---

## A checklist for the incident

```
[ ] stop writes. every step below is invalid if something is committing.
[ ] restore, and note the restore point
[ ] giro verify --record=false          → is the book sound
[ ] giro recover check <recorded tip>   → is it the book we had
[ ] giro recover resume <recorded tip>  → only if behind, and before any write
[ ] recon against every counterparty for the gap window
[ ] re-apply the gap with the original idempotency keys
[ ] giro verify && giro recover tip     → record the new position
[ ] resume writes
[ ] write down what the gap contained, in the incident record
```

The last line is not ceremony. Six months from now the `RECOVERY` entry in the
log will name a range of ids and nothing else, and somebody will want to know
what was in it.
