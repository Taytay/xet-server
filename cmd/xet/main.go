// Command xet is a small CLI client for xetd: push a local file to the
// server (chunked + deduplicated) or pull a file back down by ID.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"xet-lite/internal/client"
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
  xet push  -server http://localhost:8420 <local-file>
  xet pull  -server http://localhost:8420 -out <local-file> <file-id>
  xet stats -server http://localhost:8420`)
}

func push(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8420", "xetd server URL")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		os.Exit(1)
	}
	path := fs.Arg(0)

	c := client.New(*server)
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
	server := fs.String("server", "http://localhost:8420", "xetd server URL")
	out := fs.String("out", "", "output file path")
	fs.Parse(args)
	if fs.NArg() != 1 || *out == "" {
		usage()
		os.Exit(1)
	}
	fileID := fs.Arg(0)

	c := client.New(*server)
	if err := c.Pull(fileID, *out); err != nil {
		fmt.Fprintln(os.Stderr, "pull failed:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}

func stats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8420", "xetd server URL")
	fs.Parse(args)

	c := client.New(*server)
	s, err := c.Stats()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stats failed:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
}
