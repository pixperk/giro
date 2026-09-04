# Reconciling giro

*Complete type and function reference: [API.md](API.md).*

Every check inside a ledger proves the book is consistent with itself. None of
them can tell you the money is actually in the bank.

giro records what you believe happened. Reconciliation checks that against what
everyone else says happened.

---

## Quickstart

Five minutes, one counterparty. Everything below compiles as part of the test
suite ([example_test.go](example_test.go)), so it cannot drift from the API.

### 1. Write the adapter

The only thing you implement. It calls somebody's API and maps the answer onto
`Record` — no database, no accounts.

```go
type bankStatement struct{}

func (bankStatement) ID() string   { return "infinitus" }
func (bankStatement) Name() string { return "Infinitus Pay" }

func (bankStatement) Fetch(ctx context.Context, since time.Time) ([]recon.Record, error) {
	rows, err := myBankClient.Statement(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]recon.Record, len(rows))
	for i, r := range rows {
		out[i] = recon.Record{
			ID:        r.StatementID,   // their line id
			Reference: r.WireRef,       // the match key
			Asset:     "USD/2",
			Amount:    big.NewInt(r.Cents), // positive magnitude
			Direction: recon.Out,
		}
	}
	return out, nil
}
```

### 2. Set it up once

```go
s := storage.New(pool, "main")
source := bankStatement{}

s.RegisterAsset(ctx, "USD/2")
recon.Register(ctx, pool, "main", source)

// the boundary account standing for the bank. permitted a negative balance
// because it is the outside world's side of the book.
const atBank = ledger.Address("external:bank:infinitus:USD")
s.SetAllowNegative(ctx, atBank, "USD/2", true)
```

Both registrations are idempotent, so this can run on every boot.

### 3. Stamp payments with the reference the bank will use

```go
s.CommitTransaction(ctx, ledger.Postings{
	{Source: "client:acme", Destination: atBank, Asset: "USD/2", Amount: big.NewInt(99_725_00)},
}, storage.CommitOptions{
	Metadata: ledger.Metadata{recon.ExternalRefKey: "WIRE-2026-0142"},
})
```

This is the one thing you have to remember to do. Without it a payment has
nothing to match against, and every line the bank sends comes back as
`reference_not_found`.

### 4. Reconcile, on a schedule

```go
recon.Pull(ctx, pool, "main", source, time.Now().Add(-time.Hour))
sum, err := recon.Match(ctx, pool, "main", recon.Config{})

// sum.Matched   1
// sum.Variance  0
// sum.Unmatched map[]
```

Fetching the last hour every ten minutes is correct — re-ingesting a line does
nothing, so overlapping windows are the safe way to page a statement rather
than something to avoid.

### 5. Alert on it

```
giro verify --recon-after=4h
```

Exits non-zero if anything has been unmatched longer than the grace period.
Alert on that **and** on the absence of a recent run.

---

## What it is for

One off-ramp touches three outside parties, and each keeps its own record:

```
the chain      100,000 USDT arrived from Acme      100,000.000000
the exchange   we sold it and were paid                $99,960.00
the bank       we wired Acme                           $99,725.00
```

Reconciliation asks, every day: **do their numbers match ours?** When they
don't, either you have a bug or they made a mistake. Both happen, and neither
announces itself.

> A ledger that balances perfectly can still be completely wrong about the
> world.

**It never writes a posting.** Nothing here moves money or changes a balance. A
reconciler that could correct the book would be a second way for money to move,
and the entire value of it is having an independent opinion. What it produces is
evidence, and a queue of things a person should look at.

---

## Connecting a source

A source is anything that will tell you what it saw. giro ships no provider
clients: you write a small adapter and plug it in.

```go
type Source interface {
	ID() string   // "kraken" — staged lines are keyed by it, so it never changes
	Name() string // for people
	Fetch(ctx context.Context, since time.Time) ([]Record, error)
}
```

An adapter calls someone's API and maps the response onto `Record`. It touches
no database and knows nothing about accounts.

