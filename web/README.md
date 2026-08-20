# web

The gallery: a virtualized timeline of the whole archive and a full-size viewer.
Next.js App Router, rendered entirely on the client.

## Running

```sh
cd ../server && go run ./cmd/photod    # HTTPS on :8787, read path on :8788
pnpm install
pnpm dev                               # http://localhost:3000
```

| variable | default | meaning |
|---|---|---|
| `PHOTOD_URL` | `http://localhost:8788` | where `/api/*` is proxied |
| `NEXT_PUBLIC_MEDIA_BASE` | unset — use the proxy | origin for thumbnails, previews, and video |

Since Phase 5, photod serves `:8787` over **HTTPS** with a certificate it signs
itself, and puts the gallery's read endpoints on a second plaintext listener at
`127.0.0.1:8788`, which is what `PHOTOD_URL` now defaults to. Anything pointed at
`:8787` needs the CA: every `/api/*` request fails its certificate check, and `<img>` tags
pointed at `https://…:8787` show broken images in any browser that has not been
taught to trust the CA. The plaintext listener exists precisely so the gallery
does not have to be: it serves the read path and `/health` and nothing else, and
photod refuses every credential-carrying endpoint on it.

Reaching the gallery from another machine on the LAN is not supported. This app
is a browser on the archive host talking to the loopback listener, and there is
no authentication in front of it — widening `PLAINTEXT_ADDR` is what it always
was: the whole archive, on the network, asking for nothing.

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

Point it at 8788, never 8787. Since Phase 6 the TLS listener authenticates its
read path, and a browser cannot put a bearer token on an `<img>` — the tiles
would all 401. That limit is the browser's alone: the phone puts the header on
every rendition, which is why the app reads the archive over 8787 and needs no
signed URLs.

Next's `<Image>` is deliberately unused. It exists to resize arbitrary source
images at the edge; photod already stores a thumbnail at each size the grid
asks for, and routing it through the optimizer would add a hop to re-encode
something that is already a square WebP of the right dimensions.

## Sections

`/` is the gallery; `/collections`, `/overview`, `/search` and `/other` are
routes of their own, still placeholders. The bar that switches between them is
mounted by the root layout rather than by each page, so it survives navigation
with its highlight intact.

That highlight is one element behind the row, measured from the tabs' live
geometry and moved with a transform. The tabs themselves never change size —
only their colour does — because anything that resized one (a heavier weight
when selected, a label that appears only on the active tab) would reflow the row
underneath the animation meant to track it. Labels do drop out below 640px,
which the `ResizeObserver` on the row picks up.

The bar floats over the timeline, so the scroller carries bottom padding to
stand it on, and the zoom slider — which comes and goes — sits above it. The
viewer covers it entirely; `body[data-overlay]` then hides it outright, because
a control under a full-screen overlay should not still be reachable by Tab.

## How the timeline is virtualized

`src/lib/layout.ts` holds the geometry and nothing else — no React, no DOM — so
it is unit-tested directly.

The grid is laid out from `/v1/timeline/days` — every heading the collection
will draw and how many tiles hang under it — **before a single photograph is
fetched**. Running totals over those counts give every item in the collection an
index, so the full scroll height is exact from the first frame, the scrollbar
never shrinks as pages land, and any scroll position resolves to an item with
two binary searches.

Which means scrolling never hits a wall. A position with nothing loaded in it is
a real place in the grid with a real heading above it, drawn as a `Skeleton`
until the photo for it arrives — and arriving replaces exactly the square that
was standing in for it, so nothing shifts.

That index is also the address the timeline is fetched by. `useTimeline` holds
one sparse array over it and asks for the pages covering what the grid says it
is looking at, nearest the middle first, three at a time, dropping requests for
ground already scrolled past. Paging down still walks a keyset cursor, because a
page continuing from the one before it can say so; a fling into unvisited
territory asks for a row offset instead. Flinging to the middle of an 18,000-item
archive costs one request, not ninety.

Runs, not dates. Items are ordered by instant and filed under their own local
day, so a photo taken across a timezone boundary can put a date on both sides of
another one — the server sends that date twice because the grid draws it twice.
A real 18k library has 29 of these.

Rows are positioned with `transform`, not `top`, so scrolling repositions a
screenful of them without triggering layout.

Tiles are square because the stored thumbnail is a square center crop: the
browser never rescales it, and row height follows from cell size alone. The
cost is that the grid crops — the viewer shows the real framing.

