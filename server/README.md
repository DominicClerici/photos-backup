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

That first line needs a `.env` at the repo root holding `POSTGRES_PASSWORD`;
compose fails with `set POSTGRES_PASSWORD in .env` without one, because the
password is no longer written into the committed `docker-compose.yml`. The
container publishes 5432 on loopback only. `DATABASE_URL` and, for the tests,
`TEST_DATABASE_URL` have to carry the same password — the built-in fallbacks in
`config.FromEnv` and the test helpers are a development convenience and only
work against a volume that was initialised with them.

photod serves one page — the sign-in form — and reverse-proxies the rest. The
gallery is the Next.js app in `../web`, which runs on loopback behind this
process; see its README to bring the browser side up.

| variable | default | meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8787` | bind address for the **HTTPS** listener |
| `WEB_ORIGIN` | unset | the origin a browser reaches this archive at; empty disables browser sign-in |
| `WEB_APP_URL` | `http://127.0.0.1:3000` | the Next process to reverse-proxy; empty serves the API only |
| `WEB_IDLE` | `1h` | ends a browser session that has not been used |
| `WEB_LIFETIME` | `12h` | the absolute cap on a browser session; nothing resets it |
| `TLS_DIR` | `$PHOTOS_ROOT/tls` | the CA and the server certificate |
| `TLS_EXTRA_SANS` | unset | extra names or addresses to certify, comma-separated |
| `TLS_DISABLED` | unset | serve everything in the clear; **development only** |
| `PHOTOS_ROOT` | `./data/photos` | holds `blobs/` and `manifest.jsonl` |
| `DERIVATIVES_ROOT` | `$PHOTOS_ROOT/derivatives` | thumbnails and playback files |
| `DATABASE_URL` | local compose Postgres | Postgres connection string |
| `GEONAMES_DIR` | `./data/geonames` | the offline geocoder's extract; absent means no place names |
| `WORKER_CONCURRENCY` | `4` | metadata + thumbnail workers |
| `TRANSCODE_CONCURRENCY` | `1` | video transcode and merge workers |
| `SIGNATURE_CONCURRENCY` | `1` | workers hashing originals for the duplicate scan |
| `PREP_CONCURRENCY` | `2` | workers writing the renditions a vision model reads |
| `ML_URL` | unset | where photo-ml is listening; absent means no vision pool and no vision work |
| `VISION_CONCURRENCY` | `1` | workers handing those renditions to photo-ml |
| `PREVIEW_CONCURRENCY` | `4` | simultaneous on-demand preview conversions |
| `LIVE_PREVIEW_CONCURRENCY` | `2` | simultaneous on-demand Live Photo renditions |
| `LIVE_PREVIEW_CACHE_MB` | `64` | memory those renditions are held in between requests |
| `WORKER_DISABLED` | unset | run as a pure API server; nothing drains the queue |
| `VIDEO_ENCODER` | `libx264` | ffmpeg encoder for playback renditions; `h264_nvenc` on an NVIDIA host |
| `MAGICK_BIN` / `FFMPEG_BIN` / `FFPROBE_BIN` / `EXIFTOOL_BIN` | on `PATH` | binary overrides |
| `UPLOAD_SESSION_TTL` | `24h` | how long an abandoned partial upload is kept |
| `PURGE_INTERVAL` | `1h` | how often the trash is swept for items past their 365 days |
| `PURGE_DISABLED` | unset | never destroy anything on a timer; the trash grows without bound |
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
served on the HTTPS listener only. Everything else is open, on both listeners —
including the **gallery** group, which writes. See [Two listeners](#two-listeners)
for what that costs.

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
GET  /v1/search                  a sentence, and the photographs it was about
GET  /v1/timeline                JSON page; keyset cursor or row offset
GET  /v1/timeline/days           every heading in the collection and its size
GET  /v1/timeline/locate         where one asset sits, for a link that names an id
POST /v1/timeline/states         re-read derivative state for specific ids
GET  /v1/assets/{id}             JSON metadata for the viewer panel
GET  /v1/assets/{id}/analysis    what the ML passes said about this one photo
GET  /v1/assets/{id}/original    exact stored bytes
GET  /v1/assets/{id}/thumb       stored 256px square WebP
GET  /v1/assets/{id}/thumb/{px}  the same square at 96, 256 or 512
GET  /v1/assets/{id}/preview     2048px WebP, rendered per request
GET  /v1/assets/{id}/playback    H.264 MP4, Range-capable
GET  /v1/assets/{id}/preview/plain    the same still without its Snapchat overlay
GET  /v1/assets/{id}/playback/plain   the same video without it burned in
GET  /v1/assets/{id}/live/thumb[/{px}]  a Live Photo's motion, same sizes
GET  /v1/assets/{id}/live/preview       1080p with audio, rendered per request
GET  /v1/merges                  how much is waiting, and how much has been analysed
GET  /v1/merges/groups?kind=&state=&approved=   the review: sets of items that should be one
GET  /v1/merges/{id}/preview     a join the worker built and refused to archive
GET  /v1/jobs                    queue depth and the failure list
GET  /v1/status                  the status page: library counts, disk, queue, and what is broken
GET  /health                     also reports pending and failed job counts

gallery (writes, but no device token — see below):
POST /v1/trash                   move a selection to Recently Deleted
POST /v1/trash/restore           put a batch, or a selection of the trash, back
POST /v1/trash/purge             destroy a selection of the trash outright
DELETE /v1/collections/albums/{id}[?photos=true]   drop an album, optionally its photos
POST /v1/merges/scan             look for duplicates and split recordings again
POST /v1/merges/{id}/merge       keep one copy of a group, trash the rest
POST /v1/merges/{id}/dismiss     record that these are different photographs
POST /v1/merges/{id}/undo        put a merge back: copies out of the trash, a join into it
POST /v1/merges/{id}/force       archive a join whose parts do not add up
POST /v1/merges/{id}/approve     record that a joined recording has been looked at
POST /v1/merges/{id}/unapprove   take that back
```

`/v1/timeline`, `/v1/timeline/days` and `/v1/timeline/locate` share a filter, and
so do their vault counterparts: one of `album`/`person`/`category`, plus `sort`
(`newest` — the default — `oldest`, `longest`, `shortest`), `kind` (`image` or
`video`), `favorites=1` and `unalbumed=1`. The last four combine freely with each
other and with the collection; the first three do not combine with one another.
An unknown sort or kind is a `400`, not a silently wider timeline. See
[Sorting and filtering](#sorting-and-filtering).

The three trash endpoints take the same body: `ids`, `ranges`, or both, plus one
of `album`/`person`/`category` naming the timeline those ranges are positions in,
plus `sort`, `kind`, `favorites` and `unalbumed` saying how that timeline was
being read.
A range is `{"start": n, "end": m}`, end exclusive, counted in exactly the units
`GET /v1/timeline?skip=` offsets by — which is what lets the gallery act on a
selection of forty thousand photographs it has never fetched. Which half of the
archive a position is counted in is the endpoint's to decide, not the request's:
a delete only ever reaches the library, a restore or a purge only ever reaches
the trash.

A delete answers `{"batch": "<uuid>", "deleted": n}`. The batch is the undo —
`POST /v1/trash/restore {"batch": ...}` — and it is what the gallery's toast
carries, because by the time anybody clicks Undo every position in the timeline
has moved. `deleted` counts items rather than rows: a Live Photo is one of the
first and two of the second, and the number worth showing somebody is the one
they can count on screen.

Upload headers: `X-Photo-Filename`, `X-Photo-Md5`, `X-Photo-Size` and
`X-Photo-Local-Id` are required; `X-Photo-Captured-At` and `X-Photo-Modified-At`
(RFC3339) are optional, as are `X-Photo-Live-Parent-Local-Id` and
`X-Photo-Content-Id`, the two ways an upload can declare it is half of a Live
Photo. `X-Photo-Device-Id` is optional and no longer an identity
claim — the token names the device, and a header that disagrees with it is a 403
rather than a silent correction.

Every media response is content-addressed, so it carries a strong `ETag` and
`Cache-Control: immutable`. `/preview` checks `If-None-Match` *before*
converting, which is what keeps paging back through a viewer free. `/preview`
and `/preview/plain` are two different pictures of one asset and carry different
tags, so a browser holding one is never handed it for the other.

The unsized `/thumb` is the 256px rendition and is the only one every asset is
guaranteed to have; a size that has not been rendered yet is a `404`, never the
nearest one that exists. Since these URLs are cached forever, answering
`/thumb/512` with the 256px file would pin the wrong bytes in a browser long
after the real rendition landed. The gallery falls back on its own.

## Sorting and filtering

Four orders and four facets, over the same query, the same cursor and the same
day table.

**Newest and oldest are one index read in two directions.** `assets_timeline_visible_idx`
is `(sort_time desc, id desc)`; a backward scan of it is `(sort_time asc, id
asc)`, so oldest keeps the keyset cursor and the constant-time page that newest
has. The cursor comparison flips with it — `TimelineFilter.beyond` — and so does
the `row_number()` the day table and every range-resolving write are counted in.

**Longest and shortest have no index and are allowed not to.** They order by
`duration_seconds`, which nothing indexes: every page costs a sort of the
filtered set, and `TimelinePosition` falls back to ranking the whole timeline to
find one row. They also hand out no `next_cursor`, because a cursor is a sort key
and this one is nullable — clients page them by `skip` instead. The trade is
deliberate: these are what somebody reaches for once to find the long video, and
an index on `duration_seconds` would be paid for by every upload forever. Both
carry `nulls last`, which is not `desc`'s default, so "longest first" cannot open
with every still in the archive.

**A timeline ordered by length has no days.** `GET /v1/timeline/days` answers
with one run carrying the whole count and an empty `day`, which is the client's
signal to draw no headings and reserve no room for them. Sending a heading per
tile instead would be a description of a shape the timeline does not have.

**The facets are predicates, and `kind` is the one the index helps with.**
`favorites` is a column. `unalbumed` is a `not exists` over `album_assets` joined
to `albums`, so an album in the trash or in the vault stops hiding what was in
it — the membership rows survive a delete, which is what makes the undo work.
Neither is fast, and neither has to be: the gallery spends its time under
`kind`, and these two are answers to questions asked once.

**Everything that resolves a position uses the same order.** `Selection.pick`
numbers rows with `TimelineFilter.order`, the same fragment the page and the day
table use, so a selection made in a grid sorted oldest-first deletes the
photographs somebody was looking at. There is a test that says exactly that.

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

### One listener, two credentials

Everything arrives on `LISTEN_ADDR` over TLS: the phone, the browser, the
gallery bundle, and every thumbnail in it.

| credential | carried as | who uses it |
|---|---|---|
| device token | `Authorization: Bearer pbk_…` | the phone |
| session | `__Host-photobackup_session` cookie | the browser |

`requireAuth` takes either, and it guards the read path and the gallery's
writes alike. The upload path is narrower still — `requireDevice`, because an
upload names a device and a session names none.

There used to be a second listener. `PLAINTEXT_ADDR` served the read path and
the gallery's writes in the clear and with no credential at all, so that the
Next app need not trust a private CA to load a thumbnail. It was safe only
because it was bound to `127.0.0.1` and both processes were on the same
machine — anyone who could open that port could read every photograph, empty
the trash and unlock the vault. Authenticating the gallery was the standing fix
for it in this file for two phases; it is done, and the listener is gone with
it.

What replaced it is one origin. photod terminates TLS, serves `/v1` and the
media itself, and reverse-proxies the Next process for everything else — so the
bundle, the JSON and the thumbnails all arrive from the same place under one
cookie. That is not tidiness: a browser attaches a same-origin cookie to an
`<img>` and will not attach a bearer header to one, which is the constraint
Phase 12 established and PROJECT.md records.

The sign-in page is served by photod rather than by Next, which means an
unauthenticated visitor receives no application code at all, and means signing
in still works while the Next process is down or being deployed.

`TLS_DISABLED=1` serves that one listener in the clear, tokens and all. It
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

Five kinds of job, in four separately sized pools:

| kind | does | pool |
|---|---|---|
| `metadata` | exiftool, the 256px thumbnail, a poster frame for video, a Live Photo's 256px motion | `WORKER_CONCURRENCY` |
| `playback` | H.264/AAC MP4 capped at 1080p, `+faststart` | `TRANSCODE_CONCURRENCY` |
| `merge` | concatenates the pieces of a split Snapchat recording into one archived original | `TRANSCODE_CONCURRENCY` |
| `signature` | decodes an original into the hashes the duplicate scan compares | `SIGNATURE_CONCURRENCY` |
| `mlprep` | writes the uncropped 1536px renditions the ML passes read | `PREP_CONCURRENCY` |
| `vision` | posts those renditions to photo-ml and stores the vectors | `VISION_CONCURRENCY` |

The pools are split so a handful of 4K transcodes cannot claim every slot and
starve the thumbnails behind them — during a backfill that would look like the
gallery doing nothing at all. `signature` gets a third pool for a related reason
and a stronger one: it is a full decode of every original in the archive plus
twenty sampled frames out of every video, it takes about an hour over a library
of this size, and nothing at all is waiting for the answer.

`mlprep` gets a fourth pool for the same reason again. It is another full decode
of every visible original, its output is read by a service that does not exist
yet, and putting it in front of a thumbnail would trade a gallery somebody is
looking at for a search feature nobody has typed into. Two workers rather than
one, because each item here is a single ImageMagick rather than twenty sampled
frames.

`vision` gets a fifth, and it is the only pool here that can be absent
altogether. It is a queue in front of a single GPU — a second worker does not
make the card faster, it makes two requests wait on it — and it depends on a
process the archive is allowed not to have, so it is started only when `ML_URL`
is set and it asks photo-ml whether it is there before claiming anything. See
*What a photograph shows* below.

`merge` shares the transcode pool because it is the same kind of work — ffmpeg
over a whole video, minutes rather than milliseconds. It is the only job here
that *adds* to the archive: see Merging below.

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

`mlprep` adds `<sha>.ml.webp` — the whole photograph, **uncropped**, 1536px on
its longest edge — and `<sha>.ml.0.webp` through `.ml.5.webp` for a video, six
frames spread across its running time. Uncropped is the entire point of the
file: `.thumb512.webp` is a square centre crop, which is right for a grid of
square cells and wrong for a model, because the subject is frequently at the
edge and a centre crop decides in advance what the photograph was about. Six
frames rather than one because a clip that starts on a beach and ends in a
restaurant is findable as both only if both were looked at.

Nothing reads them yet. They exist so that the vision service, when it arrives,
is handed image bytes over loopback and never opens a file under the archive —
which is what excludes the vault by construction rather than by a `WHERE` clause
somebody can forget to write — and so that swapping the model re-reads 61,000
small WebPs instead of re-decoding 73GB of HEIC and H.265. That is the
difference between minutes and hours, and it is what makes trying a second model
a decision rather than a project. Roughly 4GB for this library. See ML_IMAGES.md.

A Snapchat memory is the other exception. Its renditions are built from the
photograph and its caption layer composed, not from the file the job was handed
— see [Overlays](#overlays) — so every row above reads one extra blob for the
few hundred assets that carry one. Stills compose per rendition, which costs a
second decode. Video cannot: nothing in a browser will lay a transparent PNG
over a playing `<video>`, so the layer is burned into the pixels, and those
videos keep a second rendition, `<sha>.plain.mp4`, without it. That one exists
only so the viewer's toggle has something to show.

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
with the error kept verbatim. Find them at `GET /v1/jobs`, or on the gallery's
status page, which reads `GET /v1/status` — the same failures with the asset's
filename attached and a button that puts the lot on the clipboard as Markdown.
The count also shows up in `/health`. A permanently failed metadata job sets the asset's
`derived_state` to `failed`, which the gallery draws as an error tile rather
than one that spins forever.

A running job heartbeats its lease. Without that, a transcode longer than the
10-minute lease would be handed to a second worker while the first was still
encoding, and repeated reclaims would eventually mark a perfectly healthy job
as failed.

### What the status page measures

`GET /v1/status` answers four questions at once, because they are all claims
about the same instant: what the library holds, what the drive holds, what the
queue is doing, and what is wrong with the server.

The disk figures come from `statfs` rather than from adding up files — that is
the only number that includes the bytes photod did not put there. `used` is
`total - available`, so the blocks ext4 reserves for root count as used and the
two always close on the total. Against that, the originals are summed from
`assets.byte_size` per media kind, and the renditions are measured by walking
the derivative tree and charging each file to the kind of the original it was
made from (a video's poster frame is a video derivative, not a photo one). The
gap between the two — the database, the vault, the reserved blocks — is reported
as a remainder rather than distributed among the slices.

Two things are deliberately left out of the attributed figures. **The vault**:
its originals and renditions are on the same disks and their bytes are real, but
"your hidden photographs come to 12GB" is exactly the question the encryption
exists to refuse, so they fall into the remainder. **The other volume**: the
deployment puts blobs on the external drive and derivatives on the SSD, so the
response reports both volumes and a `same_volume` flag, and the gallery draws
the derivative sizes outside the ring when they are not on the disk the ring is
of.

The walk is cached for a minute. It is a `stat` of every rendition — on this
archive, about a quarter of a second — and the figure moves by megabytes an
hour, so re-walking it on every ten-second poll would buy nothing.

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

## Place names

The metadata job resolves a photograph's coordinates into a city, a state and a
country and writes them to `place_city`, `place_admin1` and `place_country`.
`photobackup geocode` does the same for everything already in the archive —
11,045 assets here, and the whole run takes under a second.

It is offline. A bundled GeoNames extract and a k-d tree in memory: no network,
no per-photo API call, and no coordinates leaving the machine. It is also not
machine learning, deliberately — nothing about "photos in Chicago" should be
able to break because a GPU is busy, or have to wait for somebody to choose a
model.

The extract is not in git. Download all three files into `GEONAMES_DIR`:

```sh
mkdir -p server/data/geonames && cd server/data/geonames
curl -O https://download.geonames.org/export/dump/cities500.zip
curl -O https://download.geonames.org/export/dump/admin1CodesASCII.txt
curl -O https://download.geonames.org/export/dump/countryInfo.txt
```

The zip is read as it downloads — there is no unzip step — and `cities500.txt`
is accepted in its place. All three are required rather than optional: falling
back to raw codes for a missing file would put "IL" in some rows and "Illinois"
in others depending on what somebody happened to fetch, and those columns are
search text, where an inconsistency is invisible until a query quietly misses
half the library.

Without the extract, photographs keep their coordinates and have no place name.
The metadata job logs one line and carries on: an optional reference table must
not be able to stop a thumbnail being built.

**Which name a photograph gets.** Not the nearest one. GeoNames records a city
and the neighbourhoods, wards and villages inside and beside it as separate
points, so nearest-centre answers a photograph taken at the Eiffel Tower with
"Paris 16 Passy" and one taken in Shibuya with the name of a block of 1,800
people — both correct, both useless, because the word somebody will type is
Paris. Instead each place is given a radius from its population, and the
largest place whose radius reaches the photograph wins. Chicago comes out at
13.6km, close to the real thing; a hamlet of 500 covers 200 metres.

The visible cost: Oak Park shares a border with Chicago, sits inside the radius,
and reads as Chicago. Evanston is 18km up the lakefront, outside it, and stays
Evanston. The line falls roughly where a city's own extent falls, and the
direction of the error is the helpful one — a search box is a place to type the
name of a city, not the name of a ward. A photograph with no inhabited place
within 150km — over open water, mostly — is recorded as having been looked at
and having none, which is what stops the backfill offering the same 67 assets
forever.

The vault takes the place name too, and the reason is worth stating: "Chicago"
is legible at a glance in a way `41.85, -87.65` is not, so leaving it on a
hidden photograph's row would be a worse leak than the coordinates the scrub
already empties. It goes into the sealed document and comes back on restore.

## What a photograph shows

The `vision` job is the one piece of this server that leaves the machine's own
process tree. It reads the ML renditions `mlprep` wrote, posts them to photo-ml
over loopback, and stores the 1152 numbers that come back in
`asset_embeddings` — one row per frame, so a video that starts on a beach and
ends in a restaurant is findable as both.

That division is the whole design: **Go decodes, Python does tensors.** photo-ml
is handed image bytes over a socket by a process that has already decided which
photographs it is allowed to see. It opens no files, holds no state, and talks
to no database, which is what lets its systemd unit put `/mnt/photos` and the
derivatives tree out of reach entirely — the vault is excluded by construction
rather than by a `WHERE` clause somebody can forget to write.

### Turning it on

```sh
ML_URL=http://127.0.0.1:8789
```

That is the whole configuration. Empty or absent means there is no GPU service,
and that is a supported way to run this archive forever: the vision pool is not
started, no vision work is queued, and photographs keep their dates, places,
cameras and filenames. Not "queued and never drained" — a machine with no
photo-ml would otherwise report a permanent 17,788-item backlog for a feature it
does not have.

Setting it and restarting is what turns the library into queued work.
`jobs.ReconcileVision` finds every asset whose renditions exist and that the
current model has said nothing about, and the pool drains them: about fifteen
minutes for a library this size. `photo-ml/README.md` and
`deploy/README.md § photo-ml` have the service side.

### When photo-ml is not there

The pool asks before it claims. A worker probes `/health` — at most once every
fifteen seconds however many workers there are — and simply does not take a job
while the answer is no, so a machine whose GPU service is down or being upgraded
has a vision pool that is genuinely idle rather than one turning sixty thousand
assets into failures.

For the one job already in flight when the service goes away there is
`jobs.Defer`: the job returns to pending with its attempt **rolled back**. That
is not bookkeeping. An attempt is a claim on the file — five of them mean five
real goes at the same bytes, which is how a genuinely broken original gives up
within the hour instead of burning ffmpeg forever — and a job that never reached
the bytes has not used one. Without it, `systemctl restart photo-ml` during a
backfill would take five swings at a closed socket for every queued asset and
park the library as permanently failed, recoverable only by a hand-written
UPDATE over sixty thousand rows.

The distinction that makes this work lives in `internal/mlclient`: every
transport failure and every 5xx wraps `ErrUnavailable` and costs nothing, while
a 4xx — a rendition that is not an image — is an ordinary error that burns
attempts and eventually parks the job with the service's own sentence kept
verbatim. One undifferentiated error type would make an outage look like sixty
thousand corrupt files.

### The model, and swapping it

`siglip2-so400m-patch14-384`, at 1152 dimensions, stored as `halfvec`. The name
is a constant in three places — `db.VisionModel`, the HNSW index predicate in
`0017_vision.sql`, and photo-ml's `encoder.MODEL_NAME` — because a partial index
is only reachable from a query that repeats its predicate literally. Leave the
`where model = ...` off a search and Postgres answers the same rows by
sequential scan over sixty thousand vectors, which is correct and slow enough to
look like a bug in the model. A photo-ml reporting a different name is stored
truthfully and warned about once.

`model` is part of the embeddings' primary key, which is what makes a swap a
data operation:

```sql
delete from asset_embeddings where model = 'siglip2-so400m-patch14-384';
```

Restart, and the reconcile queues the library again — fifteen minutes, not an
hour and a half, because the renditions are already on disk and decoding them
again is the expensive half. That is why `mlprep` and `vision` are separate
kinds. Two models can also sit in the table together while they are compared;
nothing requires the old rows to go first.

### And what it is of, in words

Two more passes over the same renditions, each its own job kind and each
draining through the same pool:

| kind | model | cost over the library | what it writes |
|---|---|---|---|
| `vision` | `siglip2-so400m-patch14-384` | 16 min | `asset_embeddings`, one row per frame |
| `ocr` | `rapidocr-v6-medium` (torch, GPU) | ~50 min | `asset_ocr` |
| `describe` | `qwen3-vl-4b-instruct` | 3–7 h | `asset_descriptions`, `tags`, `asset_tags` |

Three kinds rather than one, and the reason is the third column. Re-embedding
the library is fifteen minutes and re-captioning it is hours, so a single job
would tie every encoder bench to a full re-captioning — the same coupling
`mlprep` was split out to avoid, one step further along. Swapping the captioner
is `delete from asset_descriptions where model = '...'` and one command; it
touches neither the vectors nor the recognised text.

The pool drains them **in that order**, cheapest first, because they are three
passes over one archive in front of one GPU and FIFO across them would
interleave the three so that all three finished at the end. See
`jobs.ClaimInOrder` — it is the only pool that ranks its kinds, deliberately.

A photograph gets one frame; a video gets three, the first, the middle and the
last of whatever `mlprep` managed to sample. Their captions are joined and their
tags unioned, so a clip that opens on a beach and ends in a restaurant is
findable as both. The recognised text is deduplicated across those frames, which
matters more than it sounds: a Snapchat memory carries its caption burned into
every frame by construction, and storing it three times would rank that video as
though the words were three times as present as they are.

**The backfill is a command, not a reconcile.** Every other pass here is queued
by a restart; this one is not, because four hours of GPU begun by a
`systemctl restart photod` is a restart with a surprise in it — the same
objection migrations 0016 and 0017 made to queueing from a migration. New
uploads are still described the ordinary way, a minute after they land.

```sh
photobackup ml status                              how far the passes have got
photobackup ml backfill                            the whole library
photobackup ml backfill --stills 1000 --videos 20  a sample, newest first
photobackup ml backfill --kind ocr                 one pass at a time
photobackup ml backfill --force                    redo what already has words
photobackup ml reindex                             rebuild the tsvector
```

Because it is typed, it can be bounded — which is what makes a thousand
photographs and twenty clips an evening's worth of vocabulary to build a search
page against, rather than a choice between nothing and everything.

`--force` is the one to be deliberate about. Without it a backfill asks which
assets have no words and queues those, so a changed *recipe* under an unchanged
model name — a raised `captioner.MAX_PIXELS`, a rewritten prompt, better OCR
weights — queues nothing at all, because everything already has a caption. With
it, every asset in scope is queued again.

It deletes nothing to do that, and that is deliberate. `PutDescription` and
`PutOCR` upsert, `putTags` clears and rewrites the set, and each write rebuilds
its own `asset_search` row — so each photograph's words are replaced in one
transaction as the pass reaches it. The alternative, a `delete` and an ordinary
backfill, would leave the library with no captions for the hours the pass takes;
search that answers with stale words beats search that has quietly lost its
index. It also means `ml status` barely moves during a forced run, since it
counts assets that *have* words: watch the queue instead.

### Reading it back for one photograph

```
GET /v1/assets/{id}/analysis
```

A search read backwards. `/v1/search` takes a sentence and ranks photographs out
of these four tables; this takes a photograph and hands back the words the
ranking was built from — the caption, the tags with the merge resolved and the
model's own word beside it, the recognised text unabridged, and how many frames
the encoder wrote. It is what the viewer's panel draws, and it is the only way
to see what the captioner actually called something, which is what step 9's tag
cleanup has to be read through.

It carries the **state of each ML job** as well, and that is the part worth
defending. An asset with no caption is one the captioner has not reached, or one
it failed on, or one nothing has ever queued — three different pieces of news
that a panel drawing all of them as an empty box would report as the same
silence. `jobs` is what lets the difference be said out loud.

Its own route rather than four more fields on `/v1/assets/{id}`, for two reasons
that point the same way: the detail load is on every arrow-key press through the
viewer and the panel is a toggle, and recognised text is unbounded where every
other field on that response is a scalar. A screenshot of a terminal is
kilobytes of it.

Nothing is read for an asset in the vault. The write paths all refuse a sealed
asset so the tables are empty for those rows anyway — the guard is repeated
because "the tables happen to be empty" is a fact about the past.

### Cleaning up the vocabulary

```
GET  /v1/tags                    how much there is, and which stage it is in
GET  /v1/tags/words?junk=        one of the two review lists
GET  /v1/tags/proposals?similarity=   what the words cluster into
GET  /v1/tags/merged             what has been folded, and the undo
POST /v1/tags/triage             judge the next slice of words
POST /v1/tags/embed              compare the next slice of words
POST /v1/tags/judge              move words between the two lists
POST /v1/tags/approve            sign the verdicts off, and reindex
POST /v1/tags/merge              fold a group into one word
POST /v1/tags/dismiss            these are not one word
POST /v1/tags/unmerge            take a merge apart
```

ML_IMAGES.md §2 bought an open vocabulary by promising a cleanup later, and §11
called that "a bet on cleanup happening". This is the cleanup, and it is **two
passes** rather than the one §9 sketched.

§9 describes clustering the tag names and proposing merges. Running exactly that
against the real vocabulary showed why it has to come second. A vision model
looking at a screenshot writes "login", "result", "true", "screen" and
"details"; looking at people it writes "casual", "peaceful" and "friendly"; and
looking at anything at all it sometimes writes "photograph". None of those is a
word anybody will type, none of them merges into anything, and every one of them
sits in the **weight-A half** of every tsvector it is attached to — the same
weight as the caption. They also sit *between* the real synonyms in the encoder's
space and take up their neighbour slots. Clustering three thousand words when
two thousand of them are the question is more work and a worse answer.

So: **triage**, then **merge**.

```
words → captioner judges each one → two review lists → approve
      → what survived is embedded → clustered → merges proposed → accepted
```

**The captioner marks its own homework.** `POST /triage` on photo-ml borrows the
same weights `/describe` does — no second checkpoint, no second entry in the
residency table, no second nine gigabytes competing for the card. And the answer
is not generated. Asking a model for "the junk words as a JSON list" fails in the
way small models fail: measured against this archive's own vocabulary, a 0.6B
invented words that were never in the list and repeated one of them four hundred
times. So `judge()` runs one forward pass per word and reads the answer off the
logits of the two tokens it was told to choose between. That is a classifier: it
cannot hallucinate a word, cannot skip one, cannot reorder them, and costs
prefill only — a couple of minutes over three thousand words instead of twenty.
What comes back is P(junk), which is worth more than the bit it replaces: the
review list is read in that order, because a confident wrong verdict is the one
worth catching.

**Both passes are bounded slices, and the page loops.** Neither is a job kind.
They are typed by somebody who is about to sit and read the result, nothing in
the archive is waiting on them, and they happen once per model generation — but
they are also too long to hold a request open. So each call judges 120 words or
embeds 512, answers with how many are left, and the browser calls again. The
resume point is a column with an index on it, so closing the tab halfway costs
the loop and nothing else. A pass that dies mid-slice keeps what it learned.

**A model may fill in a blank; it may never overrule a person.** `PutTriage`
writes only `where judged_at is null`. That is ML_IMAGES.md §11's seam — a name
somebody confirmed against a word a model produced — as a where clause, and it
is what makes re-running the triage safe on a vocabulary that has grown. The
review screen draws it too: a verdict nobody has confirmed is a dashed chip.

**Approving is a real claim, not a formality.** It stamps every unconfirmed
verdict as this archive owner's own, so no later pass revisits it, and it
rebuilds the whole search index in the same transaction.

Which is the other half of what is going on here. §11 warns that "nothing
rebuilds the tsvector by itself… a tag merge leaves every row already written out
of date. `photobackup ml reindex` is the answer and it has to be *remembered*."
Every write above discharges that obligation inside its own transaction:
`db.refreshForTags` rebuilds the photographs carrying the words that changed, and
the two bulk operations rebuild the library once at the end rather than most of
it per chunk. Merging "mountain" into "mountains" on this archive rewrote 276
rows and made 117 photographs findable under a word nothing had ever called
them, with no command typed afterwards.

**The clustering is a graph walk, and the threshold is a control.** Migration
0019 stores a vector per tag name with an HNSW index over it. Measured on 3,000
words: a brute-force self-join is 7.7 seconds and the same neighbours through the
index are under one. That difference is the whole reason the review screen can
offer a slider — the vectors are stored, so dragging it is one query rather than
a re-embedding.

The default is **0.93**, and it is high because it has to be. SigLIP-2's text
tower puts the *median* pair of unrelated tags at 0.73 cosine, so a threshold
that sounds generous is not: at 0.80 the clustering proposes "man ← woman" and
"black ← white". At 0.93, against this archive:

```
mountains(149) ← mountain(118), mountain range(10), mountain peaks(1), mountainous(1)
skiing(149)    ← ski(2), snowboarding(3), skier(34), skiers(62), skis(24)
phone(102)     ← mobile(18), telephone(1), phones(2), smartphone(3)
cityscape(63)  ← city(42), urban landscape(5), city skyline(25)
drinks(57)     ← beverage(1), drink(8), drinking(10)
```

Grouping is **leader clustering** rather than the obvious union-find, and the
reason is chaining: with "dog" near "puppy", "puppy" near "kitten" and "kitten"
near "cat", single linkage produces one group containing a dog and a cat, every
link individually defensible. Requiring every member to be near the *head* stops
it. Words are taken most-used first and claimed the moment they are considered,
which guarantees the direction of every merge — a word can only ever be folded
into one used at least as much as itself, so the head is the word the archive
actually speaks.

**A disagreement has to be written down or it is not a disagreement.**
`tag_merge_blocks` is the tag vocabulary's version of `db.BlockedPairs`: without
it, rejecting a proposal accomplishes nothing, because the next run computes the
same distances over the same vectors and proposes it again. It records pairs
rather than groups, because what somebody disagrees with inside a proposal is
usually one member — "mountain, mountains, and no, not mountaineering" — and
blocking the group would also block the merges they had just agreed to.

**A merge brings an earlier merge's children with it.** `canonical_id` is
resolved exactly one hop everywhere it is read, so folding "puppy" into "dog"
while leaving "doggo → puppy" alone would leave "doggo" resolving to a word that
is no longer canonical: findable as neither. `MergeTags` repoints them, and
refuses a head that is itself folded rather than following the chain.

**Nothing here destroys a row.** `asset_tags` goes on recording exactly what the
captioner wrote about each photograph. `junk` and `canonical_id` are one column
each, read at every point of use — the tsvector recipe, the parser's vocabulary,
the viewer's panel — so every button takes effect everywhere at once and every
one of them has an opposite.

## Searching

```
GET /v1/search?q=phoenix at the beach last summer
```

A query is two questions with different right answers. The name and the date
range must be exact; *at the beach* must be fuzzy. So it is split, answered
separately, and fused.

```
"phoenix at the beach last summer"
   ↓ parse
 person=Phoenix AND sort_time in [2025-06-01, 2025-09-30]
   + vector("at the beach") ⊕ fts("at the beach")   → RRF → ranked
```

The structured half becomes a `db.TimelineFilter` — the same one the gallery's
own timeline uses, which gained a date range, a place and a tag for this and
nothing else. No second query engine, and every existing index still applies.

### The parser

`internal/searchquery`, a deterministic Go grammar, and it is the parser. It
reads dates (`last summer`, `christmas 2019`, `between 2019 and 2021`, `90s`,
`past three weeks`), matches names and places against **what this archive
actually contains**, and hands whatever is left to the encoder as the visual
phrase.

Two properties make it safe to trust.

**It only recognises what is here.** People come from `asset_people`, places from
the geocoded columns. "Phoenix" is a person because there are 1,601 photographs
of one; in an archive with no Phoenix it is a word for the fuzzy half. A parser
that recognised names in general would invent filters matching nothing, which
from the outside looks exactly like an empty library.

**Every range it produces is generous.** Christmas is a week, summer runs to the
end of September, and "last summer" in August is last year's — because a window
slightly too wide costs a few extra tiles at the bottom of a ranked page, and a
window slightly too narrow costs the answer *and* the reason.

A few words are deliberately not understood. A bare "may" is not a month, a bare
"fall" is not a season, and "photos" is not a filter — `show me photos of the
beach` is a question about a beach, not an instruction to exclude every video in
the archive. `only photos`, `stills` and `no videos` are how you say that.

There is an explicit syntax underneath, for when a guess is not what is wanted:
`person:chris_morrison`, `place:breckenridge`, `tag:dog`, `after:2019-06-01`,
`kind:video`, `category:screenshots`. A value this archive does not hold is left
where it was typed rather than becoming a filter that matches nothing.

### Then the model, on top

photo-ml's `/parse` runs after the grammar and is allowed to speak **only where
the grammar was silent**. Everything it says is checked against the same
vocabulary before any of it is believed:

- a person only if the query mentions a word of that name — which is what turns
  "chris" into `Chris Morrison`, and what stops a small model that parrots its
  hint list from ANDing five people together
- a place only if the query mentions it
- a date range only if the query says something temporal at all, and only both
  ends together — half a range from a model beside half from a grammar is a
  window neither of them meant
- a visual phrase only if every word of it was typed

Media kind, category and favourites are not on its contract at all: the grammar
answers those completely and a model can only disagree. Tags are not either —
filtering by a word one model invented, chosen by another, is two guesses
stacked.

ML_IMAGES.md §11 is blunt about why: *a query parser fails confidently*. It
decides "last summer" meant 2024, silently removes the right answer, and shows
an empty grid with nothing to argue with. The asymmetry above means the failure
mode left over is a parse that is too *narrow* — the model says something true
and unverifiable and it gets dropped — which is the right direction to fail in.

### Ranking

Two rankings, fused inside the structured `WHERE`:

- **vector** — the visual phrase through the text tower, nearest frames by
  cosine distance, collapsed to their asset by `min()`. A clip is as relevant as
  its best frame.
- **full text** — `websearch_to_tsquery` over `asset_search`, one tsvector per
  asset: caption and tags at weight A, recognised text and the imported
  description at B, filename and place name at C.

**Reciprocal-rank fusion**, k=60, rather than a weighted sum. Cosine similarity
runs about 0.05–0.3 and clusters tightly; `ts_rank_cd` is unbounded and depends
on how often a word appears in a caption. They are not on comparable scales, no
rescaling makes them so, and tuning a weight between them is a job with no
natural end. Fusing the ranks throws the magnitudes away and keeps the only
thing both lists agree on.

A filtered vector search needs `hnsw.iterative_scan` — without it a search for a
person who is 9% of the library can walk forty neighbours, find none of them
match, and return nothing while the photographs sit right there. `db.Search`
sets it per transaction.

### The response echoes the parse

This is where the UX is won. The response carries back exactly what the server
understood — people, place, `after`, `before`, kind, category, and the visual
phrase that went to the encoder — so the page can draw `Phoenix ×` and
`Jun–Sep 2025 ×` as removable chips. A wrong parse is then visible and fixed
with one click rather than by retyping the sentence and hoping. **A search is an
editable filter, not an oracle.**

`parse=0` is the other half of that: with it, nothing is guessed. The filter
comes from explicit parameters (`person=`, `place=`/`city=`/`admin1=`/`country=`,
`after=`, `before=`, `kind=`, `category=`, `favorites=`, `tag=`) and the phrase
from `visual=`, so removing a chip is omitting a parameter. Removing something a
parser inferred is not expressible any other way.

`visual` is read by presence rather than by content, and that distinction is
load-bearing. Absent, it falls back to `q` — a caller that sent only a sentence
meant the whole sentence. *Present and empty* is the opposite claim and is
believed: "phoenix", all of which is a name, has no phrase for the encoder at
all, and ranking it by the word the filter has already answered exactly would
put every photograph with "phoenix" in a caption above the 1,601 of the person.
Taking the phrase chip off is exactly this, which is why it is spelled rather
than omitted.

### When photo-ml is down

`/v1/search` still answers, and says so in a `degraded` field. What is lost is
the vector ranking — the ability to find a beach nobody wrote the word "beach"
about. What is not lost is the search box, the grammar, the date and place
filters, or full-text search over every caption, tag, recognised line, filename
and place name the last backfill left in Postgres.

There is one more fallback inside the ranking itself. When the fuzzy half
produces no candidates at all but the structured half narrowed something, the
filter's own answer stands, in date order — because "Phoenix, last summer, and
no caption in the library mentions a beach" should not be an empty grid. When
the filter narrowed nothing, an unmatched phrase is genuinely no results, and
answering it with 17,788 photographs would be worse than saying so.

### Trying it

```sh
curl -s --cacert "$TLS_DIR/ca.crt" -H "Authorization: Bearer $PHOTOBACKUP_TOKEN" \
     --get --data-urlencode 'q=phoenix at the beach last summer' \
     https://localhost:8787/v1/search | python3 -m json.tool
```

The `query` object in the response is the parse. If a search surprises you, read
that first — nine times in ten the answer is there rather than in the model.

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

## Importing a Snapchat export

```sh
# The memories, which is the half worth having. Pass every unzipped directory:
# the history document is in exactly one of them and the media is in the others.
photobackup import-snapchat --half memories \
  --from ~/Downloads/snapchat_1 --from ~/Downloads/snapchat_2 \
  --from ~/Downloads/snapchat_3 --from ~/Downloads/snapchat_4 \
  --from ~/Downloads/snapchat_5 --from ~/Downloads/snapchat_6

# The chat media, separately, because it is a different population.
photobackup import-snapchat --half chat \
  --from ~/Downloads/snapchat_1 --from ~/Downloads/snapchat_2
```

A Snapchat export is not a Takeout wearing a hat. A Takeout writes a JSON
document per photograph and puts it beside the file. Snapchat writes
`json/memories_history.json` — one document for the whole account — whose rows
are a `Date`, a `Media Type` and a `Location`, and **nothing in it names a
file**. There is no identifier, no title, no path. The real export has 3,237
rows and 2,791 media files and not one declared link between them.

**The join is the modification time.** Snapchat sets each exported file's mtime
to the memory's capture instant, in UTC, to the second, and that is the only
thing relating a photograph to the record of when and where it was taken. Both
sides are truncated to the second — a zip's extended timestamp carries
nanoseconds the document never will — and against the real export it places all
2,791 files with none left over.

The consequence is worth stating plainly: **a copy that does not preserve mtimes
destroys the metadata of the entire export**, silently and unrecoverably. Import
from where the zips were unzipped, not from a copy made with something that
rewrites times.

This matters more than the equivalent does for Google, because Snapchat strips
the JPEGs completely. A still memory has no EXIF at all — no capture time, no
coordinates, nothing — so the history row is not a supplement to the file's own
metadata, it is the whole of it. The MP4s keep a QuickTime creation date.

Where several memories share a capture second, Snapchat's own `Image`/`Video`
split separates them; two stills in the same second cannot be separated at all,
and those matches are recorded as `"historyMatch": "ambiguous"` rather than
presented as facts. The run reports how many of those chose between rows that
actually *disagreed* about the location, which is the number that says what the
ambiguity costs — in the real export, 13 files out of 404 ambiguous ones.

### Overlays

A memory is two files and one photograph. `-main.jpg` is the frame the camera
captured; `-overlay.png` beside it is a transparent layer holding the caption,
the drawings, the stickers, the timestamp. **The image anybody actually saw is
in neither file** — Snapchat exports the layers, not the picture.

Both are archived. The overlay becomes an ordinary asset — same blob tree, same
manifest line, same `verify` — and `assets.overlay_asset_id` on the photograph
points at it, so the composite is rebuildable for as long as the archive exists.
The overlay is kept out of the gallery by `assets.is_overlay`, which is a term in
the timeline's visibility predicate exactly as the Live Photo pairing is; the
`archived` flag rides along to record that Snapchat never showed the layer alone.

They are related only by sharing a stem in a filename, in an export that is
deleted after the import, so the link is written down at import time. It travels
in the manifest as `import_overlay_sha256` — by content hash rather than asset
id, because a reindex generates new ids and the hash is the bytes.

**Every rendition is of the composite.** The thumbnails in the grid, the poster
on a video tile, the 2048px preview, and for video the playback rendition
itself: all of them are the two files composed, because that is the picture that
was sent. Anything that just asks for a preview — the phone app, a saved link —
gets it without knowing any of this exists.

The layer is stretched to the photograph's own frame rather than fitted into it.
Across this archive's 439 memories the two files never once agree on
dimensions — Snapchat's layer is the phone's screen and the media is what filled
it — but their aspect ratios agree to within about 2%, so stretching lands the
caption where the person put it and fitting would leave it drifting off the
edge. Stills are composed by ImageMagick at full resolution *before* the square
crop, so the tile and the viewer cut the caption in the same place. Video goes
through an ffmpeg `overlay` filter, with `eof_action=repeat` holding the single
still for the length of the clip and audio mapped by hand — `-filter_complex`
turns automatic stream selection off, and forgetting that produces a perfectly
valid silent video and no error at all.

**Do not loop the layer's input.** `-loop 1` is the obvious way to make one
still cover a whole clip and it is a trap: it makes the layer an input with no
end, and ffmpeg then bounds the encode only by whatever *other* finite output
stream it has. On a clip with an audio track it stops at the right moment and
every test passes. On a silent one it never stops — a 7.6-second memory here ran
for thirteen minutes, held sixteen cores, and wrote 300MB of a video that would
have filled the disk. `-shortest` does not save it. `eof_action=repeat` is the
filter doing the same job with a finite graph.

Two routes serve the photograph without the layer, which is what the viewer's
press-and-hold and its overlay toggle reach for:

| route | what |
|---|---|
| `GET /v1/assets/{id}/preview/plain` | the still, uncomposed; answers for an asset with no layer too |
| `GET /v1/assets/{id}/playback/plain` | `<sha>.plain.mp4`; 404 for a video with nothing to leave out |

`GET /v1/assets/{id}` reports `has_overlay`, which is the whole of what the
gallery needs to offer the toggle.

A library imported before this existed has thumbnails of half a picture, and
nothing on disk can tell them apart from finished ones — a thumbnail of the
photograph alone is a valid thumbnail of the right asset at the right size. So
migration 0010 requeues both jobs for every asset carrying a layer, once. The
missing `.plain.mp4` *is* detectable, and `verify` reports it as an ordinary
missing derivative, so `photobackup verify --fix` is the backfill for that half.

### Labelling

Everything lands under `import_source = 'snapchat'`, and the `subtypes` column
carries which kind of thing it is:

| subtype | what |
|---|---|
| `snapchat:memory` | a saved Memory, with a history row behind it |
| `snapchat:chat` | media from a conversation, which no document describes |
| `snapchat:overlay` | a drawn-on layer |
| `snapchat:thumbnail` | chat media shipped as a thumbnail of something else |
| `snapchat:discover` | publisher content, proven by a metadata document beside it |

### What is kept rather than imported

Three things reach `import_orphans` instead of becoming assets, because all
three are facts that die with the export:

- **History rows with no file.** Snapchat lists a memory, leaves the download
  link empty, and ships nothing — 446 of 3,237 in the real export. A UTC instant
  and a pair of coordinates is all that survives of each photograph.
- **Files the archive will not store.** Chat media contains voice notes
  (`audio/mp4`), which `media_kind` has no value for, and blobs Snapchat shipped
  still encrypted that nothing has the key for. Recorded with their MIME type,
  size and reason, so "the archive could not hold audio yet" is a decision that
  can be revisited against a list.
- **Overlays whose memory is missing.** They import on their own rather than
  being dropped; the handwriting is somebody's even without the photo under it.

### Sidecar shape

Snapchat's own row is stored verbatim under `history`, and every field beside it
is this importer's reading of the export — never merged into Snapchat's copy:

```json
{
  "export": "snapchat", "kind": "memory",
  "file": "2017-09-02_4c148b50-…-main.jpg", "mediaId": "4c148b50-…",
  "role": "main", "overlay": "2017-09-02_4c148b50-…-overlay.png",
  "capturedAt": "2017-09-02T06:55:44Z", "capturedAtSource": "history",
  "historyMatch": "exact",
  "history": { "Date": "2017-09-02 06:55:44 UTC", "Media Type": "Image",
               "Location": "Latitude, Longitude: 39.161533, -86.532104" }
}
```

`capturedAtSource` is not bookkeeping. For a stripped JPEG this timestamp is the
only one that exists anywhere, so how it was arrived at — `history`,
`file-modification-time`, `filename-date` — is a question somebody will need
answered.

The two halves upload as separate devices (`snapchat memories import`,
`snapchat chat media import`), so each re-runs and audits independently. Local
ids are `memories/<name>` and `chat_media/<name>` with the delivery directory
deliberately left out: Snapchat's zip numbering is an artifact of one download,
and a second download splits the same files differently.

## Merging

Two things the archive holds several times over, found by one scan and resolved
by one mechanism. See `internal/merge`, which does the finding and nothing else:
no SQL, no files, no ffmpeg.

**Duplicates.** Every asset is reduced to two 64-bit hashes taken from a 32x32
grey plane of the original — a difference hash (a gradient: survives
recompression, blind to brightness) and a perceptual hash (the low frequencies
of a DCT: describes structure). Both must agree within nine of sixty-four bits,
and the aspect ratios must be within 5%. A video is instead reduced to twenty
frames sampled at even fractions of its running time and compared position for
position against another clip of the same length, which is what survives a
change of frame rate. Matching pairs are unioned into groups, so a burst of a
hundred frames arrives as one group rather than five thousand pairs.

Nothing is merged without being asked. The review page offers the group best
first — most pixels, then most bytes, then oldest — and whichever copy is chosen
inherits the others' albums, people, caption and favourite before they go to the
trash under one batch.

**Split recordings.** Snapchat caps a memory at ten seconds and exports a longer
one as several files with nothing marking them as related. What gives them away
is `memories_history.json`: consecutive pieces are written exactly ten seconds
apart, to the second. The scan chains on `captured_at` (the history date) rather
than `sort_time`, because the QuickTime creation date on these files is when the
piece was written out and drifts by up to eighty seconds across one recording.

These are joined without being asked, by the `merge` job. The join prefers a
stream copy — two pieces from the same encoder agree about everything a container
cares about, so the output holds the camera's own frames and comes out at exactly
the sum of the inputs' durations. It falls back to re-encoding when a resolution
changed mid-recording, a piece has no audio, or a caption layer has to be burned
in, and records which in the sidecar.

**A joined recording is an original.** It is committed blob first, then two
manifest lines (an asset line and a metadata line carrying the sidecar), then the
database row — the upload path's ordering, for the upload path's reasons. Its
pieces go to the trash rather than away, so for a year both exist and `verify`
covers all of them. Nothing is trashed until the join is committed and indexed: a
crash halfway leaves a duplicate-looking minute of video beside its pieces, which
somebody can see and undo, where the other order would leave the pieces deleted
and nothing joined.

**A join that does not add up.** The joined file is probed and its running time
compared against the sum of its parts, within a tenth of a second. The failure
this catches is ffmpeg silently dropping a piece — ten whole seconds, which
could not hide under that tolerance — but it also catches a container that
overstates its own length by a fraction, and arithmetic cannot tell the two
apart. So the file the job refused is kept rather than deleted, filed under the
group's fingerprint as `<fingerprint>.join.mp4` in the derivative tree: the
review page lists the group with its error and a button that plays that file,
and `POST /v1/merges/{id}/force` queues the join again with the check disabled.
The override lives on the group, so every retry makes the same choice, and the
sidecar of anything archived that way records what the parts were expected to
add up to.

It is the one file under the derivatives root named after something other than
an asset, so no per-asset cleanup can reach it: it is removed when the group is
answered either way, and a sweep on the purge timer reconciles the whole tree
against the groups still entitled to one.

**Approving.** The joined recordings list is a log rather than a queue — every
row on it has already happened — so it only ever grows, and the status card
counting it goes on asking for attention that was paid weeks ago. Approving a
row says it has been read: it comes off the list, stops being counted, and
changes nothing else. The pieces stay in the trash, the recording stays in the
library, splitting it back up goes on working, and the review page's "show
approved" puts them all back on screen.

**Undo.** Both kinds hand back a delete batch, and `POST /v1/merges/{id}/undo`
restores it — plus, for a join, sends the joined recording to the trash so the
library does not end up holding the minute *and* the six pieces. An undone group
lands in `dismissed` rather than `pending`, because a pending set of segments
would be re-joined by the worker within the minute. Every pair inside a dismissed
group is one the scan will never link again.

**The scan is not on a timer.** The two things that create work here — an import
and a signature backfill — both end, so it runs at startup and from the button on
the review page. A sweep over an untouched library writes nothing and costs a few
seconds of comparing every signature against every other, which is the right
algorithm at twenty thousand assets and the wrong one at a million.

**The vault is excluded** from signatures entirely. A signature describes what a
photograph looks like, and the vault exists to stop this server knowing that.

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

## Deleting

Two steps, and only the second one is real.

**The trash.** `assets.deleted_at` is set, and nothing else happens: the row, the
blob, the derivatives, the album membership and the face tags all stay exactly as
they were. That one column is a term in the timeline's visibility predicate — the
same string pasted into the pages, the day table, the album covers and the
category counts — so a deleted photograph leaves the whole gallery at once, and a
restore is one `UPDATE`. Recently Deleted is the same predicate with the term
flipped, which is why it is a scope on `TimelineFilter` rather than a table.

A still takes its Live Photo motion and its Snapchat overlay with it. Those are
components rather than items: invisible in every timeline, addressable by
nothing, and half a photograph on their own. `db.family` is the one place that
relationship is expanded, and every operation here goes through it.

**The purge**, 365 days later or when somebody asks:

1. The rows are deleted and the content key is written to `purged_content`, in
   one statement. A row that is gone without a tombstone is a photograph the
   next backup uploads again, and the two must not be able to come apart.
2. A `purge` line goes in the manifest, so a rebuild knows this was a decision
   rather than a loss. `reindex` takes the retraction back out and restores the
   tombstone — the last line about a digest wins, so content purged and later
   archived again survives a rebuild.
3. The blob and every derivative are unlinked. Last, because it is the only step
   that rerunning the ones before it cannot repair: a file left behind is a
   finding `verify` reports, where a row deleted before its file would be an
   asset the archive can no longer produce.

`sync/check` answers `have` for tombstoned content, with no asset id. It is not
literally true — the archive threw those bytes away — and it is what makes a
permanent delete permanent: the phone still holds the photograph, and without it
the delete would survive until the app was next opened.

The sweep runs inside photod, hourly (`PURGE_INTERVAL`), a thousand rows at a
time. It is not a systemd timer beside `verify` because a retention that only
elapses on hosts where somebody installed a second unit is not a retention.
`PURGE_DISABLED=1` turns it off; the trash then waits indefinitely and nothing
removes an original unless it is asked to by hand.

## photobackup

The maintenance CLI. Reads the same environment photod does.

```sh
photobackup verify [--deep] [--fix]     audit the archive against itself
photobackup export --to DIR [--copy]    materialize a date tree of hardlinks
photobackup reindex [--adopt-orphans]   rebuild the database from manifest.jsonl
photobackup geocode [--all]             name the places photographs were taken
photobackup ml status                   how far the captioning passes have got
photobackup ml backfill [--kind K]      queue them; --stills N --videos N bounds
  [--stills N] [--videos N] [--force]   a sample, newest first; --force redoes
                                        what already has words
photobackup ml reindex                  rebuild the full-text index
photobackup import --from DIR [--from…] ingest a Google Photos export
photobackup import-snapchat --from DIR  ingest a Snapchat export, one half at a
  [--half memories|chat] [--from…]      time; --from once per unzipped zip
photobackup pair [--ttl 10m]            mint a single-use code to pair a device
photobackup devices [--revoke ID]       list paired devices, or unpair one
photobackup ca [--serve]                the CA to install on a device, and how
photobackup migrate [--check]           apply pending schema migrations
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

**ml** is the one backfill in this tool that is a command rather than a
reconcile. Every other pass here is queued by a restart; captioning is hours of
GPU, and a `systemctl restart photod` that quietly begins hours of GPU work is a
restart with a surprise in it. Because it is typed, it can also be *bounded* —
`--stills 1000 --videos 20` takes the newest of each, which is an evening's
worth of vocabulary to build a search page against rather than a choice between
nothing and everything. `ml reindex` is for after a tag merge or a re-geocode:
both change one column somewhere else and neither knows what a tsvector is. See
[Searching](#searching).

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

The image is `pgvector/pgvector:pg18` rather than the stock one, because
migration 0016 opens with `create extension vector` and `postgres:18-alpine`
does not ship it. Every per-package database runs the same migrations, so the
extension is created in each of them and a Postgres without it fails the whole
suite at the first `Migrate`.

On a machine that is also *running* photod, the compose Postgres cannot bind
5432 — the deployed one already has it. Point the tests at a second server
instead of stopping the archive:

```sh
docker run -d --name photobackup-test-pg -p 5433:5432 \
  -e POSTGRES_USER=photobackup -e POSTGRES_PASSWORD=photobackup \
  -e POSTGRES_DB=photobackup pgvector/pgvector:pg18
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
