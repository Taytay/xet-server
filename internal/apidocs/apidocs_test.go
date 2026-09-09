package apidocs

// Tests for the embedded Swagger UI handler: every expected file must be
// served, the merged filesystem must actually combine this package's own
// embedded files with the vendored swagger-ui-dist assets (not silently
// drop one side), and index.html must reference openapi.yaml by the exact
// relative path Handler serves it at.

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandler_ServesEveryExpectedFile(t *testing.T) {
	ts := httptest.NewServer(Handler())
	defer ts.Close()

	files := []string{
		"index.html",
		"openapi.yaml",
		"swagger-ui.css",
		"swagger-ui-bundle.js",
		"swagger-ui-standalone-preset.js",
		"favicon-16x16.png",
		"favicon-32x32.png",
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/" + f)
			if err != nil {
				t.Fatalf("GET %s error = %v", f, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", f, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if len(body) == 0 {
				t.Errorf("%s served with an empty body", f)
			}
		})
	}
}

func TestHandler_UnknownFileIs404(t *testing.T) {
	ts := httptest.NewServer(Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/does-not-exist.js")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandler_IndexReferencesOpenAPISpecByRelativePath(t *testing.T) {
	ts := httptest.NewServer(Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/index.html")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"openapi.yaml"`) {
		t.Error(`index.html does not reference "openapi.yaml" by relative path — Swagger UI would fail to load the spec`)
	}
}

func TestHandler_OpenAPISpecIsValidYAMLWithExpectedPaths(t *testing.T) {
	ts := httptest.NewServer(Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/openapi.yaml")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml (not sniffed as text/plain)", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Not a full YAML parse (no yaml package dependency in this module) —
	// just confirm the served content is recognizably the OpenAPI spec
	// and not, say, an empty or truncated embed.
	for _, want := range []string{"openapi: 3.0.3", "/v1/xorbs/{prefix}/{hash}", "/api/repos/create", "/v1/upload"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("served openapi.yaml missing expected content %q", want)
		}
	}
}

func TestNewMergedFS_CollisionBetweenLocalAndVendoredIsAnError(t *testing.T) {
	local := fstubFS{"shared.txt": []byte("local")}
	dist := fstubFS{"shared.txt": []byte("vendored")}
	if _, err := newMergedFS(local, dist); err == nil {
		t.Error("newMergedFS() with a colliding filename in both sources, want an error, got nil")
	}
}

// fstubFS is a minimal flat fs.FS backed by a filename -> contents map,
// used to construct collision scenarios newMergedFS must reject without
// needing a second real embed.FS.
type fstubFS map[string][]byte

func (f fstubFS) Open(name string) (fs.File, error) {
	data, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &fstubFile{name: name, data: data}, nil
}

func (f fstubFS) ReadDir(string) ([]fs.DirEntry, error) {
	entries := make([]fs.DirEntry, 0, len(f))
	for name, data := range f {
		entries = append(entries, fstubDirEntry{name: name, size: int64(len(data))})
	}
	return entries, nil
}

func (f fstubFS) ReadFile(name string) ([]byte, error) {
	data, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return data, nil
}

type fstubFile struct {
	name string
	data []byte
	off  int
}

func (f *fstubFile) Stat() (fs.FileInfo, error) {
	return fstubDirEntry{name: f.name, size: int64(len(f.data))}, nil
}
func (f *fstubFile) Read(p []byte) (int, error) {
	n := copy(p, f.data[f.off:])
	f.off += n
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}
func (f *fstubFile) Close() error { return nil }

type fstubDirEntry struct {
	name string
	size int64
}

func (e fstubDirEntry) Name() string               { return e.name }
func (e fstubDirEntry) IsDir() bool                { return false }
func (e fstubDirEntry) Type() fs.FileMode          { return 0 }
func (e fstubDirEntry) Info() (fs.FileInfo, error) { return e, nil }
func (e fstubDirEntry) Size() int64                { return e.size }
func (e fstubDirEntry) Mode() fs.FileMode          { return 0o444 }
func (e fstubDirEntry) ModTime() time.Time         { return time.Time{} }
func (e fstubDirEntry) Sys() any                   { return nil }

func TestDirFile_StatReportsADirectory(t *testing.T) {
	fi, err := (dirFile{}).Stat()
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !fi.IsDir() {
		t.Error("dirFile.Stat().IsDir() = false, want true")
	}
	if fi.Name() != "." {
		t.Errorf("Name() = %q, want %q", fi.Name(), ".")
	}
	if fi.Size() != 0 {
		t.Errorf("Size() = %d, want 0", fi.Size())
	}
	if fi.Mode()&fs.ModeDir == 0 {
		t.Error("Mode() missing fs.ModeDir bit")
	}
	if fi.Sys() != nil {
		t.Errorf("Sys() = %v, want nil", fi.Sys())
	}
	if !fi.ModTime().IsZero() {
		t.Errorf("ModTime() = %v, want zero value", fi.ModTime())
	}
}

func TestDirFile_ReadAndReadDirAreBenignNoops(t *testing.T) {
	d := dirFile{}
	if _, err := d.Read(make([]byte, 8)); err != io.EOF {
		t.Errorf("Read() error = %v, want io.EOF", err)
	}
	if entries, err := d.ReadDir(-1); entries != nil || err != nil {
		t.Errorf("ReadDir() = (%v, %v), want (nil, nil)", entries, err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestMemFile_FileInfoMethods(t *testing.T) {
	m := &mergedFS{files: map[string][]byte{"f.txt": []byte("hello")}}
	f, err := m.Open("f.txt")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if fi.Name() != "f.txt" {
		t.Errorf("Name() = %q, want %q", fi.Name(), "f.txt")
	}
	if fi.Size() != 5 {
		t.Errorf("Size() = %d, want 5", fi.Size())
	}
	if fi.IsDir() {
		t.Error("IsDir() = true, want false for a plain file")
	}
	if fi.Mode()&fs.ModeDir != 0 {
		t.Error("Mode() has fs.ModeDir bit set, want a plain file mode")
	}
	if fi.Sys() != nil {
		t.Errorf("Sys() = %v, want nil", fi.Sys())
	}
	if !fi.ModTime().IsZero() {
		t.Errorf("ModTime() = %v, want zero value", fi.ModTime())
	}
}

func TestMergedFS_OpenUnknownFileErrorsWithNotExist(t *testing.T) {
	m := &mergedFS{files: map[string][]byte{}}
	_, err := m.Open("nope.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open() error = %v, want fs.ErrNotExist", err)
	}
}