```go
func (k *Kraken) Fetch(ctx context.Context, since time.Time) ([]recon.Record, error) {
	lines, err := k.client.Ledgers(ctx, since)
	if err != nil {
		return nil, err
	}

	out := make([]recon.Record, len(lines))
	for i, l := range lines {
		out[i] = recon.Record{
			ID:        l.LedgerID, // their line id: staging is idempotent on it
			Reference: l.RefID,    // the match key
			Asset:     "USD/2",
			Amount:    minorUnits(l.Amount), // a positive magnitude
			Direction: recon.In,
			Raw:       l.JSON,
		}
	}
	return out, nil
}
```

A scheduled run is two calls:

```go
recon.Register(ctx, db, "main", kraken)          // once, idempotent
recon.Pull(ctx, db, "main", kraken, since)       // fetch and stage
recon.Match(ctx, db, "main", recon.Config{})     // pair what it can
```

Ingesting the same line twice does nothing, so **overlapping windows are the
safe way to page a statement** rather than something to avoid. Fetching the last
hour every ten minutes is correct and cheap.

---

## How matching works

A line matches on an exact reference or it does not match at all. No fuzzy
matching, no matching by amount and date, no guessing.

> An unmatched line costs somebody five minutes.
> A falsely matched one costs a restatement.

The cost is asymmetric, so everything is deterministic and anything ambiguous is
left alone rather than resolved on a balance of probabilities.

### Two rules, cheapest first

**One line, one movement.** Same reference, same asset, right direction.

**One line, several movements.** A consolidated wire pays many transactions
under one reference — matched *only* when the amounts sum to the line exactly.

That exactness is the whole discriminator. Several movements under one reference
is either a real batch or an ambiguous reference, and nothing in the reference
itself tells them apart: a real batch adds up to the line it paid, and two
unrelated movements that happen to share a string do not.

### Direction is checked

Without it, an outbound wire reconciles against an inbound movement of the same
size and reference. Same number, same ref, opposite direction, and a
clean-looking match that is completely wrong. Supplying `Direction` makes it
impossible.

A source that doesn't say which way the money went skips the check. That is the
source being unhelpful rather than wrong.

### It matches movements, not transactions

A statement line is one account, one asset, one amount, one direction — which is
what a movement is. A transaction can be two of those at once: selling
stablecoin for dollars moves 100,000 of one thing and 99,960 of another, and a
line on the exchange's dollar statement is talking about exactly one of them.

---

## What comes back

```go
sum, err := recon.Match(ctx, db, "main", recon.Config{})
// sum.Matched   lines paired, amounts agree
// sum.Variance  lines paired, amounts disagree
// sum.Unmatched map[Break]int
```

"Unmatched" is not one problem. It is four, and they go to different people.

| Break | What it means | Who fixes it |
|---|---|---|
| `no_reference` | The source gave no match key at all | Whoever wrote the adapter |
| `reference_not_found` | A good reference naming nothing here | Either they recorded something you didn't, or you haven't yet |
| `reference_ambiguous` | Resolves to several movements that don't sum to the line | A person, deliberately |
| `movement_already_matched` | An earlier line from this source claimed it | Usually a duplicate in the statement |

### Variances are matched, not broken

A line paired with a movement whose amount disagrees is still recorded as a
pairing, with the difference. Somebody thinks a different amount moved. That
wants a person, not a silent adjustment, so it is evidence rather than a break.

Tolerance defaults to **exact**. A bank that is a penny out is telling you
something, and widening it should be a decision about one source rather than a
convenience.

```go
recon.Config{Tolerance: big.NewInt(1)} // one minor unit, if a source needs it
```

---

## Comparing whole positions

Matching can only pair the lines a source actually sent. If a source never
mentions a movement at all, every line it *did* send matched and the report is
clean.

A balance closes that gap in one read:

```go
type BalanceSource interface {
	Source
	Balance(ctx context.Context, asset ledger.Asset) (*big.Int, error)
}

got, err := recon.CompareBalance(ctx, db, "main", kraken, "external:lp:kraken:USD", "USD/2")
if !got.Agrees() {
	log.Print(got.Error())
	// kraken says it holds 99000000000 USDT/6 for us and we say 100000000000: out by -1000000000
}
```