Cells settle on one of seven zoom levels — 64 to 512 CSS px — and stretch below
that ceiling to divide the container evenly. `thumbSizeFor` in `layout.ts` maps
a level to the stored rendition that draws it: the smallest of photod's 96, 256
and 512 that can fill the cell without being stretched. Downscaling is free and
upscaling is not, so it rounds up, and the two outer levels each get a size of
their own rather than stretching or wasting the middle one.

A tile loads a new size out of sight and swaps it in once it has arrived,
because assigning a fresh `src` to a mounted `<img>` blanks it until the bytes
land — a flash of empty grid at the end of every zoom otherwise. A size photod
has not rendered yet 404s, and the tile falls back to the 256px rendition every
asset has, so a library still waiting on `photobackup verify --fix` draws at the
wrong size rather than not at all.

## Sorting, filtering and jumping to a date

The floating pill left of the selection one, and the four sorts, five filters
and one calendar behind it. Drawn on every grid — the library, a collection,
Recently Deleted, either vault bucket — and nowhere else.

`src/lib/view.ts` holds the rules and no UI: what turns what off, which filters
a collection has left to offer, what the pill says. Pure functions, `View` in
and `View` out, so the awkward parts are statable and tested rather than
scattered through click handlers.

**The view is not the filter.** `TimelineFilter` is the place — this album, the
trash, the Hidden bucket — and changes when the route does. `View` is the order
and the narrowing chosen while standing there, and changes without navigating.
`useTimeline` reads the first through a ref and the second through a key, so a
reorder reloads the day table without remounting the grid.

**The pill and the grid are mounted by different things.** The bar is the root
layout's and the grid is a page's, so `useView` is where they meet — the same
arrangement `useSelection` has, one layer in. The grid publishes what it is, the
day table it drew from, whether that table is being replaced, and the one thing
only the scroller can do: `jump(index)`. Leaving the grid resets the view, for
the reason it drops the selection: a filter carried silently into the next album
is one somebody has to go and find before their photographs come back.

**Ordering by length is a question about videos.** Choosing Longest turns the
media filter to Videos; taking Videos away puts the order back to Newest. And
that timeline has no days in it — the server sends one dateless run, `headless()`
sees it, and the grid draws a flat wall of tiles with no headings and no room
reserved for them.

**Jump to date costs no request.** Every heading the collection will draw is
already in hand, with the position it starts at, so a date is a lookup and a
scroll offset — 2014 is as instant as one screen down. Dates outside the span
are disabled; a date inside it with nothing on it lands on the nearest day that
does. The calendar waits while a reordered day table is in flight, because a
date resolved against the old one would scroll to the right number in the wrong
list.

**A position travels with the description of the grid it was counted in.** The
view goes onto every `Target` beside the filter. Index 2 of a grid sorted
oldest-first is a different photograph, and the server numbers its rows off the
same fields.

## What the client polls, and what it does not

Two loops, both narrow:

- Tiles whose derivative is not ready yet poll `/v1/timeline/states`, **only for
  the ids currently on screen**. During a backfill the library can be tens of
  thousands of pending items; asking about all of them every few seconds would
  cost more than generating the thumbnails.
- A header chip polls `/health` for queue depth and failures, so a permanently
  failed derivative is noticeable without going to look for it.

New uploads do **not** appear without a reload. Inserting at the head mid-scroll
would shift everything under the cursor, and watching a tile finish is worth the
complexity while watching the top of the list grow is not.

What the store does do is notice. Every fetched item is checked against the
heading its index puts it under, so a page that no longer belongs where the day
table says is rejected and the table refetched — bounded to three attempts. It
catches the case that matters, a page about to be drawn in the wrong place, and
deliberately misses a shift entirely inside one long day, which moves photos
without moving any heading and is not worth a poll to find.

## The viewer

The URL carries `?asset=<id>`, so a photo can be linked and Back closes the
viewer. Stepping between photos *replaces* the history entry rather than pushing
one — otherwise Back would walk through everything just viewed instead of
returning to the grid.

A link is the one thing in the gallery that names a photograph by id rather than
by position, so opening one asks `/v1/timeline/locate` where that position is —
a single count on the server — and pins the page holding it until it arrives.
The photo at index 17,001 of 18,101 opens in about a third of a second on a cold
load, in three requests. Paging towards it, which is what this used to do, would
have been eighty-six.

