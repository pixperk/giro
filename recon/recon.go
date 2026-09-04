// Package recon compares what the ledger believes happened against what the
// outside world says happened.
//
// Every check inside the ledger proves the book is consistent with itself.
// None of them can tell you the money is actually in the bank. That question
// can only be answered by asking the bank, and this is the layer that asks.
//
// # The shape
//
// A Source is anything that will tell you what it saw: an exchange, a bank, a
// chain. It has one job, producing normalised lines, and it knows nothing
// about the ledger. Ingest stages those lines, Match pairs them with
// transactions, and what does not pair is left for a person.
//
// Providers live outside this package and outside giro. The moment a ledger
// ships a Kraken client it has stopped being a general ledger, so what ships
// here is the interface and a worked example in the tests, which is what
// proves the interface is sufficient rather than merely plausible.
//
// # It never writes a posting
//
// Nothing here moves money or changes a balance. A reconciler that could
// correct the book would be a second way for money to move, and the value of
// this layer is entirely in it having an independent opinion. What it produces
// is evidence and a queue of things a person should look at.
package recon

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/pixperk/giro/ledger"
)

// Direction is which way a line says the money went, from our side.
//
// A magnitude and a direction rather than a signed amount, for the same reason
// a posting has no sign: one way of saying a thing cannot disagree with
// itself.
type Direction string

const (
	// Unknown is a source that does not say. The line can still match; it just
	// skips the check that would catch an outbound wire pairing with an
	// inbound movement of the same size and reference.
	Unknown Direction = ""
	In      Direction = "in"
	Out     Direction = "out"
)

// Record is one statement line, as the source reported it.
type Record struct {
	// ID is the source's own line identifier, and staging is idempotent on it.
	// Ingesting the same file twice stages nothing new, which is what makes an
	// ingest safe to retry after a timeout that may or may not have landed.
	ID string

	// Reference is the match key: a wire reference, a trade id, a transaction
	// hash. Empty when the source gives none, in which case the line cannot
	// match and exists to be looked at.
	Reference string

	Asset ledger.Asset
	// Amount is a positive magnitude in the asset's minor units.
	Amount *big.Int

	Direction  Direction
	OccurredAt time.Time

	// Raw is the original line, kept for audit and for replaying a match rule
	// against what was actually received rather than against what we decided
	// it meant.
	Raw []byte
}

func (r Record) validate() error {
	switch {
	case r.ID == "":
		return fmt.Errorf("record has no id, so it cannot be staged idempotently")
	case !r.Asset.Valid():
		return fmt.Errorf("record %q: invalid asset %q", r.ID, r.Asset)
	case r.Amount == nil || r.Amount.Sign() <= 0:
		return fmt.Errorf("record %q: amount must be a positive magnitude", r.ID)
	case r.Direction != Unknown && r.Direction != In && r.Direction != Out:
		return fmt.Errorf("record %q: direction must be in, out, or empty", r.ID)
	}
	return nil
}

// A Source is somewhere outside the ledger that will say what it thinks
// happened.
//
// Implementations belong to whoever runs the ledger, not to giro. One is
// usually a few dozen lines: call an API, map the response onto Record, return
// it. It touches no database and knows nothing about accounts.
type Source interface {
	// ID is stable and identifies this source for as long as the ledger
	// exists, because staged lines are keyed by it. "kraken", not "Kraken
	// Exchange (prod)".
	ID() string

	// Name is for people.
	Name() string

	// Fetch returns what this source says happened since a point in time.
	// Returning the same line twice is harmless and expected: staging is
	// idempotent, so overlapping windows are the safe way to page through a
	// statement rather than something to avoid.
	Fetch(ctx context.Context, since time.Time) ([]Record, error)
}

// Boundary reports whether an address faces outward, and it is configuration
// rather than something the ledger knows.
//
// Matching needs it so that a line saying "money in" pairs with a movement
// that came in. Without it an outbound wire reconciles against an inbound
// movement of the same size and reference, which is a real mistake and an easy
// one.
//
// The ledger deliberately has no opinion about what an address means, so the
// convention is passed in. giro's own is external:, and it is the default,
// but a deployment that names its edges differently only has to say so.
type Boundary func(ledger.Address) bool

// Prefix is the usual Boundary: an address facing outward is named for the
// counterparty it faces.
func Prefix(p string) Boundary {
	return func(a ledger.Address) bool { return strings.HasPrefix(string(a), p) }
}

// DefaultBoundaryPrefix is giro's own convention. An account per counterparty
// and asset -- external:lp:kraken:USD -- so each one's balance is directly
// comparable to that counterparty's own statement.
const DefaultBoundaryPrefix = "external:"

// Config is how a deployment says what its conventions are. The zero value
// works and assumes giro's.
type Config struct {
	// Boundary decides which addresses face outward. Nil means
	// Prefix(DefaultBoundaryPrefix).
	Boundary Boundary

	// Tolerance is how far a line's amount may sit from the movement it is
	// paired with and still count as matched rather than as a variance, in
	// minor units. Zero means exact, which is the right default: a bank that
	// is a penny out is telling you something.
	Tolerance *big.Int
}
