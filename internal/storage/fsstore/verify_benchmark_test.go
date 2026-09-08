package fsstore

// Benchmarks quantifying storage.VerifyingStore's actual cost when
// wrapping a real fsstore.Store (real disk I/O, not an in-memory test
// double): dedup-hit latency with verification on vs. off at several
// sizes, plus confirmation that bail-at-first-mismatch genuinely scales
// with where the mismatch is positioned rather than always paying
// full-object comparison cost regardless of where the difference is.
// Lives here rather than internal/storage itself because storage's own
// package can't import fsstore without an import cycle (fsstore already
// imports storage for the Store interface it implements).

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"xet-server/internal/storage"
)

func benchmarkDedupHit(b *testing.B, size int, verify bool) {
	fs, err := New(b.TempDir())
	if err != nil {
		b.Fatalf("New() error = %v", err)
	}
	var store storage.Store = fs
	if verify {
		store = storage.NewVerifyingStore(fs)
	}

	data := make([]byte, size)
	rand.Read(data)
	ctx := context.Background()

	if _, err := store.Put(ctx, "bench-key", bytes.NewReader(data), int64(size)); err != nil {
		b.Fatalf("priming Put() error = %v", err)
	}

	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		written, err := store.Put(ctx, "bench-key", bytes.NewReader(data), int64(size))
		if err != nil {
			b.Fatalf("Put() error = %v", err)
		}
		if written {
			b.Fatal("written = true on a dedup hit, want false")
		}
	}
}

func BenchmarkDedupHit_1MB_VerifyOff(b *testing.B)  { benchmarkDedupHit(b, 1<<20, false) }
func BenchmarkDedupHit_1MB_VerifyOn(b *testing.B)   { benchmarkDedupHit(b, 1<<20, true) }
func BenchmarkDedupHit_16MB_VerifyOff(b *testing.B) { benchmarkDedupHit(b, 16<<20, false) }
func BenchmarkDedupHit_16MB_VerifyOn(b *testing.B)  { benchmarkDedupHit(b, 16<<20, true) }
func BenchmarkDedupHit_64MB_VerifyOff(b *testing.B) { benchmarkDedupHit(b, 64<<20, false) }
func BenchmarkDedupHit_64MB_VerifyOn(b *testing.B)  { benchmarkDedupHit(b, 64<<20, true) }

// benchmarkMismatchAtPosition measures how long a rejected Put takes when
// the mismatch is planted at a given fraction of the object's length —
// proving bail-at-first-mismatch cost scales with mismatch position
// rather than always reading (and comparing) the full object regardless
// of where the difference actually is.
func benchmarkMismatchAtPosition(b *testing.B, size int, fractionPosition float64) {
	fs, err := New(b.TempDir())
	if err != nil {
		b.Fatalf("New() error = %v", err)
	}
	store := storage.NewVerifyingStore(fs)
	ctx := context.Background()

	original := make([]byte, size)
	rand.Read(original)
	if _, err := store.Put(ctx, "bench-key", bytes.NewReader(original), int64(size)); err != nil {
		b.Fatalf("priming Put() error = %v", err)
	}

	mismatchOffset := int(float64(size) * fractionPosition)
	if mismatchOffset >= size {
		mismatchOffset = size - 1
	}
	different := append([]byte(nil), original...)
	different[mismatchOffset] ^= 0xFF

	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.Put(ctx, "bench-key", bytes.NewReader(different), int64(size))
		if err == nil {
			b.Fatal("Put() error = nil, want ErrContentMismatch")
		}
	}
}

func BenchmarkMismatchDetection_16MB_At1Percent(b *testing.B) {
	benchmarkMismatchAtPosition(b, 16<<20, 0.01)
}
func BenchmarkMismatchDetection_16MB_At50Percent(b *testing.B) {
	benchmarkMismatchAtPosition(b, 16<<20, 0.50)
}
func BenchmarkMismatchDetection_16MB_At99Percent(b *testing.B) {
	benchmarkMismatchAtPosition(b, 16<<20, 0.99)
}
