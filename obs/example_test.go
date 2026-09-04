package obs_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pixperk/giro/obs"
	"github.com/pixperk/giro/storage"
)

// Wiring telemetry into an embedded ledger, in full.
//
// This is the whole integration: build an Observer, hand it to the Store. The
// engine knows nothing else about it, and a Store with no Observer behaves
// exactly as it did before this package existed.
func Example() {
	ctx := context.Background()

	// Setup wires the providers and the exporter. Where the data goes is
	// decided by the standard OTEL_* environment variables, so giro has no
	// configuration surface of its own to learn.
	observer, shutdown, err := obs.Setup(ctx, "giro", obs.Options{
		// a lock wait above this also lands on the trace, naming the accounts.
		// the histogram says contention is happening; the span says where.
		SlowLock: 50 * time.Millisecond,
	})
	if err != nil {
		// telemetry that silently did not start is worse than none: the
		// absence of a signal would then mean either "nothing happened" or
		// "nothing was watching", and those wake different people.
		panic(err)
	}
	// metrics are batched, and a process that exits without flushing loses the
	// interval it was in the middle of, which is reliably the interesting one
	defer func() { _ = shutdown(ctx) }()

	// your *pgxpool.Pool; nil here because this example never queries
	var pool *pgxpool.Pool
	store := storage.New(pool, "main").Observe(observer)

	_ = store
	fmt.Println("observed")
	// Output: observed
}

// WriteCatalogue prints what will be emitted and what it will cost, so the
// cardinality is arithmetic done before deploying rather than a bill found out
// about afterwards. Two ledgers and three registered assets:
func ExampleWriteCatalogue() {
	_ = obs.WriteCatalogue(os.Stdout, 2, 3)
	// Output is a table; see obs/README.md. Deliberately unchecked here so the
	// column widths are free to improve without breaking a test.
}
