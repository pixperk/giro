package storage

import (
	"context"
	"errors"
	"time"

	"github.com/pixperk/giro/ledger"
)

// What the engine will tell you about itself, and why it is an interface.
//
// Observability is the one concern that cannot be a layer above the engine the
// way fx and recon are: it has to see inside the commit path, and the commit
// path is here. So it inverts. The engine declares the narrowest thing it
// needs -- somewhere to hand facts -- and every decision about what those
// facts become is made outside it.
//
// That keeps a telemetry framework out of the ledger's dependency graph and
// out of its test suite, which matters more here than it would elsewhere: the
// thing being instrumented is the code that moves money, and it should stay
// possible to read the whole of it.
//
// # Cardinality is the design constraint
//
// A ledger's most natural labels are account addresses, and addresses are
// unbounded. Tagging a metric by "users:alice" produces one time series per
// customer, which is how a metrics backend dies.
//
// So the split is deliberate and an implementation must respect it: the events
// below carry addresses because a *trace* can hold them usefully, and a span
// is per request rather than per series. A *metric* built from these events
// must be keyed on the low cardinality fields only -- ledger, asset, reason --
// and never on an address. The two are marked on each field.

// Observer receives what the engine did. Nil by default, and nothing is
// computed when it is nil, so leaving it unset costs nothing.
//
// Every method is called with the caller's context, so a span opened upstream
// is the parent of anything an implementation opens. Implementations must not
// block: they are called on the commit path, and one that talks to a network
// synchronously has made every transaction wait for a metrics backend.
type Observer interface {
	// Committed is called once per transaction that landed, after it did.
	Committed(ctx context.Context, e Commit)

	// Refused is called once per transaction the ledger declined.
	//
	// A refusal is not an error. "users:alice cannot spend money she does not
	// have" is the ledger doing its job, and folding it into an error rate is
	// how a correct system pages somebody at three in the morning. It is a
	// separate signal for that reason, and Reason is what makes it useful: a
	// rise in insufficient_funds is a product event, a rise in unknown_asset
	// is a bug in a caller, and account_closed is neither.
	Refused(ctx context.Context, e Refusal)

	// Contended is called when a commit waited on row locks or was restarted
	// after losing a deadlock.
	//
	// This is the signal that has no substitute. Every deposit into the ledger
	// takes a row lock on world, which makes it the hottest row in the system
	// by construction -- the designed-in wall this model runs into first. Wait
	// time on it is the number that says when to split it per counterparty,
	// and nothing outside the engine can measure it.
	Contended(ctx context.Context, e Contention)
}

// Commit describes a transaction that landed.
type Commit struct {
	Ledger string // metric label: bounded by how many ledgers you run

	// Assets touched, deduplicated. A conversion touches two.
	Assets []ledger.Asset // metric label: bounded by the asset registry

	Postings int // histogram value, not a label
	Accounts int // histogram value, not a label

	// Attempts is 1 when the commit succeeded first time. Above 1 means it
	// lost a deadlock and started over.
	Attempts int

	// Took is the whole call including every retry and the backoff between
	// them, which is what a caller actually waited.
	Took time.Duration

	// Addresses touched. SPAN ATTRIBUTE ONLY -- never a metric label.
	Addresses []ledger.Address
}

// Refusal describes a transaction the ledger declined.
type Refusal struct {
	Ledger string       // metric label
	Reason RefusalCause // metric label: a closed set, listed below
	Asset  ledger.Asset // metric label, empty when the refusal is not about one
	Took   time.Duration

	// Account the refusal was about. SPAN ATTRIBUTE ONLY -- never a label.
	Account ledger.Address
}

// Contention describes a commit that waited, or that had to start again.
type Contention struct {
	Ledger string // metric label

	// Waited is time spent inside the locking statement. Nonzero on every
	// commit; it is the distribution that matters rather than any one value.
	Waited time.Duration

	// Attempt is zero based, so a value above zero means this commit already
	// lost a deadlock at least once.
	Attempt int

	// Restarted distinguishes ordinary lock waiting from a commit that was
	// killed and had to replay from the beginning.
	Restarted bool

	// Accounts locked, in the order they were taken. SPAN ATTRIBUTE ONLY.
	// This is where the hot row shows itself by name.
	Accounts []ledger.Address
}

// RefusalCause is why the ledger said no.
//
// A closed set, because it is a metric label: an open one would be an address
// or an amount smuggled into a time series. Anything not recognised is
// CauseOther rather than the error's text.
type RefusalCause string

const (
	CauseInsufficientFunds RefusalCause = "insufficient_funds"
	CauseUnexpectedCredit  RefusalCause = "unexpected_credit"
	CauseAccountClosed     RefusalCause = "account_closed"
	CauseUnknownAsset      RefusalCause = "unknown_asset"
	CauseUnboundedSweep    RefusalCause = "unbounded_sweep"
	CauseInvalidPosting    RefusalCause = "invalid_posting"
	CauseContentionGiveUp  RefusalCause = "contention_exhausted"
	CauseOther             RefusalCause = "other"
)

