# `xet-server/internal/apidocs`

```
package apidocs // import "xet-server/internal/apidocs"

Package apidocs serves this project's OpenAPI spec (openapi.yaml, this
directory) through a fully offline Swagger UI — no CDN dependency at runtime,
since both the spec and the UI's static assets (third_party/swagger-ui-dist) are
embedded into the xetd binary via go:embed. Mounted at /api-docs by cmd/xetd.

FUNCTIONS

func Handler() http.Handler
    Handler returns an http.Handler serving index.html and openapi.yaml (this
    package) plus every vendored Swagger UI asset (swaggerui.Dist), merged into
    one flat file tree — Swagger UI's index.html references its JS/CSS/spec by
    plain relative filename, so all three sources need to appear as siblings
    under the same URL prefix.
```
