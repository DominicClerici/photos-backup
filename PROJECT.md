# photos-backup

A self-hosted photo and video backup service. An iPhone app pushes originals to a
Linux server at home, which stores them on a 6TB drive and serves them back
through a web gallery.

Built from scratch deliberately. Immich already solves this problem well; the
point here is to own the design and the code.

---

## 1. Goals

**v1 succeeds when:**

- Photos and videos taken on the iPhone reach the 6TB drive without manual effort.
- A second backup run uploads zero bytes.
- The entire archive is browsable in a web browser.
- `photobackup verify` passes across the whole library.

**Non-goals for v1:** multi-user accounts, sharing, editing, mobile gallery
browsing, ML search, USB ingest, mirroring phone deletions.

---

## 2. Decisions

| Area | Decision |
|---|---|
| Sync model | One-way archive. The server never deletes. |
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

### Why one-way

The phone produces photos; the server archives them. Deleting on the phone does
**not** delete on the server. This removes every destructive edge case, and it is
what a backup should actually do. It also makes freeing up phone space safe later,
because the archive is authoritative.

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
        |    enqueues jobs                                   |
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
              /mnt/photos  (6TB drive)
```

Three server processes under systemd, plus the web app. The split is by process
boundary, not sprinkled through one codebase.

The gallery is a separate Next.js app rather than pages served by photod. It
costs a second process on the archive machine, and buys the ability to put the
gallery on the public internet later without moving the upload endpoints or the
6TB drive along with it. photod stays a private API; the web app is the only
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
  manifest.jsonl                    append-only recovery log

$DERIVATIVES_ROOT/                  the SSD; defaults to /mnt/photos/derivatives
  ab/cd/abcd1234.thumb.webp         256px square, stored
  3f/9a/3f9a77b2.thumb.webp         poster frame for video, same shape
  3f/9a/3f9a77b2.mp4                H.264 playback rendition, video only
```

Originals live on the 6TB drive. Postgres and derivatives should live on the
workstation SSD for speed, which is what `DERIVATIVES_ROOT` is for.

The 2048px preview is **not** stored. It is rendered per request from the blob
and cached by the browser instead, since the bytes are content-addressed and can
never go stale. One viewer shows one preview at a time; a stored file would buy
nothing that `Cache-Control: immutable` does not.

`manifest.jsonl` records one line per stored blob: hash, original filename,
capture time, source device, byte size. It is the disaster-recovery path when the
database is gone.

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
long-lived device token, stored hashed server-side. The server self-signs a TLS
certificate on first run; the app pins it at pairing (trust on first use).

The app resolves the server via mDNS (`_photobackup._tcp`) on the LAN and falls
back to the Tailscale address when away. No ports are exposed to the internet.

### Failure behavior

| Condition | Result |
|---|---|
| Server unreachable | Queue holds, retries with backoff, nothing lost |
| ML service down | Search degrades to EXIF/date; backup unaffected |
| Hash mismatch | Temp discarded, item retried |
| Upload interrupted | Resumes from last acked offset |
| Bit rot on disk | Caught by `photobackup verify` |
| Database lost | Rebuilt by replaying `manifest.jsonl` |

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

Two job kinds in two separately sized pools: `metadata` (exiftool, the 256px
thumbnail, a video's poster frame) and `playback` (H.264/AAC MP4). They are
split because a handful of 4K transcodes would otherwise take every worker slot
and starve the thumbnails behind them, which during a backfill looks exactly
like a gallery that has stopped working.

Not built, deliberately: a date scrubber. The timeline pages on a keyset cursor,
so jumping to an arbitrary date means either loading everything in between or a
second index; neither is worth it before the archive is real.

### Phase 4 — Real-load hardening

Chunked resumable video upload, `photobackup verify`, `photobackup export`,
systemd units, then the real 100GB run. Expect this phase to surface problems that
110 items never could.

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

**7. Single drive.** The archive lives in exactly one place. The blob layout is
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
