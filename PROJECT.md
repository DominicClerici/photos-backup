# photos-backup

A self-hosted photo and video backup service. An iPhone app pushes originals to a
Linux server at home, which stores them on a dedicated partition of a 6TB drive
and serves them back through a web gallery.

Built from scratch deliberately. Immich already solves this problem well; the
point here is to own the design and the code.

---

## 1. Goals

**v1 succeeds when:**

- Photos and videos taken on the iPhone reach the archive drive without manual effort.
- A second backup run uploads zero bytes.
- The entire archive is browsable in a web browser.
- `photobackup verify` passes across the whole library.

**Non-goals for v1:** multi-user accounts, sharing, editing, mobile gallery
browsing, ML search, USB ingest, mirroring phone deletions.

---

## 2. Decisions

| Area | Decision |
|---|---|
| Sync model | One-way archive. Sync never deletes; the gallery can. |
| iOS app | Expo / React Native, custom dev client |
| Core server | Go |
| Web app | Next.js (App Router), separate process |
| ML service | Python, **v2**, always optional |
| Database | Postgres (pgvector-ready for v2) |
| Storage | Content-addressed blobs, SHA-256 |
| Viewing | Web gallery first; in-app gallery later |
| Transport | LAN via mDNS, Tailscale when away |
| Redundancy | Single drive for now, designed for rsync |
| USB ingest | Deferred to v2 |
| Archive / Hidden | Per-file AES-256-GCM under an X25519 vault key; password wraps the private half |

### Why one-way

The phone produces photos; the server archives them. Deleting on the phone does
**not** delete on the server. This removes every destructive edge case, and it is
what a backup should actually do. It also makes freeing up phone space safe later,
because the archive is authoritative.

