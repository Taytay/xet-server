// Package landingpage renders small, self-contained HTML pages so anyone
// opening a xetd port directly in a browser (e.g. http://localhost:8420/)
// immediately sees what that port is for and where to go next — instead
// of a raw 404 or a bare JSON response. Two variants: CASHandler for the
// CAS + Xet Data API port, HubHandler for the Hub API shim port (started
// separately via -hub-addr). No CSS/JS framework, no CDN dependency — one
// inline <style> block per page, matching this project's "fully offline"
// posture (see internal/apidocs).
package landingpage

import (
	"fmt"
	"html/template"
	"net/http"

	"xet-server/internal/api"
	"xet-server/internal/casserver"
)

const style = `
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; line-height: 1.6; }
.container { max-width: 860px; margin: 0 auto; padding: 2rem 1rem; }
header { text-align: center; margin-bottom: 2.5rem; }
h1 { font-size: 2.2rem; font-weight: 700; color: #1a1a2e; }
.subtitle { color: #666; margin-top: .3rem; font-size: 1rem; }
.links { margin-top: .8rem; }
.links a { color: #4a6fa5; margin: 0 .6rem; text-decoration: none; }
.links a:hover { text-decoration: underline; }
section { margin-bottom: 2rem; }
h2 { font-size: 1.3rem; color: #1a1a2e; margin-bottom: .8rem; border-bottom: 2px solid #e0e0e0; padding-bottom: .4rem; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: .5rem .6rem; border-bottom: 1px solid #e0e0e0; font-size: .9rem; }
th { background: #eef; font-weight: 600; }
td code { background: #f0f0f0; padding: .15rem .4rem; border-radius: 3px; font-size: .85rem; }
.code-block { background: #1a1a2e; color: #e8e8e8; padding: 1rem; border-radius: 4px; overflow-x: auto; font-family: monospace; font-size: .85rem; line-height: 1.5; }
footer { text-align: center; color: #999; font-size: .85rem; margin-top: 3rem; }
footer a { color: #4a6fa5; text-decoration: none; }
`

type endpoint struct {
	Path        string
	Method      string
	Description string
}

type pageData struct {
	Title           string
	Subtitle        string
	Style           template.CSS
	Endpoints       []endpoint
	QuickStart      string
	OtherPortNote   string
	ShowAPIDocsLink bool
}

const pageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>{{.Style}}</style>
</head>
<body>
<div class="container">
  <header>
    <h1>{{.Title}}</h1>
    <p class="subtitle">{{.Subtitle}}</p>
    {{if .ShowAPIDocsLink}}
    <p class="links"><a href="/api-docs/">Swagger UI</a></p>
    {{end}}
  </header>
  {{if .OtherPortNote}}
  <section><p>{{.OtherPortNote}}</p></section>
  {{end}}
  <section>
    <h2>Endpoints</h2>
    <table>
      <thead><tr><th>Endpoint</th><th>Method</th><th>Description</th></tr></thead>
      <tbody>
      {{range .Endpoints}}
        <tr><td><code>{{.Path}}</code></td><td>{{.Method}}</td><td>{{.Description}}</td></tr>
      {{end}}
      </tbody>
    </table>
  </section>
  {{if .QuickStart}}
  <section>
    <h2>Quick Start</h2>
    <pre class="code-block">{{.QuickStart}}</pre>
  </section>
  {{end}}
  {{if .ShowAPIDocsLink}}
  <footer><p><a href="/api-docs/">Swagger UI</a></p></footer>
  {{end}}
