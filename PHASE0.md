# Phase 0 — Spike results

Run on 2026-08-10. iPhone 14 Pro, iOS 26.1, Expo SDK 57 / React Native 0.86.2,
custom dev client built locally. Library: **3,439 photos + 134 videos**,
`accessPrivileges === 'all'`.

Everything below was measured on the device. Where the phone reports a result,
it is cross-checked against `phase0-server/`, a dependency-free Node server that
records timestamped byte arrival and independently digests what actually landed.

---

## Verdict

| # | Question | Answer |
|---|---|---|
| Q1 | Native MD5 of a large file? | **Yes**, 395–557 MB/s streaming — but it blocks the JS thread |
| Q2 | Background upload survives suspension? | **Yes** for Home-button suspension. **No** for force-quit |
| Q3 | Live Photo paired `.mov` via expo-media-library? | **Yes**, first-class API, no Swift module needed |
| Q4 | mDNS discovery from a dev client? | **Yes**, resolves and the address is dialable |
| Q1b | *(new)* Can a background upload read a PhotoKit original? | **No** — sandbox boundary. This one changes the design |

Three of the four original risks came back cheaper than PROJECT.md assumed. The
spike's real value was the fifth question, which nobody had asked.

---

## Q1 — Native MD5

`expo-file-system`'s `File.md5` is a streaming CryptoKit MD5 over 64 KB chunks
(`ios/FileSystemFile.swift:53`). It does not buffer the file.

Target: `IMG_8071.MP4`, 1,010,469,721 bytes (963.66 MB).

| run | time | throughput |
|---|---|---|
| cold | 2,437 ms | 395.4 MB/s |
| warm (page cache) | 1,731 ms | 556.7 MB/s |

Extrapolated to a 100 GB library: **~3–4.5 minutes of hashing in total.**
Negligible in aggregate.

**The cost is not time, it is that `md5` is a synchronous JSI property.** The JS
thread was dead for the full 2.4 s — a 20 ms interval fired 6 times during the
hash, with a largest gap of 2,487 ms. The native-driver animation kept moving,
so the UI thread is unaffected; everything in JS (queue, progress, taps) is not.
There is no async variant.

*Implication for Phase 2:* hashing cannot sit inline in the queue loop. Either
run it behind an explicit "preparing" state that expects to freeze, or move it
off the JS thread.

### Two corrections to assumptions made before the run

- **Legacy is not fatal.** `expo-file-system/legacy`'s
  `getInfoAsync(uri, {md5: true})` does `[NSData dataWithContentsOfFile:]`,
  which reads as "loads the whole file into RAM". It hashed the same 963 MB file
  in **2,046 ms with no crash** — `NSData` mmaps large files. It is still the
  worse choice, but it does not OOM at this size.
- **PhotoKit URIs carry a fragment.** Every resolved URI came back as
  `file:///var/mobile/Media/DCIM/108APPLE/IMG_8071.MP4#YnBsaXN0MDD...`
  (a base64 plist, `RecommendedForImmersiveMode`). `File()` handles it, but
  anything treating these as plain strings — cache keys, dedup keys, filename
  parsing — must expect the `#`.

### URI resolution is cheap here

10 videos resolved in **23–104 ms** each. `UriExtractor` re-exports slow-motion
clips through `AVAssetExportSession`, which would be very expensive; no asset in
this library triggered it. Worth re-checking on a library that has slo-mo.

---

## Q1b — Where can an upload read from? *(the finding that matters)*

Q1's upload of a PhotoKit original failed instantly. Q3's upload from the app
container succeeded. Both used `sessionType: 'background'` — which is the
**default**. Two candidate causes: the sandbox boundary, or the URI fragment.

Matrix on `IMG_2204.MOV` (349,780 bytes):

| source | session | result |
|---|---|---|
| PhotoKit URI (with fragment) | background | **FAILED** in 4 ms |
| PhotoKit URI (with fragment) | foreground | OK, 72 ms |
| fragment stripped | background | **FAILED** in 38 ms |
| fragment stripped | foreground | OK, 42 ms |
| copied into app container | background | OK, 324 ms |

The fragment is irrelevant. A background `NSURLSession` hands the file to
`nsurlsessiond`, which does not inherit the app's PhotoKit sandbox extension, so
it fails before a byte leaves the device. The error is
`UnableToUploadException: unknown error` — no indication of the real cause.

### This forks the upload flow in PROJECT.md §6

- **Foreground bulk upload** — the designed workhorse — streams originals
  directly from PhotoKit. Unaffected. No extra copies, no extra disk.
- **Background top-up** must copy each original into the app container, upload,
  then delete. That means a second full write of every byte backed up in the
  background, free space for the largest single file (963 MB in this library),
  and cleanup that survives a crash or the temp directory grows without bound.
- **Live Photo `.mov`s are already in the container** (the module extracts them
  there), so they are background-eligible for free.

