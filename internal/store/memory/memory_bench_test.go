package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// benchmarkLookup measures a status poll (Lookup) against a queue of n
// waiters — the hot path while a rush is being absorbed.
func benchmarkLookup(b *testing.B, n int) {
	s := New()
	bctx := context.Background()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("u%d", i)
		if _, err := s.Enqueue(bctx, ids[i], now); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Lookup(bctx, ids[i%n], now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLookupQueue1k(b *testing.B)   { benchmarkLookup(b, 1_000) }
func BenchmarkLookupQueue10k(b *testing.B)  { benchmarkLookup(b, 10_000) }
func BenchmarkLookupQueue100k(b *testing.B) { benchmarkLookup(b, 100_000) }
