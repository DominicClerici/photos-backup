# web

The gallery: a virtualized timeline of the whole archive and a full-size viewer.
Next.js App Router, rendered entirely on the client.

## Running

photod is the front door. Start it, and open **its** address — not this app's.

```sh
cd ../server && go run ./cmd/photod    # HTTPS on :8787
pnpm install
pnpm dev                               # 127.0.0.1:3000, reached only through photod
```

| variable | default | meaning |
|---|---|---|
| `NEXT_PUBLIC_MEDIA_BASE` | unset — same origin | origin for thumbnails, previews, and video |

There is no `/api/*` rewrite any more, and no `PHOTOD_URL`. Until recently this
app proxied to a plaintext photod listener on `127.0.0.1:8788` that served the
whole archive — reads, the trash, the vault, the upload page — with no
credential at all, safe only because it was loopback. That listener is gone.

photod now terminates TLS, authenticates every request against a passkey session
or a device token, serves `/v1` and the media itself, and reverse-proxies this
process for everything else. So the bundle, the JSON and the thumbnails all
arrive from one origin under one cookie, which is what makes the session cover a
thumbnail at all: a browser attaches a same-origin cookie to an `<img>` and will
not attach a bearer header to one.

What that means day to day:

- **Open photod's address, not `:3000`.** This app's own port serves a gallery
  with no API behind it. Hot reload still works through the proxy.
- **Sign in first.** photod serves the sign-in page itself, so an
  unauthenticated browser never receives this bundle. On a fresh archive, run
  `photobackup passkey add` for an enrollment code.
- **`WEB_ORIGIN` must match the address you open.** A passkey is bound to its
  origin. For local work that means `WEB_ORIGIN=https://localhost:8787`.

`NEXT_PUBLIC_MEDIA_BASE` is the one setting left, and it is now rarely useful:
media is same-origin by default because photod serves it directly, with no Node
hop to skip.

`NEXT_PUBLIC_MEDIA_BASE` is read at **build** time, not at start time — it is
inlined into the bundle by `next build`, so setting it in front of `pnpm start`
does nothing. Set it for the build.

```sh
pnpm test     # layout arithmetic, via node --test
pnpm lint     # tsc --noEmit
pnpm build
```

## One origin, no CORS

There are two processes and one origin. photod terminates TLS and reverse-
proxies this app, so the bundle, the JSON and the media all come from the same
place: no preflight, no allowed-origin list, and nothing for a cookie to fail to
cross.

That last part is the whole reason for the arrangement rather than a bonus. The
credential a browser can actually carry onto a subresource is a cookie — it
attaches a same-origin cookie to an `<img>` and will not attach a bearer header
to one — so any layout that puts the thumbnails somewhere other than the app has
to solve authentication a second time, with signed URLs or a second credential.
Phase 12 established this the hard way and PROJECT.md records it.

`NEXT_PUBLIC_MEDIA_BASE` survives from the arrangement before this one, where
media could skip a Node proxy hop by pointing straight at photod. There is no
hop to skip now — photod serves the media itself — so it is left unset, and
anything it is pointed at has to be an origin the session cookie reaches.

Next's `<Image>` is deliberately unused. It exists to resize arbitrary source
images at the edge; photod already stores a thumbnail at each size the grid
asks for, and routing it through the optimizer would add a hop to re-encode
something that is already a square WebP of the right dimensions.

## Sections

`/` is the gallery; `/collections`, `/status`, `/search` and `/other` are routes
of their own. Search is the one tab that is not a link: it opens the command
palette in place, and the route behind it is where that palette sends a question
it has finished asking. See Searching. `/status` is the server's dashboard — how much is archived, how
much of the drive is left, what the queue is doing, and every failed job with
its error and a button that copies the details as Markdown. The bar that
switches between them is
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

Three loops, all narrow:

- Tiles whose derivative is not ready yet poll `/v1/timeline/states`, **only for
  the ids currently on screen**. During a backfill the library can be tens of
  thousands of pending items; asking about all of them every few seconds would
  cost more than generating the thumbnails.
- A header chip polls `/health` for queue depth and failures, so a permanently
  failed derivative is noticeable without going to look for it.
