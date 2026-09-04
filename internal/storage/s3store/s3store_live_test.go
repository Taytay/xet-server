package s3store

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestAgainstLiveMinIO exercises the s3store client against a real MinIO
// instance. Skipped unless XET_S3STORE_LIVE_TEST=1 is set, since it needs a
// MinIO server actually running at XET_TEST_S3_ENDPOINT (default
// http://localhost:9000) with the given credentials.
func TestAgainstLiveMinIO(t *testing.T) {
	if os.Getenv("XET_S3STORE_LIVE_TEST") != "1" {
		t.Skip("set XET_S3STORE_LIVE_TEST=1 to run against a live MinIO instance")
	}

	endpoint := envOr("XET_TEST_S3_ENDPOINT", "http://localhost:9000")
	accessKey := envOr("XET_TEST_S3_ACCESS_KEY", "minioadmin")
	secretKey := envOr("XET_TEST_S3_SECRET_KEY", "minioadmin")
	bucket := envOr("XET_TEST_S3_BUCKET", "xet-server-test")

	s := New(endpoint, bucket, "chunks/", accessKey, secretKey, "us-east-1")
	ctx := context.Background()

	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}

	key := "test-object-abc123"
	data := []byte("hello from xet-server s3store live test")

	has, err := s.Has(ctx, key)
	if err != nil {
		t.Fatalf("Has() before Put error = %v", err)
	}
	// Best-effort cleanup from a prior failed run; not required to succeed.
	_ = has

	written, err := s.Put(ctx, key, data)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !written {
		t.Log("Put() reported written=false (object may already exist from a prior run) — continuing")
	}

	has, err = s.Has(ctx, key)
	if err != nil {
		t.Fatalf("Has() after Put error = %v", err)
	}
	if !has {
		t.Fatal("Has() = false immediately after Put()")
	}

	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get() = %q, want %q", got, data)
	}

	rangeData, err := s.GetRange(ctx, key, 6, 4)
	if err != nil {
		t.Fatalf("GetRange() error = %v", err)
	}
	want := data[6:10]
	if !bytes.Equal(rangeData, want) {
		t.Fatalf("GetRange(6, 4) = %q, want %q", rangeData, want)
	}

	written2, err := s.Put(ctx, key, data)
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	if written2 {
		t.Error("second Put() of identical key reported written=true, want false (dedup)")
	}

	presigned, err := s.PresignGet(ctx, key, 60)
	if err != nil {
		t.Fatalf("PresignGet() error = %v", err)
	}
	t.Logf("presigned URL: %s", presigned)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
