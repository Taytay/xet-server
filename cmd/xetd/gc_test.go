package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOIDFromLine(t *testing.T) {
	oid := strings.Repeat("0123456789abcdef", 4)
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{oid + " * assets/big.bin", oid, true},                    // git lfs ls-files --all --long
		{strings.ToUpper(oid) + " - old.bin", oid, true},          // a "-" marker (not checked out), upper case
		{"sha256:" + oid, oid, true},                              // a pointer file's oid line
		{oid, oid, true},                                          // bare
		{"0123456789 * assets/big.bin", "", false},                // truncated (no --long)
		{"version https://git-lfs.github.com/spec/v1", "", false}, // pointer header
		{"size 12345", "", false},
		{strings.Repeat("g", 64), "", false}, // not hex
	}
	for _, c := range cases {
		got, ok := oidFromLine(c.line)
		if got != c.want || ok != c.ok {
			t.Errorf("oidFromLine(%q) = %q, %v; want %q, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestReadKeepFile(t *testing.T) {
	oid1 := strings.Repeat("a1", 32)
	oid2 := strings.Repeat("b2", 32)
	path := filepath.Join(t.TempDir(), "keep.txt")
	content := "# exported by cron\n" + oid1 + " * a.bin\n\n" + oid2 + " - b.bin\nnot-an-oid * c.bin\nsha256:" + oid1 + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	oids, skipped, err := readKeepFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(oids, ",") != oid1+","+oid2+","+oid1 || skipped != 1 {
		t.Errorf("readKeepFile = %v (skipped %d)", oids, skipped)
	}
	if _, _, err := readKeepFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file should be an error")
	}
}

func TestParseGrace(t *testing.T) {
	cases := map[string]time.Duration{"0": 0, "30d": 30 * 24 * time.Hour, "1.5d": 36 * time.Hour, "504h": 504 * time.Hour, "90m": 90 * time.Minute}
	for in, want := range cases {
		got, err := parseGrace(in)
		if err != nil || got != want {
			t.Errorf("parseGrace(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1d", "-5h", "soon", "d"} {
		if _, err := parseGrace(bad); err == nil {
			t.Errorf("parseGrace(%q) should fail", bad)
		}
	}
}
