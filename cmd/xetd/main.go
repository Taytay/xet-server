// Command xetd runs a local HTTP server that chunks, deduplicates, and
// reconstructs files — a minimal stand-in for the Hugging Face Xet backend
// (https://huggingface.co/docs/hub/en/xet/index) for local experimentation
// with sparse/large model files.
package main

import (
	"flag"
	"log"
	"net/http"

	"xet-server/internal/api"
)

func main() {
	addr := flag.String("addr", ":8420", "listen address")
	dataDir := flag.String("data", "./xet-data", "directory for chunks and manifests")
	flag.Parse()

	srv, err := api.New(*dataDir)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	log.Printf("xetd listening on %s, data dir %s", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}
