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
	"flag"
	"log"
	"net/http"

	"xet-server/internal/api"
	"xet-server/internal/casserver"
	"xet-server/internal/hubserver"
	"xet-server/internal/storage/fsstore"
)

func main() {
	addr := flag.String("addr", ":8420", "listen address for the CAS + demo API server")
	hubAddr := flag.String("hub-addr", "", "listen address for the Hub API shim (empty = disabled)")
	casURL := flag.String("cas-url", "", "externally-reachable CAS base URL to hand out from the Hub shim (defaults to http://localhost<addr>)")
	dataDir := flag.String("data", "./xet-data", "directory for chunks, xorbs, and manifests")
	flag.Parse()

	demoSrv, err := api.New(*dataDir)
	if err != nil {
		log.Fatalf("init demo api: %v", err)
	}

	xorbStore, err := fsstore.New(*dataDir + "/xorbs")
	if err != nil {
		log.Fatalf("init xorb store: %v", err)
	}
	casSrv := casserver.New(xorbStore)

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
			log.Printf("xetd Hub API shim listening on %s (CAS base URL: %s)", *hubAddr, resolvedCASURL)
			if err := http.ListenAndServe(*hubAddr, hubSrv); err != nil {
				log.Fatal(err)
			}
		}()
	}

	log.Printf("xetd listening on %s, data dir %s (CAS protocol at /v1,/v2; demo API at /upload,/files)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