Optional and separate from `Source` on purpose: a chain can tell you what an
address holds and a statement file cannot, and a source that can't answer
shouldn't have to pretend.

giro is unusually well placed for this. A boundary account per counterparty and
asset means **your side of the comparison is already an account balance** rather
than a report to be assembled.

---

## Two conventions you configure

### Which accounts face outward

Matching needs to know your edges, so a line saying "money in" pairs with a
movement that came in. giro's own convention is one account per counterparty and
asset:

```
external:chain:tron:USDT
external:lp:kraken:USD
external:bank:infinitus:USD
```

That is the default. A ledger that names its edges differently says so once:

```go
recon.Config{BoundaryPrefix: "edge:"}
```

A prefix rather than a predicate, and that is a concession worth being honest
about: matching runs in the database, so a Go function couldn't take part in it,
and offering one that silently didn't apply would be worse than a narrower knob
that works.

### The reference a counterparty will use

A transaction's `Reference` is unique per ledger, which is right for an
identifier and wrong for a match key — a consolidated wire is several
transactions arriving under one string. So the shared key goes in metadata:

```go
storage.CommitOptions{
	Metadata: ledger.Metadata{recon.ExternalRefKey: "WIRE-2026-0142"},
}
```

Matching accepts either, because a payment with only one transaction behind it
has no reason to carry the same string twice.

---

## Running it

`giro verify` runs a matching pass and reports what is still outstanding,
alongside the ledger's own checks:

```
$ giro verify --recon-after=4h
main
  ok   conservation               4 checked    13ms
  ok   log                        2 checked     1ms
  ok   projection                 4 checked     2ms
  ok   effective_volumes          6 checked     1ms
  ok   balance_permissions        4 checked     1ms
  ok   closed_accounts            0 checked     2ms
  ok   conversions                0 checked      0s
  ok   reconciliation             7 checked     6ms
```

`--recon-after` is a grace period. A line staged a minute ago has not failed to
reconcile, it simply has not been reconciled yet — settlement files land before
the movements they describe often enough that treating every fresh line as a
break would drown the real ones.

**Alert on two conditions, not one.** Breaks above zero, *and* the absence of a
recent run. A reconciler that stopped running looks exactly like a book with
nothing wrong, which is why every check reports what it examined.

---

## The report cannot be cleaned up

The most dangerous thing about reconciliation is that a clean report is easy to
fake, and faking it moves no money.

Delete the records that did not reconcile and the postings are untouched, the
hash chain still verifies, conservation still holds — and the book now
reconciles. Nothing else in the system would notice.

So the database refuses it, not the application:

- **Match evidence is append-only.** No updates, no deletes, no truncate.
- **A staged line may be marked matched and nothing else.** You cannot revise an
  amount or a reference to make something pair.
- **A line naming an unregistered asset is refused at ingest**, rather than
  sitting in the unmatched queue where a configuration error looks like a break.

These hold against raw SQL, and against the role the application connects as,
which has no privilege to alter a table.

---

## What it will not do

**Guess.** No fuzzy matching, no matching on amount and date. Two payments of
the same size on the same day are common, and telling them apart is exactly what
a reference is for.

**Search for subsets.** No many-to-many matching, where some group of lines is
paired with some group of movements. It is combinatorial, and a set that happens
to add up is not evidence it is the *right* set.

**Fix anything.** A discrepancy is reported, never corrected. A correction is a
transaction, and deciding to make one is a person's job.

**Run itself.** Something has to call it on a schedule.

---

## Where things live

```
recon/            the mechanism. imports ledger and storage. no provider knowledge.
your repo/        kraken, infinitus, tron adapters. a few dozen lines each.
```

The moment a ledger ships a Kraken client it stops being a general ledger. What
ships here is the interface, and a worked example in the tests — which is what
proves the interface is sufficient rather than merely plausible.
