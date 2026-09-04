# obs — OpenTelemetry for giro

A separate Go module, so the ledger itself keeps one direct dependency.

```bash
go get github.com/pixperk/giro       # the engine: pgx, and nothing else
go get github.com/pixperk/giro/obs   # opt into OpenTelemetry
```

```go
observer, shutdown, err := obs.Setup(ctx, "giro", obs.Options{
    SlowLock: 50 * time.Millisecond,
})
if err != nil {
    return err  // telemetry that silently did not start is worse than none
}
defer shutdown(ctx)

store := storage.New(pool, "main").Observe(observer)
```

That is the whole integration. A `Store` with no `Observer` behaves exactly as
it did before this package existed and computes nothing per commit.

If your application already configures OpenTelemetry, use `obs.New` instead and
it will take the global providers.

---

## Where the data goes

Nothing in giro decides. `Setup` delegates to the standard OpenTelemetry
environment variables, so pointing it somewhere else is a deployment change and
not a code change:

```
OTEL_METRICS_EXPORTER        otlp (default) · prometheus · console · none
OTEL_TRACES_EXPORTER         otlp (default) · console · none
OTEL_EXPORTER_OTLP_ENDPOINT  where to send it
OTEL_EXPORTER_OTLP_PROTOCOL  grpc · http/protobuf
```

`none` is a real setting, so telemetry can be switched off in an environment
without removing the wiring — which would otherwise rot while it was disabled.

**A collector is optional.** giro speaks OTLP straight to any backend that
accepts it, which is enough for development and small deployments. A collector
earns its place in production by letting the application hand data off quickly
and by being the one place batching, retries and redaction are configured
rather than a setting in every service.

**And giro does not ship one.** The OpenTelemetry Collector is a mature,
security sensitive, high throughput binary with a large community behind it;
writing another would be the same mistake as shipping an exchange client inside
[`recon`](../recon/). It is also already modular, which is usually the reason
people reach for building one — the upstream builder (`ocb`) takes a manifest
naming exactly the receivers, processors and exporters you want and compiles a
binary with those and nothing else.

[`deploy/otel-collector.yaml`](../deploy/otel-collector.yaml) is a starting
configuration, including the redaction question below.

---

## What it emits

```
METRIC                KIND       UNIT           LABELS
giro.transactions     counter    {transaction}  giro.ledger, giro.asset
giro.refusals         counter    {transaction}  giro.ledger, giro.asset, giro.reason
giro.commit.duration  histogram  s              giro.ledger
giro.lock.wait        histogram  s              giro.ledger
giro.commit.attempts  histogram  {attempt}      giro.ledger
giro.commit.restarts  counter    {restart}      giro.ledger
giro.postings         histogram  {posting}      giro.ledger
```

`obs.WriteCatalogue(w, ledgers, assets)` prints this with the series count for
your configuration. Two ledgers and three assets is about 64 series.

`giro.transactions` is counted **once per asset**, so a conversion — one
transaction moving stablecoin one way and dollars the other — increments twice.
Counting it under only the first asset would make the second side invisible.

---

## The rule: addresses are never labels

A ledger's most natural label is an account address, and addresses are
unbounded. Labelling a metric `users:alice` gives you one time series per
customer, which is how a metrics backend dies — and you find out during the
incident the dashboard was built for.

So the split is deliberate and enforced by a test:

| | Goes on |
|---|---|
| ledger, asset, reason | metrics **and** spans |
| account addresses | spans only |

A span exists per request rather than per series, so *"which account was
this?"* stays answerable at 3am without producing a million time series to
answer it. `giro.committed` and `giro.lock.slow` span events carry the
addresses; no metric ever does.

Cardinality is therefore a function of your configuration, never of your
traffic. `TestCardinalityDoesNotGrowWithTraffic` is what keeps it that way.

---

## A refusal is not an error

`users:alice cannot spend money she does not have` is the ledger working. It
gets `giro.refusals` and a span event — **not** `codes.Error`, and not
`RecordError`.

