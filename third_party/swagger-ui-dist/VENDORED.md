# third_party/swagger-ui-dist

Vendored static assets from [swagger-api/swagger-ui](https://github.com/swagger-api/swagger-ui),
release `v5.17.14`, licensed
[Apache-2.0](https://github.com/swagger-api/swagger-ui/blob/v5.17.14/LICENSE).

Only the five files `cmd/xetd`'s embedded API-docs UI actually needs are
vendored here (not the full swagger-ui repo, which bundles docs sites and
multiple UI variants unrelated to this use) - fetched directly from the
tagged release's `dist/` directory:

```bash
DEST=third_party/swagger-ui-dist
for f in swagger-ui.css swagger-ui-bundle.js swagger-ui-standalone-preset.js favicon-16x16.png favicon-32x32.png; do
  curl -sL -o "$DEST/$f" "https://raw.githubusercontent.com/swagger-api/swagger-ui/v5.17.14/dist/$f"
done
```

`index.html` alongside these files is this project's own (not vendored) -
see `cmd/xetd/apidocs.go` for how all six files are embedded into the
`xetd` binary via `go:embed` and served at `/api-docs`.

## Upgrading

Re-run the commands above with a newer tag, then re-run `xetd`'s test
suite and manually confirm `/api-docs` still renders `docs/openapi.yaml`
correctly (a major swagger-ui version bump can change bundle internals the
custom `index.html` below relies on, e.g. `SwaggerUIBundle`/
`SwaggerUIStandalonePreset` globals).
