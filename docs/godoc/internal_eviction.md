# `xet-server/internal/eviction`

```
package eviction // import "xet-server/internal/eviction"

Package eviction implements a storage-budget sweep: periodically check how many
bytes a storage.Store backend actually holds, and if over a configured budget,
delete the least-recently-accessed blobs until back under budget. This exists so
a server left running against unbounded client uploads doesn't grow its storage
without limit — a deliberate, bounded, observable policy rather than no eviction
at all.

This package knows nothing about xorbs, shards, or the CAS protocol; it operates
purely on opaque storage keys. The metadata needed to pick eviction victims
(last-access time, size, and whether a key is currently unsafe to evict because
a download is in flight) is owned by whatever registers keys with the store —
see casserver.Server's EvictionCandidates/ForgetKey methods for the CAS server's
implementation of the Registry interface this package consumes.

TYPES

type Candidate struct {
	Key        string
	LastAccess time.Time
	Size       int64
}
    Candidate is one blob a Registry reports as eligible for eviction.

type Registry interface {
	// EvictionCandidates returns every key currently safe to evict (e.g.
	// excluding a key with an in-flight download), each with its last
	// access time and size.
	EvictionCandidates() []Candidate

	// ForgetKey removes key from the registry's own indices. Called only
	// after Store.Delete(key) has already succeeded.
	ForgetKey(key string)
}
    Registry is implemented by whatever owns key lifecycle metadata (e.g.
    casserver.Server) so the Sweeper can pick eviction victims and keep that
    owner's own indices in sync after a successful delete.

type Stats struct {
	BudgetBytes     int64 `json:"budget_bytes"`
	EvictionsTotal  int64 `json:"evictions_total"`
	BytesFreedTotal int64 `json:"bytes_freed_total"`
}
    Stats reports the Sweeper's cumulative effect, for an operator-facing
    endpoint to expose so the eviction policy's behavior is observable rather
    than merely assumed.

type Store interface {
	storage.Sizer
	storage.Deleter
}
    Store is the subset of storage.Store's optional capabilities the Sweeper
    needs: a way to measure total bytes used, and a way to remove a blob.
    A backend that implements storage.Store but not both of these (e.g. it has
    no efficient way to enumerate/size its contents) simply can't be swept — see
    NewSweeper.

type Sweeper struct {
	Store    Store
	Registry Registry
	Budget   int64
	Interval time.Duration

	// Has unexported fields.
}
    Sweeper periodically deletes least-recently-accessed blobs once total
    storage use exceeds Budget bytes. A zero-value Budget disables eviction (Run
    still ticks, but sweepOnce is a no-op) — callers that want to avoid even the
    periodic TotalBytes() call when eviction is disabled should simply not start
    a Sweeper at all.

func New(store Store, registry Registry, budgetBytes int64, interval time.Duration) *Sweeper
    New creates a Sweeper. budgetBytes <= 0 means unlimited (Run becomes a no-op
    sweep loop; prefer not starting it at all in that case).

func (sw *Sweeper) Run(ctx context.Context)
    Run blocks, sweeping every Interval until ctx is canceled. Intended to be
    started in its own goroutine.

func (sw *Sweeper) Stats() Stats
    Stats returns the Sweeper's cumulative eviction counters.
```
