package eviction

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu      sync.Mutex
	sizes   map[string]int64
	deleted []string
	delErr  error
}

func newFakeStore(sizes map[string]int64) *fakeStore {
	return &fakeStore{sizes: sizes}
}

func (f *fakeStore) TotalBytes(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, sz := range f.sizes {
		total += sz
	}
	return total, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sizes, key)
	f.deleted = append(f.deleted, key)
	return nil
}

type fakeRegistry struct {
	mu         sync.Mutex
	candidates []Candidate
	forgotten  []string
}

func (r *fakeRegistry) EvictionCandidates() []Candidate {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Candidate, len(r.candidates))
	copy(out, r.candidates)
	return out
}

func (r *fakeRegistry) ForgetKey(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgotten = append(r.forgotten, key)
	for i, c := range r.candidates {
		if c.Key == key {
			r.candidates = append(r.candidates[:i], r.candidates[i+1:]...)
			break
		}
	}
}

func TestSweepOnce_UnderBudget_NoEviction(t *testing.T) {
	store := newFakeStore(map[string]int64{"a": 50})
	reg := &fakeRegistry{candidates: []Candidate{{Key: "a", Size: 50, LastAccess: time.Now()}}}
	sw := New(store, reg, 100, time.Minute)

	sw.sweepOnce(context.Background())

	if len(store.deleted) != 0 {
		t.Errorf("deleted = %v, want none (under budget)", store.deleted)
	}
	stats := sw.Stats()
	if stats.EvictionsTotal != 0 {
		t.Errorf("EvictionsTotal = %d, want 0", stats.EvictionsTotal)
	}
}

func TestSweepOnce_OverBudget_EvictsLeastRecentlyAccessedFirst(t *testing.T) {
	now := time.Now()
	store := newFakeStore(map[string]int64{"old": 40, "mid": 40, "new": 40})
	reg := &fakeRegistry{candidates: []Candidate{
		{Key: "new", Size: 40, LastAccess: now},
		{Key: "old", Size: 40, LastAccess: now.Add(-time.Hour)},
		{Key: "mid", Size: 40, LastAccess: now.Add(-time.Minute)},
	}}
	// Budget 100; total 120. Evicting "old" (the oldest) alone brings total
	// to 80, which is under budget, so eviction should stop there.
	sw := New(store, reg, 100, time.Minute)

	sw.sweepOnce(context.Background())

	if len(store.deleted) != 1 || store.deleted[0] != "old" {
		t.Fatalf("deleted = %v, want exactly [old]", store.deleted)
	}
	if len(reg.forgotten) != 1 || reg.forgotten[0] != "old" {
		t.Errorf("forgotten = %v, want exactly [old]", reg.forgotten)
	}
	stats := sw.Stats()
	if stats.EvictionsTotal != 1 || stats.BytesFreedTotal != 40 {
		t.Errorf("stats = %+v, want EvictionsTotal=1 BytesFreedTotal=40", stats)
	}
}

func TestSweepOnce_EvictsUntilUnderBudgetOrCandidatesExhausted(t *testing.T) {
	now := time.Now()
	store := newFakeStore(map[string]int64{"a": 40, "b": 40, "c": 40})
	reg := &fakeRegistry{candidates: []Candidate{
		{Key: "a", Size: 40, LastAccess: now.Add(-3 * time.Hour)},
		{Key: "b", Size: 40, LastAccess: now.Add(-2 * time.Hour)},
		{Key: "c", Size: 40, LastAccess: now.Add(-1 * time.Hour)},
	}}
	// Budget 30; total 120. Every candidate must be evicted to even
	// attempt reaching budget, and the sweep should not crash/loop forever
	// when it still can't get under budget after evicting everything safe.
	sw := New(store, reg, 30, time.Minute)

	sw.sweepOnce(context.Background())

	if len(store.deleted) != 3 {
		t.Fatalf("deleted = %v, want all 3 candidates evicted", store.deleted)
	}
}

func TestSweepOnce_ZeroBudgetDisablesEviction(t *testing.T) {
	store := newFakeStore(map[string]int64{"a": 1000})
	reg := &fakeRegistry{candidates: []Candidate{{Key: "a", Size: 1000}}}
	sw := New(store, reg, 0, time.Minute)

	sw.sweepOnce(context.Background())

	if len(store.deleted) != 0 {
		t.Errorf("deleted = %v, want none (budget disabled)", store.deleted)
	}
}

func TestSweepOnce_InFlightCandidatesAreNeverOffered(t *testing.T) {
	// EvictionCandidates itself is responsible for excluding in-flight
	// keys (see casserver.Server.EvictionCandidates) — this test just
	// confirms the Sweeper only ever acts on what the Registry offers it,
	// never reaching into storage independently.
	store := newFakeStore(map[string]int64{"safe": 40, "in-flight-not-offered": 200})
	reg := &fakeRegistry{candidates: []Candidate{
		{Key: "safe", Size: 40, LastAccess: time.Now().Add(-time.Hour)},
		// "in-flight-not-offered" deliberately absent from candidates.
	}}
	sw := New(store, reg, 10, time.Minute)

	sw.sweepOnce(context.Background())

	for _, key := range store.deleted {
		if key == "in-flight-not-offered" {
			t.Fatal("evicted a key the Registry never offered as a candidate")
		}
	}
}

func TestStats_ReflectsBudget(t *testing.T) {
	sw := New(newFakeStore(nil), &fakeRegistry{}, 12345, time.Minute)
	stats := sw.Stats()
	if stats.BudgetBytes != 12345 {
		t.Errorf("BudgetBytes = %d, want 12345", stats.BudgetBytes)
	}
}
