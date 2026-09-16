package main

// gc.go: `xetd gc`, the client side of POST /v1/gc (internal/casserver's
// gc.go has the collector and the reasoning). It reads the keep set from
// files, posts it to a running xetd with the operator's shared secret,
// and prints the report. Meant to run from a cron job next to the git
// host:
//
//	for r in /srv/git/*.git; do
//	    git -C "$r" lfs ls-files --all --long >> /tmp/keep.txt
//	done
//	xetd gc -server http://xet:8420 -keep /tmp/keep.txt
//
// The keep file is read leniently: the first whitespace-separated token
// of every line, minus an optional "sha256:" prefix, if it is 64 hex
// characters; anything else (a path, a "*"/"-" marker, a comment, a
// blank line) is skipped. That accepts `git lfs ls-files --all --long`
// output as is, a bare list of OIDs, and pointer files.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/casserver"
)

// stringList lets -keep repeat.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runGC(args []string) {
	fs := flag.NewFlagSet("xetd gc", flag.ExitOnError)
	server := fs.String("server", "", "base URL of the running xetd CAS port (falls back to $XET_SERVER, then http://localhost:8420)")
	authToken := fs.String("auth-token", "", "the server's shared secret; a token the Hub shim minted is refused, since deleting is an operator action (falls back to $XETD_AUTH_TOKEN, then $HF_TOKEN)")
	var keepFiles stringList
	fs.Var(&keepFiles, "keep", "file listing the SHA-256 OIDs to keep (repeatable; \"-\" reads stdin). `git lfs ls-files --all --long` output works as is. Files the server's own Hub registry lists (hf upload) are kept without being listed")
	grace := fs.String("grace", "", "grace period: a xorb uploaded, fetched or advertised in a dedup answer more recently than this is never deleted, whatever the keep set says. Accepts Go durations plus a d suffix (e.g. 30d, 504h, 0). Default: the server's -gc-grace (22 days, the xet client's 3-week shard cache plus a day) - see docs/GIT_LFS.md before lowering it")
	dryRun := fs.Bool("dry-run", false, "report what a run would delete without deleting anything")
	asJSON := fs.Bool("json", false, "print the server's report as JSON instead of a summary")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: xetd gc -keep <file> [-keep <file>...] [-server <url>] [-auth-token <secret>] [-grace <duration>] [-dry-run] [-json]")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if len(keepFiles) == 0 {
		fmt.Fprintln(os.Stderr, "xetd gc: at least one -keep file is required (use -keep /dev/null to keep only what the Hub registry lists)")
		fs.Usage()
		os.Exit(2)
	}

	req := casserver.GCRequest{DryRun: *dryRun}
	var skipped int
	for _, path := range keepFiles {
		oids, n, err := readKeepFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xetd gc: read %s: %v\n", path, err)
			os.Exit(1)
		}
		req.Keep = append(req.Keep, oids...)
		skipped += n
	}
	if *grace != "" {
		d, err := parseGrace(*grace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xetd gc: -grace: %v\n", err)
			os.Exit(2)
		}
		secs := int64(d / time.Second)
		req.GraceSeconds = &secs
	}

	url := resolveGCServer(*server)
	token := auth.ResolveToken(*authToken, "XETD_AUTH_TOKEN", "HF_TOKEN")
	report, err := postGC(url, token, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xetd gc: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(report)
	} else {
		printGCReport(os.Stdout, report, len(req.Keep), skipped)
	}
	if len(report.Errors) > 0 {
		os.Exit(1)
	}
}

// resolveGCServer mirrors cmd/xet's -server precedence.
func resolveGCServer(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("XET_SERVER"); v != "" {
		return v
	}
	return "http://localhost:8420"
}

// readKeepFile returns the OIDs found in path (see the file comment for
// the format) and how many non-blank, non-comment lines held none.
func readKeepFile(path string) (oids []string, skipped int, err error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		defer f.Close()
		r = f
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if oid, ok := oidFromLine(line); ok {
			oids = append(oids, oid)
		} else {
			skipped++
		}
	}
	return oids, skipped, sc.Err()
}

// oidFromLine extracts a SHA-256 OID from the first token of line: a
// bare 64-hex string, or "sha256:<hex>" as a git-lfs pointer's oid line
// has it.
func oidFromLine(line string) (string, bool) {
	tok := strings.Fields(line)[0]
	tok = strings.TrimPrefix(tok, "sha256:")
	if len(tok) != 64 {
		return "", false
	}
	for _, c := range tok {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return "", false
		}
	}
	return strings.ToLower(tok), true
}

// parseGrace is time.ParseDuration plus a "d" (day) suffix, since a
// grace period is naturally days or weeks. "0" is accepted.
func parseGrace(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid day count %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, errors.New("grace must not be negative")
	}
	return d, nil
}

func postGC(server, token string, req casserver.GCRequest) (casserver.GCReport, error) {
	var report casserver.GCReport
	body, err := json.Marshal(req)
	if err != nil {
		return report, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+casserver.GCPath, bytes.NewReader(body))
	if err != nil {
		return report, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" && token != "None" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	// A collection rebuilds the index and walks the whole store; give
	// it as long as it needs rather than a client-side deadline.
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		return report, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return report, fmt.Errorf("server answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return report, fmt.Errorf("decode report: %w", err)
	}
	return report, nil
}

func printGCReport(w io.Writer, r casserver.GCReport, keepListed, skippedLines int) {
	verb := "deleted"
	if r.DryRun {
		verb = "would delete"
	}
	fmt.Fprintf(w, "keep set: %d OIDs listed (%d lines without one), %d after adding the Hub registry and dropping duplicates, %d unknown to the server\n",
		keepListed, skippedLines, r.KeepOIDs, r.KeepUnknown)
	for _, oid := range r.KeepUnknownSample {
		fmt.Fprintf(w, "  unknown: %s\n", oid)
	}
	fmt.Fprintf(w, "files: %d kept, %d dropped\n", r.FilesKept, r.FilesDropped)
	fmt.Fprintf(w, "shards: %d kept, %d rewritten, %d %s\n", r.ShardsKept, r.ShardsRewritten, r.ShardsDeleted, verb)
	fmt.Fprintf(w, "xorbs: %d kept (%s), %d %s (%s), %d deferred within the %s grace (%s)\n",
		r.XorbsKept, humanBytes(r.XorbsKeptBytes), r.XorbsDeleted, verb, humanBytes(r.XorbsDeletedBytes),
		r.XorbsDeferred, (time.Duration(r.GraceSeconds) * time.Second).String(), humanBytes(r.XorbsDeferredBytes))
	for _, e := range r.Errors {
		fmt.Fprintf(w, "error: %s\n", e)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
