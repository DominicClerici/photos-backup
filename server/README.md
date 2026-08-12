# photod

Accepts originals from the phone, stores them content-addressed, derives
thumbnails and playback renditions in the background, and serves the archive
back as a browsable timeline.

## Running

```sh
docker compose up -d                 # Postgres, from the repo root
go run ./cmd/photod                  # migrations run at startup, TLS generates itself
photobackup pair                     # a code to pair a device with
```

photod serves no HTML. The gallery is the Next.js app in `../web`, which
proxies `/api/*` here; see its README to bring the browser side up.

| variable | default | meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8787` | bind address for the **HTTPS** listener |
| `PLAINTEXT_ADDR` | `127.0.0.1:8788` | read-only listener, in the clear; empty disables it |
| `TLS_DIR` | `$PHOTOS_ROOT/tls` | the CA and the server certificate |
| `TLS_EXTRA_SANS` | unset | extra names or addresses to certify, comma-separated |
| `TLS_DISABLED` | unset | serve everything in the clear; **development only** |
| `PHOTOS_ROOT` | `./data/photos` | holds `blobs/` and `manifest.jsonl` |
| `DERIVATIVES_ROOT` | `$PHOTOS_ROOT/derivatives` | thumbnails and playback files |
| `DATABASE_URL` | local compose Postgres | Postgres connection string |
| `WORKER_CONCURRENCY` | `4` | metadata + thumbnail workers |
| `TRANSCODE_CONCURRENCY` | `1` | video transcode workers |
| `PREVIEW_CONCURRENCY` | `4` | simultaneous on-demand preview conversions |
| `LIVE_PREVIEW_CONCURRENCY` | `2` | simultaneous on-demand Live Photo renditions |
| `LIVE_PREVIEW_CACHE_MB` | `64` | memory those renditions are held in between requests |
| `WORKER_DISABLED` | unset | run as a pure API server; nothing drains the queue |
| `VIDEO_ENCODER` | `libx264` | ffmpeg encoder for playback renditions; `h264_nvenc` on an NVIDIA host |
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

Everything under **auth** requires `Authorization: Bearer <device token>` and is
served on the HTTPS listener only. Everything else is open, on both listeners.

```
POST /v1/pair                    redeem a pairing code for a device token

auth:
POST /v1/sync/check              dedup pre-check
POST /v1/assets                  raw body, metadata in X-Photo-* headers
POST /v1/uploads                 open or resume a chunked upload
GET  /v1/uploads/{id}            how much of it the server holds
PUT  /v1/uploads/{id}            one chunk, positioned by Content-Range
POST /v1/uploads/{id}/commit     verify, store, index
DELETE /v1/uploads/{id}          abandon
POST /v1/assets/{id}/import-metadata   an export's sidecar for an archived asset

