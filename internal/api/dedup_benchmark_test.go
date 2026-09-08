package api

// Dedup speed benchmarks: upload throughput for a highly-duplicate file
// (most chunks already stored, dedup fast-path dominates) versus a fully
// unique file of the same size (every chunk is new, full hash+store cost
// paid for each). The gap between these two quantifies what dedup
// actually buys in wall-clock terms, not just in bytes-stored — the
// dedup fast-path (storage.Store.Has-style short-circuit inside Put) must
// still chunk and hash every byte either way, so this also shows the
// floor cost that never goes away regardless of duplication.

import (
	"bytes"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func init() {
	// Suppress the per-upload slog.Info line (see handleUpload) during
	// benchmarks — B.N can run into the hundreds of iterations, and
	// interleaved log lines make -bench output unreadable without
	// affecting the measured operation itself.
	slog.SetLogLoggerLevel(slog.LevelWarn)
}

func repeatedPatternBytes(size int) []byte {
	pattern := []byte("The quick brown fox jumps over the lazy dog, again and again. ")
	out := make([]byte, size)
	for i := 0; i < size; i += len(pattern) {
		copy(out[i:], pattern)
	}
	return out
}

func randomContentBytes(size int) []byte {
	b := make([]byte, size)
	rand.Read(b)
	return b
}

// benchmarkUpload uploads the same content repeatedly to a fresh server
// per b.N-loop... but that would keep re-chunking a growing dedup index
// across iterations, which is exactly what we want to measure for the
// "highly duplicate" case (post-first-upload, every subsequent chunk is
// already known) — so unlike a typical benchmark, priming happens once
// outside the timed loop specifically to reach steady-state dedup, and
// each timed iteration re-uploads the identical bytes to measure the
// fully-deduplicated cost.
func benchmarkUploadSteadyState(b *testing.B, data []byte) {
	srv, err := New(b.TempDir())
	if err != nil {
		b.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Prime: first upload populates the chunk store, so every subsequent
	// upload of the same bytes is a full-dedup steady state.
	primeResp, err := http.Post(ts.URL+"/upload", "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		b.Fatalf("priming upload error = %v", err)
	}
	primeResp.Body.Close()

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Post(ts.URL+"/upload", "application/octet-stream", bytes.NewReader(data))
		if err != nil {
			b.Fatalf("upload error = %v", err)
		}
		resp.Body.Close()
	}
}

// benchmarkUploadAlwaysNew uploads distinct random content each
// iteration — every chunk is new on every call, so this measures the
// no-dedup-possible floor: chunk + hash + store, every byte, every time.
func benchmarkUploadAlwaysNew(b *testing.B, size int) {
	srv, err := New(b.TempDir())
	if err != nil {
		b.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Post(ts.URL+"/upload", "application/octet-stream", bytes.NewReader(randomContentBytes(size)))
		if err != nil {
			b.Fatalf("upload error = %v", err)
		}
		resp.Body.Close()
	}
}

func BenchmarkUpload_1MB_FullyDeduplicated(b *testing.B) {
	benchmarkUploadSteadyState(b, repeatedPatternBytes(1<<20))
}

func BenchmarkUpload_1MB_AlwaysNew(b *testing.B) {
	benchmarkUploadAlwaysNew(b, 1<<20)
}

func BenchmarkUpload_16MB_FullyDeduplicated(b *testing.B) {
	benchmarkUploadSteadyState(b, repeatedPatternBytes(16<<20))
}

func BenchmarkUpload_16MB_AlwaysNew(b *testing.B) {
	benchmarkUploadAlwaysNew(b, 16<<20)
}
