package proxycas

// Chaos/reliability tests for a MISBEHAVING UPSTREAM — the failure class
// this package's other tests never touch, since they all run against a
// fake upstream that answers instantly and correctly. internal/casserver's
// own chaos_test.go covers the equivalent ground for casserver's LOCAL
// storage logic (interrupted client uploads, mixed valid/malformed load,
// eviction interleaving); nothing here re-tests any of that. What's
// specific to proxycas is that every cache miss turns into a real
// outbound call to the real huggingface.co CAS, so "upstream hangs",
// "upstream freezes mid-body", and "upstream resets the connection
// mid-body" are all live production failure modes for THIS package, and
// each one has to end in a bounded, clean error — never a corrupt or
// truncated xorb ingested into the embedded server (see
// casserver.IngestXorb's doc comment on re-deriving and verifying the
// hash of everything it accepts), and never a loss of the offline-serving
// guarantee for whatever was already cached before upstream went bad.
//
// Layered under, not duplicating:
//   - internal/hfclient/timeout_test.go proves the Transport-level
//     ResponseHeaderTimeout bound in isolation; these tests prove what
//     that bound actually buys a proxycas REQUEST (a 502, promptly).
//   - internal/proxyhub/timeout_test.go proves MetadataCallTimeout bounds
//     a slow-drip upstream. proxycas deliberately has NO per-call timeout
//     field to test the equivalent of (grep: MetadataCallTimeout exists
//     only in internal/proxyhub) — a xorb transfer can legitimately be
//     large and slow, which is the same reason hfclient.defaultHTTPClient
//     sets per-phase Transport bounds but no blanket request Timeout. See
//     TestChaos_FrozenMidBodyUpstreamBoundedOnlyByCallerContext for what
//     that leaves unbounded today.
//
// On log spam: this package's httpError/writeFetchError intentionally do
// no logging of their own at all (only casserver's httpError does, Warn
// for 5xx / Debug for 4xx, one line per request, with no dedup or
// throttling machinery by design). Since proxycas returns its upstream
// failures via writeFetchError, the tests below — including the
// thundering-herd one, which drives 16 concurrent failures — add no log
// lines whatsoever, and none of these paths retries internally, so
// nothing here can multiply output beyond one line per request. No new
// throttling mechanism is warranted or added.
//
// Every test bounds its own wall clock with a short context or a short
// custom ResponseHeaderTimeout (hundreds of ms, not the real 10s), so
// this file stays fast and deterministic.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xet-server/internal/hfclient"
	"xet-server/internal/reconwire"
	"xet-server/internal/storage/fsstore"
)

// newChaosProxy builds a proxycas Server fronted by an httptest.Server,
// wired to casClient as its upstream. Same shape as newTestServer (see
// proxycas_test.go), except the caller supplies the *hfclient.CASClient
// directly instead of just an upstream URL — these tests need to inject a
// custom *http.Client (a short ResponseHeaderTimeout, so a hung-upstream
// test doesn't sit for hfclient's real 10s) and to point at a raw
// net.Listener rather than an httptest.Server.
func newChaosProxy(t *testing.T, casClient *hfclient.CASClient) (*httptest.Server, *Server) {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	proxy := New(store)

	mux := http.NewServeMux()
	mux.Handle("/", withCASClientMiddleware(proxy, casClient))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, proxy
}

// shortHeaderTimeoutCASClient targets baseURL with a 200ms
// ResponseHeaderTimeout standing in for hfclient.defaultHTTPClient's real
// 10s — the identical Transport field and mechanism, just fast enough for
// a unit test (same trick as internal/hfclient/timeout_test.go).
func shortHeaderTimeoutCASClient(baseURL string) *hfclient.CASClient {
	return &hfclient.CASClient{
		BaseURL: baseURL,
		HTTP: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 200 * time.Millisecond,
			},
		},
	}
}

// newHangingUpstream accepts TCP connections and then never writes a
// single byte of response — a completely unresponsive upstream CAS (a
// hung huggingface.co, a partition that drops responses but not the
// handshake). Mirrors internal/hfclient/timeout_test.go's
// newHangingServer; kept local because that helper is unexported in
// another package.
func newHangingUpstream(t *testing.T) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				<-done
				conn.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		close(done)
		ln.Close()
	})
	return ln.Addr().String()
}

