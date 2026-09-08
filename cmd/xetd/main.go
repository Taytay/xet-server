// Command xetd runs local HTTP servers that speak the real Xet CAS
// protocol (internal/casserver, mounted at /v1, /v2 — wire-compatible with
// hf_xet/xet-core) alongside a simpler demo chunk/dedup API
// (internal/api, mounted at /upload, /files) for quick manual testing, and
// optionally a Hub API shim (internal/hubserver) on a separate port so the
// real `hf upload`/`hf download` CLI commands work end-to-end via
// HF_ENDPOINT — mirroring how huggingface.co's Hub and CAS are actually
// separate services.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"xet-server/internal/api"
	"xet-server/internal/casserver"
	"xet-server/internal/eviction"
	"xet-server/internal/hubserver"
	"xet-server/internal/ratelimit"
	"xet-server/internal/storage/fsstore"
)

func main() {
	addr := flag.String("addr", ":8420", "listen address for the CAS + demo API server")
	hubAddr := flag.String("hub-addr", "", "listen address for the Hub API shim (empty = disabled)")
	casURL := flag.String("cas-url", "", "externally-reachable CAS base URL to hand out from the Hub shim (defaults to http://localhost<addr>)")
	dataDir := flag.String("data", "./xet-data", "directory for chunks, xorbs, and manifests")
	maxStorageBytes := flag.Int64("max-storage-bytes", 0, "if > 0, periodically evict least-recently-accessed xorbs once total xorb storage exceeds this many bytes")
	evictionInterval := flag.Duration("eviction-interval", 5*time.Minute, "how often to check storage usage against -max-storage-bytes")
	rateLimitRPS := flag.Float64("rate-limit-rps", 0, "if > 0, cap sustained xorb/shard uploads per source IP to this many requests/second (burst allowance via -rate-limit-burst)")
	rateLimitBurst := flag.Float64("rate-limit-burst", 20, "burst allowance for -rate-limit-rps — how many upload requests a source IP can make immediately before the per-second rate applies")
	flag.Parse()

	if os.Getenv("DEBUG") != "" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	demoSrv, err := api.New(*dataDir)
	if err != nil {
		log.Fatalf("init demo api: %v", err)
	}

	xorbStore, err := fsstore.New(*dataDir + "/xorbs")
	if err != nil {
		log.Fatalf("init xorb store: %v", err)
	}
	casSrv := casserver.New(xorbStore)

	if *maxStorageBytes > 0 {
		sweeper := eviction.New(xorbStore, casSrv, *maxStorageBytes, *evictionInterval)
		casSrv.SetEvictionStats(sweeper.Stats)
		go sweeper.Run(context.Background())
		slog.Info("eviction sweep enabled", "budgetBytes", *maxStorageBytes, "interval", *evictionInterval)
	}

	if *rateLimitRPS > 0 {
		casSrv.SetUploadRateLimiter(ratelimit.New(*rateLimitBurst, *rateLimitRPS))
		slog.Info("upload rate limiting enabled", "requestsPerSecond", *rateLimitRPS, "burst", *rateLimitBurst)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", casSrv)
	mux.Handle("/v2/", casSrv)
	mux.Handle("/", demoSrv)

	if *hubAddr != "" {
		resolvedCASURL := *casURL
		if resolvedCASURL == "" {
			resolvedCASURL = "http://localhost" + *addr
		}
		hubSrv := hubserver.New(resolvedCASURL, casSrv)
		go func() {
			slog.Info("xetd Hub API shim listening", "addr", *hubAddr, "casBaseURL", resolvedCASURL)
			if err := http.ListenAndServe(*hubAddr, logRequests(hubSrv)); err != nil {
				log.Fatal(err)
			}
		}()
	}

	slog.Info("xetd listening", "addr", *addr, "dataDir", *dataDir)
	if err := http.ListenAndServe(*addr, logRequests(mux)); err != nil {
		log.Fatal(err)
	}
}

// logRequests wraps a handler to log every request at debug level: the
// request line as soon as it's received (before any handler code runs, so
// a hung/slow handler still shows up as "received" in the log), and the
// response status/duration once it completes. Only active when DEBUG is
// set (see slog.SetLogLoggerLevel above) — slog.Debug is a no-op call
// otherwise, so this has no cost in normal operation.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Debug("request received", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Debug("request completed", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
	})
}

// statusWriter captures the status code passed to WriteHeader so
// logRequests can log it after the handler runs; http.ResponseWriter
// itself exposes no way to read back what a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