open:
GET  /v1/timeline                JSON page, newest first, keyset cursor
POST /v1/timeline/states         re-read derivative state for specific ids
GET  /v1/assets/{id}             JSON metadata for the viewer panel
GET  /v1/assets/{id}/original    exact stored bytes
GET  /v1/assets/{id}/thumb       stored 256px square WebP
GET  /v1/assets/{id}/thumb/{px}  the same square at 96, 256 or 512
GET  /v1/assets/{id}/preview     2048px WebP, rendered per request
GET  /v1/assets/{id}/playback    H.264 MP4, Range-capable
GET  /v1/assets/{id}/live/thumb[/{px}]  a Live Photo's motion, same sizes
GET  /v1/assets/{id}/live/preview       1080p with audio, rendered per request
GET  /v1/jobs                    queue depth and the failure list
GET  /health                     also reports pending and failed job counts
```

Upload headers: `X-Photo-Filename`, `X-Photo-Md5`, `X-Photo-Size` and
`X-Photo-Local-Id` are required; `X-Photo-Captured-At` and `X-Photo-Modified-At`
(RFC3339) are optional, as are `X-Photo-Live-Parent-Local-Id` and
`X-Photo-Content-Id`, the two ways an upload can declare it is half of a Live
Photo. `X-Photo-Device-Id` is optional and no longer an identity
claim — the token names the device, and a header that disagrees with it is a 403
rather than a silent correction.

Every media response is content-addressed, so it carries a strong `ETag` and
`Cache-Control: immutable`. `/preview` checks `If-None-Match` *before*
converting, which is what keeps paging back through a viewer free.

The unsized `/thumb` is the 256px rendition and is the only one every asset is
guaranteed to have; a size that has not been rendered yet is a `404`, never the
nearest one that exists. Since these URLs are cached forever, answering
`/thumb/512` with the 256px file would pin the wrong bytes in a browser long
after the real rendition landed. The gallery falls back on its own.

## Authentication

One credential per device, obtained once and used forever.

```sh
photobackup pair                          # prints ABCD-EFGH, ten minutes, one use
photobackup devices                       # who is paired, and when they last wrote
photobackup devices --revoke <id>         # that token stops working immediately
```

The code is eight Crockford base32 characters — no I, L, O or U, so nothing in it
can be misread as a digit — and redeeming it is what mints the token. Both are
stored as sha256 digests, so a dump of this database cannot be replayed to write
into the archive.

sha256 rather than a password hash, deliberately. A token is 256 bits of
randomness with no dictionary behind it, so there is nothing for argon2 to slow
down — and a 3GB video is around 375 authenticated chunk requests, which is 375
verifications per upload that a password KDF would make expensive for no gain.

`photobackup pair` writes to the database rather than asking photod for a code,
so it works whether or not the daemon is up and there is no admin endpoint to
protect. Being able to mint a credential is exactly the authority that write
access to the database already carries.

### What the token settles

The device id used to be a random string the app made up for itself. It is now a
uuid the server issues at pairing, and every device id that reaches the database
comes from the authenticated identity — not from a header or a JSON field.
`X-Photo-Device-Id` and the `deviceId` body field are still read, only to be
checked against the token, so a stale install is told rather than quietly
attributed to the right device anyway.

Upload sessions are scoped by it too. A session id is `sha256(deviceId, localId,
md5, size)`, so a second paired device that knew what the first was sending could
otherwise resume, commit, or abort its transfer; every session endpoint checks
ownership first and answers 404 when it fails, because confirming that a session
exists is part of what is being withheld.

### What it does not cover

The gallery's read endpoints — `/v1/timeline`, `/v1/assets/*`, `/v1/jobs` — are
**open to anyone who can reach them**. That is a deliberate Phase 5 scope: the
write path is closed, the read path is not yet. Anyone on the LAN can page
through the archive and download originals. It is an accepted, known gap, in the
same category as the single drive, and `TestReadPathNeedsNoToken` exists so that
changing it is a decision rather than an accident.

### Two listeners

| | serves | who reaches it |
|---|---|---|
| `LISTEN_ADDR` (HTTPS) | everything | the phone |
| `PLAINTEXT_ADDR` (HTTP) | the read path and `/health` | the Next app, the browser, the CLI |

The plaintext listener exists so the gallery does not have to trust a private CA
to load a thumbnail. Pairing is absent from its routing table and every
authenticated route answers `426 Upgrade Required`, so **a device token cannot
travel unencrypted regardless of where that listener is bound**. That is a
property of the routes rather than of a check inside a handler, which is what
makes widening `PLAINTEXT_ADDR` to the LAN — something the gallery may
legitimately want — safe for credentials, whatever it does for the read path.

`TLS_DISABLED=1` collapses both onto one cleartext listener, tokens and all. It
exists for development, it logs a warning saying exactly that, and it is the one
switch here that gives something away.

### When the database is down

The write path closes, because authenticating a device reads it. That is a change
from Phase 4, where the same request stored its blob and then failed to index it.

Nothing is lost either way: the phone never gets an ack, so the item stays queued
and the bytes arrive when Postgres does. The alternative is caching tokens in
memory, which would buy an early blob write at the cost of a revoked device still
uploading for as long as the cache held it. Refusing is the cheaper answer.

## TLS

photod issues its own certificates on first run. There is no certificate
authority to buy from and no domain name involved.

```sh
photobackup ca                 # where it is, its fingerprints, what it covers
photobackup ca --serve         # hand it to a phone once, then stop
```

Two certificates with very different jobs:

| | lifetime | changes when |
|---|---|---|
| `ca.crt` / `ca.key` | 10 years | never, if it can be helped |
| `server.crt` / `server.key` | 395 days | the machine's addresses change, or 30 days before expiry |

The CA is the trust anchor and the only file that goes on the phone. The leaf is
disposable, and that split is the point. A DHCP lease handing the archive machine
a new address, or a Tailscale interface that only comes up after photod started,
would otherwise leave a certificate whose SANs no longer cover the address the
phone dials — a backup that stops working for a reason nothing on either screen
would explain. photod re-checks its addresses every five minutes and reissues the
leaf when they move, swapping it in through `GetCertificate` with no restart.
Because the CA never moves, the phone never has to be touched again.

The leaf certifies every non-loopback address on every interface that is up, plus
the loopbacks, the hostname, `<hostname>.local`, `localhost`, and anything in
`TLS_EXTRA_SANS`. **A phone dialling an address that is not on that list will
refuse the connection**; `photobackup ca` prints the current list, which is the
first thing to check when the app cannot connect.

Address collection is deliberately not shared with `internal/discovery`. That one
requires a multicast-capable interface, which is right for mDNS and wrong here: a
Tailscale TUN device is not multicast-capable, so reusing it would leave the
Tailscale address out of the certificate and break only the away-from-home path.

A CA that is present but unreadable stops the daemon rather than being replaced.
Minting a new one silently would leave every paired phone rejecting this server
with nothing to explain it, and the choice between restoring the file and
re-pairing every device belongs to a human.

`TLS_DIR` defaults to `$PHOTOS_ROOT/tls` because that is the one directory photod
is guaranteed to own, but it belongs on the SSD on a real deployment: `ca.key` is
machine state rather than part of the library, and it is the only file here whose
loss means re-pairing every device. `deploy/photod.env.example` moves it.

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
| `metadata` | exiftool, the 256px thumbnail, a poster frame for video, a Live Photo's 256px motion | `WORKER_CONCURRENCY` |
| `playback` | H.264/AAC MP4 capped at 1080p, `+faststart` | `TRANSCODE_CONCURRENCY` |

The pools are split so a handful of 4K transcodes cannot claim every slot and
starve the thumbnails behind them — during a backfill that would look like the
gallery doing nothing at all.

Splitting the pools bounds the slots, not the CPU, and libx264 will take every
core it can reach whatever its pool size is. Measured on the archive machine
(Ryzen 9 9950X, RTX 5060 Ti) against a queue of 1080p60 HEVC iPhone clips:

| encoder | queue of 149s of footage | 48 thumbnails alongside it |
|---|---|---|
| `libx264`, `TRANSCODE_CONCURRENCY=1` | 25.4s | — |
| `libx264`, `TRANSCODE_CONCURRENCY=4` | 23.4s | 7.8s (3.6x slower than idle) |
| `h264_nvenc`, `TRANSCODE_CONCURRENCY=4` | 9.8s | 2.9s (1.3x slower than idle) |

Raising `TRANSCODE_CONCURRENCY` on libx264 buys little, because one clip
already saturates the machine — 90s of CPU for 4s of wall on a single 18s
clip. The encoder is the lever that matters: NVENC costs about a twelfth of
the CPU, which is what keeps the metadata pool moving while the queue drains.
`VIDEO_ENCODER=h264_nvenc` with `TRANSCODE_CONCURRENCY=4` is the archive
machine's configuration; the defaults stay portable.

Stored on disk: `<sha>.thumb.webp`, and `<sha>.mp4` for video. The 2048px
preview is rendered per request and never stored; the browser's cache does the
caching a derivative file would.

A Live Photo's paired video is the exception to both rows. It queues no
transcode and gets no poster — the tile it appears in belongs to the still it is
paired with — and stores one rendition, `<sha>.live.mp4`, 256px square and
silent, which the grid plays on hover. The 1080p version the viewer plays on
press-and-hold is rendered per request like a photo preview, and held in memory
afterwards because a `<video>` asks for its bytes more than once. See PROJECT.md
§5 for why the split falls this way.

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

**A video's offset comes from a different tag.** QuickTime writes `CreateDate`
in UTC with no zone and `CreationDate` in local time with one, and reading only
the first gives the right instant with the timezone thrown away — 4,526 of the
real export's files, every one a video. `CreationDate` is read ahead of both UTC
tags, which is what took the share of the library with a known local time from
32% to 61%.

## What the metadata job reads

Everything in this table, off the original, on every upload and every reindex.
The percentages are of the real 15,689-file export, and they are why each one is
here — a tag nothing in the library carries is not worth a column.

| | | |
|---|---|---|
| the exposure | `iso`, `f_number`, `exposure_seconds`, `focal_length`, `focal_length_35`, `flash` | 50% |
| the rest of the fix | `gps_altitude`, `gps_direction`, `gps_accuracy`, `gps_at` | 57% / 30% |
| the stream | `video_codec`, `frame_rate`, `bitrate`, `audio_codec`, `audio_channels` | every video |
| faces | `faces`, as fractions of the image — geometry, not identity | 9% |
| the file's own caption | `exif_description` | 12% |
| colour and capture | `color_profile`, `capture_type` | 35% / 32% |
| all of it, verbatim | `exif_metadata` | every file |

`exif_metadata` is the same bargain `import_metadata` makes for a sidecar: the
columns beside it are a choice about what deserves an index, that choice is
wrong about something, and this is what makes being wrong cost a query rather
than a full-archive re-read. It is not the recovery copy — the original is, and
it outranks this.

Two rules hold across the whole table. `exif_description` fills `description`
only where nothing else has, so a caption typed into Google Photos outranks a
camera's "Screenshot" no matter which job ran last — the same shape
`import_gps_lat` already has. And a tag decodes leniently: files really do
record an ISO of `75.4582213796711` and a `Software` of `12.4` where a string
belongs, and read strictly, one odd tag fails the whole record, retries four
more times, and marks a good photograph broken and thumbnail-less.

## What only the phone knows

A heart, an album, and "this is a screenshot" are decisions a person made.
Nothing in the bytes records them, and re-reading the original recovers none of
them, so the app posts them to the same endpoint the Takeout importer posts its
sidecars to — `POST /v1/assets/:id/import-metadata`, with `source:
"ios-photokit"` — after the bytes are safely archived.

It is a second request rather than more upload headers for the reason the
importer's is: an album list has no business in a header, and a failure has to
cost the description rather than the photograph. It also runs for an asset the
archive already held, which is the only moment a library backed up before any of
this existed can still hand over its hearts and albums.

## Pairing a Live Photo

A Live Photo is two files, and there are two independent ways to know they are
two halves of one thing. The archive uses both, and writes both to the same two
columns, so nothing downstream has to know which one did the work.

**The declaration.** The phone names the still's local id on the video's upload
(`X-Photo-Live-Parent-Local-Id`). It is known before a byte is sent, so the
pairing is complete the moment the second half commits. It is also unavailable
to anything that is not the phone.

**The content identifier.** Apple stamps a UUID into both halves at capture: the
maker note on a HEIC or JPEG, `com.apple.quicktime.content.identifier` on a MOV
or MP4. Google's export preserves both, so this is what pairs a Takeout, a
restored backup, and anything copied off a Mac. It is not known until something
has read the file, which for an upload is after the bytes are already committed
— so the metadata worker records it (`assets.content_id`) and resolves the
pairing from whichever half it reaches second. An importer that has already read
it can send `X-Photo-Content-Id` to have the pairing resolve in the same
transaction as the insert, which saves building a poster and a transcode for a
file that turns out to be three seconds of a Live Photo.

The file always wins over the header. A client that declares an identifier the
bytes do not carry costs a pairing until the worker looks, and nothing after:
the resolution is undone and the asset it hid comes back.

**A declared video is hidden from the timeline immediately; a discovered one is
hidden only once it resolves.** The phone only declares a pairing for a video
whose still is already queued behind it, so there is nothing to wait for. An
export has no such guarantee — it can hold a paired video whose still was
deleted years ago, and 44 of the 130 files in the sample export are exactly
that. Those import as ordinary videos, with a poster and a playback rendition,
and pair themselves the day their still turns up: the still's own metadata job
adopts them, requeues their derivatives, and they leave the timeline.

One identifier can name more than one still — an export can hold a HEIC and a
JPEG re-export of the same capture — and the first one archived wins, ordered so
the choice does not move under a reindex.

## Importing a Google Photos export

```sh
photobackup import --from ~/Takeout/Google\ Photos [--dry-run]

# An export delivered as several zips is one export. Pass all of them.
photobackup import \
  --from ~/takeout-1/Takeout/Google\ Photos \
  --from ~/takeout-2/Takeout/Google\ Photos
```

It walks the export first and uploads second, because three decisions have to be
made over the whole tree before anything is sent: which video belongs to which
photo, which sidecar describes which file, and which directories are albums.
Then it uploads every still before any video, so a paired video's row finds its
still already archived.

**`--from` repeats, and a split export needs it to.** Google delivers a large
Takeout as numbered zips and splits an item from the JSON describing it across
them freely — `20180116_000028.mp4` in the sixth zip, its sidecar in the first.
Imported one directory at a time, 5,979 of the real export's 11,282 sidecars
describe a file that is not there, and everything they knew is gone the day the
export is deleted. Passing every directory to one run drops that to zero: the
scan groups sidecars, albums and media by where they sit *inside* the export
rather than on disk, so a file's local id is the same whether the export was
unzipped into one directory or six.

Uploads go over HTTPS to photod rather than straight into the blob tree, even
though the command runs on the archive machine. That way an import commits in
exactly the order an upload does, and two processes never append to
`manifest.jsonl` at once. It mints itself a device credential from the database
— the same authority `photobackup pair` already exercises — and reuses it across
runs, which is what makes a re-run cost one request per two hundred files
instead of re-reading the export. Re-running after a failure is the supported
recovery, and uploads zero bytes for everything that landed.

What it reads out of the export:

| from | what |
|---|---|
| the file | the Apple content identifier, and everything exiftool reads |
| `*.supplemental-metadata.json` | capture time, coordinates, caption, favourite, people, trash |
| the directory | album membership, and the album's title from its `metadata.json` |

A file with no extension is read like any other. exiftool recursing a directory
skips those by default, and a Takeout strips the extension off every Live
Photo's paired video — 239 of the real export's 15,689 files, every one of them
video. Left at the default the scan never sees them, so the import never uploads
them and the loss is invisible: no error, no unmatched sidecar, just a library
missing its motion.

The sidecars matter most for the files Google stripped: a screenshot or a saved
image has no EXIF at all, and `photoTakenTime` is the only capture time it has.
Coordinates from a sidecar are kept in `import_gps_lat`/`import_gps_lon` and
feed `gps_lat`/`gps_lon` only where the file itself carried none — the metadata
worker rewrites those two on every run, so a value merged straight into them
would survive exactly until the next reindex.

The whole sidecar is stored verbatim in `assets.import_metadata` as well. The
export is usually deleted the week after the import, and that JSON is then the
only copy of every field nobody has modelled yet.

Sidecar naming is the fiddly part and `internal/takeout` owns all of it: Google
caps the sidecar filename at 51 characters and truncates the
`.supplemental-metadata` suffix to fit (`.supplemental-met.json`, `.s.json`),
migrates a `(1)` collision counter to the end of the whole name, and writes no
sidecar at all for a Live Photo's video half — which inherits the still's. A
sidecar that matches no file is reported rather than dropped, because that is
the signal the rules have changed again.

`--dry-run` reports everything above and sends nothing. Trashed items are
skipped unless `--include-trash`; `archived` and `favorited` are recorded and
not acted on, because the gallery has nowhere to show them yet.

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
photobackup import --from DIR [--from…] ingest a Google Photos export
photobackup pair [--ttl 10m]            mint a single-use code to pair a device
photobackup devices [--revoke ID]       list paired devices, or unpair one
photobackup ca [--serve]                the CA to install on a device, and how
```

**verify** runs five passes: assets against blobs, blobs against assets, the
manifest in both directions, and the derivative state against the files it
claims. Default is `stat` only and takes seconds; `--deep` re-hashes every
original, which is the bit-rot check and what the weekly timer runs.

`--fix` applies only the repairs with one obvious answer: append a missing
manifest line from the database row, re-enqueue a derivative that has gone
missing, delete an abandoned partial. It is also how a new thumbnail size is
backfilled — a library ingested before the size existed is missing a derivative
by exactly the same test — so a run of `photobackup verify --fix` after adding
one requeues every asset that needs re-rendering. It never deletes a blob and never
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

On a machine that is also *running* photod, the compose Postgres cannot bind
5432 — the deployed one already has it. Point the tests at a second server
instead of stopping the archive:

```sh
docker run -d --name photobackup-test-pg -p 5433:5432 \
  -e POSTGRES_USER=photobackup -e POSTGRES_PASSWORD=photobackup \
  -e POSTGRES_DB=photobackup postgres:18-alpine
export TEST_DATABASE_URL=postgres://photobackup:photobackup@localhost:5433/photobackup?sslmode=disable
```

The api tests pair a real device and send its token on every write, rather than
reaching the handlers through a door only they can open. So "does the write path
require a token" is answered by every test in the package instead of by one of
them, and `internal/api/auth_test.go` covers the refusals: no token, an unknown
token, a revoked one, a session belonging to another device, and a server wired up
with no device store at all — which must serve no write path rather than an
unauthenticated one.

Fixtures live in `server/testdata/`, shared across the media packages:
`iphone-portrait.heic` is a real original off the phone and is the one that
exercises the combination that actually ships — HEIC, a rotated sensor read,
sub-second timestamps carrying their own UTC offset, and GPS.

Each package creates and migrates its own database (`photobackup_test_db`,
`photobackup_test_api`, `photobackup_test_jobs`, `photobackup_test_worker`,
`photobackup_test_devices`) on first run. They must stay separate: `go test ./...` runs packages concurrently,
and a shared database means one package truncates `assets` while another is
mid-test.

`TEST_DATABASE_URL` selects a different Postgres *server*; the per-package
database name is always appended to it, so the isolation survives the override.
