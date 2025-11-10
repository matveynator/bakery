# Bakery Platform Overview

This wiki page explains how the single-binary Go application serves both the customer storefront and the baker admin tools.

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
