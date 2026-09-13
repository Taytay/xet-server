// Package swaggerui embeds the vendored Swagger UI static assets (see
// VENDORED.md in this directory for provenance/upgrade instructions) so
// cmd/xetd can serve interactive API docs fully offline, with no CDN
// dependency at runtime.
package swaggerui

import "embed"

// Dist holds swagger-ui.css, swagger-ui-bundle.js,
// swagger-ui-standalone-preset.js, and the two favicons - everything
// internal/apidocs needs to serve a working Swagger UI page.
//
//go:embed swagger-ui.css swagger-ui-bundle.js swagger-ui-standalone-preset.js favicon-16x16.png favicon-32x32.png
var Dist embed.FS
