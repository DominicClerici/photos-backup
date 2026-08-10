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
| `UPLOAD_SESSION_TTL` | `24h` | how long an abandoned partial upload is kept |
| `MDNS_DISABLED` | unset | stop advertising; publish via Avahi instead |

Moving to the archive machine is `PHOTOS_ROOT=/mnt/photos` plus a
`DERIVATIVES_ROOT` on the SSD. Nothing else changes; `deploy/` has the systemd
units and the Fedora notes.

`DERIVATIVES_ROOT` is separate because the two directories want opposite
things: originals want the big slow drive and can never be regenerated;
derivatives want the fast disk the gallery is served from and can always be
rebuilt from the blobs.

## Endpoints

```
POST /v1/sync/check              dedup pre-check
POST /v1/assets                  raw body, metadata in X-Photo-* headers
POST /v1/uploads                 open or resume a chunked upload
GET  /v1/uploads/{id}            how much of it the server holds
PUT  /v1/uploads/{id}            one chunk, positioned by Content-Range
POST /v1/uploads/{id}/commit     verify, store, index
DELETE /v1/uploads/{id}          abandon
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

## Resumable uploads

Anything under 64MB goes through `POST /v1/assets` in one request. Above that,
the client opens a session and sends 8MB chunks.

The session id is **derived from the declaration** — `sha256(deviceId, localId,
md5, size)` — rather than allocated. So `POST /v1/uploads` is both "begin" and
"where did I get to": a phone killed mid-video knows nothing about the transfer
beyond the file it was sending, asks the same question again, and is told the
offset. Nothing about an in-flight upload has to survive on the client.

Partial uploads live in `$PHOTOS_ROOT/incoming` as `<id>.part` alongside a
`<id>.json` holding the declaration. The received length *is* the size of the
part file, so there is no counter that can disagree with the bytes. Every chunk
is appended and `fsync`ed before its new offset is reported, which is what makes
a reported offset a promise rather than a guess.

A chunk that starts anywhere else gets `409` **with the real offset in the body**,
so a client whose idea of progress has drifted re-seeks instead of restarting a
550MB video. Commit re-reads the assembled file to compute both digests, checks
them against the declaration, and renames it into the blob tree — from there it
is the same code a single-shot upload runs, so the two cannot drift apart.

Abandoned sessions are swept at startup and hourly (`UPLOAD_SESSION_TTL`), and
by `photobackup verify --fix`.

`incoming/` is under `PHOTOS_ROOT` and not separately configurable, because a
committed session becomes a blob by rename and a rename cannot cross a
filesystem.

## Classifying an upload

The filename's extension is trusted when it is recognised, and the leading 512
bytes settle it when it is not. That is not a nicety: a Google Takeout export
strips the extension off every Live Photo's paired video, and 216 of the 3,116
files in the Phase 4 test corpus arrived that way. Without sniffing they became
`application/octet-stream`, were filed as images, failed their thumbnail, and
never had a playback rendition queued.

Go's `http.DetectContentType` is not enough on its own — it only recognises MP4
brands containing "mp4", so it calls every HEIC and every QuickTime movie an
octet-stream. `internal/mediatype` reads the `ftyp` brands properly, including
the compatible-brand list where HEIC usually hides, and recognises the
`ftyp`-less QuickTime that iOS writes with `moov` at the end, which even
`file(1)` reports as "data".

A file nothing identifies is still archived, as an octet-stream. An archive that
refuses what it cannot classify is not an archive.

## Timestamp precision

Client-supplied capture and modification times are truncated to milliseconds on
the way in, and compared at that precision afterwards.

This is load-bearing. A device mapping is only trusted while its stored
`modified_at` still equals what the phone reports, and that equality is exact.
Go carries nanoseconds, Postgres stores microseconds, and JSON and RFC3339
headers disagree about how many decimals to write — so a client that spells the
same instant two different ways stores one and asks about the other, is told
"unknown" forever, and re-hashes its entire library on every run. The Phase 4
load harness did exactly this, and at 100GB it is the difference between a
backup that finishes and one that reads every original on the phone every time.

Milliseconds because that is the precision an iOS asset date actually has once
it has been through JavaScript; `Date.toISOString()` always emits three decimals.

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

The one known gap — a crash between the rename and the manifest append leaves a
blob with no manifest line — is what `photobackup verify --fix` reconciles, and
what `photobackup reindex --adopt-orphans` recovers if the database is also gone.

## photobackup

The maintenance CLI. Reads the same environment photod does.

```sh
photobackup verify [--deep] [--fix]     audit the archive against itself
photobackup export --to DIR [--copy]    materialize a date tree of hardlinks
photobackup reindex [--adopt-orphans]   rebuild the database from manifest.jsonl
```

**verify** runs five passes: assets against blobs, blobs against assets, the
manifest in both directions, and the derivative state against the files it
claims. Default is `stat` only and takes seconds; `--deep` re-hashes every
original, which is the bit-rot check and what the weekly timer runs.

`--fix` applies only the repairs with one obvious answer: append a missing
manifest line from the database row, re-enqueue a derivative that has gone
missing, delete an abandoned partial. It never deletes a blob and never
"repairs" a hash mismatch — the only honest response to an original that no
longer matches its own name is a human with a second copy.

| exit | meaning |
|---|---|
| 0 | intact |
| 1 | findings, none of them lost or damaged originals |
| 2 | originals missing or no longer matching their hash |

**export** materializes `YYYY/YYYY-MM-DD/original-filename` as hardlinks to the
blobs, using the same sort time and UTC offset the gallery groups on. The blob
tree is meaningless without the database; this is how that price is paid back,
and it costs no bytes. It refuses to cross a filesystem boundary rather than
silently falling back to copying — `--copy` if that is genuinely wanted.

Measure that claim with one `du` over both trees, not two: `du -shc blobs
export` reports the export at 0B because it counts each inode once, while `du
-sh export` on its own reports the full library again. The second number is the
apparent size, not new bytes on the disk.

A live export is also a second reference to every blob, so deleting one out of
the blob tree frees nothing until the export goes too. Accidental protection
rather than a designed feature, but worth knowing before concluding a delete did
not take.

**reindex** replays `manifest.jsonl`, restoring asset rows *and* the device
mappings. The mappings matter as much as the rows: without them the phone is
told "unknown" for everything it holds and re-hashes its whole library on the
next run, recovering the archive but not the property that makes backing it up
cheap. `--adopt-orphans` also indexes blobs the log never recorded, reading
their type off the file. Idempotent, so running it against a merely incomplete
database is safe.

It restores those mappings only for the device that *uploaded* each blob. A
second device holding the same photos never stored anything — `sync/check`
matched it by content and recorded the mapping in the database alone — and the
manifest is a log of stored blobs, so there was never a line to replay. After a
rebuild that device is told "unknown" once, hashes its library, matches by
content again, and is back to instant on the run after. One expensive run per
extra device, self-healing, and not fixable without logging something other than
what was stored.

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
