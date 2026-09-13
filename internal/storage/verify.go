// Package storage's VerifyingStore lives in this file rather than
// storage.go to keep the core interface definitions separate from this
// optional decorator.
package storage

import (
	"bytes"
	"context"
	"io"
)

// verifyChunkSize is how much of the existing blob and the incoming
// content VerifyingStore reads and compares per round, on a dedup hit.
// Chosen to be large enough that comparison overhead (goroutine
// scheduling, channel sends) is negligible relative to I/O, small enough
// that a mismatch near the start of a large object is detected almost
// immediately rather than after a large partial read.
const verifyChunkSize = 256 * 1024

// VerifyingStore wraps a Store to add an opt-in verification pass on
// every dedup hit: when Put finds key already exists, instead of trusting
// the content hash alone (the default, and by far the cheaper, behavior -
// see Store's own doc comment), it reads the existing stored blob and the
// incoming reader concurrently and compares them chunk-by-chunk, bailing
// at the first mismatch rather than reading either side in full once a
// difference is found.
//
// This exists purely as defense-in-depth against a hash collision or
// undetected storage-layer corruption - both exceedingly unlikely with
// BLAKE3, but "exceedingly unlikely" is a probability, not a guarantee,
// and this project would rather refuse a write it can't vouch for than
// silently trust a hash match that turns out to be wrong. It is
// deliberately NOT the default: it doubles I/O for every dedup hit (which
// is the common case for a healthy, working-as-intended deployment), so
// wrapping a Store with this is an explicit opt-in (see cmd/xetd's
// -verify-dedup flag), not something every caller pays for.
//
// If the incoming content doesn't match on a dedup hit, Put returns an
// error satisfying errors.Is(err, ErrContentMismatch) and does NOT
// overwrite the existing stored blob - a mismatch means something is
// already wrong (a collision or corruption), and blindly overwriting
// would destroy the only evidence of which side is actually correct
// without fixing anything.
//
// A key that does not yet exist passes straight through to Inner.Put with
// no extra reads of any kind - the non-dedup-hit path costs nothing
// beyond what Inner.Put itself costs, whether or not verification is
// enabled.
type VerifyingStore struct {
	Inner Store
}

// NewVerifyingStore wraps inner. The returned *VerifyingStore implements
// whichever of Deleter, Sizer, and URLPresigner inner itself implements -
// wrapping a backend that lacks one of these (e.g. fsstore has no
// URLPresigner) must not make it appear to gain that capability, or
// callers that type-assert for it (casserver's presigned-URL fallback,
// eviction's Sweeper) would be silently misled. See the capabilityShim
// types below for how this is done without VerifyingStore itself
// unconditionally implementing all three.
func NewVerifyingStore(inner Store) Store {
	_, hasDeleter := inner.(Deleter)
	_, hasSizer := inner.(Sizer)
	_, hasPresigner := inner.(URLPresigner)

	base := &VerifyingStore{Inner: inner}
	switch {
	case hasDeleter && hasSizer && hasPresigner:
		return &verifyingStoreDSP{base}
	case hasDeleter && hasSizer:
		return &verifyingStoreDS{base}
	case hasDeleter && hasPresigner:
		return &verifyingStoreDP{base}
	case hasSizer && hasPresigner:
		return &verifyingStoreSP{base}
	case hasDeleter:
		return &verifyingStoreD{base}
	case hasSizer:
		return &verifyingStoreS{base}
	case hasPresigner:
		return &verifyingStoreP{base}
	default:
		return base
	}
}

func (v *VerifyingStore) Has(ctx context.Context, key string) (bool, error) {
	return v.Inner.Has(ctx, key)
}

func (v *VerifyingStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return v.Inner.Get(ctx, key)
}

func (v *VerifyingStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	return v.Inner.GetRange(ctx, key, offset, length)
}

// Put implements the verify-on-dedup-hit behavior described on
// VerifyingStore's doc comment.
func (v *VerifyingStore) Put(ctx context.Context, key string, r io.Reader, size int64) (written bool, err error) {
	exists, err := v.Inner.Has(ctx, key)
	if err != nil {
		return false, err
	}
	if !exists {
		return v.Inner.Put(ctx, key, r, size)
	}

	existing, err := v.Inner.Get(ctx, key)
	if err != nil {
		return false, err
	}
	defer existing.Close()

	if err := compareStreams(ctx, existing, r, size); err != nil {
		return false, err
	}
	return false, nil
}