A link to a photo that is not in the collection being browsed — an album page
handed a library link — answers 404, and the grid is left up with nothing
opened. That is the ordinary case rather than an error, and it is now answered
immediately instead of after paging through the entire album.

Photos load `/preview`, rendered per request from the original, which means the
viewer works even for an asset whose thumbnail job has not run. Videos load the
H.264 rendition, and fall back to a download link for the untouched original
when the transcode is missing or failed.

A Snapchat memory is a photograph and a caption layer drawn over it, archived as
two files. Everything the server serves for one is the two composed, so the
viewer shows the picture that was sent without asking for anything special. What
it adds is a way to look underneath: press and hold on the photo, or the toggle
at the top right (`O`), and the layer comes off.

The gesture is the Live Photo's, deliberately — same delay, same badge, same
place — because it is the same gesture and nobody should have to learn it twice.
Nothing in this archive is both, and the two never collide.

For a still, the photograph is a second `<img>` mounted from the start over the
composite at `opacity: 0`, so the hold costs nothing and the picture is already
in the browser's cache. Video cannot work that way: the caption is in the pixels,
because nothing will composite a PNG over a playing `<video>`, so the toggle
swaps the source to `/playback/plain` and carries the playhead across. That
rendition is built by a second encode and a library that has not finished
backfilling will 404 it, which falls back to the composite rather than a black
rectangle. There is no press-and-hold on video for the same reason: a hold that
costs a download is not a hold.

The toggle stays where it was put as you step between photos. Memories arrive in
runs, and someone who turned the captions off to look at one almost always wants
the next one the same way.

## Selecting, and deleting

A selection is **runs of timeline indices**, not a set of ids. The grid is
addressed by position — the day table fixes a place for every photograph in a
collection before any of them are fetched — so a drag through five thousand
placeholder tiles means exactly the five thousand photographs those squares stand
for, and "everything below here" is one interval rather than a list the browser
would have to page through to learn. `src/lib/ranges.ts` holds that arithmetic,
with no React in it, and is unit-tested directly.

Which is also how an operation is sent. `POST /v1/trash` takes the same runs plus
the filter they were counted in, and the server resolves them in one statement.
Ids go too where the client has them — the tile under a right-click — because
those are exact. Resolving ranges to ids in the browser first would be a second
round trip that widens the race it was meant to close.

Three ways to start one, and all three end in the same place:

- **The context menu** on a tile. A right-click inside a live selection acts on
  the selection; anywhere else it acts on that one tile, and says which — "Delete
  video" rather than "Delete 1 photo".
- **The sheet above the selection pill**, which is mounted by the root layout and
  so cannot see the grid it is reporting on. The grid hands its actions up when it
  mounts (`SelectionActions`), and that is also what decides which actions exist:
  in the library a selection can be deleted, in Recently Deleted the same gesture
  means restore or destroy.
- **Delete or Backspace**, while selection mode is on.

Every delete in a menu or the sheet is an **armed button**: it says "Delete", and
only once it says "Confirm" — filled, destructive — does the next click do
anything. A modal to ask the same question is a third surface and a focus trap
for something the control can ask itself. It disarms after four seconds and when
the surface it lives in closes. The keyboard gets the alert dialog instead,
because a keystroke has no first click to spend.

Afterwards a toast says what happened and offers **Undo** for ten seconds. It
undoes by *batch* — the uuid the server stamped on the rows that operation
touched — because by then the timeline has been reloaded and every position in it
means something else. The toast lives in the root layout for the same reason: the
grid that started the delete may well have been unmounted by the time it shows.

The reload is not an optimisation to skip. A delete moves every index after it,
so the day table the grid was drawn from is describing a timeline that no longer
exists; patching tiles out in place would leave the geometry lying about what is
where. `timeline.retry()` refetches the table and the pages hanging off it.

## Recently Deleted

`/trash` is the same grid, the same viewer, the same zoom and the same paging as
the gallery, over `GET /v1/timeline?trash=1` — one predicate flipped on the
server. It is a *scope*, not a collection: a collection narrows the library, and
this replaces the rule that says what the library is.

That is the whole reason it costs a route and a header rather than a screen. A
separate deleted-items view would be a second grid to keep in step with the
first, and it would be the worse of the two, because nobody looks at it often
enough to notice it rotting.

What differs is only what a selection can do: Restore, and a Delete forever that
is armed like every other destructive control and has no undo behind it. Items
sit here for 365 days; photod purges them on its own.