A reasonable v1 policy: foreground does the 100 GB backfill from PhotoKit
directly; background only tops up recent items, copying as it goes, and skips
anything above a size threshold.

---

## Q2 — Background upload across suspension

20 MB synthetic payload, server throttled to 200 KB/s so the transfer spans
~102 s and there is time to background the app.

**Home-button suspension — passes.** The server logged **102 one-second ticks,
zero of them with zero bytes.** Arrival held flat at ~196 KB/s for the entire
run while the app sat in the background.

```
16:05:19   4.9%   delta=196608
16:05:29  14.9%   delta=262144
   ...  (no gaps)
16:06:49  92.7%   delta=196608
16:06:57  upload-complete  102,699 ms  md5Match=true
```

The JS side survived as well: progress callbacks kept firing with
`appState=background`, and the promise resolved at `appState=background` — iOS
woke the app on the session completion handler rather than discarding it.

**Force-quit — fails, as expected.** Swiping the app away produced
`upload-aborted` at 3,367,648 bytes after 16,194 ms. User-initiated termination
cancels background transfers; system-initiated suspension does not.

*Implication:* Risk #2 in PROJECT.md is milder than written for suspension, and
exactly as written for force-quit. The queue must treat a force-quit as "this
item is unacked, re-send it", which is already the design in §6 step 5. Note the
Expo docs' caveat still stands and was not contradicted here: if the app is
*terminated*, the JS `UploadTask` is not restored, so the ack must be
recoverable from server state rather than from a surviving promise.

---

## Q3 — Live Photo paired `.mov`

`Asset.getMediaSubtypes()` and `Asset.getLivePhotoVideoUri()` are first-class in
SDK 57. **No native module is needed.** Risk #1 in PROJECT.md is retired.

- 3 Live Photos found within the first 4 assets scanned (36 ms).
- Paired `.mov` extracted in **35–42 ms** each, 3.93–4.36 MB.
- Stills were 0.79–1.05 MB HEIC.
- All three uploaded with server-verified MD5 matches.

One caveat: `LivePhotoVideoUriExtractor` writes the paired video to
`NSTemporaryDirectory()` under a fresh UUID and never removes it. Every Live
Photo backed up leaves a copy behind unless v1 deletes it after upload.

---

## Q4 — mDNS discovery

`react-native-zeroconf` 0.14.0 is a legacy bridge module, and it works under
RN 0.86 bridgeless through the interop layer.

```
resolved: photod-spike -> Dominics-MacBook-Pro-2.local.:8787
addresses = 10.0.4.120, fe80::c87:c767:1969:5d50, fd45:f1a:cfd9:1:10aa:267a:7f77:e4a8
txt       = {"srv":"photobackup-spike","path":"/","ver":"1"}
```

Both the IPv4 address (28 ms) and the `.local` hostname (59 ms) returned
HTTP 200 from `/health`. IPv6 link-local and ULA addresses are surfaced too.

Required Info.plist keys, all confirmed necessary:

```
NSLocalNetworkUsageDescription
NSBonjourServices = ["_photobackup._tcp"]
NSAppTransportSecurity.NSAllowsLocalNetworking = true   # for plain HTTP to 10.x
```

The Local Network prompt appears on the first scan. Denying it makes the scan
return empty with no error — indistinguishable from "server not running", so v1
must surface that state explicitly.

---

## Incidental findings

- **`AssetInfo` has no byte-size field.** Neither does `AssetMetadata`. The
  `{localId, size, createdAt, md5}` payload in §6 step 2 cannot be built from
  the media library alone — size requires resolving the URI and stat'ing it.
- **Enumeration is cheap.** `Query.exeForMetadata()` over all 3,573 assets
  returned fast enough to be unnoticeable.
- **`accessPrivileges` works** and is the right detector for Risk #4.

---

## Not yet measured

- **Real LAN upload throughput.** The 963 MB upload failed on the sandbox issue
  before transferring, and the unthrottled Q3 uploads (4 MB each, 6–23 MB/s)
  are too small and too noisy to extrapolate from.
- **Cost of stat'ing every asset for size.** Video URI resolution is 23–104 ms;
  images use a different code path (`requestContentEditingInput`) and were not
  timed. At ~3,500 assets this could be minutes, which matters for §6 step 2.
- **Copy-into-container throughput.** Only measured on a 0.33 MB file (38 ms),
  which is almost entirely fixed overhead. Needs a real number on a large video
  to price the background-upload path.
- **Behaviour on cellular, low-power mode, or a locked screen**, where iOS may
  treat background transfers as discretionary.
- **Slow-motion video**, which triggers an `AVAssetExportSession` re-export on
  URI resolution. None in this library.

---

## Artifacts

- `expo-app/` — spike app. Throwaway per PROJECT.md, kept for reference.
- `phase0-server/server.mjs` — test server. `node server.mjs`, advertises
  `_photobackup._tcp` via `dns-sd`, throttles uploads, records a byte-arrival
  timeline at `GET /timeline`.
