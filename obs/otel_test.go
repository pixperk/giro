package obs

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/pixperk/giro/ledger"
	"github.com/pixperk/giro/storage"
)

// These tests read the metrics back out of a real SDK rather than asserting
// that a method was called. An instrument that is created and never recorded
// to, or recorded to under the wrong attributes, is exactly the bug that
// survives a mock and is found on the day the dashboard is needed.

func collect(t *testing.T) (*Observer, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))

	o, err := New(Options{Meter: provider, SlowLock: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	return o, func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		return rm
	}
}

// find returns the data points of one instrument, or fails naming what was
// actually emitted, because "not found" on its own is the least useful
// possible message here.
func find(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Aggregation {
	t.Helper()
	var seen []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen = append(seen, m.Name)
			if m.Name == name {
				return m.Data
			}
		}
	}
	t.Fatalf("no metric %q; emitted: %v", name, seen)
	return nil
}

func attrsOf(t *testing.T, agg metricdata.Aggregation) []map[string]string {
	t.Helper()
	var out []map[string]string
	switch d := agg.(type) {
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			out = append(out, kv(p.Attributes.ToSlice()))
		}
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			out = append(out, kv(p.Attributes.ToSlice()))
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			out = append(out, kv(p.Attributes.ToSlice()))
		}
	default:
		t.Fatalf("unhandled aggregation %T", agg)
	}
	return out
}

func kv(in []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, a := range in {
		m[string(a.Key)] = a.Value.Emit()
	}
	return m
}

func TestACommitProducesOneSeriesPerAsset(t *testing.T) {
	o, read := collect(t)
	ctx := context.Background()

	// a conversion: one transaction, two assets
	o.Committed(ctx, storage.Commit{
		Ledger: "main", Assets: []ledger.Asset{"USD/2", "USDT/6"},
		Postings: 2, Accounts: 4, Attempts: 1, Took: 12 * time.Millisecond,
		Addresses: []ledger.Address{"treasury:usdt", "external:lp:kraken:USDT", "external:lp:kraken:USD", "ops:usd"},
	})

	got := attrsOf(t, find(t, read(), "giro.transactions"))
	if len(got) != 2 {
		t.Fatalf("%d series, want one per asset: %v", len(got), got)
	}
	var assets []string
	for _, a := range got {
		if a["giro.ledger"] != "main" {
			t.Errorf("ledger label = %q", a["giro.ledger"])
		}
		assets = append(assets, a["giro.asset"])
	}
	slices.Sort(assets)
	if !slices.Equal(assets, []string{"USD/2", "USDT/6"}) {
		t.Errorf("assets = %v, want both sides of the conversion counted", assets)
	}
}

// The rule the whole package is shaped around. If this ever fails, the metrics
// backend is the thing that finds out.
func TestNoMetricIsLabelledByAccountAddress(t *testing.T) {
	o, read := collect(t)
	ctx := context.Background()

	o.Committed(ctx, storage.Commit{
		Ledger: "main", Assets: []ledger.Asset{"USD/2"}, Postings: 1, Accounts: 2,
		Attempts: 1, Took: time.Millisecond,
		Addresses: []ledger.Address{"world", "users:alice"},
	})
	o.Refused(ctx, storage.Refusal{
		Ledger: "main", Reason: storage.CauseInsufficientFunds,
		Asset: "USD/2", Account: "users:bob", Took: time.Millisecond,
	})
	o.Contended(ctx, storage.Contention{
		Ledger: "main", Waited: time.Millisecond,
		Accounts: []ledger.Address{"world", "users:alice"},
	})

	allowed := map[string]bool{"giro.ledger": true, "giro.asset": true, "giro.reason": true}
	rm := read()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, a := range attrsOf(t, m.Data) {
				for k, v := range a {
					if !allowed[k] {
						t.Errorf("%s carries label %q=%q, which is not in the allowed set", m.Name, k, v)
					}
					// the addresses used above, in case a label is renamed
					if strings.Contains(v, "users:") || v == "world" {
						t.Errorf("%s label %q = %q: an address reached a metric", m.Name, k, v)
					}
				}
			}
		}
	}
}

