# recon — API reference

Complete surface of `github.com/pixperk/giro/recon`. For what it is and why it
behaves this way, see [README.md](README.md).

Every function takes the ledger name explicitly. A `recon` call is scoped to one
ledger the same way a `storage.Store` is, and passing it rather than holding it
keeps this package free of state.

**Contents** — [Interfaces](#interfaces) · [Types](#types) ·
[Staging](#staging) · [Matching](#matching) · [Balances](#balances) ·
[Checks](#checks) · [Constants](#constants) · [Errors](#errors)

---

## Interfaces

### `Source`

What you implement per counterparty. giro ships none of these.

```go
type Source interface {
	ID() string
	Name() string
	Fetch(ctx context.Context, since time.Time) ([]Record, error)
}
```

| Method | Contract |
|---|---|
| `ID()` | Stable for the life of the ledger. Staged lines are keyed by it, so changing it orphans everything already ingested. `"kraken"`, not `"Kraken Exchange (prod)"`. |
| `Name()` | For people. Free to change. |
| `Fetch(ctx, since)` | What this source says happened since a point in time. **Returning the same line twice is expected and harmless** — staging is idempotent, so overlapping windows are the safe way to page a statement. |

An implementation touches no database and knows nothing about accounts.

### `BalanceSource`

A `Source` that can also state its own position. Optional.

```go
type BalanceSource interface {
	Source
	Balance(ctx context.Context, asset ledger.Asset) (*big.Int, error)
}
```

`Balance` returns what the counterparty says it holds **for you**, in the
asset's minor units. Positive means they hold it; a payable is negative.

Separate from `Source` because a chain can answer this and a statement file
cannot, and a source that cannot answer should not have to pretend.

### `DB`

What this package needs from a database handle. Both `*pgxpool.Pool` and
`pgx.Tx` satisfy it.

```go
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
```

An interface rather than a `*storage.Store`, so `recon` depends on a shape and
not on the engine. The direction is the point: **the ledger never imports this.**

---

## Types

### `Record`

One statement line, normalised.

```go
type Record struct {
	ID         string        // the source's own line id
	Reference  string        // the match key
	Asset      ledger.Asset
	Amount     *big.Int      // positive magnitude, minor units
	Direction  Direction
	OccurredAt time.Time
	Raw        []byte        // the line as received
}
```

| Field | Required | Notes |
|---|---|---|
| `ID` | yes | Staging is idempotent on `(source, ID)`. This is what makes an ingest safe to retry after a timeout that may or may not have landed. |
| `Reference` | no | Empty means the line can never match, and exists to be looked at. |
| `Asset` | yes | Must be registered on the ledger, or ingest refuses the line. |
| `Amount` | yes | Must be **positive**. Direction carries the sign, so one way of saying a thing cannot disagree with itself. |
| `Direction` | no | Empty skips the direction check. |
| `OccurredAt` | no | When the source says it happened. Stored, not currently matched on. |
| `Raw` | no | Kept so a rule can be replayed against what was received rather than what you decided it meant. |

### `Direction`

```go
const (
	Unknown Direction = ""    // the source does not say
	In      Direction = "in"  // money arrived, from our side
	Out     Direction = "out" // money left
)
```

`Unknown` matches either way. That is the source being unhelpful rather than
wrong — but supplying a direction is what stops an outbound wire pairing with an
inbound movement of the same size and reference.

### `Config`

```go
type Config struct {
	BoundaryPrefix string   // "" means DefaultBoundaryPrefix
	Tolerance      *big.Int // nil means exact
}
```

The zero value works and assumes giro's conventions.

`BoundaryPrefix` names the addresses that face outward. A prefix rather than a
predicate because matching runs in the database — a Go function could not take
part, and offering one that silently did not apply would be worse.

`Tolerance` is how far a line's amount may sit from the movement it pairs with
and still count as matched rather than a variance, in minor units. Zero is the
right default: a bank that is a penny out is telling you something.

### `Summary`

```go
type Summary struct {
	Matched   int
	Variance  int
	Unmatched map[Break]int
}
```

`Matched` and `Variance` are both **pairings that were recorded**. A variance is
not a failure to match; it is a match whose amounts disagree, with the
difference stored.

### `Break`

Why a line did not match.

```go
const (
	NoReference Break = "no_reference"
	NotFound    Break = "reference_not_found"
	Ambiguous   Break = "reference_ambiguous"
	Contested   Break = "movement_already_matched"
)
```

| Break | Cause | Where the fix is |
|---|---|---|
| `NoReference` | The source gave no match key | The adapter or the source configuration |
| `NotFound` | A good reference naming nothing here | Either they recorded something you did not, or you have not yet |
| `Ambiguous` | Resolves to several movements that do not sum to the line | A person. Deliberately not guessed at. |
| `Contested` | An earlier line from this source already claimed the movement | Usually a duplicate in the statement |

### `BalanceComparison`

```go
type BalanceComparison struct {
	Source     string
	Account    ledger.Address
	Asset      ledger.Asset
	Ours       *big.Int
	Theirs     *big.Int
	Difference *big.Int // Theirs - Ours. Zero is agreement.
}

func (c BalanceComparison) Agrees() bool
func (c BalanceComparison) Error() string
```

`Ours` is **the boundary account's balance, negated**. A boundary account holds
the outside world's side of the book, so it carries the mirror of your position.
Negating puts both sides in the same terms, which is the only way the difference
means anything.

### `StaleBreak`

A staged line nobody has resolved. Implements `error`.

```go
type StaleBreak struct {
	Source, RecordID, Reference string
	Asset                       ledger.Asset
	Amount                      *big.Int
	Since                       time.Time
}
```

---

## Staging

### `Register`

```go
func Register(ctx context.Context, db DB, ledgerName string, s Source) error
```

Declares a source. Idempotent, so a startup routine can run it on every boot.

A line from an unregistered source is **refused at ingest**, so a typo in a
source id is an error rather than a queue of records nothing will ever match.

### `Ingest`

```go
func Ingest(ctx context.Context, db DB, ledgerName, sourceID string, records []Record) (staged int, err error)
```

Stages lines and returns how many were new. Idempotent per `(source, record ID)`.

**Every line is validated before any is staged.** A file with one malformed row
stages nothing, because a partial ingest leaves nobody able to say which half
arrived.

Refuses: a record with no `ID`, an invalid or unregistered `Asset`, a
non-positive `Amount`, a `Direction` that is neither `in` nor `out`.

### `Pull`

```go
func Pull(ctx context.Context, db DB, ledgerName string, s Source, since time.Time) (staged int, err error)
```

`Fetch` then `Ingest`. The whole job of a scheduled run.

---

## Matching

### `Match`

```go
func Match(ctx context.Context, db DB, ledgerName string, cfg Config) (Summary, error)
```

Pairs every unmatched staged line it can.

**Idempotent.** A line already matched is not looked at again, so running twice
changes nothing and running on a schedule is the intended use.

**Writes no postings and changes no balance.** A reconciler able to correct the
book would be a second way for money to move.

Two rules, cheapest first:

1. **Exact** — one line, one movement, one reference. Recorded as `exact_ref`.
2. **Consolidated** — one line paying several movements that share a reference,
   *only* when the amounts sum to it exactly. Recorded as `consolidated_ref`.

A movement contested by two lines goes to the earliest staged one, by explicit
ordering, so repeated runs give the same answer rather than whatever the planner
felt like.

Matches are against **movements**, not transactions. A statement line is one
account, one asset, one amount, one direction — a transaction can be two of
those at once.

---

## Balances

### `CompareBalance`

```go
func CompareBalance(
	ctx context.Context, db DB, ledgerName string,
	s BalanceSource, account ledger.Address, asset ledger.Asset,
) (BalanceComparison, error)
```

Asks a source what it holds and compares it to the boundary account standing for
it. Nothing is written.

Closes the gap matching cannot see: if a source never mentions a movement at
all, every line it *did* send matched and the report is clean.

---

## Checks

### `Unmatched`

```go
func Unmatched(ctx context.Context, db DB, ledgerName string, olderThan time.Duration) (checked int, err error)
```

Reports staged lines older than a cutoff, oldest first, as joined `StaleBreak`
errors. `checked` counts **everything staged**, not findings — a run against a
ledger nothing has been ingested into has looked at nothing, and that is not the
same as finding nothing wrong.

`olderThan` is a grace period. A line staged a minute ago has not failed to
reconcile, it simply has not been reconciled yet.

Returns an error if `olderThan` is negative.

### `Check`

```go
func Check(ctx context.Context, db DB, ledgerName string, cfg Config, olderThan time.Duration) (checked int, err error)
```

`Match` then `Unmatched`, shaped for `giro verify`. Matching first, because a
check that reports breaks without having tried to resolve them is reporting the
state of the last run rather than the state of the book.

```
giro verify --recon-after=4h
```

---

## Constants

```go
const DefaultBoundaryPrefix = "external:"
const ExternalRefKey = "giro/external.ref"

const (
	RuleExact        = "exact_ref"
	RuleConsolidated = "consolidated_ref"
)
```

`ExternalRefKey` is the transaction metadata key holding the reference a
counterparty will use. It exists because `Reference` is unique per ledger, which
is right for an identifier and wrong for a match key — a consolidated wire is
several transactions arriving under one string.

Matching accepts either `Reference` or this key.

`RuleExact` and `RuleConsolidated` are recorded on every match, so a pairing can
be explained later.

---

## Errors

Every error names the source and line it concerns.

| Condition | Shape |
|---|---|
| Malformed record | `error` from `Ingest`, naming the index and the problem |
| Unregistered source | Foreign key violation from the database |
| Unregistered asset | Foreign key violation from the database |
| Unmatched lines past the cutoff | `errors.Join` of `StaleBreak`, one per line |
| Positions disagree | `BalanceComparison` with `Agrees() == false` — *returned, not an error* |

A balance disagreement is deliberately not an error type. Two organisations
disagreeing about a position is a fact to be reported, not a fault in the call
that discovered it.

---

## What the database enforces

Not API, but it constrains what any caller can do, including one that bypasses
this package.

- **`recon_matches` is append-only.** No update, no delete, no truncate.
- **`recon_records` permits `matched_count` and `matched_at` to change, and
  nothing else.** An amount or a reference cannot be revised to make a line pair.
- **A line naming an unregistered asset or source is refused at insert.**

These hold against raw SQL, and against the role the application connects as.
