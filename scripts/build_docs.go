// Command build_docs renders this repo's Markdown docs to browsable,
// self-contained HTML (used by `make docs`), and before that regenerates
// docs/godoc/*.md — the committed, GitHub-friendly Go package reference —
// from `go doc -all` output for every internal package.
//
// docs/godoc/*.md is committed source (renders natively on GitHub); the
// HTML this program writes into docs/build/ is a gitignored build
// artifact, not something to commit.
//
// Mermaid code fences (```mermaid) degrade gracefully: if the `mmdc`
// (mermaid-cli) binary is on PATH and can actually render (it needs a
// headless-Chrome binary, which is not guaranteed to be installed), each
// diagram is rendered to a static SVG and inlined via goldmark-mermaid's
// server-side compiler. Otherwise the diagram's source is shown in a
// styled <pre> block with a link to open it on mermaid.live, so the page
// still degrades cleanly instead of showing a raw fenced code block or a
// blank page from a failed script load.
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
	gmmermaid "go.abhg.dev/goldmark/mermaid"
)

const pageTemplate = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>%s — Xet Server</title>
<style>
body{font-family:-apple-system,sans-serif;max-width:900px;margin:2rem auto;padding:0 1rem;line-height:1.6;color:#1f2328}
a{color:#0969da;text-decoration:none} a:hover{text-decoration:underline}
pre{background:#f6f8fa;padding:0.8rem;border-radius:6px;overflow-x:auto}
code{background:#f6f8fa;padding:0.15rem 0.35rem;border-radius:3px;font-size:0.9em;font-family:ui-monospace,monospace}
pre code{background:none;padding:0}
.nav{margin-bottom:1.5rem;padding-bottom:1rem;border-bottom:1px solid #d0d7de}
table{border-collapse:collapse;margin:1rem 0} th,td{border:1px solid #d0d7de;padding:0.4rem 0.8rem}
.mermaid-fallback{border:1px dashed #d0d7de;border-radius:6px;padding:0.8rem}
.mermaid-fallback p{margin:0 0 0.5rem;color:#57606a;font-size:0.9em}
.mermaid-svg{text-align:center}
%s
</style>
</head><body>
%s
%s
</body></html>
`

const navHTML = `<div class="nav"><a href="index.html">&larr; Docs Index</a></div>`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	serveAddr := flag.String("serve", "", "if set (e.g. \":8000\"), serve docs/build/ at this address after building instead of exiting")
	checkMermaid := flag.Bool("check-mermaid", false, "print which Mermaid renderer CLI (if any) can render diagrams and exit, without building anything")
	flag.Parse()

	if *checkMermaid {
		printMermaidCLIStatus()
		return nil
	}

	repoRoot, err := repoRootDir()
	if err != nil {
		return err
	}

	if err := genGodoc(repoRoot); err != nil {
		return fmt.Errorf("generating docs/godoc/*.md: %w", err)
	}

	if err := buildHTML(repoRoot); err != nil {
		return err
	}

	if *serveAddr == "" {
		return nil
	}
	buildDir := filepath.Join(repoRoot, "docs", "build")
	fmt.Printf("\nServing %s at http://localhost%s/ (Ctrl+C to stop)\n", buildDir, *serveAddr)
	return http.ListenAndServe(*serveAddr, http.FileServer(http.Dir(buildDir)))
}

// repoRootDir finds the repo root by walking up from the current directory
// looking for the top-level go.mod (module xet-server), not scripts/go.mod.
// The Makefile always invokes this from the repo root, but resolving it
// properly avoids depending on that.
func repoRootDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.HasPrefix(string(data), "module xet-server\n") {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repo root (xet-server go.mod) above %s", dir)
		}
		dir = parent
	}
}

// -----------------------------------------------------------------------
// Step 1: regenerate docs/godoc/*.md from `go doc -all`
// -----------------------------------------------------------------------

func genGodoc(repoRoot string) error {
	outDir := filepath.Join(repoRoot, "docs", "godoc")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	existing, err := filepath.Glob(filepath.Join(outDir, "*.md"))
	if err != nil {
		return err
	}
	for _, f := range existing {
		if err := os.Remove(f); err != nil {
			return err
		}
	}

	pkgs, err := listPackages(repoRoot)
	if err != nil {
		return err
	}

	var index strings.Builder
	index.WriteString("# Go Package Reference\n\n")
	index.WriteString("Generated by `scripts/build_docs.go` via `go doc -all`. Re-run after\n")
	index.WriteString("changing exported types/functions or package doc comments to keep this\n")
	index.WriteString("in sync — it is not generated automatically as part of `make build`.\n\n")
	index.WriteString("See also [ARCHITECTURE.md](../ARCHITECTURE.md) and [PROTOCOL.md](../PROTOCOL.md).\n\n")

	for _, pkg := range pkgs {
		name := strings.TrimPrefix(pkg, "xet-server/")
		fname := strings.ReplaceAll(name, "/", "_") + ".md"
		fmt.Printf("Generating docs for %s...\n", pkg)

		out, docErr := runGoDoc(repoRoot, pkg)
		body := strings.TrimRight(out, "\n")
		if docErr != nil || body == "" {
			body = "(no exported symbols or doc failed)"
		}

		content := fmt.Sprintf("# `%s`\n\n```\n%s\n```\n", pkg, body)
		if err := os.WriteFile(filepath.Join(outDir, fname), []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(&index, "- [%s](%s)\n", name, fname)
	}

	if err := os.WriteFile(filepath.Join(outDir, "README.md"), []byte(index.String()), 0o644); err != nil {
		return err
	}

	fmt.Printf("\nDocs written to %s/ (commit these — they're meant to render on GitHub)\n", outDir)
	return nil
}

func listPackages(repoRoot string) ([]string, error) {
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.Contains(line, "/cmd/") {
			continue
		}
		pkgs = append(pkgs, line)
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

func runGoDoc(repoRoot, pkg string) (string, error) {
	cmd := exec.Command("go", "doc", "-all", pkg)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// -----------------------------------------------------------------------
// Step 2: render every committed .md file to HTML in docs/build/
// -----------------------------------------------------------------------

type docSource struct {
	path  string
	title string
}

func buildHTML(repoRoot string) error {
	mermaidCLI := resolveMermaidCLI()
	if mermaidCLI != "" {
		fmt.Printf("%s available — diagrams will render as SVG\n", mermaidCLI)
	} else {
		fmt.Println("no working Mermaid renderer (tried: " + strings.Join(mermaidRenderers, ", ") + ") — diagrams will show as source + mermaid.live link")
	}

	buildDir := filepath.Join(repoRoot, "docs", "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(buildDir, "*.html"))
	if err != nil {
		return err
	}
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			return err
		}
	}

	var sources []docSource
	for _, e := range []struct{ name, title string }{
		{"README.md", "Xet Server"},
		{"CONTRIBUTING.md", "Contributing"},
		{"CHANGELOG.md", "Changelog"},
		{"LICENSE.md", "License"},
	} {
		p := filepath.Join(repoRoot, e.name)
		if _, err := os.Stat(p); err == nil {
			sources = append(sources, docSource{p, e.title})
		}
	}

	docsDir := filepath.Join(repoRoot, "docs")
	docsMd, err := filepath.Glob(filepath.Join(docsDir, "*.md"))
	if err != nil {
		return err
	}
	sort.Strings(docsMd)
	for _, p := range docsMd {
		sources = append(sources, docSource{p, titleFromStem(stem(p))})
	}

	godocDir := filepath.Join(docsDir, "godoc")
	godocMd, err := filepath.Glob(filepath.Join(godocDir, "*.md"))
	if err != nil {
		return err
	}
	sort.Strings(godocMd)
	for _, p := range godocMd {
		title := stem(p)
		if title == "README" {
			title = "Go Reference"
		}
		sources = append(sources, docSource{p, title})
	}

	linkMap := make(map[string]string, len(sources))
	for _, s := range sources {
		abs, err := filepath.Abs(s.path)
		if err != nil {
			return err
		}
		linkMap[abs] = outName(s.path)
	}

	style := styles.Get("github")
	pygmentsCSS, err := chromaCSS(style)
	if err != nil {
		return err
	}

	md := newMarkdown(style, mermaidCLI)

	type indexItem struct{ name, title string }
	var indexItems []indexItem

	for _, s := range sources {
		raw, err := os.ReadFile(s.path)
		if err != nil {
			return err
		}
		src := rewriteRelativeMDLinks(string(raw), s.path, linkMap)

		var buf bytes.Buffer
		if err := md.Convert([]byte(src), &buf); err != nil {
			return fmt.Errorf("rendering %s: %w", s.path, err)
		}

		outPath := filepath.Join(buildDir, outName(s.path))
		page := fmt.Sprintf(pageTemplate, html.EscapeString(s.title), pygmentsCSS, navHTML, buf.String())
		if err := os.WriteFile(outPath, []byte(page), 0o644); err != nil {
			return err
		}

		rel, _ := filepath.Rel(repoRoot, s.path)
		fmt.Printf("Rendered %s -> docs/build/%s\n", rel, filepath.Base(outPath))

		// Individual godoc/internal_*.md package pages are only reachable
		// via the Go Reference page's own index — no need to duplicate
		// them in the top-level docs index.
		inGodoc := filepath.Base(filepath.Dir(s.path)) == "godoc"
		if !inGodoc || stem(s.path) == "README" {
			indexItems = append(indexItems, indexItem{filepath.Base(outPath), s.title})
		}
	}

	var indexBody strings.Builder
	indexBody.WriteString("<h1>Xet Server — Documentation</h1><ul>")
	for _, it := range indexItems {
		fmt.Fprintf(&indexBody, `<li><a href="%s">%s</a></li>`, it.name, html.EscapeString(it.title))
	}
	indexBody.WriteString("</ul>")

	indexPage := fmt.Sprintf(pageTemplate, "Docs Index", pygmentsCSS, "", indexBody.String())
	if err := os.WriteFile(filepath.Join(buildDir, "index.html"), []byte(indexPage), 0o644); err != nil {
		return err
	}

	fmt.Printf("\nBuilt %d pages into %s/ (run 'make docs-serve' to browse)\n", len(sources), buildDir)
	return nil
}

func stem(p string) string {
	base := filepath.Base(p)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func titleFromStem(s string) string {
	words := strings.Split(strings.ReplaceAll(s, "_", " "), " ")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func outName(mdPath string) string {
	if filepath.Base(filepath.Dir(mdPath)) == "godoc" {
		return "godoc_" + stem(mdPath) + ".html"
	}
	return stem(mdPath) + ".html"
}

var mdLinkRE = regexp.MustCompile(`(\]\()([^)\s]+\.md)((?:#[^)]*)?\))`)

// rewriteRelativeMDLinks rewrites relative .md links to the flattened .html
// output filenames they'll be rendered to in docs/build/, so cross-doc
// navigation works in the output site instead of 404ing on paths that only
// make sense in the source tree (e.g. `../ARCHITECTURE.md`, `docs/PROTOCOL.md`).
func rewriteRelativeMDLinks(text, sourcePath string, linkMap map[string]string) string {
	sourceDir := filepath.Dir(sourcePath)
	return mdLinkRE.ReplaceAllStringFunc(text, func(m string) string {
		groups := mdLinkRE.FindStringSubmatch(m)
		prefix, target, suffix := groups[1], groups[2], groups[3]
		resolved, err := filepath.Abs(filepath.Join(sourceDir, target))
		if err != nil {
			return m
		}
		if out, ok := linkMap[resolved]; ok {
			return prefix + out + suffix
		}
		return m
	})
}

// -----------------------------------------------------------------------
// Markdown -> HTML conversion (goldmark, GFM tables/autolinks/TOC anchors,
// chroma syntax highlighting, goldmark-mermaid diagrams)
// -----------------------------------------------------------------------

func newMarkdown(style *chroma.Style, mermaidCLI string) goldmark.Markdown {
	mermaidExt := &gmmermaid.Extender{
		RenderMode: gmmermaid.RenderModeServer,
		Compiler:   &fallbackMermaidCompiler{cli: mermaidCLI},
	}

	return goldmark.New(
		goldmark.WithExtensions(extension.GFM, mermaidExt),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(
			ghtml.WithUnsafe(),
			renderer.WithNodeRenderers(util.Prioritized(&codeBlockRenderer{style: style}, 100)),
		),
	)
}

// codeBlockRenderer overrides goldmark's default fenced/indented code block
// rendering to run chroma syntax highlighting (pygments-equivalent) instead
// of emitting a plain <pre><code> block. Registering at priority 100 (lower
// than the default HTML renderer's 1000) makes this renderer's entry sort
// first in util.PrioritizedSlice's ascending sort — renderer.Render walks
// that slice back-to-front when calling RegisterFuncs, so it registers
// last and wins for these two node kinds, while every other node kind
// still falls through to goldmark's default renderer.
//
// Mermaid fences never reach here: gmmermaid.Transformer rewrites them into
// a dedicated AST node kind before this renderer sees the tree, so they're
// handled by mermaidExt's own renderer instead.
type codeBlockRenderer struct {
	style *chroma.Style
}

func (r *codeBlockRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.renderFenced)
	reg.Register(ast.KindCodeBlock, r.renderPlain)
}

func (r *codeBlockRenderer) renderFenced(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	fcb := n.(*ast.FencedCodeBlock)
	lang := ""
	if l := fcb.Language(source); l != nil {
		lang = string(l)
	}
	writeHighlighted(w, blockLines(source, n), lang, r.style)
	return ast.WalkContinue, nil
}

func (r *codeBlockRenderer) renderPlain(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	writeHighlighted(w, blockLines(source, n), "", r.style)
	return ast.WalkContinue, nil
}

func blockLines(source []byte, n ast.Node) string {
	lines := n.Lines()
	var b strings.Builder
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		b.Write(line.Value(source))
	}
	return b.String()
}

// writeHighlighted tokenises code with chroma and writes a
// <div class="codehilite"><pre>...</pre></div> block, matching the
// structure python-markdown's codehilite extension used to produce so the
// existing page CSS applies unchanged. lang is a fenced-code-block info
// string (e.g. "go", "bash"); empty runs chroma's content-based lexer
// analyser, matching codehilite's guess_lang default.
func writeHighlighted(w util.BufWriter, code, lang string, style *chroma.Style) {
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Analyse(code)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		_, _ = w.WriteString(`<pre><code>`)
		_, _ = w.WriteString(html.EscapeString(code))
		_, _ = w.WriteString(`</code></pre>`)
		return
	}

	formatter := chromahtml.New(chromahtml.WithClasses(true), chromahtml.ClassPrefix(""))
	_, _ = w.WriteString(`<div class="codehilite">`)
	if err := formatter.Format(w, style, iterator); err != nil {
		_, _ = w.WriteString(html.EscapeString(code))
	}
	_, _ = w.WriteString(`</div>`)
}

func chromaCSS(style *chroma.Style) (string, error) {
	formatter := chromahtml.New(chromahtml.WithClasses(true), chromahtml.ClassPrefix(""))
	var buf bytes.Buffer
	if err := formatter.WriteCSS(&buf, style); err != nil {
		return "", err
	}
	// Scope chroma's bare `.chroma`/`.bg` selectors under `.codehilite` (the
	// wrapper writeHighlighted emits) to match the class name the page CSS
	// and writeHighlighted's markup already use.
	return strings.ReplaceAll(buf.String(), ".chroma ", ".codehilite "), nil
}

// -----------------------------------------------------------------------
// Mermaid diagrams: try merman-cli or mmdc (mermaid-cli) via
// goldmark-mermaid's Compiler interface, else styled fallback + link to
// mermaid.live
// -----------------------------------------------------------------------

// mermaidRenderers lists candidate CLIs for rendering Mermaid source to
// SVG, in preference order. merman-cli (github.com/Latias94/merman) is
// tried first: it's a self-contained Rust binary with no headless-Chrome
// dependency, unlike mmdc which needs puppeteer + chrome-headless-shell
// and fails in many sandboxed/CI environments even when installed. Both
// expose an `-i <in> -o <out>` interface (merman-cli is intentionally
// mmdc-compatible), so one runner works for both.
var mermaidRenderers = []string{"merman-cli", "mmdc"}

// fallbackMermaidCompiler implements gmmermaid.Compiler. When a renderer
// CLI is available it shells out to it directly (bypassing
// gmmermaid.CLICompiler, which returns an error — not a distinguishable
// "unavailable" signal — on failure); when none are available or the
// render fails, it renders the styled fallback block instead of failing
// the whole page.
type fallbackMermaidCompiler struct {
	cli string // resolved renderer binary name, or "" if none available
}

func (c *fallbackMermaidCompiler) Compile(ctx context.Context, req *gmmermaid.CompileRequest) (*gmmermaid.CompileResponse, error) {
	if c.cli != "" {
		if svg, err := renderMermaidToSVG(c.cli, req.Source); err == nil {
			return &gmmermaid.CompileResponse{SVG: svg}, nil
		}
	}
	return &gmmermaid.CompileResponse{SVG: mermaidFallbackHTML(req.Source)}, nil
}

// resolveMermaidCLI returns the first available renderer from
// mermaidRenderers that can actually render a probe diagram, or "" if
// none can (not installed, or installed but non-functional — e.g. mmdc
// without a working headless-Chrome).
func resolveMermaidCLI() string {
	for _, cli := range mermaidRenderers {
		if mermaidCLIWorks(cli) {
			return cli
		}
	}
	return ""
}

func mermaidCLIWorks(cli string) bool {
	if _, err := exec.LookPath(cli); err != nil {
		return false
	}
	dir, err := os.MkdirTemp("", "mermaid-probe")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "probe.mmd")
	out := filepath.Join(dir, "probe.svg")
	if err := os.WriteFile(src, []byte("graph TD; A-->B;"), 0o644); err != nil {
		return false
	}
	if err := runMermaidCLI(cli, src, out); err != nil {
		return false
	}
	_, err = os.Stat(out)
	return err == nil
}

func renderMermaidToSVG(cli, source string) (string, error) {
	dir, err := os.MkdirTemp("", "mermaid-render")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "diagram.mmd")
	out := filepath.Join(dir, "diagram.svg")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		return "", err
	}
	if err := runMermaidCLI(cli, src, out); err != nil {
		return "", err
	}
	svg, err := os.ReadFile(out)
	if err != nil {
		return "", err
	}
	return `<div class="mermaid-svg">` + string(svg) + `</div>`, nil
}

func runMermaidCLI(cli, inPath, outPath string) error {
	cmd := exec.Command(cli, "-i", inPath, "-o", outPath)
	return cmd.Run()
}

// mermaidFallbackHTML shows the diagram source with a link to open it on
// mermaid.live. mermaid.live's #pako: fragment isn't the raw source —
// it's a JSON envelope ({"code": ..., "mermaid": {...}}) that's
// zlib-deflated and base64url-encoded (the "pako" library name refers to
// its zlib implementation). Passing the raw source through url.QueryEscape
// produces a link mermaid.live cannot decode at all.
func mermaidFallbackHTML(source string) string {
	escaped := html.EscapeString(source)
	link := "https://mermaid.live/edit#pako:" + encodeMermaidLivePako(source)
	return `<div class="mermaid-fallback">` +
		`<p>Mermaid diagram (rendering unavailable in this build — ` +
		fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener">open on mermaid.live</a>)</p>`, link) +
		`<pre>` + escaped + `</pre>` +
		`</div>`
}

// encodeMermaidLivePako builds mermaid.live's "pako" state fragment: a
// JSON document ({"code": source, "mermaid": {"theme": "default"}})
// deflated with zlib and base64url-encoded, unpadded. Confirmed against
// mermaid.live's actual decoder (it uses the "pako" JS zlib port, hence
// the fragment name) — a plain URL-encoded diagram source, which is what
// this used to send, doesn't decode there at all.
func encodeMermaidLivePako(source string) string {
	state, err := json.Marshal(map[string]any{
		"code": source,
		"mermaid": map[string]any{
			"theme": "default",
		},
	})
	if err != nil {
		return ""
	}

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(state)
	_ = zw.Close()

	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(buf.Bytes())
}

// printMermaidCLIStatus reports which Mermaid renderer CLI (if any) is
// available and functional, for `make pre-check` and `-check-mermaid`.
func printMermaidCLIStatus() {
	for _, cli := range mermaidRenderers {
		if _, err := exec.LookPath(cli); err != nil {
			continue
		}
		if mermaidCLIWorks(cli) {
			fmt.Printf("%s ✓ — diagrams will render as SVG\n", cli)
			return
		}
		fmt.Printf("%s found but couldn't render a test diagram — trying other renderers\n", cli)
	}
	fmt.Println("no working Mermaid renderer found (tried: " + strings.Join(mermaidRenderers, ", ") +
		") — optional; 'make docs' will show diagrams as source + mermaid.live link instead. " +
		"Install merman-cli (`cargo install merman-cli`, no headless-Chrome needed) or mmdc " +
		"(`npm install -g @mermaid-js/mermaid-cli` + `npx puppeteer browsers install chrome-headless-shell`).")
}
