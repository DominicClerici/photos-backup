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
  manifest.jsonl                    append-only recovery log

$DERIVATIVES_ROOT/                  the SSD; defaults to /mnt/photos/derivatives
  ab/cd/abcd1234.thumb.webp         256px square, stored
  3f/9a/3f9a77b2.thumb.webp         poster frame for video, same shape
  3f/9a/3f9a77b2.mp4                H.264 playback rendition, video only
  7c/21/7c21ba05.live.mp4           256px square, a Live Photo's motion
```

Originals live on the archive partition — 500GB of the 6TB drive, mounted at
`/mnt/photos`. Postgres and derivatives should live on the workstation SSD for
speed, which is what `DERIVATIVES_ROOT` is for.

The 2048px preview is **not** stored. It is rendered per request from the blob
and cached by the browser instead, since the bytes are content-addressed and can
never go stale. One viewer shows one preview at a time; a stored file would buy
nothing that `Cache-Control: immutable` does not.

### Live Photos

A Live Photo is two files, and the phone is the only party that knows they
belong together — the still and its ~3s video share nothing but a capture time,
and pairing on that would marry a photo to whatever else was shot in the same
second. So the app declares it, on the video's upload, and the server stores the
declaration rather than trying to infer one.

The declaration is separate from its resolution, because they become true at
different moments. `live_parent_local_id` is what the phone said and is known
the instant the bytes land; it is what makes an asset a paired video at all, so
it decides which derivatives get built and what the timeline hides.
`live_parent_asset_id` is the still it resolved to, and cannot be filled in
until that still exists — the two halves share a capture time, the upload queue
orders by capture time, and nothing decides which goes up first, so it resolves
from whichever side arrives second.

**The paired video is never an item of its own.** It is archived, verified, and
downloadable like everything else; it simply is not a thing anyone took a
picture of, so the timeline shows the still and carries the motion as one extra
field on it. That is also why it is filtered on the declaration rather than the
resolution: a video whose still has not arrived yet is still not a photograph.

What it gets built is deliberately lopsided against what an ordinary video gets:

| | ordinary video | Live Photo's video |
|---|---|---|
| poster `.thumb.webp` | yes | **no** — the tile belongs to the still |
| stored H.264 `.mp4` | yes, via the transcode queue | **no** |
| `.live.mp4`, 256px square | no | yes, in the metadata job |
| 1080p with audio | n/a | rendered per request, stored nowhere |

Roughly a third of an iPhone library is a Live Photo. Putting each of those
three seconds through the transcode queue would swamp the videos that genuinely
need it, for a rendition only ever played while a mouse button is held down —
so the viewer's copy is rendered on demand, the same trade §5 already makes for
the 2048px photo preview. The 256px one *is* stored, because the grid asks for
it on hover and an ffmpeg per hover is not a thing that can be allowed.

The one place this differs from the photo preview: those renditions are kept in
memory briefly after rendering. A `<video>` asks for its bytes more than once —
Safari opens with a range probe before requesting the file properly — and
without it the same three seconds would go through ffmpeg two or three times for
a single press-and-hold.

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

**7. Unauthenticated gallery.** Closed on the TLS listener: the read path now
takes the same device token the write path does, so nothing on the LAN reads the
archive without having been paired. What remains is the plaintext listener, which
stays open by design for the browser gallery and is bound to loopback — it is now
the only unauthenticated way in, and widening `PLAINTEXT_ADDR` is the whole risk
rather than one half of it. A device token still cannot cross it: pairing and
every authenticated route are absent from its routing table, so widening it
exposes photos but never a credential. Putting the gallery on the internet, which
§4 keeps as an option, still needs an answer for the browser — see below.

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