- The status page polls `/v1/status` every ten seconds, and stops while the tab
  is in the background. One request carries the library counts, the disk, the
  queue and the failure list, because they are all claims about the same instant
  and a page assembled from four of them can contradict itself.

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

### What the panel knows

`I`, or the button at the top right, opens the details panel — a sidebar on a
wide screen, a bottom sheet under 700px. It leads with what the models said
about the photograph and ends with what the camera recorded, because the first
is what the picture is *of* and the second is how it was taken.

The ML half is `/v1/assets/{id}/analysis`, fetched on its own and only while the
panel is open: the detail above it is on every arrow-key press, and recognised
text is unbounded — a photograph of a terminal is kilobytes of it, which nobody
stepping through the grid with the panel shut has asked to download.

It draws the caption, the tags in the captioner's own confidence order, the
people, anything a person typed under the photograph at import, and the whole of
the recognised text in a scrolling block. A tag or a name is a button: clicking
one asks the archive for everything else it was called that. A search rather
than a filter on the current view, because "what else did it call a beach" is a
question about the archive and not a narrowing of the album that happens to be
open.

**Nothing here is drawn as a blank.** A photograph with no caption is one the
captioner has not reached, one it failed on, or one nothing has ever queued, and
the panel says which — a sentence per pass, and none at all for a photograph
that has been all the way through. That is the parser warning in `ML_IMAGES.md`
§11 one surface further out: a model that silently says nothing looks exactly
like a photograph with nothing in it, and the two are not the same news.

Two things the panel deliberately keeps apart. A tag carries an asterisk when a
merge has folded it into another word, and the tooltip says what was actually
written — the vocabulary cleanup is a data operation over thousands of strings
and has to be reviewable from the photographs it changed. And **people are not
tags**: a name somebody confirmed at import and a word a model produced are
different kinds of claim, drawn in different chips with the source spelled out,
because the day face clustering arrives those confirmed names are what will name
the clusters.

In the vault the panel draws the same fields from the sealed document, and
nothing in it is clickable. A hidden photograph's names came out of the vault,
and typing one into `/v1/search` would put it in the URL, in the browser's
history, and in the list of recent searches this app keeps.

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

## Searching

Two surfaces, one endpoint. The palette is where a question is asked; `/search`
is where the whole answer lives.

`CommandPalette` is mounted once by the root layout and opened from anywhere —
`⌘K`/`ctrl-K`, or the Search tab, which is a button rather than a link. A search
is a question rather than a destination, and sending somebody to an empty
results page in order to type into it is one screen between the thought and the
answer. The route still exists and is still what the tab highlights for; it is
where the palette hands the ranking over to.

It is a *palette* rather than a search box because of what comes next: a typed
sentence will eventually name an action as well as a subject, and the difference
between the two should be a group in the list rather than a second surface.
Nothing in it filters locally — `shouldFilter` is off, because the list is an
answer the server ranked and cmdk's fuzzy match over six captions would be a
second, worse opinion drawn on top of a fused one.

Typing waits 300ms before asking. A search here is not a filter over a local
list: it is a parse, a text encoding on the GPU, and a fused scan of two
rankings. The previous answer stays on screen, dimmed, while the next one is in
flight, so the palette never flashes empty between keystrokes. An emptied box
clears instantly — there is nothing to wait for, and that is what makes
backspace feel like an undo.

### The URL is the request

`/search` reads its query straight out of the URL and `lib/search.requestOf`
passes on only the parameters `/v1/search` reads — `asset`, which is the open
photograph, is the page's own business. So a search is linkable, and Back is an
undo.

`?q=` alone asks the server to read the sentence. Taking a chip off rewrites the
URL into the explicit spelling — `parse=0` beside the fields that survived —
because a parse can be merged on top of but never subtracted from. That is the
whole of `lib/search`: `explicitParams` materialises a reading into parameters,
`withoutChip` leaves one out, and both are pure and unit-tested. A date is one
chip and comes off at both ends; the phrase is emptied rather than dropped,
since an absent `visual` means "fall back to what was typed". See server/README
§ The response echoes the parse.