</div>
</body>
</html>
`

var tmpl = template.Must(template.New("landingpage").Parse(pageTemplate))

func render(w http.ResponseWriter, data pageData) {
	data.Style = template.CSS(style)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "render landing page: "+err.Error(), http.StatusInternalServerError)
	}
}

// serveRootOnly wraps a rendered page so it only answers "/" — anything
// else 404s, exactly like http.ServeMux would if this were registered as
// a normal pattern-based route, since a bare HandlerFunc otherwise
// matches every path under it.
func serveRootOnly(data pageData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		render(w, data)
	}
}

// CASHandler serves the landing page for xetd's main port (CAS protocol +
// Xet Data API + interactive API docs). addr is this server's own listen
// address (used only to build the quick-start example's -addr value, e.g.
// ":8420" — never turned into a fabricated hostname/URL, since this
// server may be bound to any interface or reached through any hostname);
// hubAddr is the Hub API shim's listen address, or "" if it wasn't
// started — used only to decide whether to mention it exists.
func CASHandler(addr, hubAddr string) http.HandlerFunc {
	note := ""
	if hubAddr != "" {
		note = fmt.Sprintf("A Hub API shim is also running on %s, for the real `hf` CLI — open it directly for its own quick-start guide.", hubAddr)
	}
	data := pageData{
		Title:    "Xet Server",
		Subtitle: "A wire-compatible Xet CAS server, plus a simple Xet Data API for quick manual testing.",
		Endpoints: []endpoint{
			{casserver.XorbsPath, "POST", "Upload a serialized xorb (CAS protocol)"},
			{casserver.XorbsPath, "GET", "Fetch raw xorb bytes, honors Range"},
			{casserver.ShardsPath, "POST", "Upload a serialized shard"},
			{casserver.ReconstructionsPath, "GET", "File to xorb/chunk-range map"},
			{casserver.ReconstructionsPathV2, "GET", "Multi-range-optimized reconstruction map"},
			{casserver.ChunksPath, "GET", "Global chunk-dedup lookup"},
			{casserver.StoragestatsPath, "GET", "Eviction/storage policy stats"},
			{api.UploadPath, "POST", "Xet Data API: chunk, dedup, and store a file"},
			{api.FilesPrefix + "{id}", "GET", "Xet Data API: download a file"},
			{api.StatsPath, "GET", "Xet Data API: store-wide dedup stats"},
			{"/api-docs/", "GET", "Interactive Swagger UI for every endpoint on this port and the Hub shim"},
		},
		QuickStart: fmt.Sprintf(`xetd -addr %s -data ./xet-data

xet push -server http://<this-host>%s ./model.safetensors
xet pull -server http://<this-host>%s -out ./restored.safetensors <file-id>`, addr, addr, addr),
		OtherPortNote:   note,
		ShowAPIDocsLink: true,
	}
	return serveRootOnly(data)
}

// HubHandler serves the landing page for xetd's Hub API shim port
// (started separately via -hub-addr), which speaks huggingface_hub's own
// REST API so the real `hf upload`/`hf download` CLI commands work
// end-to-end via HF_ENDPOINT. Swagger UI is not mounted on this port (it
// lives on the CAS port, documenting both), so this page doesn't link to it
// directly — hubAddr's own OtherPortNote-equivalent isn't needed since the
// CAS port's landing page already cross-links here.
func HubHandler() http.HandlerFunc {
	data := pageData{
		Title:    "Xet Server — Hub API shim",
		Subtitle: "A huggingface_hub-compatible Hub API shim, paired with a Xet CAS server on another port.",
		Endpoints: []endpoint{
			{"/api/repos/create", "POST", "Create (or no-op re-touch) a repo"},
			{"/api/{repo_type}s/{repo_id}/revision/{revision}", "GET", "Repo info at a revision (resolve before listing/downloading)"},
			{"/api/{repo_type}s/{repo_id}/tree/{revision}", "GET", "List every file committed to a revision"},
			{"/api/{repo_type}s/{repo_id}/branch/{branch}", "POST", "Create a branch (revision)"},
			{"/api/{repo_type}s/{repo_id}/xet-read-token/{revision}", "GET", "Issue a CAS endpoint + read-scoped bearer token"},
			{"/api/{repo_type}s/{repo_id}/xet-write-token/{revision}", "GET", "Issue a CAS endpoint + write-scoped bearer token"},
			{"/api/{repo_type}s/{repo_id}/commit/{revision}", "POST", "Commit file entries to a revision"},
			{"/api/{repo_type}s/{repo_id}/preupload/{revision}", "POST", "Negotiate per-file upload mode"},
			{"/{repo_id}/resolve/{revision}/{filename}", "HEAD", "File metadata (triggers the Xet download path)"},
		},
		QuickStart: `export HF_ENDPOINT="http://<this-host>:<this-port>"
export HF_TOKEN="anything"   # ignored unless xetd was started with -auth-token

hf upload myuser/my-model ./model.safetensors model.safetensors
hf download myuser/my-model model.safetensors --local-dir ./downloaded`,
	}
	return serveRootOnly(data)
}

