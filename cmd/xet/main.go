// Command xet is a small CLI client for xetd: push a local file to the
// server (chunked + deduplicated) or pull a file back down by ID.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/client"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "push":
		push(os.Args[2:])
	case "pull":
		pull(os.Args[2:])
	case "stats":
		stats(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  xet push  [-server <url>] [-auth-token <token>] <local-file>
  xet pull  [-server <url>] [-auth-token <token>] -out <local-file> <file-id>
  xet stats [-server <url>] [-auth-token <token>]

-server falls back to $XET_SERVER, then http://localhost:8420, when omitted.
-auth-token falls back to $XET_AUTH_TOKEN, then $HF_TOKEN, when omitted.`)
}

// defaultServer is the last resort when neither -server nor $XET_SERVER is
// set - this CLI's pre-v0.8.0 behavior of assuming a xetd instance on the
// default port on the same machine.
const defaultServer = "http://localhost:8420"

// resolveServer applies the same precedence as auth.ResolveToken (explicit
// flag wins, then environment, here just one variable deep) but with a
// concrete fallback URL instead of the "None" sentinel, since an empty
// server URL isn't a meaningful "no server configured" state the way an
// empty auth token is - every command needs *some* URL to talk to.
//
// $XET_SERVER is this CLI's own variable, not $HF_ENDPOINT: HF_ENDPOINT
// points the real `hf` CLI at xetd's Hub API shim (a different port,
// speaking huggingface_hub's repo-based protocol), while -server here
// points at the CAS + Xet Data API port - reusing HF_ENDPOINT would
// silently send xet to the wrong server for anyone who already has it
// exported for `hf`.
func resolveServer(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("XET_SERVER"); v != "" {
		return v
	}
	return defaultServer
}

// newClient builds a client.Client for baseURL, wiring authToken into a
// auth.BearerCredentialHelper if non-empty/non-"None" - mirroring xetd's
// own -auth-token convention exactly, so pointing xet at a xetd started
// with -auth-token <secret> is just passing the same flag here. Empty or
// "None" (the default) leaves the client's Cred at auth.NoopCredentialHelper{},
// this CLI's pre-v0.8.0 behavior of sending no credential at all.
//
// authToken is resolved via auth.ResolveToken: an explicit flag wins,
// otherwise falls back to $XET_AUTH_TOKEN, then $HF_TOKEN (the same
// variable the real `hf` CLI reads, so an HF_TOKEN already exported for
// `hf` "just works" against xetd too, with no separate credential to
// configure).
func newClient(baseURL, authToken string) *client.Client {
	c := client.New(baseURL)
	resolved := auth.ResolveToken(authToken, "XET_AUTH_TOKEN", "HF_TOKEN")
	if resolved != "" && resolved != "None" {
		c.Cred = auth.NewBearerCredentialHelper(resolved)
	}
	return c
}

func push(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	server := fs.String("server", "", "xetd server URL; falls back to $XET_SERVER, then http://localhost:8420, if omitted")
	authToken := fs.String("auth-token", "None", "bearer token to send with every request; \"None\" (the default) falls back to $XET_AUTH_TOKEN or $HF_TOKEN, then sends no credential if neither is set")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		os.Exit(1)
	}
	path := fs.Arg(0)

	c := newClient(resolveServer(*server), *authToken)
	res, err := c.Push(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "push failed:", err)
		os.Exit(1)
	}

	fmt.Printf("file_id:  %s\n", res.FileID)
	fmt.Printf("size:     %d bytes\n", res.Size)
	fmt.Printf("chunks:   %d total, %d new\n", res.ChunksTotal, res.ChunksNew)
	fmt.Printf("stored:   %d bytes (%.1f%% deduplicated)\n", res.BytesStored, res.DedupPercent)
}

func pull(args []string) {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	server := fs.String("server", "", "xetd server URL; falls back to $XET_SERVER, then http://localhost:8420, if omitted")
	authToken := fs.String("auth-token", "None", "bearer token to send with every request; \"None\" (the default) falls back to $XET_AUTH_TOKEN or $HF_TOKEN, then sends no credential if neither is set")
	out := fs.String("out", "", "output file path")
	fs.Parse(args)
	if fs.NArg() != 1 || *out == "" {
		usage()
		os.Exit(1)
	}
	fileID := fs.Arg(0)

	c := newClient(resolveServer(*server), *authToken)
	if err := c.Pull(fileID, *out); err != nil {
		fmt.Fprintln(os.Stderr, "pull failed:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}

func stats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	server := fs.String("server", "", "xetd server URL; falls back to $XET_SERVER, then http://localhost:8420, if omitted")
	authToken := fs.String("auth-token", "None", "bearer token to send with every request; \"None\" (the default) falls back to $XET_AUTH_TOKEN or $HF_TOKEN, then sends no credential if neither is set")
	fs.Parse(args)

	c := newClient(resolveServer(*server), *authToken)
	s, err := c.Stats()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stats failed:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
}
