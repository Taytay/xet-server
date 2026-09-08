package storage

// Tests for VerifyingStore: correctness of the verify-on-dedup-hit
// behavior, correct capability passthrough (the decorator must not
// falsely claim a capability its inner store lacks), and race safety
// under concurrent Puts.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// spyStore wraps an in-memory map-backed Store and counts calls, so tests
// can assert exactly how many times each method was invoked (e.g. "Put on
// a new key must not call Get at all").
type spyStore struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	getCalls int
	putCalls int
}

func newSpyStore() *spyStore {
	return &spyStore{blobs: make(map[string][]byte)}
}

func (s *spyStore) Has(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[key]
	return ok, nil
}

func (s *spyStore) Put(_ context.Context, key string, r io.Reader, size int64) (bool, error) {
	s.mu.Lock()
	s.putCalls++
	s.mu.Unlock()

	data, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.blobs[key]; exists {
		return false, nil
	}
	s.blobs[key] = data
	return true, nil
}

func (s *spyStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.getCalls++
	data, ok := s.blobs[key]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *spyStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[offset:end])), nil
}

func (s *spyStore) counts() (get, put int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls, s.putCalls
}

func (s *spyStore) rawBlob(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.blobs[key]...)
}

// --- capability test doubles -------------------------------------------

// minimalStore implements only the core Store interface — none of
// Deleter, Sizer, URLPresigner.
type minimalStore struct{ *spyStore }

// fullCapabilityStore implements Store plus all three optional
// capabilities.
type fullCapabilityStore struct {
	*spyStore
	deleted       []string
	presignCalled bool
}

func (f *fullCapabilityStore) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}
func (f *fullCapabilityStore) TotalBytes(_ context.Context) (int64, error) {
	var total int64
	f.spyStore.mu.Lock()
	for _, b := range f.spyStore.blobs {
		total += int64(len(b))
	}
	f.spyStore.mu.Unlock()
	return total, nil
}
func (f *fullCapabilityStore) PresignGet(_ context.Context, key string, _ int) (string, error) {
	f.presignCalled = true
	return "https://example.invalid/" + key, nil
}

// deleterAndSizerOnlyStore implements Deleter and Sizer but not
// URLPresigner — the case that must NOT be mistaken for full capability.
type deleterAndSizerOnlyStore struct{ *spyStore }

func (d *deleterAndSizerOnlyStore) Delete(_ context.Context, key string) error { return nil }
func (d *deleterAndSizerOnlyStore) TotalBytes(_ context.Context) (int64, error) {
	return 0, nil
}

func TestVerifyingStore_NewKey_PassesThroughWithNoExtraReads(t *testing.T) {
	spy := newSpyStore()
	vs := NewVerifyingStore(&minimalStore{spy})

	data := []byte("brand new content")
	written, err := vs.Put(context.Background(), "new-key", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !written {
		t.Error("written = false, want true for a genuinely new key")
	}

	getCalls, putCalls := spy.counts()
	if getCalls != 0 {
		t.Errorf("Get calls = %d, want 0 — a new key must not trigger any read for comparison", getCalls)
	}
	if putCalls != 1 {
		t.Errorf("Put calls = %d, want exactly 1", putCalls)
	}
}

func TestVerifyingStore_DedupHit_MatchingContent(t *testing.T) {
	spy := newSpyStore()
	vs := NewVerifyingStore(&minimalStore{spy})
	ctx := context.Background()

	data := []byte("identical content, uploaded twice")
	if _, err := vs.Put(ctx, "k", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}

	written, err := vs.Put(ctx, "k", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	if written {
		t.Error("written = true on second Put(), want false (dedup hit, verified match)")
	}

	if got := spy.rawBlob("k"); !bytes.Equal(got, data) {
		t.Errorf("stored blob = %q, want %q (unchanged)", got, data)
	}
}

func TestVerifyingStore_DedupHit_MismatchedContent_LeavesOriginalUntouched(t *testing.T) {
	spy := newSpyStore()
	vs := NewVerifyingStore(&minimalStore{spy})
	ctx := context.Background()

	original := []byte("the real, correctly-stored content")
	if _, err := vs.Put(ctx, "k", bytes.NewReader(original), int64(len(original))); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}

	// Same length, different bytes — simulates a hash collision or
	// corrupted-on-disk scenario, not a truncation.
	different := []byte("the WRONG but same-length content!")
	if len(different) != len(original) {
		t.Fatalf("test fixture bug: lengths differ (%d vs %d)", len(different), len(original))
	}

	_, err := vs.Put(ctx, "k", bytes.NewReader(different), int64(len(different)))
	if err == nil {
		t.Fatal("Put() error = nil, want ErrContentMismatch")
	}
	if !errors.Is(err, ErrContentMismatch) {
		t.Errorf("Put() error = %v, want errors.Is(err, ErrContentMismatch)", err)
	}

	if got := spy.rawBlob("k"); !bytes.Equal(got, original) {
		t.Errorf("stored blob = %q after a mismatch, want unchanged original %q", got, original)
	}
}

func TestVerifyingStore_DedupHit_MismatchNearEnd_StillDetected(t *testing.T) {
	spy := newSpyStore()
	vs := NewVerifyingStore(&minimalStore{spy})
	ctx := context.Background()

	size := verifyChunkSize*3 + 100
	original := bytes.Repeat([]byte("A"), size)
	if _, err := vs.Put(ctx, "k", bytes.NewReader(original), int64(len(original))); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}

	different := append([]byte(nil), original...)
	different[size-1] = 'B' // last byte differs

	_, err := vs.Put(ctx, "k", bytes.NewReader(different), int64(len(different)))
	if !errors.Is(err, ErrContentMismatch) {
		t.Errorf("Put() error = %v, want errors.Is(err, ErrContentMismatch) for a mismatch in the final chunk", err)
	}
}