// A refusal must be countable by reason, because the reasons have different
// owners: insufficient_funds is a product event, unknown_asset is a caller bug.
func TestRefusalsAreCountedByReason(t *testing.T) {
	o, read := collect(t)
	ctx := context.Background()

	for _, r := range []storage.RefusalCause{
		storage.CauseInsufficientFunds,
		storage.CauseInsufficientFunds,
		storage.CauseUnknownAsset,
	} {
		o.Refused(ctx, storage.Refusal{Ledger: "main", Reason: r, Asset: "USD/2"})
	}

	sum, ok := find(t, read(), "giro.refusals").(metricdata.Sum[int64])
	if !ok {
		t.Fatal("giro.refusals is not a counter")
	}
	counts := map[string]int64{}
	for _, p := range sum.DataPoints {
		counts[kv(p.Attributes.ToSlice())["giro.reason"]] = p.Value
	}
	if counts["insufficient_funds"] != 2 || counts["unknown_asset"] != 1 {
		t.Errorf("counts = %v, want 2 insufficient_funds and 1 unknown_asset", counts)
	}
}

// A refusal is not a failure, and must not reach the counter that failures do.
func TestARefusalIsNotCountedAsACommit(t *testing.T) {
	o, read := collect(t)
	o.Refused(context.Background(), storage.Refusal{
		Ledger: "main", Reason: storage.CauseInsufficientFunds, Asset: "USD/2",
	})

	for _, sm := range read().ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "giro.transactions" {
				t.Errorf("a refusal incremented giro.transactions")
			}
		}
	}
}

// Waiting on a lock and losing a deadlock are different events with different
// remedies -- shard the hot row, or fix the lock ordering -- so they are
// different instruments.
func TestWaitingAndRestartingAreSeparateSignals(t *testing.T) {
	o, read := collect(t)
	ctx := context.Background()

	o.Contended(ctx, storage.Contention{Ledger: "main", Waited: 5 * time.Millisecond})
	o.Contended(ctx, storage.Contention{Ledger: "main", Attempt: 1, Restarted: true})

	rm := read()
	hist, ok := find(t, rm, "giro.lock.wait").(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("giro.lock.wait is not a histogram")
	}
	if n := hist.DataPoints[0].Count; n != 1 {
		t.Errorf("lock.wait count = %d, want 1: a restart was recorded as a wait", n)
	}
	sum, ok := find(t, rm, "giro.commit.restarts").(metricdata.Sum[int64])
	if !ok {
		t.Fatal("giro.commit.restarts is not a counter")
	}
	if v := sum.DataPoints[0].Value; v != 1 {
		t.Errorf("restarts = %d, want 1", v)
	}
}

// The catalogue is a promise about what exists. A promise nothing checks is
// how a dashboard ends up querying a metric that was renamed a year ago.
func TestTheCatalogueMatchesWhatIsEmitted(t *testing.T) {
	o, read := collect(t)
	ctx := context.Background()

	o.Committed(ctx, storage.Commit{
		Ledger: "main", Assets: []ledger.Asset{"USD/2"}, Postings: 1, Accounts: 2, Attempts: 1,
	})
	o.Refused(ctx, storage.Refusal{Ledger: "main", Reason: storage.CauseOther})
	o.Contended(ctx, storage.Contention{Ledger: "main", Waited: time.Millisecond})
	o.Contended(ctx, storage.Contention{Ledger: "main", Restarted: true})

	emitted := map[string]bool{}
	for _, sm := range read().ScopeMetrics {
		for _, m := range sm.Metrics {
			emitted[m.Name] = true
		}
	}
	for _, m := range Metrics {
		if !emitted[m.Name] {
			t.Errorf("the catalogue lists %q but nothing emits it", m.Name)
		}
		delete(emitted, m.Name)
	}
	for name := range emitted {
		t.Errorf("%q is emitted but absent from the catalogue", name)
	}
}