// RefusalCauses is every value CauseOf can return, so a metrics catalogue can
// state its own cardinality rather than discovering it in production.
var RefusalCauses = []RefusalCause{
	CauseInsufficientFunds, CauseUnexpectedCredit, CauseAccountClosed,
	CauseUnknownAsset, CauseUnboundedSweep, CauseInvalidPosting,
	CauseContentionGiveUp, CauseOther,
}

// CauseOf classifies an error into the closed set above.
//
// It also reports whether the error was a refusal at all. A failed connection
// or a context cancellation is not the ledger declining anything, and counting
// it as one would put an infrastructure problem in the same series as a
// customer being short of money.
func CauseOf(err error) (RefusalCause, bool) {
	var (
		funds  *InsufficientFundsError
		credit *UnexpectedCreditError
		closed *AccountClosedError
		asset  *UnknownAssetError
		sweep  *UnboundedSweepError
		post   *PostingError
	)
	switch {
	case err == nil:
		return "", false
	case errors.As(err, &funds):
		return CauseInsufficientFunds, true
	case errors.As(err, &credit):
		return CauseUnexpectedCredit, true
	case errors.As(err, &closed):
		return CauseAccountClosed, true
	case errors.As(err, &asset), errors.Is(err, ErrUnknownAsset):
		return CauseUnknownAsset, true
	case errors.As(err, &sweep):
		return CauseUnboundedSweep, true
	case errors.As(err, &post), errors.Is(err, ErrNoPostings):
		return CauseInvalidPosting, true
	default:
		return "", false
	}
}

// Observe sets the Observer. Not safe to call once the Store is in use, which
// is deliberate: this is composition root wiring, and making it swappable at
// runtime would mean a lock on the commit path to protect a field that is set
// once at startup.
func (s *Store) Observe(o Observer) *Store {
	s.obs = o
	// an implementation that does both is the common case, so wiring it twice
	// should not be something a caller has to remember
	if t, ok := o.(Tracer); ok {
		s.tracer = t
	}
	return s
}

// The emit helpers exist so the call sites read as one line and so the nil
// check lives in exactly one place per event. Building an event allocates --
// the address slices especially -- so the guard has to come first, and callers
// must not assemble anything before calling these.

func (s *Store) observeCommit(ctx context.Context, e Commit) {
	if s.obs == nil {
		return
	}
	e.Ledger = s.ledger
	s.obs.Committed(ctx, e)
}

func (s *Store) observeRefusal(ctx context.Context, e Refusal) {
	if s.obs == nil {
		return
	}
	e.Ledger = s.ledger
	s.obs.Refused(ctx, e)
}

func (s *Store) observeContention(ctx context.Context, e Contention) {
	if s.obs == nil {
		return
	}
	e.Ledger = s.ledger
	s.obs.Contended(ctx, e)
}

// observing reports whether anything is listening, so a call site can skip
// assembling an event nobody will read.
func (s *Store) observing() bool { return s.obs != nil }

// Tracer is the second half of observability, and it is a separate interface
// because it is a different shape.
//
// Observer is told what happened after it happened, which is all a counter
// needs. A span is not that: it has to exist while the work runs, and it has
// to hand back a context so the next span nests inside it. So the engine asks
// for a scope rather than a notification.
//
// Optional. An Observer that only counts things need not implement it, and the
// engine checks once at wiring time rather than per commit.
type Tracer interface {
	// Start opens a unit of work. The returned context replaces the one passed
	// in for the duration, so anything started with it becomes a child. The
	// returned function ends it, and is given the error the work produced --
	// nil on success.
	//
	// A refusal is passed here as an error because the span should say what
	// happened; whether that marks the span failed is the implementation's
	// decision, and for a refusal it should not.
	Start(ctx context.Context, name string) (context.Context, func(err error))
}

// Span names. Written out because they are an interface: somebody will build a
// dashboard that filters on them.
const (
	// SpanCommit covers a whole CommitTransaction call, retries included, so
	// its duration is what the caller actually waited.
	SpanCommit = "giro.commit"

	// SpanAttempt covers one pass through the database transaction. More than
	// one of these inside a commit means it lost a deadlock and started again.
	SpanAttempt = "giro.commit.attempt"

	// SpanLock covers the row locking statement. This is the child that
	// answers "was it the lock?", which is the first question worth asking
	// about a slow commit and the one nothing outside the engine can answer.
	SpanLock = "giro.lock"
)

// TraceWith sets the Tracer. Like Observe, this is composition root wiring and
// is not safe once the Store is in use.
//
// An Observer that also implements Tracer is picked up by Observe on its own,
// so this exists for the case where they are different objects.
func (s *Store) TraceWith(t Tracer) *Store {
	s.tracer = t
	return s
}

// start opens a span, or returns the context unchanged and a no-op when
// nothing is tracing. Every call site can then be two lines with no branch.
func (s *Store) start(ctx context.Context, name string) (context.Context, func(error)) {
	if s.tracer == nil {
		return ctx, func(error) {}
	}
	return s.tracer.Start(ctx, name)
}