What that rule was always protecting is the *sync path*, and it still holds there:
nothing the phone does, and no sequence of runs, retries or reconnections, can
remove an original. What changed in Phase 10 is that a person looking at the
gallery can — deliberately, by hand, with a year to change their mind. See
[Deletion](#phase-10--deletion). The property worth keeping was never "nothing is
ever removed"; it was "nothing is removed by accident".

Phase 11 bends it once more, and less far. An archived or hidden photograph is
not removed at all: it is on the same drive, verified by the same `verify`, and
one password away from being back in the timeline. What is gone is the ability
of anything except that password to read it — including this server, most of the
time. See [Archive and Hidden](#phase-11--archive-and-hidden).

### Why Expo, given the constraints

Native Swift has better access to every hard part of this project. Expo was chosen
anyway for iteration speed, and it works because the intended flow — open the app,
it backs up — is foreground-dominant, which is exactly where Expo is strongest.
The known cost is Live Photo handling (see Risks).

### Why content-addressed storage

Beyond speed: writes become **idempotent**. Write to temp, fsync, rename to the
hash path. A retried or half-finished upload cannot corrupt or duplicate anything.
Hash-prefix fanout also keeps directories small rather than dumping thousands of
files into a single month folder.

The tradeoff is that the blob tree is meaningless without the database. Mitigated
by an append-only `manifest.jsonl` beside the blobs, so a total DB loss is
recoverable by replay. `photobackup export` materializes a human-readable date tree
of hardlinks on demand, costing no extra bytes.

---

## 3. Environment

- **Server:** Fedora Workstation, 6TB external drive, NVIDIA GPU (for v2 ML).
- **Archive partition:** the 6TB drive is split, and only part of it is the
  archive. `/dev/sda1` is 500GB of ext4 mounted at `/mnt/photos`; `/dev/sda2`
  holds the remaining 5TB as NTFS, mounted into the desktop session and untouched
  by photod. Against a ~100GB library that is roughly 5x headroom, so v1 does not
  need the rest — but "the 6TB drive" is shorthand for a 500GB slice of it
  everywhere in these documents, and growing the archive later means resizing
  across a partition boundary rather than just using free space.
- **Phone:** iPhone. **iCloud Photos is not in use** — every original is physically
  on the device, so backup is a pure local-disk read with no cloud round-trip.
- **Library:** ~100GB in real use. ~100 photos + ~10 videos as the test fixture.

---

## 4. Architecture

```
     iPhone  (Expo / React Native)        browser
       - PhotoKit enumeration               |
       - local SQLite upload queue          v
       - foreground bulk upload    +------------------------+
       - background top-up         |  web  (Next.js)        |
              |                    |    virtualized timeline|
              |                    |    full-size viewer    |
              |                    |    /api/* -> photod    |
              |                    +-----------+------------+
              |                                |
              +---- HTTPS over LAN (mDNS) or Tailscale ----+
                                  |
                                  v
        +---------------------------------------------------+
        |  photod  (Go)                          always-on   |
        |    device pairing + token auth                     |
        |    POST /v1/sync/check     dedup pre-check         |
        |    POST /v1/assets         single-shot upload      |
        |    PUT  /v1/assets/:id/chunk   resumable upload    |
        |    GET  /v1/assets/...     originals + derivatives |
        |    GET  /v1/timeline       paged gallery JSON      |
        |    POST /v1/trash          delete, restore, purge  |
        |    enqueues jobs + hourly trash sweep              |
        +----------+---------------------------+------------+
                   |                           |
                   v                           v
        +---------------------+     +-------------------------+
        | photo-worker  (Go)  |     | photo-ml  (Python)  v2  |
        |   EXIF extraction   |     |   CLIP embeddings       |
        |   thumbnails        |     |   face detect / cluster |
        |   video posters     |     |   ONNX + CUDA           |
        |   -> ffmpeg,libheif |     |   stateless HTTP        |
        +----------+----------+     +------------+------------+
                   |                             |
                   v                             v
              Postgres  (metadata, jobs, pgvector in v2)
                   |
                   v
              /mnt/photos  (500GB ext4 partition, 6TB drive)
```

Three server processes under systemd, plus the web app. The split is by process
boundary, not sprinkled through one codebase.

The gallery is a separate Next.js app rather than pages served by photod. It
costs a second process on the archive machine, and buys the ability to put the
gallery on the public internet later without moving the upload endpoints or the
archive drive along with it. photod stays a private API; the web app is the only
part that would ever face outward.

The browser talks to one origin. Next rewrites `/api/*` to photod, so the JSON
is same-origin and CORS never enters the picture. Thumbnails and video are the
exception: `<img>` and `<video>` are not subject to CORS, so they can point
straight at photod and skip the proxy hop entirely.

**Hard rule:** `photo-ml` is optional forever. If it is down, mid-model-swap, or
saturating the GPU, backups still complete and photos still display. Search
degrades to date, filename, camera, and EXIF. The interesting part must never be
able to break the boring part that protects the photos.

The worker's heavy lifting is not a language decision — HEIC decode and video
thumbnails are `libheif` and `ffmpeg` subprocess calls. Go only orchestrates.

---

## 5. Storage layout

```
/mnt/photos/
  blobs/
    ab/cd/abcd1234....HEIC          originals, sha256-addressed, immutable
    3f/9a/3f9a77b2....MOV
  vault/
    5e/71/5e71c0de....enc           an original in Archive or Hidden, encrypted
  manifest.jsonl                    append-only recovery log

$DERIVATIVES_ROOT/                  the SSD; defaults to /mnt/photos/derivatives
  ab/cd/abcd1234.thumb.webp         256px square, stored
  ab/cd/abcd1234.thumb96.webp       96px, for the two smallest zoom levels
  ab/cd/abcd1234.thumb512.webp      512px, for the two largest
  3f/9a/3f9a77b2.thumb.webp         poster frame for video, same shape and sizes
  3f/9a/3f9a77b2.mp4                H.264 playback rendition, video only
  7c/21/7c21ba05.live.mp4           256px square, a Live Photo's motion
  7c/21/7c21ba05.live96.mp4         the same three seconds at the other sizes
  7c/21/7c21ba05.live512.mp4

$DERIVATIVES_ROOT/vault/
  5e/71/5e71c0de.thumb.webp.enc     every rendition of a hidden item, encrypted
  5e/71/5e71c0de.mp4.enc            — a thumbnail is the photograph
```

Three thumbnail sizes because the grid zooms across a range no single size
covers: a 256px file is fifteen times the pixels a 64px cell will use, times a
screenful of tiles, and the same file is visibly soft stretched into a 512px
one. The gallery picks the smallest size that fills the cell — 96, 256, 512 —
and 256 keeps its unadorned name so introducing the other two re-rendered
nothing that already existed.

All three come out of one subprocess per asset, because decoding is what a
rendition costs: a 12-megapixel HEIC takes a few hundred milliseconds to get
into memory and a couple of milliseconds to squeeze into a 96px square. The same
holds for the Live Photo clips, which are one ffmpeg with a `split` filter
rather than three.

A library ingested before a size existed has only the sizes it was ingested
with. `photobackup verify --fix` is the backfill: a missing size reads as a
missing derivative, and the repair for that has always been to requeue the job
that renders it. Until that runs the gallery draws the base rendition at those
zoom levels, which is a size mismatch and not a hole.

Originals live on the archive partition — 500GB of the 6TB drive, mounted at
`/mnt/photos`. Postgres and derivatives should live on the workstation SSD for
speed, which is what `DERIVATIVES_ROOT` is for.

The 2048px preview is **not** stored. It is rendered per request from the blob
and cached by the browser instead, since the bytes are content-addressed and can
never go stale. One viewer shows one preview at a time; a stored file would buy
nothing that `Cache-Control: immutable` does not.

### Live Photos

A Live Photo is two files, and there are two independent ways to know they
belong together. The archive uses both, and writes both to the same two columns,
so nothing downstream has to know which one did the work.

**The declaration.** The app names the still on the video's upload. It is known
before a byte is sent, so the pairing is complete the moment the second half
commits — and it is unavailable to anything that is not the phone.

**The content identifier.** Apple stamps a UUID into both halves at capture: the
maker note on a HEIC or JPEG, `com.apple.quicktime.content.identifier` on a MOV
or MP4. Google's export preserves both, so this is what pairs a Takeout, a
restored backup, and anything copied off a Mac — everything the declaration
cannot reach. It is not known until something has read the file, which for an
upload is after the bytes are already committed, so the metadata worker records
it (`assets.content_id`) and resolves from whichever half it reaches second.

The first of these was Phase 7's whole design, on the reasoning that the two
files "share nothing but a capture time". That was wrong, and expensively so: it
made the feature reachable only by files the phone itself uploaded, which is the
minority of this archive. What they share is a UUID, in the bytes, put there by
the camera. A header is a convenience; the maker note is the fact, and the file
overrules the header whenever they disagree.

The declaration is separate from its resolution, because they become true at
different moments. `live_parent_local_id` is what the phone said and is known
the instant the bytes land. `live_parent_asset_id` is the still it resolved to,
and cannot be filled in until that still exists — the two halves share a capture
time, the upload queue orders by capture time, and nothing decides which goes up
first, so it resolves from whichever side arrives second.

**The paired video is never an item of its own.** It is archived, verified, and
downloadable like everything else; it simply is not a thing anyone took a
picture of, so the timeline shows the still and carries the motion as one extra
field on it.

**A declared video is hidden immediately; a discovered one is hidden only once
it resolves.** The phone only ever declares a pairing for a video whose still is
already queued behind it, so there is nothing to wait for. An export has no such
guarantee: it can hold a paired video whose still was deleted years ago, and 44
of the 130 files in the sample export are exactly that. Hiding those on the
strength of an identifier would archive them into invisibility — bytes on the
drive that nothing could ever show. They import as ordinary videos and pair
themselves the day their still turns up, at which point the still's own metadata
job adopts them, requeues their derivatives, and they leave the timeline.

What it gets built is deliberately lopsided against what an ordinary video gets:

| | ordinary video | Live Photo's video |
|---|---|---|
| poster `.thumb*.webp` | yes | **no** — the tile belongs to the still |
| stored H.264 `.mp4` | yes, via the transcode queue | **no** |
| `.live*.mp4`, one per thumbnail size | no | yes, in the metadata job |
| 1080p with audio | n/a | rendered per request, stored nowhere |

Roughly a third of an iPhone library is a Live Photo. Putting each of those
three seconds through the transcode queue would swamp the videos that genuinely
need it, for a rendition only ever played while a mouse button is held down —
so the viewer's copy is rendered on demand, the same trade §5 already makes for
the 2048px photo preview. The thumbnail-sized ones *are* stored, because the
grid asks for one on hover and an ffmpeg per hover is not a thing that can be
allowed.

The one place this differs from the photo preview: those renditions are kept in
memory briefly after rendering. A `<video>` asks for its bytes more than once —
Safari opens with a range probe before requesting the file properly — and
without it the same three seconds would go through ffmpeg two or three times for
a single press-and-hold.

`manifest.jsonl` records one line per stored blob: hash, original filename,
capture time, source device, byte size, and the content identifier that pairs a
Live Photo. It is the disaster-recovery path when the database is gone.

It carries a second kind of line too. An import learns things about a blob after
the bytes have landed — the sidecar beside it in the export — and those are
appended as their own `{"type":"metadata"}` lines rather than folded into the
asset line, which is append-only and already written by then. The sidecar is
stored raw, so a rebuild re-reads it with the current parser instead of
replaying an older parser's conclusions: understanding more about an export
later is then a `reindex`, not a re-import of files that have since been
deleted.

---

## 6. Upload flow

The core principle: **the phone never uploads what the server already has**, and it
determines that cheaply.

1. **Enumerate.** The app walks PhotoKit and writes every asset into a local
   SQLite queue (`localId`, size, created, state). This queue is the source of
   truth for progress and survives app termination — essential for a 100GB
   backfill.

2. **Check.** Per batch, `POST /v1/sync/check` with
   `{localId, size, createdAt, md5}`. The MD5 comes from `expo-file-system`'s
   **native** digest, so 100GB is never hashed in JavaScript.

3. **Server answers** `have` or `want` per item.

4. **Upload only the `want` set.**
   - Under ~64MB: single-shot `POST`.
   - Larger (videos): chunked resumable `PUT` with a byte offset, so a 3GB clip
     surviving a dropped connection resumes instead of restarting.

5. **Server commits.** Streams to a temp file, computes SHA-256 as bytes arrive,
   verifies against the declared MD5, `fsync`s, then renames into the blob path.
   Only then does it ack. Only after the ack does the phone mark the item done.
   A crash at any point re-uploads at worst one file; it never loses one.

6. **Derivatives are async.** The server enqueues a job and returns. Upload
   throughput is never blocked behind ffmpeg.

### Pairing and transport

The server prints a pairing code. The app submits it once and receives a
long-lived device token, stored hashed server-side and kept in the iOS keychain
on the phone. It is the phone's only credential: uploading and reading the
gallery both use it, so revoking a device withdraws both at once. The server
issues its own CA and server certificate on first run.

**Trust is established out of band, not pinned at pairing.** The original plan was
trust on first use — pin the self-signed certificate when the code is redeemed —
and it cannot be built: iOS gives JavaScript no way to override TLS trust
evaluation. `fetch` and `expo-file-system`'s `File.upload()` both run on
NSURLSession, neither exposes a trust delegate to JS, and `NSAllowsLocalNetworking`
permits plaintext HTTP without touching certificate validation. Pinning would
mean reimplementing the upload path in Swift, including the streaming-from-PhotoKit
call Phase 0 proved. So the CA is installed on the phone once instead, and every
existing code path works unchanged. Phase 5 has the details.

The app resolves the server via mDNS (`_photobackup._tcp`) on the LAN and falls
back to the Tailscale address when away. No ports are exposed to the internet.

### Failure behavior

| Condition | Result |
|---|---|
| Server unreachable | Queue holds, retries with backoff, nothing lost |
| Device token revoked | Run stops at once, nothing marked failed, app asks to pair |
| Database down | Write path refuses; queue holds, bytes arrive when it returns |
| ML service down | Search degrades to EXIF/date; backup unaffected |
| Hash mismatch | Temp discarded, item retried |
| Upload interrupted | Resumes from last acked offset |
| Bit rot on disk | Caught by `photobackup verify` |
| Database lost | Rebuilt by replaying `manifest.jsonl` |
| Deleted by mistake | In Recently Deleted for 365 days; one Undo, or restore later |
| Purged, but still on the phone | Tombstoned by content key, so sync answers "have" |

---

## 7. Phases

### Phase 0 — Spike (~1-2 days, throwaway)

Four Expo assumptions to break early rather than in week three. Each must be
answered by running on the real iPhone, not by reading documentation.

- Can `expo-file-system` produce a **native MD5** of a large file?
- Do **background upload sessions** survive app suspension?
- Can the **paired `.mov` of a Live Photo** be retrieved via `expo-media-library`?
- Does **mDNS discovery** work from a dev client?

### Phase 1 — Walking skeleton

One manually-picked photo travels phone → Go server → blob on disk → Postgres row
→ visible on a bare web page. Ugly but end-to-end. Everything after this widens a
pipe that already works.

### Phase 2 — Sync engine

Local SQLite queue, `sync/check` batching, resume across app kills, backoff.
**Exit criteria:** the 110 test items back up cleanly, and a second run uploads
zero bytes.

### Phase 3 — Derivatives and gallery

Worker pool, ffmpeg/libheif thumbnails, video posters, virtualized web timeline,
full-size viewer.

Two job kinds in two separately sized pools: `metadata` (exiftool, the stored
thumbnails, a video's poster frame) and `playback` (H.264/AAC MP4). They are
split because a handful of 4K transcodes would otherwise take every worker slot
and starve the thumbnails behind them, which during a backfill looks exactly
like a gallery that has stopped working.

Not built at the time, deliberately: a date scrubber. The timeline paged on a
keyset cursor, so jumping to an arbitrary date meant either loading everything in
between or a second index, and neither was worth it before the archive was real.

The archive is real now, and the second index turned out to be worth building for
a different reason. `/v1/timeline/days` returns every heading a collection will
draw and how many tiles hang under it — one ordered pass over the filtered rows,
79KB for 18,101 items and 9KB once gzipped — which lets the gallery lay the whole
grid out, at full height, before it fetches a photograph. Scrolling stops hitting
a wall, the scrollbar stops shrinking as pages land, and positions in that table
double as row offsets the timeline can be asked to start at, so a fling into the
middle of the library is one request rather than a walk. `/v1/timeline/locate`
runs the mapping the other way, turning the id in a shared link into a position
with a single count — the last place in the gallery that walked. A date scrubber
is now a small thing on top of all this rather than the reason for it.

It is also the address space a mass selection will be expressed in: "everything
in August 2021" is a range of indices the client already knows the bounds of,
whether or not it holds a single one of those photos.

### Phase 4 — Real-load hardening

Chunked resumable video upload, `photobackup verify`, `photobackup export`,
`photobackup reindex`, systemd units, then the real 100GB run. Expect this phase
to surface problems that 110 items never could.

Rehearsed against a 9.6GB / 3,116-file Google Takeout export before the real
run, driven by `cmd/loadgen` — a Go client that speaks the phone's exact
protocol, so the archive can be run at size without a phone in the loop. It is a
test client, not a product surface; if it and the app ever disagree, it is wrong.

Four things that 110 items could not have shown, all of them found by the
rehearsal rather than by reading the code:

**216 files had no extension.** A Takeout export strips it off every Live
Photo's paired video. Classified by filename alone they became octet-stream
images: thumbnail failed, no playback rendition queued, error tile in the
gallery — 7% of the library. The fix is sniffing the leading bytes when the name
says nothing, which also had to handle the `ftyp`-less QuickTime iOS writes,
where even `file(1)` gives up and says "data".

**Every run re-hashed the entire library.** The upload path stored `modified_at`
at one precision and `sync/check` compared it at another, so the device mapping
could never match and the second run fell all the way through to a content
check. It self-healed after two runs, which is exactly why 110 items never
showed it. At 100GB it is the difference between a 300ms no-op and reading every
original on the phone, every time. Timestamps are now normalized on the way in.

**Videos are the long pole.** 962 of the 3,116 files need a transcode, and at
`TRANSCODE_CONCURRENCY=1` that queue outlives the uploads by a wide margin. The
split pools did their job — thumbnails were never starved, and the gallery was
usable long before the transcodes finished — but the number wants raising on a
machine that is otherwise idle. It drained to zero with nothing failed.

**The lease survives a hard kill.** photod and its ffmpeg were killed outright
partway through the transcode queue. On restart the abandoned job sat in
`running` until its lease expired, was reclaimed, ran again, and finished: one
job in the whole queue ended with `attempts = 2` and none ended failed. That the
reclaim path was exercised by an actual crash rather than by a test with a zero
lease is the only reason it counts for much.

Deferred deliberately: pairing, device tokens, and TLS. §6 describes them and no
phase owned them; they are their own phase now, after the pipe is proven.

### Phase 5 — Pairing, device tokens, TLS

The write path is closed. Everything that can change the archive requires a
device token over TLS; the gallery's read path is deliberately left open.

**The design changed on contact with iOS.** §6 planned trust on first use, and it
is not buildable in JavaScript — see the note there. What replaced it is a mini-CA
the server runs for itself: a ten-year CA installed on the phone once, signing a
disposable leaf. Nothing native, no domain, no third party, and every existing
request path works untouched.

The split between the two certificates is what makes it maintainable. The leaf is
reissued whenever the machine's set of addresses changes or its expiry comes into
view, and swapped in through `GetCertificate` without a restart. That exists for a
specific failure: a DHCP lease handing the machine a new address, or a Tailscale
interface that comes up after photod started, would otherwise leave the phone
dialling an address the certificate does not cover — and the away-from-home half
of the setup is the half nobody tests. The CA never moves, so the phone is never
touched again.

Three things the phase settled that were not obvious going in:

**A device id the client picks is a label, not an identity.** The app used to
generate `ios-a3f9x2` and send it in a header. Every device id that reaches the
database now comes from the token, and a header that disagrees is a 403 rather
than a silent correction. Upload sessions inherit the same scoping: a session id
is `sha256(deviceId, localId, md5, size)`, so a second paired device that knew
what the first was uploading could otherwise resume, commit, or abort it.

**A revoked token needed its own failure kind.** The sync engine blames a failure
on the server, on the transport, or on the item. A 401 is none of those — the
server is answering perfectly and the item is fine — and charged to items it would
walk the entire library into `failed`, five attempts each, over something one
pairing fixes. It ends the run instead, marks nothing, and the app drops the dead
credential so the pairing form is what appears rather than a Start button that
cannot work.

**Closing the write path cost a Phase 4 property.** Authenticating reads Postgres,
so a database outage now refuses uploads outright, where before the blob landed
and only the indexing failed. Nothing is lost — the phone never gets an ack and
retries — and the alternative was caching tokens in memory, which buys an early
blob write in exchange for a revoked device that keeps working for as long as the
cache holds. Refusing is the cheaper answer, and it is written down because it is
a deliberate regression rather than an oversight.

Deferred deliberately, and the reason it is worth naming: **the gallery has no
authentication.** `/v1/timeline`, `/v1/assets/*` and `/v1/jobs` are open to
anyone who can reach them. The obstacle is not the login form, it is that
PROJECT.md points `<img>` and `<video>` straight at photod to skip the proxy hop,
and a cookie is not sent cross-origin — so closing it means either giving up that
optimization or minting HMAC-signed media URLs into the timeline JSON. It is a
real gap, in the same category as the single drive: accepted, known, and written
down rather than discovered.

*Closed on the phone's listener since Phase 6, below. The obstacle above is a
browser's, and the phone is not one.*

### Phase 6 — Authenticated reads, and a way into the archive from the app

The read path on the TLS listener is behind `requireDevice`, exactly as the write
path is; the plaintext loopback listener still serves it open, which is what the
browser gallery reads through. `/health` is the one exception on both, because
the app pings a remembered address before it holds a token and after one has been
revoked.

**The Phase 5 obstacle turned out to be a browser's, not the archive's.** Closing
the read path was written up as a choice between losing the direct-to-photod media
hop and minting signed URLs. Neither was needed here: React Native puts headers on
image, video and download requests — `expo-image` and `expo-video` both take a
`headers` field, and so does `File.downloadFileAsync` — so every rendition
authenticates with the token already in the keychain. There is no second
credential, no expiry to manage, and no URL that is itself a secret. The web
gallery's version of this problem is untouched and still real; it simply was never
the same problem.

**Reads are guarded by the routing table, not by a check in a handler.**
`readRoutes` takes the guard as an argument and each listener supplies its own,
which keeps the property Phase 5 established: which listener a request arrived on
settles what it may do, and that stays true when a route is added.

**Revocation now covers reading too**, which is what makes it one lever rather
than two. The cost is the one Phase 5 already named — authenticating means
touching Postgres — and a scrolling gallery asks far more often than an upload
does. Left unmeasured on purpose: it is an indexed lookup over loopback, and a
read-path token cache is the answer if it ever shows up in a profile.

Shipped in the app as a proof of connection rather than a gallery: `GalleryClient`
and an access check that reads the timeline, fetches a thumbnail, and confirms the
same read is refused without the token. The dashboard grows in the browser first
and gets ported onto this.

### Phase 7 — Live Photos

The gallery was showing every Live Photo twice: once as the photograph, once as
a silent three-second clip filed beside it. §5 has the design that fixed it and
the reasoning behind each half; what the phase settled that was not obvious
going in:

**The pairing had to be declared, and the app already knew it.** Since Phase 2
the sync engine has enumerated a Live Photo as two queue items and given the
paired video a local id of `<still>#live`. The server had no idea, because
nothing ever told it — the id is opaque to it by design, and reading meaning
into that suffix would have made a client-side naming convention into a wire
format. One header closed the gap and nothing else moved.

**Neither half can be assumed to arrive first.** Both carry the same capture
time, the queue orders by capture time, and an interrupted run can deliver them
minutes apart. The link resolves from whichever side lands second, which is two
statements in the same transaction as the insert and removes the entire class of
bug where a backfill's ordering decides whether the gallery looks right.

**Hiding a paired video is not the same risk as hiding a failed derivative.**
Phase 3 established that an asset is never invisible: a tile is drawn for
pending, ready and failed alike, because a photo that is archived but unreachable
is worse than an ugly one. That rule is about failures. A paired video is hidden
while everything about it is working perfectly, and it is hidden because it is
not a photograph — the archive still holds it, verifies it, and serves the
original. The one case it costs something is a still that never uploads, whose
video is then hidden with nothing to hang it on. Deliberate: three seconds of
soundless video is not what anyone lost.

*(The first of those three is half wrong, and Phase 8 corrects it. The pairing
did not have to be declared; the declaration was simply the only evidence Phase 7
looked for.)*

### Phase 8 — Google Photos import

The majority of this library is not on the phone. It is a Google Photos export,
and Phase 7's pairing could not touch it: a Takeout declares nothing, so every
Live Photo in it landed as a photograph and a silent clip filed beside it —
exactly the bug Phase 7 was supposed to have fixed. §5 has the design. What the
phase settled:

**The evidence was in the files all along.** Apple's content identifier is in
both halves, Google preserves it, and one `exiftool` tag reads it from the maker
note on a still and the QuickTime keys atom on a video. In the 130-file sample
export it pairs 20 Live Photos that nothing else could have paired. The lesson
is narrower than "read the file": Phase 7 reasoned from what the *phone* knew and
never asked what the *bytes* knew, and the answer had been sitting in the sample
data the whole time.

**Hiding on discovery is not the same as hiding on declaration.** A third of the
sample export's videos carry an identifier whose still is not in the export.
Under Phase 7's rule they would all have vanished. The timeline predicate is now
"declared *or* resolved", which costs a duplicate-looking tile for as long as it
takes a still to arrive and never costs an asset.

**An importer is a client, not a second server.** `photobackup import` walks the
export and uploads over HTTPS to photod like the phone does, even though it runs
on the archive machine and could write the blob tree directly. That buys one
commit ordering instead of two, one manifest writer instead of a race, and free
resumability from the sync protocol that already exists. It costs a loopback
copy of 100GB, once.

**Metadata Google kept and the file did not.** A screenshot has no EXIF at all;
its sidecar's `photoTakenTime` is the only capture time it has. Album membership
exists nowhere but the directory layout, and people tags nowhere but the JSON.
All of it is normalized into columns *and* stored verbatim, because the export
gets deleted and no amount of care today anticipates which field matters in a
year. Sidecar filenames are their own small horror — truncated to a length cap,
collision counters migrated to the end, absent entirely for a Live Photo's video
half — and `internal/takeout` exists to keep that in one testable place.

**Sidecar coordinates needed a column of their own.** The metadata worker
rewrites `gps_lat`/`gps_lon` from the file on every run, so a sidecar's
coordinates merged into them would survive exactly until the next reindex. They
live in `import_gps_lat`/`import_gps_lon` and feed the canonical pair only where
the file itself carried nothing — which makes the precedence hold in both
directions and under any order of arrival.

### Phase 9 — Capturing all of it

An audit of both ingest paths against the real 15,689-file export, before the
export gets deleted and the question becomes unanswerable. Two of the findings
were losses rather than omissions:

**239 files were never uploaded.** exiftool recursing a directory reads only the
extensions it knows and skips a file that has none — which is what a Takeout
leaves of every Live Photo's paired video. The importer asks exiftool what is in
the tree, so those files did not exist as far as it was concerned: no error, no
unmatched sidecar, nothing to notice. Phase 4 had already been bitten by
extensionless videos and fixed classification for them; the new scan gate put the
loss back one layer up, where it was silent instead of visible. The fixture
export had none, which is the second half of why it survived.

**Importing zip-by-zip discarded 53% of the sidecars.** Google splits an item
from the JSON describing it across numbered zips freely, and sidecars were
matched inside one directory. Six separate runs leave 5,979 of 11,282 sidecars
describing a file that is not there. `--from` now repeats and the scan groups by
where a file sits *inside* the export rather than on disk, which also makes a
local id the same whether the export was unzipped once or six times.

**The rest was the file being read too narrowly.** Twelve tags were read where
half the library carries an exposure, 57% an altitude, a third a video stream
nothing recorded, and 9% face boxes something else had already found. None of it
was lost — the originals are archived, so it was a requeue away — but none of it
was answerable either. The tag list is wider, the tags worth an index have
columns, and everything read is also kept verbatim in `exif_metadata`: the same
bargain the sidecar already makes, for the same reason, since the choice of what
deserves a column is wrong about something.

Two things that only showed up against real files. A video's timezone lives in
`CreationDate`, not the `CreateDate` beside it, which is UTC with no zone — the
share of the library with a known local time went from 32% to 61% on that one
tag. And a 2008 JPEG records an ISO of `75.4582213796711`: decoded strictly that
is not an error in one tag but an error for the whole record, which fails the
metadata job, retries four more times, and marks a good photograph broken and
thumbnail-less. Every tag now decodes leniently.

**The phone was the thinner of the two paths.** It sent a filename and two
timestamps. PhotoKit also knows the heart, the albums, and Apple's own subtypes —
screenshot, portrait, panorama, burst — and none of it survives the phone being
wiped or the photo being deleted from it. It now travels as a sidecar to the same
endpoint the importer uses, `source: "ios-photokit"`, after the bytes are safe:
an album list has no business in a header, and losing the description must not
cost the photograph. It runs for assets the archive already held too, which is
the only moment a library backed up before any of this existed can still hand its
hearts over.

Left deliberately: the read path. Every column here is written and none is served
— deciding what the gallery shows is the next question, and it is a much easier
one to change your mind about than what was captured.

### Phase 10 — Deletion

The archive learns to forget, in two steps, because the interesting problem is
not removing a photograph but making sure nothing removes one by accident.

**The trash is a scope, not a place.** `assets.deleted_at` is one column, and the
timeline's visibility predicate — the one pasted into the pages, the day table,
the album covers, the category counts and its own partial index — grows one term.
Recently Deleted is that predicate with the term flipped, which means it is the
same query, the same keyset cursor, the same day table and the same grid, so the
page is a route and a header rather than a second gallery to keep in step with
the first. A deleted photograph leaves its albums, its people and every category
on the same statement, because all of them were already asking the same question.

**A selection is positions, so an operation is too.** The grid addresses the
timeline by index — that is what lets a drag cover eleven thousand photographs
the browser has never fetched — so `POST /v1/trash` takes runs of positions plus
the filter they were counted in, and resolves them server-side in one statement.
Ids are accepted beside them for the tile under a right-click, which is exact.
The alternative, resolving ranges to ids in the client first, is a second round
trip that widens the same race it was meant to close.

**The undo is a batch, not a list.** Each delete stamps a uuid on the rows it
touched, and the toast carries that. By the time anybody clicks Undo the timeline
has been redrawn and every position in it means something else — the batch is the
only handle that still means what it meant. It also settles the two edge cases a
list would get wrong: the paired videos and caption layers that were carried
along are in it, and anything already in the trash when the delete ran is not.

**Two clicks beat a dialog, except on the keyboard.** Every delete in a menu or
the selection pill is an armed button: it says "Delete", and only once it says
"Confirm" does it delete anything. A modal to ask the same question is a third
surface and a focus trap. Delete and Backspace get the dialog instead, because a
keystroke has no first click to spend.

**Purging is the only operation that is real, so it is the only one that is
hard.** Rows and the content tombstone go in one statement — a row deleted
without a tombstone is a photograph the next backup uploads again, and the two
must not be able to come apart. Then the manifest line, so a rebuild knows this
was a decision rather than a loss. Then the bytes, last, because they are the one
step rerunning the others cannot repair. `verify` reports a file left behind;
nothing reports an asset the archive can no longer produce.

**A delete that the next backup undoes is not a delete.** The phone still holds
the photograph, and sync/check asks by content key, so a purge leaves that key
behind in `purged_content` and the answer to "shall I upload this again" is a
truthful-enough "have". It is a wall rather than a decision on purpose: choosing
which purged photographs may come back is a thing to do in the gallery, where
somebody can see what they are choosing between.

**Deleting an album is not deleting photographs.** An album is a grouping an
import produced, so the default drops the row and leaves every picture where it
was; "Delete album and photos" is the other reading and has to be aimed at. Both
share one batch, so the undo puts the album and its contents back together — an
album restored empty would be a worse outcome than either half.

The expiry runs inside photod on an hourly sweep rather than on a systemd timer
beside `verify`, for the same reason the upload sweep does: a retention that only
elapses on hosts where somebody installed a second unit is not a retention.

### Phase 11 — Archive and Hidden

Two buckets a photograph can be put into, and the first thing in this project
that is kept from somebody holding the disk rather than merely from the timeline.

**They are one mechanism and two destinations, on purpose.** Archive and Hidden
do exactly the same thing; what differs is the reason a person has for reaching
for one. "I have seen enough of this" and "this is nobody else's business" are
not the same sentence, and a single destination with a checkbox on it would be
the kind of tidiness that makes a product worse. Everything in the code says
`vault` when it means either.

**Putting something in must not need the password. Taking anything out must.**
This is the constraint the whole design is bent around. Hiding a photograph
happens at a right-click in a gallery that has been open all afternoon, and a
password prompt there teaches people to leave the vault unlocked — which costs
more than every property the password was buying. So the vault is an X25519
keypair rather than a password-derived key: the public half sits in the clear
and anything may encrypt to it, the private half is sealed under Argon2id and
nothing reads a byte back without it. Archiving forty photographs on a locked
vault works, and produces forty files this server cannot open.

**A thumbnail is the photograph.** The original is encrypted, and so is every
rendition made from it — the three thumbnail sizes, the playback file, the Live
Photo's motion. Encrypting a 12-megapixel HEIC and leaving a 256px copy of the
same picture on the SSD would be a vault with a window in it. The renditions are
decrypted straight into the response, chunk by chunk, and never touch a disk in
the clear; the three that are rendered on demand — the 2048px preview, a Live
Photo's 1080p clip, a composited caption layer — are the exception, because
ImageMagick and ffmpeg want a seekable path, and they get a staged copy that
lasts as long as the render.

**And the metadata is the photograph too.** A row saying `IMG_5874.HEIC`,
`iPhone 15 Pro`, 41.78N 122.58W, "at the border", album *Iceland 2025*, people
*Brody, Dominic* describes the picture well enough that not having the picture
is a detail. So the row is scrubbed: forty-odd columns emptied into a sealed
document, and the album and face rows deleted outright — which is also the
feature's own requirement that a hidden photograph leaves its albums, its people
and every category. The categories need no statement at all, because every one
of them is a predicate over columns the scrub has just emptied, inside a scope
the row has just left.

Two things deliberately stay in the clear, and the second is a real cost. The
structural columns — what is a paired video, what is a caption layer — because
they are what says this row is part of another one. And the content key, because
`sync/check` answers "have I got this?" from `(md5, size)`: without it the phone
would offer the photograph again on the next backup and the archive, having
genuinely forgotten it, would take it. Hiding a photograph would restore it.
What that leaks is that somebody holding the database can test whether a file
they already have is in the vault; they learn nothing they did not already know.

**The vault's gallery is computed in memory, because it has to be.**
`order by sort_time` over a column that no longer holds a capture time is not a
query that can be fixed, and keeping a plaintext index to sort the encrypted
rows by would be encrypting nothing. So unlocking builds an index: every sealed
document opened, sorted, grouped into albums and people and categories, and
served back in exactly the shapes the library's own endpoints use — same page,
same cursor, same day table. The client cannot tell which half of the archive it
is drawing, which is the point: there is one grid, one viewer, one zoom, and
encrypting half the archive did not earn a second one. It is affordable because
a vault is not a library — the library grows by itself, a vault is what somebody
went and hid, one gesture at a time.

**Hiding creates before it destroys, which is the opposite of a purge.** A purge
commits the database first and takes the bytes last, because a file left behind
is a finding and a row without a file is a loss. Hiding goes the other way: the
ciphertext is written while the plaintext is still there, then the transaction,
then the unlink. The window that leaves — a crash after the commit and before
the unlink, which is a hidden photograph still readable on the drive — is the
one failure here that is a security bug rather than an inconvenience, so it is
not left to chance. An hourly sweep looks for it, beside the trash's expiry.

**A restore puts a photograph back where it was, and says nothing when it
cannot.** The albums travel in the sealed document as ids and titles, so the
restore rejoins the ones that still exist and drops the ones that do not. No
warning, no resurrected album: the album was deleted on purpose, weeks ago, and
being told about it at the moment of a restore would be a notification about
somebody else's decision. The photograph goes back to the gallery either way.

**There is no delete inside the vault.** Taking a photograph out and then
deleting it is two decisions, and one button that decrypted a file in order to
throw it away would be spending the password on the only operation that does not
need it. The vault's menu offers Unarchive or Unhide and nothing else.

**The word on the button says what the button is about.** One photograph is a
photo or a video, because the grid knows which; several are *items*, because a
selection of eleven photographs and two videos is neither eleven photos nor
thirteen. An album is called "album" and not by its title — "Archive Iceland
2025" reads like a sentence about Iceland — and a person *is* called by their
name, because "Hide person" would be asking somebody to remember which circle
they right-clicked. Delete goes through the same function, so the three verbs
cannot end up describing the same selection three different ways.

**The old "Archived" is not this.** Google's export carries an archive flag,
imported since Phase 8 and stored on `assets.archived`. It is a category like
any other, over photographs that are otherwise entirely ordinary members of this
library, and nothing about it is encrypted or was decided here. It keeps the
past tense it was imported with and stays where it was, above the two new rows;
the new one takes the plain noun. They have nothing in common but a word.

### Phase 12 — The gallery on the network (built, then removed)

A browser anywhere in the house could open the archive, behind one shared
password, over the same TLS listener the phone has used since Phase 5. photod
reverse-proxied the Next app so that the bundle, the JSON and the thumbnails all
arrived from one origin, and a single session cookie authenticated all three —
which was the point, because a browser attaches a same-origin cookie to an
`<img>` and will not attach a bearer header to one.

**It has since been removed, whole.** One shared password with no accounts, no
roles and no revocation was always meant as a house key rather than an identity
system, and it is being replaced by something more robust rather than extended.
The removal touched no data: sessions lived only in the serving process's
memory, so there was no table, no migration and no file on disk to unwind. What
came out was `internal/websession`, `internal/api/websession.go`, the
`GALLERY_PASSWORD` / `GALLERY_SESSION_TTL` / `WEB_URL` settings, the `photoweb`
unit, and the web app's `SignIn` overlay and session module. The guard on the
read and gallery routes is the Phase 6 guard again, exactly.

What the phase established and what is worth keeping when it is replaced: the
credential a browser can actually carry onto a subresource is a cookie, not a
header, so whatever comes next still wants the app and the media on one origin.
Signed media URLs remain unnecessary — there is no URL that is itself a secret.
And the constraint that made the old design safe is unchanged: `PLAINTEXT_ADDR`
is loopback and unauthenticated, and widening it is the whole risk.

### Phase 13 — Albums you can make

Until this, the only thing in the archive that could create an album was an
import. The gallery could browse one, hide one and delete one; it could not make
one, and it could not put a photograph into one. Three writes, and almost
nothing new underneath them — the table, the membership and the timeline filter
have been there since Phase 8.

**An album is a title and a membership list, and nothing else.** No ordering of
its own, no cover somebody picked, no per-album settings. Every question about
what is *in* one is answered by the timeline with an album filter, which already
pages, virtualizes, zooms and selects. Adding a second way to look at an album
would be adding a worse one.

**A selection is positions here too.** `POST /v1/collections/albums/{id}/items`
takes the same runs-plus-filter body the trash and the vault take, resolved by
the same `Selection.pick`. Removing is the same body on `DELETE`, because a
selection *is* a body and the verb that says "take these out" has nowhere else
to carry what to take out.

**Making and filling are one request.** The usual way an album comes into
existence is out of a selection — right-click, Add to album, Create "Iceland" —
and splitting that in half leaves a failure mode where the album exists and is
empty and somebody has to notice.

**The name is unique among the albums that still exist.** The constraint an
import relies on counted rows the gallery cannot see, so deleting "Iceland" held
its name hostage for the 365 days it sat in Recently Deleted. Migration 0013
scopes it to the live rows. It is deliberately *not* scoped by bucket: a hidden
album's title still occupies the name, which leaks one bit — you can learn that
*some* hidden album is called that by being told the name is taken — and buys
the thing that matters more, which is that hiding an album is an update of one
column and can therefore never fail on a uniqueness check.

**The vault gets the same three writes and shares none of their code.** A hidden
photograph's albums live inside its sealed document, so filing one is opening
that document, adding a line, and sealing it again — `POST
/v1/vault/{bucket}/albums/{id}/items`, with the bucket in the path so no request
can put an archived photograph into a hidden album. Two things follow. This is
the first operation in the feature that a locked vault cannot do, and not by
policy: a document has to be opened to be added to. And an album made inside a
bucket is an archived album from the moment it exists, because the alternative —
make it in the library, then hide it — puts its title on the collections page in
between.

**Removing from an album is armed, and is not red.** Two clicks, because "remove
these forty" is a thing to have meant rather than a thing to discover. Not the
delete's colour, because nothing is destroyed: every photograph stays in the
library, in its other albums and in the timeline, and the toast carries an Undo
whenever the request named exact ids.

**The menu searches, and the ticks only appear when they can mean something.**
"Add to album" is a submenu with a box at the top; typing anywhere in it goes to
the box. With one photograph selected the albums it is already in are ticked and
clicking a ticked one takes it out. With several, they are not — a selection of
forty has forty answers, and a tick that meant "some of them" is not a thing
anybody wants to read off a menu they opened to file something.

### Phase 14 — Sorting, filtering and jumping to a date

The grid could show you a collection. It could not show you part of one, and it
could only ever show it in one order — so "the videos from the trip", "the
photographs I never filed", and "that afternoon in 2019" were all the same
gesture: scroll until you find it. This is the floating pill beside the
selection one, and the four sorts, five filters and one calendar behind it.

**The order and the collection are different questions.** A collection is a
*place* — this album, the trash, the Hidden bucket — and it changes when the
route does. A view is the order and the narrowing chosen while standing in that
place, and it changes under somebody's hands without going anywhere. They travel
as separate things (`db.TimelineFilter` gained fields rather than variants; the
client passes `View` beside `TimelineFilter`), and the second one reloads the day
table without remounting anything.

**A facet is an adjective, not a place.** At most one album, person or category
can be named, because their intersection is a question nothing poses. Photos,
Videos, Favorites and Not-in-an-album combine freely with each other and with
whatever collection they are asked inside, because "the videos in this album that
are in no other album" is a question somebody does pose. All photos is not a
filter at all; it is the absence of the rest, which is why pressing it turns them
off and turning the last one off lights it.

**Two of the four orders are the index read backwards.** Newest and oldest are
the same b-tree walked in opposite directions, so both keep the keyset cursor,
the constant-time page and the day table. Longest and shortest order by a column
with no index at all: they cost a sort of the filtered set per page and hand out
no cursor, so the client pages them by offset. That is deliberate. They are what
somebody reaches for once to find the twenty-minute video, not what they scroll a
hundred thousand photographs in — and the price of an index on `duration_seconds`
is paid by every upload forever.

**Ordering by length is a question about videos, so asking it says so.** Choosing
Longest turns the media filter to Videos, and taking Videos away puts the order
back to Newest. A photograph has no duration, and a grid of stills sorted by one
is a control that has stopped meaning anything while still looking like it works.

**A timeline in that order has no days, and says so.** The day table comes back
as a single run with no date on it, and the grid draws a flat wall of tiles with
no headings and no room reserved for them. The dates are still in there — every
photograph has one — but they fall in an order that has nothing to do with the
calendar, and a heading above every tile would be a ruin of that shape rather
than a description of it.

**Jump to date costs no request.** The client is already holding every heading
the collection will draw and the position each one starts at; a date is a lookup
in that table and a scroll offset. Which is what makes 2014 as instant as one
screen down. Dates outside the archive's span are disabled, dates inside it with
nothing on them land on the nearest day that has something, and the control sets
the order to Newest on the way in — a date is a position in a timeline read in
date order, and there is no such position in one read by length.

**A position means nothing without the whole description of the grid.** A
selection is runs of positions, so the sort and the facets travel with every
range on every write, exactly as the collection already did. Index 2 of a grid
sorted oldest-first is a different photograph, and the row number a delete
resolves against comes off the same filter the grid was drawn from. Getting this
wrong would not have been a cosmetic bug.

**The vault gets all of it and shares none of the code.** Its timeline is
computed in Go over decrypted rows, so filtering is a walk over a slice and
sorting is `sort.SliceStable` — and the "this one is not optimised" caveats above
simply do not arise, because there was never an index to miss.

**A collection does not offer the filter it already is.** Inside the Videos
category there is no Photos/Videos toggle; inside an album there is no
Not-in-an-album; inside Favorites there is no Favorites. One question asked of
the filter the page was built from, rather than a special case per page.

**Leaving the grid is leaving the view.** Same rule the selection has always
had, for the same reason: a filter silently carried into the next album is one
somebody has to go and find before their photographs come back.

### Phase 15 — Duplicates, and recordings that arrived in pieces

Two problems the archive had and could not see, which turn out to be the same
problem: several rows that ought to be one item.

Content addressing already caught every case where the *bytes* agreed —
`assets.sha256` is unique, and across 23,000 items there is not one `md5` pair
left to find. What it cannot catch is the same photograph recompressed by an
export, or a recording Snapchat cut into ten-second files. Both end with the
library holding a thing several times over; only the evidence differs.

**One pair of tables, two kinds of evidence, and only one of them asks.** A
`duplicate` group is found by comparing pixels and is a judgement about which
of four nearly identical photographs is worth keeping — nothing here is entitled
to make it. A `video-segments` group is found by comparing timestamps in a
document Snapchat wrote, and the worker resolves it without anybody being asked,
because "these six files are one minute of video" is not a matter of taste.
Everything after the finding is shared: elect a keeper, carry what the others
knew onto it, move them to the trash with a batch, and remember the batch as the
undo.

**The split-video signature is a ten-second grid, and it is exact.** Consecutive
pieces of one recording are written into `memories_history.json` exactly ten
seconds apart, to the second — 287 links at 10.00s in this archive against
single digits at every other spacing. The EXIF time on the same files is when
the piece was *written out*, and it drifts by up to eighty seconds across one
recording, which is why the scan reads `captured_at` and ordering by `sort_time`
puts a minute of video in the wrong order.

**The obvious corroboration does not work, and that is a measurement.** The last
frame of one piece and the first frame of the next are three hundredths of a
second apart, so they ought to be nearly identical. Measured across the 271
candidate links against a control set of memories twenty to a hundred and twenty
seconds apart: median difference-hash distance 21 bits across a real boundary
and 30 across the control, overlapping from 5 to 43. Handheld video of one scene
stays similar to itself for a whole minute — a second apart *inside* one piece
measured further apart than a real boundary did. A gate on that would have
refused a large fraction of genuine merges to exclude a couple of false ones.
What is relied on instead is the timestamps, and the fact that the merge is
undoable.

**A joined recording is an original, not a derivative.** It goes into `blobs/`
under its own digest, gets an asset line and a metadata line in
`manifest.jsonl`, and `verify` covers it like anything else — and the pieces it
was made from go to the trash rather than away, so for a year both exist and
either can be got back. It is the one file in the archive that no camera
produced, which is why its sidecar records the parts by digest and whether the
join copied or re-encoded.

**It copies. It does not re-encode.** Two consecutive pieces came out of the same
encoder on the same phone a tenth of a second apart, so joining them moves
packets rather than pixels: the output holds the camera's own frames and comes
out at exactly the sum of the inputs' durations. The re-encode path exists for
the handful of groups where a resolution changed mid-recording, a piece has no
audio, or a caption layer has to go into the pixels — and it says in the sidecar
which of those it was.

**Two hashes, because they fail differently.** A difference hash is a gradient:
it survives compression and resizing, and it finds two photographs of nothing in
particular to be the same nothing. A perceptual hash is the low frequencies of a
DCT: it describes structure, and it is what notices two flat frames are flat
differently. Requiring both is what keeps a wall and a different wall out of one
group. The archive's own fixtures make the point — `photo.jpg` is a vertical
gradient and `bare.jpg` is flat grey, and their difference hashes are both
exactly zero.

**The threshold barely matters, which is the useful finding.** Swept from 3 to 12
bits over six thousand real photographs: 513 groups at 3, 484 at 9, 494 at 12,
and around 380 pairs and 60 triples at every setting. Copies of one photograph
land within a handful of bits and unrelated photographs land past thirty, with
very little in between for a threshold to arbitrate. Nine, in the middle of the
plateau, because the failure modes are not symmetric — a false positive costs one
click on a page somebody is looking at anyway.

**What the threshold does change is how far transitivity runs.** A burst of
ninety-eight frames chains into one group at every setting tried. Those groups
are correct, and they are why the review page folds a group after twelve
thumbnails: a burst really is a hundred near-identical photographs, and what to
do about it is exactly the judgement this refuses to make on anybody's behalf.

**A video is a sequence, not a frame.** Twenty frames at even fractions of the
running time, hashed by the same code that hashes a still, compared position for
position against another clip of the same length. Sampled by *time* rather than
frame number, which is what survives a re-encode: a clip at 15fps and the same
clip at 30fps share nothing at frame seven and the same picture 35% of the way
through. The duration filter in front of it does less work than it looks like —
Snapchat caps a clip at ten seconds, so this archive holds 229,000 pairs of
unrelated videos agreeing on length to within two percent — so the sequence is
doing all of the discriminating.

**Signatures get their own worker pool.** A third pool for the reason there is a
second: this decodes every original in the archive and samples twenty frames out
of every video, it takes an hour over a library this size, and nobody is waiting
for it. Behind the metadata pool it would stall the gallery; behind the transcode
pool it would stall the viewer.

**The vault is excluded, and not as an optimisation.** A signature is a
description of what a photograph looks like. Computing one for something in the
vault would be this server writing down the thing the vault exists to stop it
knowing.

**A dismissal sticks by pair, not by group.** Refusing {a, b} and then finding
{a, b, c} is a different fingerprint and the same rejected question with a
stranger in it. So every pair inside a dismissed group becomes a pair the scan
will never link again — and an undone join lands in `dismissed` rather than back
in `pending`, because a pending set of segments would be re-joined by the worker
within the minute and the undo would appear not to have worked.

### Phase 16 — Shared albums

An iCloud Shared Album is not in the photo library. `PHFetchOptions` defaults to
`typeUserLibrary`, so the enumerator that has read this camera roll since Phase 2
cannot see one however it is sorted or predicated: shared assets exist only
inside collections of subtype `albumCloudShared`, and the way to them is through
the collection. Which meant that for fifteen phases, every photograph anybody
else had shared with this phone — and every one this phone had shared back — was
invisible to the backup and to the archive.

**The survey answered the question that decides whether any of this is worth
doing.** Apple documents a 2,048-pixel cap on the long edge of anything in a
shared album, which would make the shared copy a downscale and archiving it a
matter of keeping a thumbnail. Measured on this phone: **4,400 of 4,700 shared
stills are above it.** Whatever the documentation says, what is actually sitting
in these albums is at or near original resolution, and worth having.

**Nothing is backed up until an album is ticked.** The picker was built during
the survey, where an unanswered question could safely default to "all of them"
because looking cost nothing. Ticking an album now means uploading it, so the
default inverted: an empty selection is the honest starting state. The cost is
stated rather than hidden — an album joined next month is not archived until
somebody ticks it.

**Shared assets go in the same queue, and the queue grew a column to say so.**
`items.source` is `library` or `shared`, and it is stored rather than derived
because the distinction outlives the run that discovered it: nothing about a
local identifier says which of the two it names, and an item queued today is
opened by a run tomorrow. Everything else — the states, the backoff, the circuit
breaker, resume after a kill — is the machinery that already worked, unchanged.

**A shared asset never enters the hashing state, and that is the whole design.**
The Phase 2 rule is that the phone must not hash 100GB to discover the server
already has it, so an item is checked by local id first and hashed only if the
answer is inconclusive. For a shared asset the rule inverts: there is no local
original, so *reading it is downloading it from iCloud*, and hashing before
uploading would fetch every shared photograph from Apple twice. So it goes
straight from the first check to the upload path, and the native download hashes
the bytes as they stream past — one trip, both answers. The price is that the
archive is never asked whether it already holds this content, and it is small:
Apple re-encodes what goes into a shared album, so its copy rarely matches
anything already archived, and the upload is content-addressed and refuses to
store the same bytes twice regardless.

**One download at a time, through a gate that breathes.** Three upload workers
each opening whatever the queue handed them is right for a LAN and wrong for
Apple. `SharedFetchGate` serializes the downloads and stretches the pause between
them as failures accumulate, shrinking it again as they stop — the arithmetic
the survey's pacer already used, shared with it so the two cannot drift apart.
The retrying itself stays with the engine: a second retry loop nested inside the
first would multiply the two and take an hour to declare one asset failed.

**The album is the filing and the contributor is the provenance.** A shared
photograph arrives filed under the shared album's title, in the same
`ios-photokit` namespace as any other album off this phone — nothing on the
server records that it was shared, which is deliberate. What is recorded is who
added it, in the sidecar, surfaced as one row in the viewer's panel. That name is
the only provenance a shared photograph has: Apple re-encodes the file, so there
is no maker note to read, and the name is gone the day the album is left.

**Getting it means reading a key Apple does not document.** PhotoKit has no
public property for the contributor — the Photos app draws the name under every
asset in a shared album, and there is no supported way to ask for it. So it is
read by KVC, and every read is guarded by `responds(to:)` first, because
`value(forKey:)` on a key the class does not have raises an Objective-C exception
that Swift cannot catch: an unguarded read of a key Apple renames is not a
missing name, it is the app going down mid-backup. This is not App Store code and
could not be. The trade is a name that would otherwise be lost against a private
key that may stop working, in which case the row goes quiet and nothing else
changes.

**What the resource is called outranks what the asset is called.** PhotoKit goes
on calling a shared asset `IMG_4021.HEIC` after iCloud has handed over a JPEG,
and the server trusts a recognised extension over the sniffed bytes. Left alone,
every shared JPEG in the archive would be stored and served as a HEIC. The
upload is named after the resource Apple actually sent.

### v2

- **ML service.** Python, CLIP semantic search, then face detection and clustering.
  The job queue, embeddings table, and stub ML client ship in v1, so this drops in
  with no schema migration.
- **USB ingest.** A host-side daemon, triggered by a udev rule, mounts the phone's
  `DCIM` over AFC (`libimobiledevice` / `ifuse`) and ingests anything the library
  lacks, de-duplicating against the same blob store.

---

## 8. Risks

**1. Live Photo pairing.** The most likely thing to force native code. Retrieving
a Live Photo's companion `.mov` through `expo-media-library` is the shakiest part
of the Expo path. Fallback is one small Swift native module — contained, not a
rewrite. Phase 0 settles it.

**2. Background upload is not a promise.** iOS decides when the app runs. The
design accounts for this: foreground is the workhorse, background is a bonus
top-up. The UI should say so plainly rather than imply backups happen while the
app is closed.

**3. Apple developer account.** A free account requires re-signing every 7 days,
which is untenable for an app depended on daily. The $99/year account is
effectively mandatory.

**4. iOS limited photo access.** Choosing "Select Photos" instead of "All Photos"
at the permission prompt silently limits the app to a subset. Must be detected
explicitly and surfaced as a warning.

**5. HEIC decode on the server.** Fedora needs RPM Fusion for libheif codecs.
Cheap to verify now, annoying to discover in Phase 3.

**6. Battery and heat.** Pushing 100GB will heat the phone significantly. Bulk
backfill should be gated on charging + Wi-Fi by default, with a manual override.

**7. Unauthenticated gallery.** Open again, deliberately, since the Phase 12
browser gate was removed. The TLS listener is token-only, so nothing on the LAN
reads the archive without having been paired — but a browser cannot carry a
token, which means there is no supported way for a laptop or an iPad in the
house to open the gallery at all. The plaintext listener is what the development
gallery talks to, it asks for nothing, and it is bound to loopback; widening
`PLAINTEXT_ADDR` is still the whole risk. A device token cannot cross it —
pairing and every authenticated route are absent from its routing table — so
widening it exposes photographs but never a credential. Closing this properly is
the open design question the gate's removal reopened, and whatever answers it
still has to put the app and the media on one origin: a cookie is the only
credential a browser will attach to an `<img>`. Putting the gallery on the
internet, which §4 keeps as an option, is a different question again.

**8. Losing `ca.key`.** It is the one file whose loss means physically re-pairing
every device, and the one whose disclosure lets somebody impersonate the archive
to a phone that trusts it. Mode 0600 in a 0700 directory on the SSD, and
deliberately not on the archive drive that gets rsynced elsewhere.

**9. Single drive.** The archive lives in exactly one place. The blob layout is
deliberately rsync-friendly and `photobackup verify` detects bit rot, so adding a
second drive or an encrypted offsite target later requires no code changes. Until
then, this is an accepted, known risk.

---

## 9. Deliberately not built

An iOS app **cannot** communicate over USB. Third-party apps have no general USB
API; `ExternalAccessory` requires MFi-certified hardware with a registered
protocol, which a Mac or Fedora workstation is not. The app cannot even detect
that a cable is attached.

USB backup is therefore only possible as a **host-initiated** path: the computer
reads the phone's `DCIM` folder over AFC/PTP. That is a separate ingester with no
app involvement, and it sees only the camera roll — no albums or favorites
metadata. It is scoped to v2.
