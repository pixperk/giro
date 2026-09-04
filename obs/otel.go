// Package obs turns what the giro engine reports into OpenTelemetry.
//
// It implements storage.Observer and nothing else. The engine has no idea this
// exists, which is what keeps a telemetry framework out of the code that moves
// money -- and out of that code's dependency tree, so the ledger itself is
// still one direct dependency.
//
// # What it emits, and the rule it follows
//
// Metrics are keyed on ledger, asset and reason. Never on an account address.
// A ledger's most natural label is an address, and addresses are unbounded:
// one series per customer is how a metrics backend dies. Addresses go on
// spans, where they belong -- a span is per request rather than per series, so
// "which account was this" is answerable during an incident without producing
// a million time series to answer it.
//
//	giro.transactions          counter    ledger, asset          committed
//	giro.refusals              counter    ledger, asset, reason  declined, and why
//	giro.commit.duration       histogram  ledger                 seconds, end to end
//	giro.lock.wait             histogram  ledger                 seconds inside FOR UPDATE
//	giro.commit.attempts       histogram  ledger                 1 unless it lost a deadlock
//	giro.commit.restarts       counter    ledger                 deadlocks lost
//	giro.postings              histogram  ledger                 postings per transaction
//
// Cardinality is therefore ledgers x (assets + reasons + 5), and the reasons
// are a closed set of eight. Catalogue prints the whole of it.
//
// # What it deliberately does not do
//
// It does not turn a refusal into an error. "users:alice cannot spend money
// she does not have" is the ledger working, and a span marked Error for it
// would light up every dashboard in the building during an ordinary Tuesday.
// Refusals get their own counter and a span event; only genuine failures get
// codes.Error.
//
// # Spans
//
// It also implements storage.Tracer, so the engine opens real spans and they
// nest: giro.commit covers the call a caller waited on, giro.commit.attempt
// covers one pass through the database transaction, and giro.lock covers the
// row locking statement. That nesting is what turns "the commit took 45ms"
// into "40ms of it was waiting on world".
//
// A refusal never sets an error status on a span, for the same reason it is
// not counted as an error.
package obs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/storage"
)

// ScopeName is the instrumentation scope every instrument here is created
// under, so a collector can attribute them.
const ScopeName = "github.com/pixperk/giro"

// Options configures an Observer. The zero value uses the global providers,
// which is what an application that has already called otel.SetMeterProvider
// wants.
type Options struct {
	// Meter provides the instruments. Nil uses the global provider, which is
	// what an application that has already called otel.SetMeterProvider wants.
	Meter metric.MeterProvider

	// Tracer provides the spans. Nil uses the global provider.
	Tracer trace.TracerProvider

	// SlowLock is the threshold above which a lock wait is also recorded as a
	// span event, so a contended commit can be found in a trace rather than
	// only inferred from a histogram. Zero means never.
	//
	// It exists because the histogram tells you contention is happening and
	// the span tells you which account it happened on -- and the second
	// question is the one you actually have at 3am.
	SlowLock time.Duration
}

// Observer implements storage.Observer against OpenTelemetry.
type Observer struct {
	commits  metric.Int64Counter
	refusals metric.Int64Counter
	restarts metric.Int64Counter
	duration metric.Float64Histogram
	lockWait metric.Float64Histogram
	attempts metric.Int64Histogram
	postings metric.Int64Histogram
	tracer   trace.Tracer
	slowLock time.Duration
}

// New builds an Observer, or returns the error OpenTelemetry gave for the
// first instrument it could not create.
//
// It returns an error rather than panicking or swallowing: telemetry that
// silently did not start is worse than none, because the absence of a signal
// then means either "nothing happened" or "nothing was ever watching", and
// those need different people woken up.
func New(opts Options) (*Observer, error) {
	mp := opts.Meter
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	m := mp.Meter(ScopeName)

	tp := opts.Tracer
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	o := &Observer{slowLock: opts.SlowLock, tracer: tp.Tracer(ScopeName)}
	var err error
	fail := func(name string, e error) error {
		return fmt.Errorf("create instrument %s: %w", name, e)
	}

	if o.commits, err = m.Int64Counter("giro.transactions",
		metric.WithDescription("Transactions committed."),
		metric.WithUnit("{transaction}")); err != nil {
		return nil, fail("giro.transactions", err)
	}
	if o.refusals, err = m.Int64Counter("giro.refusals",
		metric.WithDescription("Transactions the ledger declined. Not errors: a refusal is the ledger working."),
		metric.WithUnit("{transaction}")); err != nil {
		return nil, fail("giro.refusals", err)
	}
	if o.restarts, err = m.Int64Counter("giro.commit.restarts",
		metric.WithDescription("Commits restarted after losing a deadlock. Sorted lock ordering should keep this near zero."),
		metric.WithUnit("{restart}")); err != nil {
		return nil, fail("giro.commit.restarts", err)
	}
	if o.duration, err = m.Float64Histogram("giro.commit.duration",
		metric.WithDescription("End to end commit latency, including retries and backoff."),
		metric.WithUnit("s")); err != nil {
		return nil, fail("giro.commit.duration", err)
	}
	if o.lockWait, err = m.Float64Histogram("giro.lock.wait",
		metric.WithDescription("Time inside the row locking statement. The hot row shows up here first."),
		metric.WithUnit("s")); err != nil {
		return nil, fail("giro.lock.wait", err)
	}
	if o.attempts, err = m.Int64Histogram("giro.commit.attempts",
		metric.WithDescription("Attempts a commit needed. 1 unless it lost a deadlock."),
		metric.WithUnit("{attempt}")); err != nil {
		return nil, fail("giro.commit.attempts", err)
	}
	if o.postings, err = m.Int64Histogram("giro.postings",
		metric.WithDescription("Postings per transaction."),
		metric.WithUnit("{posting}")); err != nil {
		return nil, fail("giro.postings", err)
	}
	return o, nil
}