This matters more than it sounds. Generic HTTP middleware folds a `422` into a
4xx error rate, and a correct ledger under ordinary load then looks like a
system in trouble. Worse, the day something genuinely breaks, the signal is
buried in the noise of customers being short of money.

Group `giro.refusals` by `giro.reason`, because the reasons have different
owners:

| Reason | Means | Who |
|---|---|---|
| `insufficient_funds` | Somebody tried to spend what they do not have | Product. A rise is a user-behaviour or pricing change. |
| `unknown_asset` | A caller named an asset this ledger does not handle | **A bug in a caller.** Should be flat at zero. |
| `invalid_posting` | Malformed request | **A bug in a caller.** Should be flat at zero. |
| `account_closed` | Movement against an account taken out of service | Operations. Usually expected, briefly. |
| `unexpected_credit` | An account bounded above went positive | **Look at this.** A cost line leaning the wrong way means a loss was booked as a gain. |
| `unbounded_sweep` | A sweep with no ceiling and nothing to bound it | A bug in a caller. |
| `contention_exhausted` | Gave up after the retry limit | **Page.** See below. |
| `other` | Not classified | If this is non-zero, a new error type needs a cause. |

---

## What to alert on

Two of these have no substitute and nothing else can measure them.

**`giro.commit.restarts` above zero.** Sorted lock ordering is supposed to make
deadlocks impossible; a commit is never the victim, because whichever
transaction closes the cycle is the one Postgres kills and giro's always
acquires first and waits. A non-zero rate means that ordering has been broken
somewhere. It is a correctness signal wearing a performance signal's clothes.

**`giro.refusals{reason="contention_exhausted"}` above zero.** A transaction
that gave up after the retry limit. A caller was told no for a reason that has
nothing to do with their money.

**`giro.lock.wait` p99 climbing.** This is the designed-in wall. Every deposit
into the ledger takes a row lock on `world`, which makes it the hottest row in
the system by construction. When this histogram starts climbing, `world` is the
reason, and splitting it per counterparty — `external:bank:northwind:USD` and
friends — is the remedy. Nothing outside the engine can measure this: from the
caller's side, a contended commit and a slow one look identical.

Set `Options.SlowLock` and the slow waits also land on the trace, naming the
accounts. The histogram tells you contention is happening; the span tells you
which row.

Alongside these, alert on **the absence of `giro verify` runs** — see
[deploy/](../deploy/). A detector that stopped running looks exactly like a book
with nothing wrong, and no metric emitted from the commit path will tell you
that.

---

## Spans

`Observer` also implements `storage.Tracer`, so the engine opens real spans and
they nest:

```
giro.commit                 what the caller waited for, retries included
└── giro.commit.attempt     one pass through the database transaction
    └── giro.lock           the row locking statement
```

That nesting is the point. It turns *"the commit took 45ms"* into *"40ms of it
was waiting on `world`"*, which is a different sentence with a different
remedy. More than one `giro.commit.attempt` inside a commit means it lost a
deadlock and started again.

A refusal never sets an error status on a span — same reason it is not counted
as an error. It gets `giro.reason` and an event instead.

**Addresses are on spans.** That is the right place for them, but it does mean
a span attribute carries customer identifiers. Decide deliberately whether
those leave your network; the collector config has a commented-out processor
that drops them at the boundary, which keeps the shape of the trace and the
lock timing while losing whose money it was.

---

## What this package does not do

**It does not instrument `giro serve`.** The shipped binary is in the core
module, which cannot import this one without taking on the dependency tree this
module exists to keep out. Telemetry is for the embedded case — which is how
giro is meant to be used in production anyway, and the same answer
[`recon`](../recon/) gives about providers.

**It must not block.** `Observer` methods are called on the commit path. An
implementation that talks to a network synchronously has made every transaction
wait for a metrics backend. The OpenTelemetry SDK batches; anything you write
yourself should too.
