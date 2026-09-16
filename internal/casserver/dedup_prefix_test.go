package casserver

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

// xet-core's openapi spec documents "default-merkledb" as the prefix for
// GET /v1/chunks/{prefix}/{hash}, but the clients people actually run do
// not use it: cas_client/src/remote_client.rs in xet-core >= 1.5 queries
// with PREFIX_DEFAULT ("default", the same prefix as xorb uploads). Seen
// with hf_xet 1.6.0 (`hf upload`) and git-xet 0.2.1: every global-dedup
// query was answered 400 "unsupported chunk-dedup prefix", the client
// logged it as not_found, and a second machine re-uploaded every byte of
// a file the server already had. The server must answer the prefix real
// clients send.
func TestChunkDedup_AcceptsXorbPrefix(t *testing.T) {
	ts, _ := newTestServer(t)
	blob, xorbHash, chunkHashes := buildXorb(t, [][]byte{[]byte("queried with the"), []byte("xorb prefix")})
	resp, err := http.Post(ts.URL+"/v1/xorbs/"+xorbPrefix+"/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	fileHash := merklehash.ComputeDataHash([]byte("queried with thexorb prefix"))
	uploadFooterlessShard(t, ts, fileHash, xorbHash, chunkHashes, []uint32{16, 11})

	for _, prefix := range []string{chunkDedupPrefix, xorbPrefix} {
		resp, err := http.Get(ts.URL + "/v1/chunks/" + prefix + "/" + chunkHashes[0].Hex())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /v1/chunks/%s/<known chunk>: status %d, want 200", prefix, resp.StatusCode)
		}
	}

	// An unknown chunk is still 404 under either prefix (the client
	// treats 404 as "no dedup available" and carries on), and a prefix
	// no client uses is still rejected.
	unknown := merklehash.ComputeDataHash([]byte("never uploaded"))
	for _, prefix := range []string{chunkDedupPrefix, xorbPrefix} {
		resp, err := http.Get(ts.URL + "/v1/chunks/" + prefix + "/" + unknown.Hex())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /v1/chunks/%s/<unknown chunk>: status %d, want 404", prefix, resp.StatusCode)
		}
	}
	resp, err = http.Get(ts.URL + "/v1/chunks/nonsense/" + chunkHashes[0].Hex())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /v1/chunks/nonsense/<hash>: status %d, want 400", resp.StatusCode)
	}
}