// Cardinality must be a function of configuration, never of traffic. If this
// ever depends on how many accounts exist, an address has become a label.
func TestCardinalityDoesNotGrowWithTraffic(t *testing.T) {
	one := Cardinality(1, 2)
	if one <= 0 {
		t.Fatalf("Cardinality(1, 2) = %d", one)
	}
	// ten ledgers is ten times the series; a thousand customers is still none
	if ten := Cardinality(10, 2); ten <= one {
		t.Errorf("Cardinality(10,2) = %d, not above Cardinality(1,2) = %d", ten, one)
	}
	// a sanity bound: this is meant to be a small number
	if n := Cardinality(3, 5); n > 200 {
		t.Errorf("3 ledgers and 5 assets produce %d series, which is not the design", n)
	}
}

func TestTheCatalogueRendersWithItsCardinality(t *testing.T) {
	var b strings.Builder
	if err := WriteCatalogue(&b, 2, 3); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"giro.transactions", "giro.lock.wait", "series", "account address"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalogue output does not mention %q:\n%s", want, out)
		}
	}
}

// Spans have to nest, or they answer nothing. A flat list of durations cannot
// say what fraction of a commit was spent waiting on a lock, and that is the
// first question anybody asks about a slow one.
func TestSpansNestSoTheLockIsAFractionOfTheCommit(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	o, err := New(Options{Meter: metric.NewMeterProvider(), Tracer: tp})
	if err != nil {
		t.Fatal(err)
	}

	// the shape the engine produces: commit wraps attempt wraps lock
	ctx, endCommit := o.Start(context.Background(), storage.SpanCommit)
	ctx, endAttempt := o.Start(ctx, storage.SpanAttempt)
	_, endLock := o.Start(ctx, storage.SpanLock)
	endLock(nil)
	endAttempt(nil)
	endCommit(nil)

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("%d spans, want 3", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}

	lock, ok := byName[storage.SpanLock]
	if !ok {
		t.Fatalf("no %s span; got %v", storage.SpanLock, spans)
	}
	attempt := byName[storage.SpanAttempt]
	commit := byName[storage.SpanCommit]

	if lock.Parent.SpanID() != attempt.SpanContext.SpanID() {
		t.Error("the lock span is not a child of the attempt")
	}
	if attempt.Parent.SpanID() != commit.SpanContext.SpanID() {
		t.Error("the attempt span is not a child of the commit")
	}
	// one trace, or they cannot be looked at together
	if lock.SpanContext.TraceID() != commit.SpanContext.TraceID() {
		t.Error("the lock and the commit are in different traces")
	}
}

// The same rule as the metric: a refusal is the ledger working. A span marked
// Error for it would make an ordinary Tuesday look like an incident.
func TestARefusedCommitDoesNotMarkTheSpanFailed(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	o, err := New(Options{Meter: metric.NewMeterProvider(), Tracer: tp})
	if err != nil {
		t.Fatal(err)
	}

	_, end := o.Start(context.Background(), storage.SpanCommit)
	end(&storage.InsufficientFundsError{
		Account: "users:alice", Asset: "USD/2",
		Available: big.NewInt(0), Requested: big.NewInt(100),
	})

	span := exporter.GetSpans()[0]
	if span.Status.Code == codes.Error {
		t.Error("a refusal marked the span as an error")
	}
	var sawReason bool
	for _, a := range span.Attributes {
		if string(a.Key) == "giro.reason" && a.Value.Emit() == "insufficient_funds" {
			sawReason = true
		}
	}
	if !sawReason {
		t.Errorf("the span does not say why it was refused: %v", span.Attributes)
	}
}

// The distinction that makes the above safe: something that is not a refusal
// still has to light up.
func TestARealFailureDoesMarkTheSpanFailed(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	o, err := New(Options{Meter: metric.NewMeterProvider(), Tracer: tp})
	if err != nil {
		t.Fatal(err)
	}

	_, end := o.Start(context.Background(), storage.SpanCommit)
	end(errors.New("connection refused"))

	if code := exporter.GetSpans()[0].Status.Code; code != codes.Error {
		t.Errorf("status = %v, want Error: a broken connection is not a refusal", code)
	}
}
