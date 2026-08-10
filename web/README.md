# web

The gallery: a virtualized timeline of the whole archive and a full-size viewer.
Next.js App Router, rendered entirely on the client.

## Running

```sh
cd ../server && go run ./cmd/photod    # the API, on :8787
pnpm install
pnpm dev                               # http://localhost:3000
```

| variable | default | meaning |
|---|---|---|
| `PHOTOD_URL` | `http://localhost:8787` | where `/api/*` is proxied |
| `NEXT_PUBLIC_MEDIA_BASE` | unset — use the proxy | origin for thumbnails, previews, and video |

Both are read at **build** time, not at start time. `rewrites()` is evaluated by
`next build` and frozen into `.next/routes-manifest.json`, so setting
`PHOTOD_URL` in front of `pnpm start` does nothing — the server keeps proxying to
whatever was set when it was built, and every `/api/*` request 500s with
`ECONNREFUSED` if that host is not there. Set it for the build.

```sh
pnpm test     # layout arithmetic, via node --test
pnpm lint     # tsc --noEmit
pnpm build
```

## Two origins, no CORS

photod is a separate process, so the browser has to reach two servers. Rather
than configuring allowed origins, `next.config.ts` rewrites `/api/*` to photod.
The JSON is then same-origin: no preflight, no origin list, and nothing to get
wrong when this eventually sits behind a real domain.

Media is exempt from that reasoning. `<img>` and `<video>` are not subject to
CORS unless they opt in, so `NEXT_PUBLIC_MEDIA_BASE` can point them straight at
photod and skip the proxy — worth doing when several thousand thumbnails would
otherwise stream through Node. Leaving it unset uses the proxy, which always
works.

Next's `<Image>` is deliberately unused. It exists to resize arbitrary source
images at the edge; photod already emits a thumbnail at exactly the size the
grid wants, and routing it through the optimizer would add a hop to re-encode
something that is already a 256px WebP.

## How the timeline is virtualized

`src/lib/layout.ts` holds the geometry and nothing else — no React, no DOM — so
it is unit-tested directly.

Items are grouped into days and turned into a **row model**: each row is either
a date heading or one line of tiles, and every row's height is known before
anything renders. Total scroll height is therefore exact from the first frame,
the scrollbar never jumps as pages load, and any scroll position resolves to a
range of rows with two binary searches.

Rows are positioned with `transform`, not `top`, so scrolling repositions a
screenful of them without triggering layout.

Tiles are square because the stored thumbnail is a square center crop: the
browser never rescales it, and row height follows from cell size alone. The
cost is that the grid crops — the viewer shows the real framing.

Cells target 160 CSS px and stretch to divide the container evenly. That is
deliberately near the ceiling for a 256px thumbnail: much larger and a 2x
display starts to show it. Raising `THUMB_SIZE` on the server and re-running the
metadata jobs is the lever if the grid should get bigger.

## What the client polls, and what it does not

Two loops, both narrow:

- Tiles whose derivative is not ready yet poll `/v1/timeline/states`, **only for
  the ids currently on screen**. During a backfill the library can be tens of
  thousands of pending items; asking about all of them every few seconds would
  cost more than generating the thumbnails.
- A header chip polls `/health` for queue depth and failures, so a permanently
  failed derivative is noticeable without going to look for it.

New uploads do **not** appear without a reload. The timeline is a keyset page
sequence anchored at the newest item, and inserting at the head mid-scroll would
shift everything under the cursor. Watching a tile finish is worth the
complexity; watching the top of the list grow is not.

## The viewer

The URL carries `?asset=<id>`, so a photo can be linked and Back closes the
viewer. Stepping between photos *replaces* the history entry rather than pushing
one — otherwise Back would walk through everything just viewed instead of
returning to the grid.

Opening a link to a photo that is not in the loaded pages keeps paging until it
turns up. Bounded by the library size at ~16KB a page, which beats adding a
second lookup endpoint or refusing to open the link.

Photos load `/preview`, rendered per request from the original, which means the
viewer works even for an asset whose thumbnail job has not run. Videos load the
H.264 rendition, and fall back to a download link for the untouched original
when the transcode is missing or failed.