// compareStreams reads existing and incoming concurrently in
// verifyChunkSize rounds, comparing each round's bytes, and returns
// ErrContentMismatch (wrapped with which byte offset differed) at the
// first difference - without waiting to finish reading either stream in
// full. Returns nil if both streams are byte-identical for all n bytes.
//
// A stream ending before n bytes have been read (io.EOF or
// io.ErrUnexpectedEOF from io.ReadFull) is itself treated as a mismatch,
// not propagated as a raw I/O error: it means the existing stored blob or
// the incoming reader is shorter than the declared size n, which is just
// as much "this isn't actually the same content" as a byte difference
// would be. Any other read error (a real I/O failure) is propagated as-is
// - it means comparison couldn't be completed at all, so no correctness
// claim (match or mismatch) can be made either way, distinct from either
// verified outcome.
func compareStreams(ctx context.Context, existing io.Reader, incoming io.Reader, n int64) error {
	type readResult struct {
		buf []byte
		err error
	}

	existingCh := make(chan readResult, 1)
	incomingCh := make(chan readResult, 1)

	remaining := n
	for remaining > 0 {
		want := verifyChunkSize
		if int64(want) > remaining {
			want = int(remaining)
		}

		go func() {
			buf := make([]byte, want)
			nRead, err := io.ReadFull(existing, buf)
			existingCh <- readResult{buf: buf[:nRead], err: err}
		}()
		go func() {
			buf := make([]byte, want)
			nRead, err := io.ReadFull(incoming, buf)
			incomingCh <- readResult{buf: buf[:nRead], err: err}
		}()

		existingRes := <-existingCh
		incomingRes := <-incomingCh

		if isRealIOError(existingRes.err) {
			return existingRes.err
		}
		if isRealIOError(incomingRes.err) {
			return incomingRes.err
		}
		if existingRes.err != nil || incomingRes.err != nil {
			// One or both streams ended short of the declared n bytes -
			// treat as a mismatch rather than propagating EOF/
			// ErrUnexpectedEOF as if comparison itself had failed.
			return errContentMismatchAt(n - remaining)
		}
		if !bytes.Equal(existingRes.buf, incomingRes.buf) {
			return errContentMismatchAt(n - remaining)
		}

		remaining -= int64(want)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// isRealIOError reports whether err is a genuine I/O failure rather than
// io.ReadFull's own "stream ended before the buffer was filled" signals
// (io.EOF, io.ErrUnexpectedEOF), which compareStreams treats as a
// mismatch condition instead.
func isRealIOError(err error) bool {
	return err != nil && err != io.EOF && err != io.ErrUnexpectedEOF
}

func errContentMismatchAt(offset int64) error {
	return &contentMismatchError{offset: offset}
}

type contentMismatchError struct {
	offset int64
}

func (e *contentMismatchError) Error() string {
	return ErrContentMismatch.Error()
}

func (e *contentMismatchError) Unwrap() error {
	return ErrContentMismatch
}

// Offset returns the byte offset at which the mismatch was first
// detected, for callers that want to log it.
func (e *contentMismatchError) Offset() int64 {
	return e.offset
}

// --- capability shims -------------------------------------------------
//
// NewVerifyingStore must return a value whose type implements exactly the
// optional capability interfaces (Deleter, Sizer, URLPresigner) that
// inner itself implements - no more, no less. A single *VerifyingStore
// type with all three methods defined unconditionally would make every
// wrapped store appear to support all three regardless of what inner
// actually supports, which is exactly the false-capability bug this
// wrapper must not introduce (see NewVerifyingStore's doc comment). Each
// shim below embeds *VerifyingStore for the core Store methods and adds
// only the delegate method(s) for the capability combination it
// represents.

type verifyingStoreD struct{ *VerifyingStore }

func (v *verifyingStoreD) Delete(ctx context.Context, key string) error {
	return v.Inner.(Deleter).Delete(ctx, key)
}

type verifyingStoreS struct{ *VerifyingStore }

func (v *verifyingStoreS) TotalBytes(ctx context.Context) (int64, error) {
	return v.Inner.(Sizer).TotalBytes(ctx)
}

type verifyingStoreP struct{ *VerifyingStore }

func (v *verifyingStoreP) PresignGet(ctx context.Context, key string, expirySeconds int) (string, error) {
	return v.Inner.(URLPresigner).PresignGet(ctx, key, expirySeconds)
}

type verifyingStoreDS struct{ *VerifyingStore }

func (v *verifyingStoreDS) Delete(ctx context.Context, key string) error {
	return v.Inner.(Deleter).Delete(ctx, key)
}
func (v *verifyingStoreDS) TotalBytes(ctx context.Context) (int64, error) {
	return v.Inner.(Sizer).TotalBytes(ctx)
}

type verifyingStoreDP struct{ *VerifyingStore }

func (v *verifyingStoreDP) Delete(ctx context.Context, key string) error {
	return v.Inner.(Deleter).Delete(ctx, key)
}
func (v *verifyingStoreDP) PresignGet(ctx context.Context, key string, expirySeconds int) (string, error) {
	return v.Inner.(URLPresigner).PresignGet(ctx, key, expirySeconds)
}

type verifyingStoreSP struct{ *VerifyingStore }

func (v *verifyingStoreSP) TotalBytes(ctx context.Context) (int64, error) {
	return v.Inner.(Sizer).TotalBytes(ctx)
}
func (v *verifyingStoreSP) PresignGet(ctx context.Context, key string, expirySeconds int) (string, error) {
	return v.Inner.(URLPresigner).PresignGet(ctx, key, expirySeconds)
}

type verifyingStoreDSP struct{ *VerifyingStore }

func (v *verifyingStoreDSP) Delete(ctx context.Context, key string) error {
	return v.Inner.(Deleter).Delete(ctx, key)
}
func (v *verifyingStoreDSP) TotalBytes(ctx context.Context) (int64, error) {
	return v.Inner.(Sizer).TotalBytes(ctx)
}
func (v *verifyingStoreDSP) PresignGet(ctx context.Context, key string, expirySeconds int) (string, error) {
	return v.Inner.(URLPresigner).PresignGet(ctx, key, expirySeconds)
}
