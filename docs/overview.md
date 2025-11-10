# Bakery Platform Overview

This wiki page captures the full scope of the bakery delivery platform, summarizing every requested capability and the way it is satisfied inside the single-binary Go application. Each subsection restates the user needs and explains where the implementation lives so the whole task is easy to audit.

## Requirements Summary

| User Request | Implementation | Location |
| --- | --- | --- |
| Single binary with Go backend and frontend inspired by https://masamadre.ru/order | `cmd/server/main.go` wires the HTTP server that serves the embedded SPA located in `pkg/httpapi/public_html` | `cmd/server/main.go`, `pkg/httpapi/public_html/app.gohtml`, `pkg/httpapi/server.go` |
| Residents of the Белая Ромашка district can schedule bread and croissant deliveries with morning cadence control | Order domain models capture delivery cycles and quantities while the UI captures preferred days | `pkg/order/model.go`, `pkg/order/service.go`, `pkg/httpapi/public_html/app.gohtml` |
| Store phone, address, and cadence details per order | Validation-enforced forms funnel into channel-backed services so data persists in the in-memory SQL driver | `pkg/order/service.go`, `pkg/storage/memorydriver/driver.go` |
| Admin console to manage inventory availability, bake times, and pricing | Embedded admin panel lets bakers add, update, and remove batches while syncing with inventory services | `pkg/inventory/service.go`, `pkg/httpapi/public_html/app.gohtml` |
| All assets embedded via `go:embed` and stored under `public_html` | HTTP layer embeds SPA assets from `pkg/httpapi/public_html` ensuring no external files are needed | `pkg/httpapi/server.go` |
| CLI flags for version, domain-based TLS, port selection, and pluggable databases | Main entrypoint parses the documented flags and passes configuration to the services | `cmd/server/main.go` |
| Free delivery messaging for the neighborhood | Customer storefront prominently highlights the free delivery promise | `pkg/httpapi/public_html/app.gohtml` |
| No mutexes, use channels for coordination, keep SQL driver standard-library compatible | Memory driver exposes `database/sql/driver` interfaces and channels orchestrate operations | `pkg/storage/memorydriver/driver.go` |
| Cross-compilation script plus GitHub Actions release on "stable release" commits targeting macOS, Linux, Windows, FreeBSD, and OpenBSD for amd64/arm64 | Script drives `go build` for each platform; workflow publishes release artifacts when triggered | `scripts/build_release.sh`, `.github/workflows/release.yml` |
| Documentation lives under `docs/` as wiki pages | This overview file serves as the central wiki for operators | `docs/overview.md` |

## Running Locally

- `go run ./cmd/server` starts the HTTP server on port 8765 by default.
- Pass `-domain example.com` in production to enable automatic HTTPS bootstrapping.
- The in-memory SQL driver persists data to `bakery.db` by default and requires no external services.

## Releasing

- Include the words `stable release` in the commit message on the `main` branch to trigger GitHub Actions.
- The workflow cross-compiles binaries for Linux, macOS, Windows, FreeBSD, and OpenBSD on both amd64 and arm64.
- Artifacts are uploaded to a tagged GitHub release named `Bakery stable release <commit>`.

## Admin Tips

- Use the admin console at `/admin` to add batches with baked time, price, and available quantity.
- Deleting a batch immediately removes it from the public menu and future deliveries.
