# `xet-server/internal/landingpage`

```
package landingpage // import "xet-server/internal/landingpage"

Package landingpage renders small, self-contained HTML pages so anyone opening
a xetd port directly in a browser (e.g. http://localhost:8420/) immediately
sees what that port is for and where to go next — instead of a raw 404 or a
bare JSON response. Two variants: CASHandler for the CAS + Xet Data API port,
HubHandler for the Hub API shim port (started separately via -hub-addr).
No CSS/JS framework, no CDN dependency — one inline <style> block per page,
matching this project's "fully offline" posture (see internal/apidocs).

FUNCTIONS

func CASHandler(addr, hubAddr string) http.HandlerFunc
    CASHandler serves the landing page for xetd's main port (CAS protocol +
    Xet Data API + interactive API docs). addr is this server's own listen
    address (used only to build the quick-start example's -addr value, e.g.
    ":8420" — never turned into a fabricated hostname/URL, since this server
    may be bound to any interface or reached through any hostname); hubAddr is
    the Hub API shim's listen address, or "" if it wasn't started — used only to
    decide whether to mention it exists.

func HubHandler() http.HandlerFunc
    HubHandler serves the landing page for xetd's Hub API shim port (started
    separately via -hub-addr), which speaks huggingface_hub's own REST API
    so the real `hf upload`/`hf download` CLI commands work end-to-end via
    HF_ENDPOINT. Swagger UI is not mounted on this port (it lives on the
    CAS port, documenting both), so this page doesn't link to it directly —
    hubAddr's own OtherPortNote-equivalent isn't needed since the CAS port's
    landing page already cross-links here.
```