// frozenBodyHandler answers with a full, honest-looking 200 (correct
// Content-Length for contentLength bytes) and flushes those headers
// immediately — so hfclient's ResponseHeaderTimeout is satisfied and
// never fires — then writes NO body at all until its request context is
// canceled or the test finishes. This is the "connected but frozen byte
// stream" case: strictly harder than internal/proxyhub's slowDripHandler,
// which eventually delivers.
//
// sawCancel is closed when the handler observes its own request context
// being canceled, which is how these tests confirm a downstream
// disconnect actually propagates all the way out to the upstream
// connection instead of orphaning it.
func frozenBodyHandler(contentLength int, testDone <-chan struct{}, sawCancel chan<- struct{}) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", contentLength))
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			once.Do(func() { close(sawCancel) })
		case <-testDone:
		}
	}
}

func TestChaos_HungUpstreamBeforeHeadersFailsFastWith502(t *testing.T) {
	// Cold cache miss against an upstream that completes the TCP
	// handshake and then answers nothing, ever. The request must end in a
	// bounded time with an error status — this is exactly what
	// hfclient's Transport ResponseHeaderTimeout is there to guarantee,
	// observed here through a whole proxycas request rather than at the
	// client layer in isolation.
	addr := newHangingUpstream(t)
	ts, proxy := newChaosProxy(t, shortHeaderTimeoutCASClient("http://"+addr))

	_, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("never arrives")})

	start := time.Now()
	resp, err := ts.Client().Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET error = %v, want a real HTTP response (a bounded 502, not a dropped connection)", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (a hung upstream is an upstream fault, not a 404)", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("request took %v, want well under 2s (ResponseHeaderTimeout must bound a hung upstream fetch)", elapsed)
	}
	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server was populated by a fetch that never received any response bytes")
	}
}

func TestChaos_FrozenMidBodyUpstreamBoundedOnlyByCallerContext(t *testing.T) {
	// FINDING, asserted here as current behavior rather than as a wish:
	// once upstream has sent its response headers, NOTHING on the
	// proxycas fetch path bounds how long the body read may take. There
	// is no read deadline, no body-read timeout, and no proxycas
	// equivalent of internal/proxyhub's MetadataCallTimeout (deliberately
	// — a real xorb transfer can be large and slow; see
	// hfclient.defaultHTTPClient's doc comment on why there is no blanket
	// request Timeout either). ensureXorbCached passes r.Context()
	// straight through to hfclient, so the ONLY thing that ever stops a
	// frozen-mid-body upstream fetch is the downstream caller giving up.
	//
	// What that means, precisely: a patient downstream client (one with
	// no timeout of its own) plus a frozen upstream pins one proxy
	// goroutine, one staged temp file inside IngestXorb, and one upstream
	// connection for as long as the freeze lasts. This test therefore
	// asserts the two properties that DO hold today: the cancellation
	// propagates end-to-end (no orphaned upstream read after the
	// downstream client is gone), and nothing partial is ever ingested.
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	sawCancel := make(chan struct{})

	blob, xorbHash := buildFooterlessXorb(t, [][]byte{bytes.Repeat([]byte("frozen stream payload "), 64)})
	casTS := httptest.NewServer(frozenBodyHandler(len(blob), testDone, sawCancel))
	defer casTS.Close()

	ts, proxy := newChaosProxy(t, shortHeaderTimeoutCASClient(casTS.URL))

	// The caller's own deadline is the only bound in play — keep it short
	// so this test costs a fraction of a second regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), nil)

	start := time.Now()
	resp, err := ts.Client().Do(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatal("GET returned a response from an upstream that never wrote a body byte, want the caller's own deadline to fire")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded (the caller's deadline is the only bound on a frozen body read)", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("request took %v, want it to end at the caller's ~400ms deadline", elapsed)
	}

	// The downstream disconnect must actually tear down the upstream
	// fetch. If this ever times out, a frozen upstream would leak a
	// connection and a goroutine per abandoned request.
	select {
	case <-sawCancel:
	case <-time.After(2 * time.Second):
		t.Error("upstream never saw its request canceled — a downstream disconnect must propagate out to the upstream fetch, not orphan it")
	}

	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server was populated from a body that was never delivered")
	}
	if has, err := proxy.Embedded.HasXorbBytes(context.Background(), xorbHash); err == nil && has {
		t.Error("partial/empty xorb bytes were stored from a frozen upstream body")
	}
}