// ProxyCASHandler serves the landing page for cmd/xet-proxyd's CAS-facing
// port — CASHandler's counterpart for the proxy binary rather than xetd:
// same route table shape (it embeds a real casserver.Server — see
// internal/proxycas's package doc comment on why the endpoint list is
// identical), but the subtitle/quick-start explain the caching/relay
// behavior instead of xetd's "this is the only copy" framing. hubAddr is
// the proxy's Hub-facing port, or "" if -hub-addr wasn't set.
func ProxyCASHandler(addr, hubAddr string) http.HandlerFunc {
	note := ""
	if hubAddr != "" {
		note = fmt.Sprintf("A Hub-facing proxy is also running on %s, relaying to the real huggingface.co — open it directly for its own quick-start guide.", hubAddr)
	}
	data := pageData{
		Title:    "Xet Proxy Server",
		Subtitle: "A caching pull-through proxy for the real Xet CAS: relays to huggingface.co on a miss, serves from cache (or falls back to it on any upstream failure) otherwise.",
		Endpoints: []endpoint{
			{casserver.XorbsPath, "POST", "Relay a xorb upload upstream, then cache it locally too"},
			{casserver.XorbsPath, "GET", "Serve a cached xorb, or fetch-and-cache from upstream on a miss"},
			{casserver.ShardsPath, "POST", "Relay a shard upload upstream, then cache it locally too"},
			{casserver.ReconstructionsPath, "GET", "Serve a cached reconstruction, or fetch-and-cache the whole file's from upstream on a miss"},
			{casserver.ReconstructionsPathV2, "GET", "Same as above, multi-range-optimized response shape"},
			{casserver.ChunksPath, "GET", "Always relayed live (no stable local cache key)"},
			{"/api-docs/", "GET", "Interactive Swagger UI (same protocol as xetd's — see openapi.yaml)"},
		},
		QuickStart: fmt.Sprintf(`xet-proxyd -addr %s -hub-addr <hub-port> -data ./xet-proxy-data

# point HF_ENDPOINT at the Hub-facing port (not this one) for the real hf CLI —
# see the Hub-facing port's own landing page for that quick-start.`, addr),
		OtherPortNote:   note,
		ShowAPIDocsLink: true,
	}
	return serveRootOnly(data)
}

// ProxyHubHandler serves the landing page for cmd/xet-proxyd's Hub-facing
// port — HubHandler's counterpart for the proxy binary. Same route table
// as HubHandler (internal/proxyhub embeds a real hubserver.Server too),
// but every route here can fall back to a live relay-and-cache against
// the real huggingface.co, and every read falls back to whatever's
// already cached on any upstream failure.
func ProxyHubHandler() http.HandlerFunc {
	data := pageData{
		Title:    "Xet Proxy Server — Hub API shim",
		Subtitle: "A caching pull-through proxy for the real huggingface.co Hub API — point HF_ENDPOINT here and the real `hf` CLI works, offline-resilient once something's been fetched once.",
		Endpoints: []endpoint{
			{"/api/repos/create", "POST", "Relay repo creation upstream, then mirror it into the local cache"},
			{"/api/{repo_type}s/{repo_id}/revision/{revision}", "GET", "Serve cached repo info, or fetch-and-cache from upstream"},
			{"/api/{repo_type}s/{repo_id}/tree/{revision}", "GET", "Serve a cached file listing, or fetch-and-cache from upstream"},
			{"/api/{repo_type}s/{repo_id}/branch/{branch}", "POST", "Relay branch creation upstream, then mirror it into the local cache"},
			{"/api/{repo_type}s/{repo_id}/xet-read-token/{revision}", "GET", "Relay for the real CAS endpoint/token (never served from cache alone)"},
			{"/api/{repo_type}s/{repo_id}/xet-write-token/{revision}", "GET", "Same as above, write-scoped"},
			{"/api/{repo_type}s/{repo_id}/commit/{revision}", "POST", "Relay a commit upstream, then mirror it into the local cache"},
			{"/api/{repo_type}s/{repo_id}/preupload/{revision}", "POST", "Always relayed live (upload-mode negotiation)"},
			{"/{repo_id}/resolve/{revision}/{filename}", "HEAD", "Serve cached file metadata, or fetch-and-cache from upstream"},
		},
		QuickStart: `export HF_ENDPOINT="http://<this-host>:<this-port>"
export HF_TOKEN="anything"   # forwarded upstream unchanged — see README's Authentication section

hf download someuser/some-model --local-dir ./downloaded
# every repo/file this proxy has served once stays servable even after
# huggingface.co becomes unreachable — see the main README's
# "Caching pull-through proxy" section for the full offline-handoff story.`,
	}
	return serveRootOnly(data)
}
