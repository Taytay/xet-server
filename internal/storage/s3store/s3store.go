// Package s3store implements storage.Store against any S3-compatible HTTP
// API (AWS S3, MinIO, etc.) using hand-rolled SigV4 request signing
// (internal/sigv4) instead of a third-party SDK. Uses path-style addressing
// (http://endpoint/bucket/key), which both MinIO and AWS S3 support.
package s3store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"xet-server/internal/sigv4"
	"xet-server/internal/storage"
)

var (
	_ storage.Store        = (*Store)(nil)
	_ storage.URLPresigner = (*Store)(nil)
)

type Store struct {
	Endpoint string // e.g. "http://localhost:9000", no trailing slash
	Bucket   string
	Prefix   string // optional key prefix, e.g. "chunks/"
	Signer   *sigv4.Signer
	HTTP     *http.Client
	Now      func() time.Time // overridable for tests; defaults to time.Now
}

func New(endpoint, bucket, prefix, accessKey, secretKey, region string) *Store {
	return &Store{
		Endpoint: strings.TrimSuffix(endpoint, "/"),
		Bucket:   bucket,
		Prefix:   prefix,
		Signer:   sigv4.New(accessKey, secretKey, region, "s3"),
		HTTP:     http.DefaultClient,
		Now:      time.Now,
	}
}

func (s *Store) objectURL(key string) string {
	return fmt.Sprintf("%s/%s/%s%s", s.Endpoint, s.Bucket, s.Prefix, key)
}

func (s *Store) do(req *http.Request, payloadHash string) (*http.Response, error) {
	s.Signer.SignRequest(req, payloadHash, s.Now())
	return s.HTTP.Do(req)
}

func (s *Store) Has(ctx context.Context, key string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.objectURL(key), nil)
	if err != nil {
		return false, err
	}
	resp, err := s.do(req, sigv4.UnsignedPayload)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("s3store: HEAD %s: unexpected status %s", key, resp.Status)
	}
	return true, nil
}

// Put uploads data under key if not already present. S3 has no native
// "create if absent" semantic, so this does a HEAD-then-PUT; a benign
// race (two callers uploading the identical bytes for the same
// content-addressed key concurrently) just means both write the same
// content and both report "written" — the CAS dedup logic that matters
// for cost/perf still works because the vast majority of calls hit an
// existing key on Has and skip the PUT entirely.
func (s *Store) Put(ctx context.Context, key string, data []byte) (written bool, err error) {
	exists, err := s.Has(ctx, key)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	req.ContentLength = int64(len(data))
	payloadHash := sigv4.HashPayload(data)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := s.do(req, payloadHash)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("s3store: PUT %s: unexpected status %s: %s", key, resp.Status, body)
	}
	return true, nil
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req, sigv4.UnsignedPayload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("s3store: GET %s: unexpected status %s: %s", key, resp.Status, body)
	}
	return io.ReadAll(resp.Body)
}

func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	// HTTP Range end is inclusive.
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))

	resp, err := s.do(req, sigv4.UnsignedPayload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("s3store: GET %s (range): unexpected status %s: %s", key, resp.Status, body)
	}
	return io.ReadAll(resp.Body)
}

// PresignGet implements storage.URLPresigner: returns a SigV4
// query-authenticated URL a client can GET directly against the S3/MinIO
// endpoint, bypassing our own server for the bytes themselves.
func (s *Store) PresignGet(_ context.Context, key string, expirySeconds int) (string, error) {
	return s.Signer.PresignURL(http.MethodGet, s.objectURL(key), expirySeconds, s.Now())
}

// EnsureBucket creates the bucket if it doesn't already exist. MinIO and S3
// both accept an empty-body PUT to the bucket root for this.
func (s *Store) EnsureBucket(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.Endpoint+"/"+s.Bucket, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req, sigv4.HashPayload(nil))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200/409 (BucketAlreadyOwnedByYou / already exists) are both fine.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("s3store: PUT bucket %s: unexpected status %s: %s", s.Bucket, resp.Status, body)
}
