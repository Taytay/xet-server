// Command xetd runs a local HTTP server that speaks the real Xet CAS
// protocol (internal/casserver, mounted at /v1, /v2 — wire-compatible with
// hf_xet/xet-core) alongside a simpler demo chunk/dedup API
// (internal/api, mounted at /upload, /files) for quick manual testing.
package main

import (
	"flag"
	"log"
	"net/http"

	"xet-server/internal/api"
	"xet-server/internal/casserver"
	"xet-server/internal/storage/fsstore"
)

func main() {
	addr := flag.String("addr", ":8420", "listen address")
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

	log.Printf("xetd listening on %s, data dir %s (CAS protocol at /v1,/v2; demo API at /upload,/files)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