// resetMidBodyUpstream serves a raw, hand-written HTTP/1.1 response
// promising Content-Length bytes, writes only the first half of them,
// then slams the TCP connection shut — a mid-body reset/truncation,
// which no amount of httptest.Server politeness can reproduce (it always
// frames its own responses correctly). Returns the listener address.
func resetMidBodyUpstream(t *testing.T, blob []byte) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				// Read (and discard) the request headers so the client
				// considers the request fully sent.
				buf := make([]byte, 4096)
				conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				conn.Read(buf)
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", len(blob))
				conn.Write(blob[:len(blob)/2])
				// ...and that's all upstream ever sends: close mid-body.
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestChaos_ConnectionResetMidBodyNeverCachesTruncatedXorb(t *testing.T) {
	// The anti-corruption requirement: a xorb whose transfer died halfway
	// must never end up in the embedded server, and must surface as a
	// clean 502 rather than a panic or a silently short 200.
	//
	// Two independent layers already guarantee this, which is why there's
	// no bug to report here: (1) IngestXorb's io.Copy of resp.Body
	// returns io.ErrUnexpectedEOF when fewer bytes arrive than
	// Content-Length promised, failing the ingest before anything is
	// indexed; and (2) even if a truncation somehow read cleanly,
	// IngestXorb re-derives the hash from the chunk stream and rejects a
	// mismatch (ErrXorbHashMismatch) — it never trusts the claimed hash.
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{bytes.Repeat([]byte("truncated payload "), 64)})
	addr := resetMidBodyUpstream(t, blob)

	ts, proxy := newChaosProxy(t, shortHeaderTimeoutCASClient("http://"+addr))

	start := time.Now()
	resp, err := ts.Client().Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET error = %v, want a clean error status (the proxy must not drop its own downstream connection)", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d (body %q), want 502 for a mid-body upstream reset", resp.StatusCode, body)
	}
	if elapsed > 2*time.Second {
		t.Errorf("request took %v, want it to fail promptly on the reset", elapsed)
	}
	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("a truncated xorb was indexed in the embedded server — cached corruption")
	}
	if has, err := proxy.Embedded.HasXorbBytes(context.Background(), xorbHash); err == nil && has {
		t.Error("a truncated xorb's bytes were stored in the embedded server — cached corruption")
	}
}

func TestChaos_ResetMidBodyLeavesNoPoisonedStateForACleanRetry(t *testing.T) {
	// The retry half of the previous test, mirroring casserver's own
	// TestChaos_InterruptedUploadThenCleanRetry: after a mid-body reset,
	// a later fetch of the SAME hash against a recovered upstream must
	// succeed and serve byte-correct content — the failed attempt must
	// leave nothing behind for the retry to inherit.
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{bytes.Repeat([]byte("retry payload "), 64)})

	// Upstream starts out truncating every response mid-body, then flips
	// to healthy once `healthy` is set.
	var healthy atomic.Bool
	var fetches atomic.Int64
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if healthy.Load() {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blob)))
			w.Write(blob)
			return
		}
		// Promise the full length, deliver half, then abort the response
		// — http.ErrAbortHandler makes net/http close the connection
		// without writing a trailing anything, which the client sees as a
		// truncated body.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blob)))
		w.WriteHeader(http.StatusOK)
		w.Write(blob[:len(blob)/2])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer casTS.Close()

	ts, proxy := newChaosProxy(t, shortHeaderTimeoutCASClient(casTS.URL))
	url := ts.URL + "/v1/xorbs/default/" + xorbHash.Hex()

	failResp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatalf("GET (truncating upstream) error = %v", err)
	}
	io.Copy(io.Discard, failResp.Body)
	failResp.Body.Close()
	if failResp.StatusCode == http.StatusOK {
		t.Fatal("GET against a truncating upstream returned 200, want an error status")
	}
	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Fatal("truncated fetch left the xorb indexed in the embedded server")
	}

	healthy.Store(true)

	okResp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatalf("GET (recovered upstream) error = %v", err)
	}
	defer okResp.Body.Close()
	got, _ := io.ReadAll(okResp.Body)
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("status after upstream recovered = %d, want 200 (the failed attempt must not poison the retry)", okResp.StatusCode)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("served %d bytes, want the full %d — content differs after a reset-then-retry", len(got), len(blob))
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("successful retry did not populate the embedded server")
	}
	if n := fetches.Load(); n != 2 {
		t.Errorf("upstream fetched %d times, want 2 (the failed attempt must not have been cached as a negative result)", n)
	}
}

