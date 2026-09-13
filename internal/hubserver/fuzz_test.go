package hubserver

// fuzz_test.go fuzzes the untrusted-input parsers on this server's request
// path. Everything here is reached before any authentication or repo
// lookup happens, straight off an arbitrary HTTP request line, so the bar
// is: never panic, never hang, and never return a repo ID that could
// escape the namespace the caller actually addressed.
//
// The existing fuzz corpus in this project targets binary wire formats
// (shardformat, xorbformat, lz4, merklehash). These are the textual
// counterpart - a URL path is just as attacker-controlled as a xorb body,
// and slicing one by hand (index arithmetic around a separator) is exactly
// the kind of code that panics on an input nobody thought to try.

import (
	"strings"
	"testing"
)

// FuzzParseResolvePath asserts parseResolvePath is total: for ANY input it
// either rejects cleanly or returns a well-formed decomposition, and never
// panics (a panic here is a remote DoS - it's reached pre-auth).
//
// The invariants checked on success are the ones the caller relies on:
// repoID is exactly "{namespace}/{name}" (two non-empty segments), the
// filename is non-empty, and repoType is one of the three known values -
// so a caller can't be handed a repoID with an embedded slash that would
// later be re-split into a different repo than the request addressed.
func FuzzParseResolvePath(f *testing.F) {
	seeds := []string{
		"",
		"/",
		"//",
		"///////",
		"resolve/",
		"/resolve/",
		"a/b/resolve/main/f.bin",
		"datasets/a/b/resolve/main/f.bin",
		"spaces/a/b/resolve/main/f.bin",
		"buckets/a/b/resolve/main/f.bin",
		"a/b/resolve/main/nested/dir/f.bin",
		"a/b/resolve/refs/pr/1/f.bin",
		"datasets/a/b/resolve/main/",
		"a/b/resolve/main",
		"resolve/resolve/resolve/resolve",
		"/resolve//resolve//",
		"a/b/resolve/" + strings.Repeat("x/", 64) + "f.bin",
		strings.Repeat("a/", 512) + "resolve/main/f.bin",
		"a/b/resolve/main/" + strings.Repeat("f", 4096),
		"\x00/\x00/resolve/\x00/\x00",
		"ns/nm/resolve/ma in/f b.bin",
		"ns/nm/resolve/main/f.bin/resolve/other/g.bin",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rest string) {
		repoType, repoID, revision, filename, ok := parseResolvePath(rest)
		if !ok {
			// Rejection must be total: no partial results leaking out.
			if repoType != "" || repoID != "" || revision != "" || filename != "" {
				t.Fatalf("rejected input returned non-empty fields: type=%q id=%q rev=%q file=%q",
					repoType, repoID, revision, filename)
			}
			return
		}

		switch repoType {
		case "model", "dataset", "space":
		default:
			t.Fatalf("unknown repoType %q for input %q", repoType, rest)
		}

		// repoID must be exactly two segments. If it had more, a caller
		// re-splitting it would address a different repo than the request.
		parts := strings.Split(repoID, "/")
		if len(parts) != 2 {
			t.Fatalf("repoID %q from %q is not exactly namespace/name", repoID, rest)
		}
		if parts[0] == "" || parts[1] == "" {
			t.Fatalf("repoID %q from %q has an empty segment", repoID, rest)
		}
		if filename == "" {
			t.Fatalf("accepted %q but returned an empty filename", rest)
		}
	})
}

// FuzzExtractRepoIDBefore asserts the LFS-batch repo-ID extractor never
// panics and never invents segments. It shares parseResolvePath's
// "last two segments" rule, so it shares the same escape concern: the
// result is used directly as a repo identity.
func FuzzExtractRepoIDBefore(f *testing.F) {
	seeds := []string{
		"",
		lfsBatchPathSuffix,
		"a/b.git" + lfsBatchPathSuffix,
		"datasets/a/b.git" + lfsBatchPathSuffix,
		"spaces/a/b.git" + lfsBatchPathSuffix,
		"a.git" + lfsBatchPathSuffix,
		"/" + lfsBatchPathSuffix,
		strings.Repeat("x/", 512) + "a/b.git" + lfsBatchPathSuffix,
		"a/b.git",
		"\x00/\x00.git" + lfsBatchPathSuffix,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rest string) {
		got := extractRepoIDBefore(rest, lfsBatchPathSuffix)
		// Whatever comes back must be a substring of the input: this
		// function only ever selects, never synthesizes.
		trimmed := strings.TrimSuffix(rest, lfsBatchPathSuffix)
		if !strings.Contains(trimmed, got) {
			t.Fatalf("extractRepoIDBefore(%q) = %q, which is not present in the input", rest, got)
		}
		if strings.Count(got, "/") > 1 {
			t.Fatalf("extractRepoIDBefore(%q) = %q has more than one separator", rest, got)
		}
	})
}

// FuzzRepoTypeFromPrefix asserts the prefix mapper is total and closed:
// any prefix at all maps to exactly one of the three known repo types,
// with an unrecognized one degrading to "model" rather than propagating
// attacker-controlled text into a URL this server later builds.
func FuzzRepoTypeFromPrefix(f *testing.F) {
	f.Add("")
	f.Add("datasets")
	f.Add("spaces")
	f.Add("models")
	f.Add("buckets")
	f.Add("datasets/extra")
	f.Add("../../etc")
	f.Add("\x00")
	f.Add(strings.Repeat("a/", 256))

	f.Fuzz(func(t *testing.T, prefix string) {
		var parts []string
		if prefix != "" {
			parts = strings.Split(prefix, "/")
		}
		got := repoTypeFromPrefix(parts)
		switch got {
		case "model", "dataset", "space":
		default:
			t.Fatalf("repoTypeFromPrefix(%q) = %q, outside the closed set", prefix, got)
		}
	})
}
