package proxycas

// Regression tests for the real-HF xorb-caching path: an upstream
// reconstruction whose fetch_info carries presigned CDN URLs (the real
// Xet CAS does NOT serve xorb bodies over GET /v1/xorbs/ - its allow list
// is HEAD,POST - so presigned URLs are the only way to get the bytes).
// cacheXorbFromFetchInfo must fetch each covered range, concatenate them,
// and ingest the resulting xorb; with no fetch_info it must fall back to
// the direct /v1/xorbs/ fetch. All new tests - nothing rewrites an
// existing assertion.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/reconwire"
)

// cdnRangeServer serves blob at /blob, honoring inclusive-end single Range
// headers, recording every range it was asked for.
func cdnRangeServer(blob []byte) (*httptest.Server, *[]string) {
	var ranges []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rh := r.Header.Get("Range")
		ranges = append(ranges, rh)
		var start, end int
		fmt.Sscanf(rh, "bytes=%d-%d", &start, &end)
		if end >= len(blob) {
			end = len(blob) - 1
		}
		if start < 0 || start > end || start >= len(blob) {
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(blob[start : end+1])
	}))
	return ts, &ranges
}

// TestReconstruction_MissFetchesXorbViaPresignedURL pins that with
// fetch_info present, the proxy gets the xorb bytes from the presigned
// CDN URL (with the exact Range) rather than the direct xorb endpoint,
// ingests it, and serves the reconstruction wholly locally afterward.
func TestReconstruction_MissFetchesXorbViaPresignedURL(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("xorb bytes")})
	fileHash := hashFromByte(0xE1)

	cdn, ranges := cdnRangeServer(blob)
	defer cdn.Close()

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
		FetchInfo: map[string][]reconwire.FetchInfoEntry{
			xorbHash.Hex(): {
				{URL: cdn.URL + "/blob", URLRange: reconwire.ByteRange{Start: 0, End: int64(len(blob) - 1)}},
			},
		},
	}
	// The xorb is deliberately NOT in upstream.xorbs, so a fallback to
	// GET /v1/xorbs/ would 404 - only the presigned path can succeed.
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	resp, err := http.Get(ts.URL + "/v2/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("xorb not cached after presigned-URL fetch")
	}
	if len(*ranges) == 0 {
		t.Error("presigned URL was never fetched")
	} else if (*ranges)[0] != fmt.Sprintf("bytes=0-%d", len(blob)-1) {
		t.Errorf("presigned fetch Range = %q, want bytes=0-%d", (*ranges)[0], len(blob)-1)
	}

	// A follow-up request must now be a pure local hit - no further CDN
	// or upstream reconstruction calls.
	beforeCDN := len(*ranges)
	beforeRecon := upstream.reconCount
	resp2, err := http.Get(ts.URL + "/v2/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("second GET error = %v", err)
	}
	resp2.Body.Close()
	if len(*ranges) != beforeCDN {
		t.Errorf("presigned URL fetched again on a local hit (%d -> %d)", beforeCDN, len(*ranges))
	}
	if upstream.reconCount != beforeRecon {
		t.Errorf("upstream reconstruction fetched again on a local hit (%d -> %d)", beforeRecon, upstream.reconCount)
	}
}

// TestReconstruction_PresignedRangesConcatenatedInOrder pins that when a
// xorb's fetch_info spans multiple disjoint byte ranges, they are fetched
// in ascending offset order and concatenated into the full xorb before
// ingestion.
func TestReconstruction_PresignedRangesConcatenatedInOrder(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("0123456789abcdefghij")})
	fileHash := hashFromByte(0xE2)
	mid := int64(len(blob) / 2)

	cdn, _ := cdnRangeServer(blob)
	defer cdn.Close()

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
		FetchInfo: map[string][]reconwire.FetchInfoEntry{
			xorbHash.Hex(): {
				// Listed out of order on purpose - cacheXorbFromFetchInfo
				// must sort by URLRange.Start before concatenating.
				{URL: cdn.URL + "/blob", URLRange: reconwire.ByteRange{Start: mid, End: int64(len(blob) - 1)}},
				{URL: cdn.URL + "/blob", URLRange: reconwire.ByteRange{Start: 0, End: mid - 1}},
			},
		},
	}
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	resp, err := http.Get(ts.URL + "/v2/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("xorb not cached after concatenated presigned fetch")
	}
}

// TestReconstruction_NoFetchInfoFallsBackToDirectXorbFetch pins the
// backward-compatible path: an upstream that serves xorbs directly (this
// project's own casserver, or a test stand-in) and whose reconstruction
// carries no fetch_info is still cached via GET /v1/xorbs/.
func TestReconstruction_NoFetchInfoFallsBackToDirectXorbFetch(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("xorb bytes")})
	fileHash := hashFromByte(0xE3)

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.xorbs[xorbHash.Hex()] = blob
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
		// no FetchInfo field
	}
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	resp, err := http.Get(ts.URL + "/v2/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("xorb not cached via the direct /v1/xorbs/ fallback")
	}
}