func TestChaos_ThunderingHerdOnColdKeyWithSlowUpstream(t *testing.T) {
	// Many concurrent requests for the SAME cold key while upstream is
	// slow. There is deliberately no single-flight/dedup layer in this
	// package, so more than one upstream fetch is expected and fine —
	// what must hold is that concurrent fetch-and-ingest of one hash is
	// race-free (this test is meaningful mainly under -race), that every
	// caller gets correct bytes, and that the end state is a single
	// valid cached xorb rather than a partially-written one.
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{bytes.Repeat([]byte("herd payload "), 64)})

	var fetches atomic.Int64
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(50 * time.Millisecond) // slow enough that the herd overlaps, short enough to stay fast
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blob)))
		w.Write(blob)
	}))
	defer casTS.Close()

	ts, proxy := newChaosProxy(t, shortHeaderTimeoutCASClient(casTS.URL))
	url := ts.URL + "/v1/xorbs/default/" + xorbHash.Hex()

	const concurrency = 16
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var ok, corrupt atomic.Int64
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			resp, err := ts.Client().Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return
			}
			if !bytes.Equal(got, blob) {
				corrupt.Add(1)
				return
			}
			ok.Add(1)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("thundering herd did not complete within 10s — possible deadlock on the cold-key fetch path")
	}

	if corrupt.Load() != 0 {
		t.Errorf("%d of %d concurrent responses had wrong content — concurrent fetch-and-ingest of one key corrupted data", corrupt.Load(), concurrency)
	}
	if got := ok.Load(); got != concurrency {
		t.Errorf("%d of %d concurrent requests succeeded with correct bytes, want all", got, concurrency)
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server has no footer for the herd's xorb after the run")
	}
	if n := fetches.Load(); n < 1 || n > concurrency {
		t.Errorf("upstream fetched %d times, want between 1 and %d", n, concurrency)
	} else {
		// Informational, not asserted: no single-flight layer exists, so
		// the exact count is a scheduling detail, not a contract.
		t.Logf("upstream fetched %d times for %d concurrent cold-key requests", n, concurrency)
	}

	// One final serial request must now be a pure local hit.
	before := fetches.Load()
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatalf("post-herd GET error = %v", err)
	}
	resp.Body.Close()
	if after := fetches.Load(); after != before {
		t.Errorf("post-herd GET triggered %d more upstream fetches, want 0 (must be a local hit)", after-before)
	}
}

