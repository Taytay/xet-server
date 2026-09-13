# `github.com/guilt/xet-server/third_party/swagger-ui-dist`

```
package swaggerui // import "github.com/guilt/xet-server/third_party/swagger-ui-dist"

Package swaggerui embeds the vendored Swagger UI static assets (see VENDORED.md
in this directory for provenance/upgrade instructions) so cmd/xetd can serve
interactive API docs fully offline, with no CDN dependency at runtime.

VARIABLES

var Dist embed.FS
    Dist holds swagger-ui.css, swagger-ui-bundle.js,
    swagger-ui-standalone-preset.js, and the two favicons - everything
    internal/apidocs needs to serve a working Swagger UI page.
```
