# photod

Accepts originals from the phone, stores them content-addressed, derives
thumbnails and playback renditions in the background, and serves the archive
back as a browsable timeline.

## Running

```sh
docker compose up -d                 # Postgres, from the repo root
go run ./cmd/photod                  # migrations run at startup
```

photod serves no HTML. The gallery is the Next.js app in `../web`, which
proxies `/api/*` here; see its README to bring the browser side up.

| variable | default | meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8787` | bind address |
| `PHOTOS_ROOT` | `./data/photos` | holds `blobs/` and `manifest.jsonl` |
| `DERIVATIVES_ROOT` | `$PHOTOS_ROOT/derivatives` | thumbnails and playback files |
| `DATABASE_URL` | local compose Postgres | Postgres connection string |
| `WORKER_CONCURRENCY` | `4` | metadata + thumbnail workers |
| `TRANSCODE_CONCURRENCY` | `1` | video transcode workers |
| `PREVIEW_CONCURRENCY` | `4` | simultaneous on-demand preview conversions |
| `WORKER_DISABLED` | unset | run as a pure API server; nothing drains the queue |
| `VIDEO_ENCODER` | `libx264` | ffmpeg encoder for playback renditions |
| `MAGICK_BIN` / `FFMPEG_BIN` / `FFPROBE_BIN` / `EXIFTOOL_BIN` | on `PATH` | binary overrides |
| `MDNS_DISABLED` | unset | stop advertising; publish via Avahi instead |

Moving to the archive machine is `PHOTOS_ROOT=/mnt/photos` plus a
`DERIVATIVES_ROOT` on the SSD. Nothing else changes.

`DERIVATIVES_ROOT` is separate because the two directories want opposite
things: originals want the big slow drive and can never be regenerated;
derivatives want the fast disk the gallery is served from and can always be
rebuilt from the blobs.

## Endpoints

```
POST /v1/sync/check              dedup pre-check
POST /v1/assets                  raw body, metadata in X-Photo-* headers
GET  /v1/timeline                JSON page, newest first, keyset cursor
POST /v1/timeline/states         re-read derivative state for specific ids
GET  /v1/assets/{id}             JSON metadata for the viewer panel
GET  /v1/assets/{id}/original    exact stored bytes
GET  /v1/assets/{id}/thumb       stored 256px square WebP
GET  /v1/assets/{id}/preview     2048px WebP, rendered per request
GET  /v1/assets/{id}/playback    H.264 MP4, Range-capable
GET  /v1/jobs                    queue depth and the failure list
GET  /health                     also reports pending and failed job counts
```

Upload headers: `X-Photo-Filename`, `X-Photo-Md5`, `X-Photo-Size`,
`X-Photo-Device-Id`, `X-Photo-Local-Id` are required;
`X-Photo-Captured-At` and `X-Photo-Modified-At` (RFC3339) are optional.

Every media response is content-addressed, so it carries a strong `ETag` and
`Cache-Control: immutable`. `/preview` checks `If-None-Match` *before*
converting, which is what keeps paging back through a viewer free.

## Derivatives

Two jobs per asset, in two separately sized pools:

| kind | does | pool |
|---|---|---|
| `metadata` | exiftool, the 256px thumbnail, a poster frame for video | `WORKER_CONCURRENCY` |
| `playback` | H.264/AAC MP4 capped at 1080p, `+faststart` | `TRANSCODE_CONCURRENCY` |

The pools are split so a handful of 4K transcodes cannot claim every slot and
starve the thumbnails behind them — during a backfill that would look like the
gallery doing nothing at all.

Stored on disk: `<sha>.thumb.webp`, and `<sha>.mp4` for video. The 2048px
preview is rendered per request and never stored; the browser's cache does the
caching a derivative file would.

Thumbnails are square center crops. The grid is fixed square cells, so a square
thumbnail is never downscaled by the browser and the timeline can compute its
layout from row height alone. The cost is that the grid crops.

Derivative writes are staged and renamed but never `fsync`ed. A blob is
irreplaceable and pays for durability on every write; a derivative can be
rebuilt by re-running one job, so paying `fsync` on every thumbnail through a
40,000-item backfill buys nothing.

### When something breaks

A job retries with exponential backoff up to 5 attempts, then parks as `failed`
with the error kept verbatim. Find them at `GET /v1/jobs`; the count also shows
up in `/health`. A permanently failed metadata job sets the asset's
`derived_state` to `failed`, which the gallery draws as an error tile rather
than one that spins forever.

A running job heartbeats its lease. Without that, a transcode longer than the
10-minute lease would be handed to a second worker while the first was still
encoding, and repeated reclaims would eventually mark a perfectly healthy job
as failed.

## Capture time

Two capture times are stored and neither overwrites the other: `captured_at` is
what the phone reported, `exif_captured_at` is what the file itself says. The
timeline sorts on `coalesce(exif_captured_at, captured_at, uploaded_at)`.

That ordering is what puts a photo shot in 2019 and imported into Photos last
week in 2019 rather than at the top. `exif_offset_minutes` is only set when the
file actually recorded a timezone; when it is null, the capture time was read as
UTC because there was nothing better to assume, and the viewer should not claim
to know the local time.

`/v1/timeline` sends that offset with every item, which is what lets the gallery
file a photo under the day it was taken instead of the day it falls on in
whatever timezone the browser happens to be in. A photo shot at 23:50 in Vermont
belongs under that day for a viewer in Berlin too.

## Commit ordering

An upload is committed blob first, then the manifest line, then the database
row — and the derivative job is enqueued *inside* the same transaction as the
row. There is no window where an asset exists that no worker will pick up.

A crash after the blob lands leaves the archive intact and the index behind;
re-uploading the same bytes reconciles it, because every step keys off the
SHA-256 and is idempotent.

The one known gap: a crash between the rename and the manifest append leaves a
blob with no manifest line. `photobackup verify` reconciles that in Phase 4.

## Dependencies

| tool | needed for |
|---|---|
| `magick` (ImageMagick with libheif) | thumbnails and previews |
| `exiftool` | capture times, camera, GPS |
| `ffmpeg` / `ffprobe` | video posters, dimensions, playback renditions |

```sh
brew install imagemagick exiftool ffmpeg          # macOS
sudo dnf install ImageMagick perl-Image-ExifTool ffmpeg   # Fedora, with RPM Fusion
```

Missing tools are logged at startup and never fatal. Uploads reach the disk on a
host where ffmpeg was never installed; only the derivatives fail. Verify the
HEIC delegate with:

```sh
magick your-photo.HEIC /tmp/out.webp
```

On macOS a libheif upgrade can leave the delegate linked against a stale x265;
`brew reinstall libheif imagemagick` fixes it. On Fedora it comes from RPM
Fusion.

## Tests

Tests use a real Postgres and the real media tools — nothing is mocked. The only
thing the worker does is sequence those tools, so a test with a stubbed ffmpeg
would be verifying the stub.

```sh
docker compose up -d
go test ./...
```

Fixtures live in `server/testdata/`, shared across the media packages:
`iphone-portrait.heic` is a real original off the phone and is the one that
exercises the combination that actually ships — HEIC, a rotated sensor read,
sub-second timestamps carrying their own UTC offset, and GPS.

Each package creates and migrates its own database (`photobackup_test_db`,
`photobackup_test_api`, `photobackup_test_jobs`, `photobackup_test_worker`) on
first run. They must stay separate: `go test ./...` runs packages concurrently,
and a shared database means one package truncates `assets` while another is
mid-test.

`TEST_DATABASE_URL` selects a different Postgres *server*; the per-package
database name is always appended to it, so the isolation survives the override.
