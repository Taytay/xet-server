// Package eviction implements a storage-budget sweep: periodically check
// how many bytes a storage.Store backend actually holds, and if over a
// configured budget, delete the least-recently-accessed blobs until back
// under budget. This exists so a server left running against unbounded
// client uploads doesn't grow its storage without limit — a deliberate,
// bounded, observable policy rather than no eviction at all.
//
// This package knows nothing about xorbs, shards, or the CAS protocol; it
// operates purely on opaque storage keys. The metadata needed to pick
// eviction victims (last-access time, size, and whether a key is
// currently unsafe to evict because a download is in flight) is owned by
// whatever registers keys with the store — see casserver.Server's
// EvictionCandidates/ForgetKey methods for the CAS server's
// implementation of the Registry interface this package consumes.
package eviction

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"xet-server/internal/storage"
)

// Candidate is one blob a Registry reports as eligible for eviction.
type Candidate struct {
	Key        string
	LastAccess time.Time
	Size       int64
}

// Registry is implemented by whatever owns key lifecycle metadata (e.g.
// casserver.Server) so the Sweeper can pick eviction victims and keep that
// owner's own indices in sync after a successful delete.
type Registry interface {
	// EvictionCandidates returns every key currently safe to evict (e.g.
	// excluding a key with an in-flight download), each with its last
	// access time and size.
	EvictionCandidates() []Candidate

	// ForgetKey removes key from the registry's own indices. Called only
	// after Store.Delete(key) has already succeeded.
	ForgetKey(key string)
}

// Store is the subset of storage.Store's optional capabilities the
// Sweeper needs: a way to measure total bytes used, and a way to remove a
// blob. A backend that implements storage.Store but not both of these
// (e.g. it has no efficient way to enumerate/size its contents) simply
// can't be swept — see NewSweeper.
type Store interface {
	storage.Sizer
	storage.Deleter
}

// Stats reports the Sweeper's cumulative effect, for an operator-facing
// endpoint to expose so the eviction policy's behavior is observable
// rather than merely assumed.
type Stats struct {
	BudgetBytes     int64 `json:"budget_bytes"`
	EvictionsTotal  int64 `json:"evictions_total"`
	BytesFreedTotal int64 `json:"bytes_freed_total"`
}

// Sweeper periodically deletes least-recently-accessed blobs once total
// storage use exceeds Budget bytes. A zero-value Budget disables eviction
// (Run still ticks, but sweepOnce is a no-op) — callers that want to
// avoid even the periodic TotalBytes() call when eviction is disabled
// should simply not start a Sweeper at all.
type Sweeper struct {
	Store    Store
	Registry Registry
	Budget   int64
	Interval time.Duration

	mu              sync.Mutex
	evictionsTotal  int64
	bytesFreedTotal int64
}

// New creates a Sweeper. budgetBytes <= 0 means unlimited (Run becomes a
// no-op sweep loop; prefer not starting it at all in that case).
func New(store Store, registry Registry, budgetBytes int64, interval time.Duration) *Sweeper {
	return &Sweeper{Store: store, Registry: registry, Budget: budgetBytes, Interval: interval}
}

// Run blocks, sweeping every Interval until ctx is canceled. Intended to
// be started in its own goroutine.
func (sw *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(sw.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sw.sweepOnce(ctx)
		}
	}
}

func (sw *Sweeper) sweepOnce(ctx context.Context) {
	if sw.Budget <= 0 {
		return
	}
	total, err := sw.Store.TotalBytes(ctx)
	if err != nil {
		slog.Warn("eviction: measure storage size", "error", err)
		return
	}
	if total <= sw.Budget {
		return
	}

	candidates := sw.Registry.EvictionCandidates()
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastAccess.Before(candidates[j].LastAccess)
	})

	for _, c := range candidates {
		if total <= sw.Budget {
			break
		}
		if err := sw.Store.Delete(ctx, c.Key); err != nil {
			slog.Warn("eviction: delete blob", "key", c.Key, "error", err)
			continue
		}
		sw.Registry.ForgetKey(c.Key)
		total -= c.Size

		sw.mu.Lock()
		sw.evictionsTotal++
		sw.bytesFreedTotal += c.Size
		sw.mu.Unlock()

		slog.Info("eviction: evicted blob", "key", c.Key, "reason", "over storage budget",
			"bytesFreed", c.Size, "lastAccess", c.LastAccess, "totalBytesAfter", total)
	}

	if total > sw.Budget {
		slog.Warn("eviction: still over budget after evicting all safe candidates",
			"totalBytes", total, "budgetBytes", sw.Budget)
	}
}

// Stats returns the Sweeper's cumulative eviction counters.
func (sw *Sweeper) Stats() Stats {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return Stats{
		BudgetBytes:     sw.Budget,
		EvictionsTotal:  sw.evictionsTotal,
		BytesFreedTotal: sw.bytesFreedTotal,
	}
}
