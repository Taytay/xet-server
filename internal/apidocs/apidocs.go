// Package apidocs serves this project's OpenAPI spec (openapi.yaml, this
// directory) through a fully offline Swagger UI — no CDN dependency at
// runtime, since both the spec and the UI's static assets
// (third_party/swagger-ui-dist) are embedded into the xetd binary via
// go:embed. Mounted at /api-docs by cmd/xetd.
package apidocs

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	swaggerui "xet-server/third_party/swagger-ui-dist"
)

//go:embed openapi.yaml index.html
var local embed.FS

// Handler returns an http.Handler serving index.html and openapi.yaml
// (this package) plus every vendored Swagger UI asset
// (swaggerui.Dist), merged into one flat file tree — Swagger UI's
// index.html references its JS/CSS/spec by plain relative filename, so
// all three sources need to appear as siblings under the same URL prefix.
func Handler() http.Handler {
	merged, err := newMergedFS(local, swaggerui.Dist)
	if err != nil {
		// Both embedded filesystems are compiled into the binary and
		// verified by TestHandler_ServesEveryExpectedFile — a failure
		// here means the binary itself is broken, not a runtime
		// condition any caller can recover from.
		panic("apidocs: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(merged))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http's built-in MIME table has no entry for .yaml, so
		// http.FileServer would otherwise sniff/serve it as text/plain —
		// harmless to Swagger UI's own fetch+parse, but wrong.
		if strings.HasSuffix(r.URL.Path, ".yaml") {
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// newMergedFS presents local and dist's files as one flat fs.FS, as if
// every entry from both lived in the same directory. Errors if any
// filename collides between the two (which would silently shadow one
// asset) — this package's own files (openapi.yaml, index.html) are
// expected never to collide with vendored Swagger UI asset names.
func newMergedFS(local, dist fs.FS) (fs.FS, error) {
	files := make(map[string][]byte)
	for _, sub := range []fs.FS{local, dist} {
		entries, err := fs.ReadDir(sub, ".")
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if _, exists := files[name]; exists {
				return nil, fs.ErrExist
			}
			data, err := fs.ReadFile(sub, name)
			if err != nil {
				return nil, err
			}
			files[name] = data
		}
	}
	return &mergedFS{files: files}, nil
}

// mergedFS is a minimal read-only fs.FS backed by an in-memory filename ->
// contents map, flat (no subdirectories) — everything this package needs
// to embed to serve Swagger UI lives at a single directory depth.
type mergedFS struct {
	files map[string][]byte
}

func (m *mergedFS) Open(name string) (fs.File, error) {
	if name == "." {
		// http.FileServer always opens "." first to Stat() whether the
		// requested path is a directory (e.g. it redirects "/index.html"
		// to "./" then opens "." before opening "index.html" within it) —
		// this flat filesystem's one implicit directory is the root
		// itself.
		return &dirFile{}, nil
	}
	data, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memFile{name: name, size: int64(len(data)), Reader: bytes.NewReader(data)}, nil
}

// dirFile is the root "." directory http.FileServer expects to be able to
// Stat() (to confirm the request path is a directory) before opening
// index.html within it. Never Read from — only Stat/Close are called on
// it in that code path.
type dirFile struct{}

func (dirFile) Stat() (fs.FileInfo, error)         { return dirFileInfo{}, nil }
func (dirFile) Read([]byte) (int, error)           { return 0, io.EOF }
func (dirFile) Close() error                       { return nil }
func (dirFile) ReadDir(int) ([]fs.DirEntry, error) { return nil, nil }

type dirFileInfo struct{}

func (dirFileInfo) Name() string       { return "." }
func (dirFileInfo) Size() int64        { return 0 }
func (dirFileInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (dirFileInfo) ModTime() time.Time { return time.Time{} }
func (dirFileInfo) IsDir() bool        { return true }
func (dirFileInfo) Sys() any           { return nil }

// memFile is a read-only, seekable in-memory fs.File — http.FileServer
// needs Seek (for HTTP Range support) on top of plain io.Reader, which
// bytes.Reader already provides; this just adds the fs.File/fs.FileInfo
// methods http.FileServer also requires.
type memFile struct {
	*bytes.Reader
	name string
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f, nil }
func (f *memFile) Close() error               { return nil }

func (f *memFile) Name() string       { return f.name }
func (f *memFile) Size() int64        { return f.size }
func (f *memFile) Mode() fs.FileMode  { return 0o444 }
func (f *memFile) ModTime() time.Time { return time.Time{} }
func (f *memFile) IsDir() bool        { return false }
func (f *memFile) Sys() any           { return nil }