// Committed records one landed transaction.
func (o *Observer) Committed(ctx context.Context, e storage.Commit) {
	ledgerAttr := attribute.String("giro.ledger", e.Ledger)

	// once per asset, because a conversion is one transaction in two assets
	// and counting it under only the first would make the second invisible
	for _, a := range e.Assets {
		o.commits.Add(ctx, 1, metric.WithAttributes(ledgerAttr, assetAttr(a)))
	}
	o.duration.Record(ctx, e.Took.Seconds(), metric.WithAttributes(ledgerAttr))
	o.attempts.Record(ctx, int64(e.Attempts), metric.WithAttributes(ledgerAttr))
	o.postings.Record(ctx, int64(e.Postings), metric.WithAttributes(ledgerAttr))

	// the addresses go here and only here. a span is per request, so naming
	// every account is answerable without becoming a series per customer.
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.AddEvent("giro.committed", trace.WithAttributes(
			ledgerAttr,
			attribute.Int("giro.postings", e.Postings),
			attribute.Int("giro.accounts", e.Accounts),
			attribute.Int("giro.attempts", e.Attempts),
			attribute.StringSlice("giro.addresses", addressStrings(e.Addresses)),
		))
	}
}

// Refused records one declined transaction.
//
// Note what is absent: no span.SetStatus(codes.Error) and no RecordError. The
// ledger refusing a transaction is the ledger doing its job, and marking the
// span failed would make a correct system look broken to every tool that reads
// span status -- which is most of them.
func (o *Observer) Refused(ctx context.Context, e storage.Refusal) {
	attrs := []attribute.KeyValue{
		attribute.String("giro.ledger", e.Ledger),
		attribute.String("giro.reason", string(e.Reason)),
	}
	if e.Asset != "" {
		attrs = append(attrs, assetAttr(e.Asset))
	}
	o.refusals.Add(ctx, 1, metric.WithAttributes(attrs...))

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		evt := attrs
		if e.Account != "" {
			evt = append(evt, attribute.String("giro.account", string(e.Account)))
		}
		span.AddEvent("giro.refused", trace.WithAttributes(evt...))
	}
}

// Contended records lock waiting, and restarts separately from waiting.
func (o *Observer) Contended(ctx context.Context, e storage.Contention) {
	ledgerAttr := attribute.String("giro.ledger", e.Ledger)

	if e.Restarted {
		o.restarts.Add(ctx, 1, metric.WithAttributes(ledgerAttr))
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.AddEvent("giro.restarted", trace.WithAttributes(
				ledgerAttr, attribute.Int("giro.attempt", e.Attempt)))
		}
		return
	}

	o.lockWait.Record(ctx, e.Waited.Seconds(), metric.WithAttributes(ledgerAttr))

	// only the slow ones reach a span, because every commit produces one of
	// these and an event per commit would double the size of every trace to
	// say "the lock was fast", which nobody asks
	if o.slowLock > 0 && e.Waited >= o.slowLock {
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.AddEvent("giro.lock.slow", trace.WithAttributes(
				ledgerAttr,
				attribute.Float64("giro.waited_seconds", e.Waited.Seconds()),
				attribute.StringSlice("giro.accounts", addressStrings(e.Accounts)),
			))
		}
	}
}

// assetAttr keeps the asset label in one place. It is bounded by the asset
// registry, which is a table an operator writes to deliberately, so it is safe
// as a label in a way an address never is.
func assetAttr(a ledger.Asset) attribute.KeyValue {
	return attribute.String("giro.asset", string(a))
}

func addressStrings(in []ledger.Address) []string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = string(a)
	}
	return out
}

// compile time proof that this is what the engine asked for
var _ storage.Observer = (*Observer)(nil)

// Start opens a span, implementing storage.Tracer.
//
// The engine hands back the error the work produced, and what happens to it is
// decided here rather than there: a refusal is recorded on the span as an
// event and leaves its status unset, because "users:alice cannot spend money
// she does not have" is the ledger working. Only something that is not a
// refusal marks the span Error.
//
// Without this, giro produces no traces of its own -- events would attach to
// whatever span the caller happened to have, or to nothing at all, and the
// question "was it the lock?" would have no structure to answer it with.
func (o *Observer) Start(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := o.tracer.Start(ctx, name)
	return ctx, func(err error) {
		defer span.End()
		if err == nil {
			return
		}
		if cause, refused := storage.CauseOf(err); refused {
			span.SetAttributes(attribute.String("giro.reason", string(cause)))
			span.AddEvent("giro.refused")
			return // deliberately not an error status
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

var _ storage.Tracer = (*Observer)(nil)
