package storage

// What the write path actually costs, and where it stops scaling.
//
// The design says writes to one ledger are serialised, because every commit
// takes an exclusive lock on that ledger's row to allocate ids and read the
// chain tip, and holds it until commit. These measure that rather than assume
// it.
//
// Run them with a bounded iteration count, for example:
//
//	go test -bench . -benchtime 200x -run '^$' ./storage/

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/pixperk/giro/ledger"
)

func transfer(from, to ledger.Address, amount int64) ledger.Postings {
	return ledger.Postings{
		{Source: from, Destination: to, Asset: "USD/2", Amount: n(amount)},
	}
}

// one caller at a time: the floor, with no contention at all.
func BenchmarkCommitSequential(b *testing.B) {
	ctx, s, _ := testStore(b)
	fund(b, ctx, s, "payer", 1_000_000_000)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := s.CommitTransaction(ctx, transfer("payer", "payee", 1), CommitOptions{}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
}

// every caller spends from the same account, so they queue on its volume row
// as well as on the ledgers row. this is the worst realistic case: a treasury
// or fee account touched by most traffic.
func BenchmarkCommitHotAccount(b *testing.B) {
	for _, callers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			ctx, s, _ := testStore(b)
			fund(b, ctx, s, "payer", 1_000_000_000)

			var seq atomic.Int64
			b.ResetTimer()
			b.SetParallelism(callers)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					dst := ledger.Address(fmt.Sprintf("payee:%d", seq.Add(1)))
					if _, err := s.CommitTransaction(ctx, transfer("payer", dst, 1), CommitOptions{}); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
			b.ReportMetric(float64(s.Retries()), "retries")
		})
	}
}

// disjoint accounts, so the only thing shared is the ledgers row. if
// throughput here matches the hot account case, the ledgers row is the
// bottleneck rather than account contention.
func BenchmarkCommitDisjointAccounts(b *testing.B) {
	for _, callers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			ctx, s, _ := testStore(b)

			var seq atomic.Int64
			b.ResetTimer()
			b.SetParallelism(callers)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := seq.Add(1)
					src, dst := ledger.Address(fmt.Sprintf("a:%d", i)), ledger.Address(fmt.Sprintf("b:%d", i))
					if _, err := s.CommitTransaction(ctx,
						ledger.Postings{
							{Source: "world", Destination: src, Asset: "USD/2", Amount: n(10)},
							{Source: src, Destination: dst, Asset: "USD/2", Amount: n(10)},
						}, CommitOptions{}); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
		})
	}
}

// separate ledgers share no row at all, which is the escape hatch the design
// claims: writes scale by adding ledgers, not by adding callers to one.
func BenchmarkCommitAcrossLedgers(b *testing.B) {
	const ledgers = 8
	ctx, _, pool := testStore(b)

	stores := make([]*Store, ledgers)
	for i := range stores {
		name := fmt.Sprintf("l%d", i)
		if _, err := pool.Exec(ctx, "insert into ledgers (name) values ($1)", name); err != nil {
			b.Fatal(err)
		}
		stores[i] = New(pool, name)
		fund(b, ctx, stores[i], "payer", 1_000_000_000)
	}

	var seq atomic.Int64
	b.ResetTimer()
	b.SetParallelism(ledgers)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := seq.Add(1)
			s := stores[i%ledgers]
			if _, err := s.CommitTransaction(ctx,
				transfer("payer", ledger.Address(fmt.Sprintf("payee:%d", i)), 1), CommitOptions{}); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
}

// one batch of fifty against fifty single commits. the batch pays for the
// ledgers row lock once instead of fifty times.
func BenchmarkBatchVersusSingles(b *testing.B) {
	const size = 50

	b.Run("batch", func(b *testing.B) {
		ctx, s, _ := testStore(b)
		fund(b, ctx, s, "payer", 1_000_000_000)

		items := make([]BatchItem, size)
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			for j := range items {
				items[j] = BatchItem{Postings: transfer("payer", ledger.Address(fmt.Sprintf("p:%d:%d", i, j)), 1)}
			}
			if _, err := s.CommitBatch(ctx, items, CommitOptions{}); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "tx/s")
	})

	b.Run("singles", func(b *testing.B) {
		ctx, s, _ := testStore(b)
		fund(b, ctx, s, "payer", 1_000_000_000)

		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			for j := range size {
				if _, err := s.CommitTransaction(ctx,
					transfer("payer", ledger.Address(fmt.Sprintf("p:%d:%d", i, j)), 1), CommitOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "tx/s")
	})
}

// reads should not be affected by any of the above.
func BenchmarkReads(b *testing.B) {
	ctx, s, _ := testStore(b)
	fund(b, ctx, s, "payer", 1_000_000)
	for range 200 {
		mustCommit(b, ctx, s, "payer", "payee", 1)
	}

	b.Run("balances", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.GetBalances(ctx, "payee"); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("statement page", func(b *testing.B) {
		q := ListMovesQuery{Filter: MoveFilter{Address: "payee"}, Limit: 25}
		for b.Loop() {
			if _, err := s.ListMoves(ctx, q); err != nil {
				b.Fatal(err)
			}
		}
	})
}