func TestChaos_AlreadyCachedXorbServedInstantlyWhileUpstreamFrozen(t *testing.T) {
	// The offline-fallback guarantee under chaos rather than under a
	// clean shutdown (which TestFetchXorb_OfflineAfterCacheStillServes
	// already covers): upstream is still accepting connections and still
	// answering with headers, it just never delivers a body again. An
	// already-cached xorb must still be served immediately, with zero
	// upstream traffic — proving the cache check in ensureXorbCached
	// short-circuits before any upstream call, so upstream chaos cannot
	// leak into a request that doesn't need upstream at all.
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	sawCancel := make(chan struct{}, 1)

	blob, cachedHash := buildFooterlessXorb(t, [][]byte{bytes.Repeat([]byte("cached payload "), 64)})
	_, coldHash := buildFooterlessXorb(t, [][]byte{[]byte("never cached")})

	var frozen atomic.Bool
	var fetches atomic.Int64
	frozenHandler := frozenBodyHandler(len(blob), testDone, sawCancel)
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if frozen.Load() {
			frozenHandler(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blob)))
		w.Write(blob)
	}))
	defer casTS.Close()

	ts, _ := newChaosProxy(t, shortHeaderTimeoutCASClient(casTS.URL))

	primeResp, err := ts.Client().Get(ts.URL + "/v1/xorbs/default/" + cachedHash.Hex())
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	io.Copy(io.Discard, primeResp.Body)
	primeResp.Body.Close()
	if primeResp.StatusCode != http.StatusOK {
		t.Fatalf("priming GET status = %d, want 200", primeResp.StatusCode)
	}
	primed := fetches.Load()

	frozen.Store(true)

	// Sanity check that upstream really is frozen now, so the assertion
	// below isn't vacuously true: a COLD key must hang until this
	// caller's own short deadline fires (the gap documented in
	// TestChaos_FrozenMidBodyUpstreamBoundedOnlyByCallerContext).
	coldCtx, coldCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer coldCancel()
	coldReq, _ := http.NewRequestWithContext(coldCtx, http.MethodGet, ts.URL+"/v1/xorbs/default/"+coldHash.Hex(), nil)
	if coldResp, err := ts.Client().Do(coldReq); err == nil {
		coldResp.Body.Close()
		t.Fatalf("cold-key GET status = %d against a frozen upstream, want it to hit the caller's deadline", coldResp.StatusCode)
	}
	afterCold := fetches.Load()
	if afterCold <= primed {
		t.Fatal("cold-key GET never reached upstream — the frozen-upstream precondition did not hold")
	}

	// The real assertion: the already-cached xorb is still served, fast,
	// with no upstream call at all, while upstream is frozen.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/xorbs/default/"+cachedHash.Hex(), nil)

	start := time.Now()
	resp, err := ts.Client().Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("cached GET against a frozen upstream error = %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a cached xorb must be servable while upstream is frozen)", resp.StatusCode)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("served %d bytes, want the cached %d", len(got), len(blob))
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("cached GET took %v, want it served immediately from the embedded server with no upstream involvement", elapsed)
	}
	if n := fetches.Load(); n != afterCold {
		t.Errorf("cached GET made %d upstream call(s), want 0", n-afterCold)
	}
}

func TestChaos_ReconstructionWithFrozenXorbFetchIngestsNothing(t *testing.T) {
	// A reconstruction miss fans out into a xorb fetch per term (see
	// ensureFileReconCached), so upstream chaos on the XORB leg has to
	// fail the whole reconstruction cleanly — it must not leave a
	// half-populated file reconstruction referencing a xorb the embedded
	// server doesn't actually have, which would make a later
	// reconstruction response advertise fetch URLs for missing bytes.
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{bytes.Repeat([]byte("recon xorb payload "), 32)})
	fileHash := hashFromByte(0xC0)

	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	sawCancel := make(chan struct{}, 1)

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.xorbs[xorbHash.Hex()] = blob
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
	}
	// The reconstruction itself answers fine; only its xorb leg freezes.
	frozenXorb := frozenBodyHandler(len(blob), testDone, sawCancel)
	reconHandler := upstream.handler()
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchXorbPath(r.URL.Path) != "" {
			frozenXorb(w, r)
			return
		}
		reconHandler(w, r)
	}))
	defer casTS.Close()

	ts, proxy := newChaosProxy(t, shortHeaderTimeoutCASClient(casTS.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/reconstructions/"+fileHash.Hex(), nil)
	if resp, err := ts.Client().Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("reconstruction returned 200 although its xorb fetch never completed")
		}
	}

	if proxy.Embedded.HasFileRecon(fileHash) {
		t.Error("file reconstruction was ingested even though one of its xorbs was never fetched — would advertise fetch URLs for absent bytes")
	}
	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("xorb was indexed although its body never arrived")
	}
}