### The grid is the gallery's

`Timeline` draws it, with `ranked` set. The day table is one run with no date,
which `lib/layout.headless` already renders as a flat wall of tiles with no
headings and no room reserved for them — relevance is the answer to the question
that was asked, and chronology is not.

`ranked` also stops the grid claiming the floating sort-and-filter pill. There is
no order to choose here and the filter is the query; a pill offering "Newest"
over a relevance ranking would either do nothing or throw the answer away. The
chips are the filter and they are on the page.

`useSearch` is `useTimeline`'s contract over an offset-paged ranking. Three
things differ, and each is a property of ranking rather than an omission. It
never uses a cursor, because this order's sort key is computed from the query
and exists on no row. The first page settles the question — it comes back with
the server's reading, and every later page is asked for in that reading's own
terms, so the parser runs once per search rather than once per page and page
four cannot disagree with page one. And there is no resync: a photograph
uploaded mid-search does not shift a ranking it has no place in.

### Selecting in a ranking

Every other grid names a selection by position, because the day table gives
every photograph a place before any of them are downloaded and "everything below
here" is one interval rather than forty thousand identifiers. A ranking has no
such table, so a range means nothing off the page — index 2 of "phoenix at the
beach" is index 2 of nothing the server can reconstruct.

`useSearchActions` wraps the library's own actions and spells the positions out
into ids first, fetching whatever pages the selection covers. It travels with no
filter and no view, and the absence is the point: both exist to make a position
mean something, and by the time any of this reaches the server there are no
positions left in it. `SelectionActions.resolve` is the same step for the one
request the grid builds rather than spends — the create-album dialog.

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

## Cleaning up what the captioner wrote

`/tags`, reached from a card on the status page. Four lists, and they are two
pairs rather than four things: **Words** and **Junk** are the triage, and
**Suggestions** and **Merged** are the merge. They are in that order because the
first changes the answer to the second — clustering three thousand words when a
third of them are interface text off a screenshot is more work and a worse
result. See server/README.md § Cleaning up the vocabulary for why.

The two review lists are a **wall of chips**, in use order, and the one thing you
can do to any of them is move it to the other list. Rows would fit forty on a
screen and would suggest each word deserves a decision; three thousand words read
at a glance is the actual job. A chip's outline is the whole of ML_IMAGES.md
§11's seam made visible: **solid means a person decided it, dashed means it is
the captioner's opinion and nobody has confirmed it**. Without that, the two
lists read as facts about the archive from the moment a pass finishes, which is
the confident-and-invisible failure that paragraph is about.

Hovering a chip shows what the word is actually on. That is not a nicety —
"casual" reads as a plausible tag until you see four unrelated photographs under
it, and "screenshot" reads as junk until you see it is on a hundred and seventy.
On the suggestion cards the photographs are inline instead, because that is the
card where somebody is deciding, and three thumbnails per word are what separate
"doggo means dog" from "doggo is what this model calls a wolf".

**The passes have a progress bar because they are a loop, not a request.** The
server judges 120 words or embeds 512 per call and says how many are left; the
page calls again until nothing is. So there is a real number to show, and showing
it is what makes two minutes legible rather than a frozen button. Navigating away
stops the loop and loses nothing.

A suggestion card offers **two ways of disagreeing**, and they are different. A
member can be wrong while the group is right — "mountain, mountains, and no, not
mountaineering" — which is an untick, not a rejection. Both are sent with the
merge, because a disagreement nobody wrote down is proposed again on the next
run. The head is preselected as the most-used word, for the reason the duplicate
review preselects its keeper, and it is a radio rather than a fact: that rule is
right most of the time and wrong in the interesting cases.

The similarity slider stops at 0.85 rather than 0. SigLIP-2's text tower puts the
median pair of unrelated tags at 0.73, so everything below that is not a looser
setting, it is a broken one — 0.80 proposes "man ← woman". It is a live control
at all because the vectors are stored: dragging it is one query, not a
re-embedding.

Nothing on the page destroys anything, and every button has an opposite. `junk`
and `canonical_id` are one column each, read wherever a search resolves a word.