func TestVerifyingStore_CapabilityPassthrough_MinimalStoreHasNone(t *testing.T) {
	vs := NewVerifyingStore(&minimalStore{newSpyStore()})

	if _, ok := vs.(Deleter); ok {
		t.Error("wrapped minimalStore satisfies Deleter, want false")
	}
	if _, ok := vs.(Sizer); ok {
		t.Error("wrapped minimalStore satisfies Sizer, want false")
	}
	if _, ok := vs.(URLPresigner); ok {
		t.Error("wrapped minimalStore satisfies URLPresigner, want false")
	}
}

func TestVerifyingStore_CapabilityPassthrough_FullCapabilityStore(t *testing.T) {
	inner := &fullCapabilityStore{spyStore: newSpyStore()}
	vs := NewVerifyingStore(inner)

	deleter, ok := vs.(Deleter)
	if !ok {
		t.Fatal("wrapped fullCapabilityStore does not satisfy Deleter, want true")
	}
	if err := deleter.Delete(context.Background(), "some-key"); err != nil {
		t.Errorf("Delete() error = %v", err)
	}
	if len(inner.deleted) != 1 || inner.deleted[0] != "some-key" {
		t.Errorf("inner.deleted = %v, want [\"some-key\"] — Delete must forward to inner", inner.deleted)
	}

	sizer, ok := vs.(Sizer)
	if !ok {
		t.Fatal("wrapped fullCapabilityStore does not satisfy Sizer, want true")
	}
	if _, err := sizer.TotalBytes(context.Background()); err != nil {
		t.Errorf("TotalBytes() error = %v", err)
	}

	presigner, ok := vs.(URLPresigner)
	if !ok {
		t.Fatal("wrapped fullCapabilityStore does not satisfy URLPresigner, want true")
	}
	if _, err := presigner.PresignGet(context.Background(), "some-key", 60); err != nil {
		t.Errorf("PresignGet() error = %v", err)
	}
	if !inner.presignCalled {
		t.Error("PresignGet did not forward to inner")
	}
}

func TestVerifyingStore_CapabilityPassthrough_PartialCapabilityNotOverclaimed(t *testing.T) {
	// Implements Deleter+Sizer but NOT URLPresigner — the exact shape a
	// bug would most easily miss (three capabilities means 8 combinations
	// to get right, not just "all or nothing").
	inner := &deleterAndSizerOnlyStore{newSpyStore()}
	vs := NewVerifyingStore(inner)

	if _, ok := vs.(Deleter); !ok {
		t.Error("wrapped store does not satisfy Deleter, want true")
	}
	if _, ok := vs.(Sizer); !ok {
		t.Error("wrapped store does not satisfy Sizer, want true")
	}
	if _, ok := vs.(URLPresigner); ok {
		t.Error("wrapped store satisfies URLPresigner, want false — inner does not implement it")
	}
}

func TestVerifyingStore_ConcurrentPuts_NoRaceNoGoroutineLeak(t *testing.T) {
	spy := newSpyStore()
	vs := NewVerifyingStore(&minimalStore{spy})
	ctx := context.Background()

	before := runtime.NumGoroutine()

	const iterations = 200
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i%10) // heavy key reuse to force real dedup-hit races
			data := []byte(fmt.Sprintf("content for group %d", i%10))
			vs.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
		}(i)
	}
	wg.Wait()

	// Goroutines spawned by compareStreams are short-lived and should
	// have exited by the time Put returns; allow a brief settle window
	// before asserting, to avoid flaking on GC/scheduler timing rather
	// than an actual leak.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+5 { // small slack for test runtime goroutines
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine count settled at %d, started at %d — possible leak from compareStreams", runtime.NumGoroutine(), before)
}
