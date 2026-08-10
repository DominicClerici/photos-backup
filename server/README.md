# photod

The Phase 1 server: accepts one original at a time, stores it content-addressed,
and serves it back on a bare web page.

## Running

```sh
docker compose up -d                 # Postgres, from the repo root
go run ./cmd/photod                  # migrations run at startup
open http://localhost:8787/
```

| variable | default | meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8787` | bind address |
| `PHOTOS_ROOT` | `./data/photos` | holds `blobs/` and `manifest.jsonl` |
| `DATABASE_URL` | local compose Postgres | Postgres connection string |

Moving to the archive machine is `PHOTOS_ROOT=/mnt/photos`; nothing else changes.

## Endpoints

```
POST /v1/assets                  raw body, metadata in X-Photo-* headers
GET  /v1/assets/{id}/original    exact stored bytes
GET  /v1/assets/{id}/web         WebP rendition, converted per request
GET  /                           gallery
GET  /health
```

Upload headers: `X-Photo-Filename`, `X-Photo-Md5`, `X-Photo-Size`,
`X-Photo-Device-Id`, `X-Photo-Local-Id` are required;
`X-Photo-Captured-At` (RFC3339) is optional.

## Commit ordering

An upload is committed blob first, then the manifest line, then the database
row. A crash after the blob lands leaves the archive intact and the index
behind; re-uploading the same bytes reconciles it, because every step keys off
the SHA-256 and is idempotent.

The one known gap: a crash between the rename and the manifest append leaves a
blob with no manifest line. `photobackup verify` reconciles that in Phase 4.

## Dependencies

`magick` (ImageMagick with the libheif delegate) must be on `PATH` for
`/web` to render HEIC. Verify with:

```sh
magick your-photo.HEIC /tmp/out.webp
```

On macOS a libheif upgrade can leave the delegate linked against a stale x265;
`brew reinstall libheif imagemagick` fixes it. On Fedora the delegate comes from
RPM Fusion.

## Tests

Tests use a real Postgres and a real ImageMagick — nothing is mocked.

```sh
docker compose up -d
go test ./...
```

Each package creates and migrates its own database (`photobackup_test_db`,
`photobackup_test_api`) on first run. They must stay separate: `go test ./...`
runs packages concurrently, and a shared database means one package truncates
`assets` while another is mid-test.

`TEST_DATABASE_URL` selects a different Postgres *server*; the per-package
database name is always appended to it, so the isolation survives the override.
