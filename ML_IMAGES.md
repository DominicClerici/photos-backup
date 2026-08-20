# ML_IMAGES — search over what the photographs are of

The plan for the search feature: a plain-text box that answers questions about
dates, places, people, objects and vibes, over a library the archive already
holds. This document is the design and the order of work.

It was a plan and is now largely a record: **steps 2 through 8 of §9 are
built** — pgvector, migrations `0016`, `0017` and `0018`, the offline geocoder,
the `mlprep` renditions, photo-ml with all five of its routes, the `vision`,
`ocr` and `describe` passes, the captions, the free-form tags, the `asset_search`
tsvector, the Go query grammar, `GET /v1/search` with reciprocal-rank fusion over
both halves, and the page — a command palette with live results and a ranked
grid with the parse drawn as removable chips. Search answers, and it is reachable
by pressing ⌘K. What is left is step 9, the tag merge, which needs a finished
vocabulary to merge.

The rule from PROJECT.md §4 governs everything below: **photo-ml is optional
forever.** If it is down, mid-model-swap, or saturating the GPU, backups still
complete and photos still display. Search degrades; it does not fail, and it
never takes the boring part that protects the photographs down with it.

---

## 1. What the library actually contains

Measured against the real archive, not the fixture.

| | |
|---|---|
| Assets total | 23,080 — 73 GB |
| Assets the timeline shows | **17,788** (14,970 stills, 2,818 videos) |
| The difference | 5,292 Live Photo halves and Snapchat overlays |
| With a GPS fix | 11,045 (62%) |
| Carrying Google's person labels | 4,576 rows in `asset_people`, ~13 names |
| Carrying face geometry from XMP | 1,493 |
| Free on the derivatives SSD | 768 GB |

The second row is the one that shapes the work. The ML pass is 23% smaller than
the library, and the rule for what to skip is already written down and already
indexed:

```sql
assets_timeline_visible_idx on (sort_time desc, id desc)
  where live_parent_local_id = '' and live_parent_asset_id is null
    and not is_overlay and deleted_at is null and vault = ''
```

Vault, trash, overlays and paired videos fall out of scope for free, and they
fall out for the right reasons rather than as an optimisation. A paired video is
not an item, it is a still's motion. An overlay is not a photograph, it is one
layer of somebody's handwriting over one. Indexing either would put a second
copy of the same moment into every result page.

### The blocker

`postgres:18-alpine` **has no `vector` extension.** `pg_trgm` and `unaccent` are
available; `vector` is not. *(Done — the compose image is now
`pgvector/pgvector:pg18`, which is also the archive machine's Postgres, since it
runs from that same compose file. The swap turned out to be a data operation
rather than an image tag: alpine's musl collates `en_US.utf8` in byte order and
glibc does not, so every text index had to be rebuilt before anything connected.
deploy/README.md § The database has the procedure and the reasoning.)*

---

## 2. Decisions

**Retrieval is hybrid, not one thing.** A query is split: the parts a database
can answer exactly (dates, people, places, media kind, camera, favourites)
become SQL, and the leftover visual phrase becomes a vector search. "Phoenix at
the beach last summer" is two questions with different right answers — the name
and the date range must be exact, and *at the beach* must be fuzzy. A pure
embedding search cannot do arithmetic on "last summer"; a pure keyword search
cannot find a beach nobody wrote the word "beach" about.

```
"phoenix at the beach last summer"
   ↓ parse
 person=Phoenix AND sort_time in [2025-06, 2025-09]
   + vector("at the beach") ⊕ fts("beach")   → RRF → ranked
```

**Faces are deferred.** Person search keeps working off the 4,576 labels Google
already exported. Our own detection and clustering is its own phase, with its
own schema — see §11.

**Quality over indexing speed.** The backfill is allowed to be an overnight run.
It happens once per model, and a model is chosen rarely.

**Place names come from an offline geocoder.** A bundled GeoNames extract, no
network, no per-photo API call, no coordinates leaving the machine. It will not
know "the cabin" until something teaches it; it will know every city on earth on
day one.

**Tags are free-form first, merged later.** The vision model writes whatever
words it wants. Cleanup is a data operation over the accumulated vocabulary once
the whole library has been through — not a fixed list guessed at in advance and
then found to be missing the half of the archive that is Snapchat. §5 describes
what makes the merge non-destructive.

**Web only, this phase.** `expo-app` is a backup client; search follows once the
query API has settled and stopped moving.

---

## 3. The decision that makes the rest easy

**Go decodes. Python does tensors. `photo-ml` never touches `/mnt/photos`.**

The metadata worker already owns HEIC, libheif and ffmpeg. Teaching Python to
decode originals as well buys two decode stacks that will eventually disagree
about Display P3, and a Python process holding read access to the archive.

So the Go worker writes a new derivative — the **ML rendition**: the full frame,
uncropped, longest edge 512, WebP. For video, several of them sampled across the
clip.

```
<sha>.ml.webp             the still, uncropped
<sha>.ml.0.webp … .5      six frames spread across a video
```

*Uncropped* is the point. The existing `.thumb512.webp` is a square centre crop,
which is exactly right for a grid of fixed square cells and exactly wrong for a
vision model: the dog is often at the edge, and a centre crop is a machine
deciding in advance what the photograph was about.

What this buys:

- `photo-ml` receives image bytes over loopback. It holds no state, opens no
  files, and talks to no database. It is the stateless HTTP service PROJECT.md
  §4 already drew.
- The vault is excluded **by construction** rather than by a `WHERE` clause
  somebody can forget to write. This is the same reasoning §7 already applies to
  signatures: computing a description of what a photograph looks like, for
  something in the vault, would be this server writing down the thing the vault
  exists to stop it knowing.
- Swapping the vision model later re-reads 61,000 small WebPs instead of
  re-decoding 73 GB of HEIC and H.265. The difference is minutes against hours,
  and it is what makes trying a second model a decision rather than a project.

Cost: roughly 4 GB of renditions on a disk with 768 GB free.

---

## 4. Schema — migration `0016_search.sql`

*(Built. 0015 was taken by `merge_review`; the search schema is 0016. One
deliberate omission, noted below the SQL: the HNSW index.)*

```sql
create extension vector;

-- One row per frame. A still has frame 0; a video has one per sample.
create table asset_embeddings (
    asset_id  uuid not null references assets (id) on delete cascade,
    frame     int  not null default 0,
    model     text not null,
    embedding halfvec(1152) not null,
    primary key (asset_id, frame, model)
);
create index on asset_embeddings using hnsw (embedding halfvec_cosine_ops)
    where model = '<current>';   -- NOT in 0016; see below

create table asset_descriptions (asset_id uuid, model text, caption text, generated_at timestamptz);
create table asset_ocr          (asset_id uuid, model text, text text,    generated_at timestamptz);

-- Free-form vocabulary, with the merge built in from the start.
create table tags (
    id           bigserial primary key,
    name         text not null unique,
    canonical_id bigint references tags (id)
);
create table asset_tags (asset_id uuid, tag_id bigint, confidence real);

-- One tsvector per asset: caption A, tags A, ocr B, filename and place C.
create table asset_search (asset_id uuid primary key, tsv tsvector);
create index on asset_search using gin (tsv);

alter table assets
    add column place_city    text,
    add column place_admin1  text,
    add column place_country text,
    add column place_source  text,
    add column geocoded_at   timestamptz;
```

Four things about that shape, each of which is the answer to a way this goes
wrong later.

**The HNSW index is deferred to the migration that names a model.** Its
predicate is `where model = '<current>'`, and the model is not chosen until the
bench in §9 step 1 has run. An index built in 0016 would either name a model
nobody has measured or cover every model at once, which is the thing the
predicate exists to avoid. Everything else in that block is in 0016 and is
inert until something writes to it.

*(That migration is now `0017_vision.sql` and the model is
`siglip2-so400m-patch14-384`. The bench was skipped rather than run: §4 had
already pinned the width at 1152, which pins the family, and `model` in the
primary key means measuring a second encoder later is a delete and a fifteen-
minute requeue rather than a project. The one consequence worth knowing is that
a partial index is only reachable from a query repeating its predicate
literally — leave `where model = ...` off a search and Postgres answers the same
rows by sequential scan over sixty thousand vectors, which is correct and slow
enough to look like a bug in the model. `db.VisionModel` is that literal.)*

**`model` is on every row, and it is in the index predicate.** A model swap
becomes `delete where model = <old>` plus a requeue. Never a migration, never a
truncate, and the old and the new can sit in the table together while they are
compared against each other. The bench in §9 depends on this.

**`halfvec`, not `vector`.** fp16 halves the storage and the recall difference
at 61,000 rows of 1152-dimension embeddings is not measurable. `vector` can be
adopted later without touching a query.

**One row per video frame, matched on the best one.** Ranking takes
`max(similarity)` across an asset's frames. A clip that goes from the beach to a
restaurant is findable as both, which averaging into a single vector per video
would make it neither.

**`tags.canonical_id` is the entire free-form-then-merge plan.** The model writes
what it likes into `tags`. Merging "puppy" into "dog" sets one column: no
re-run, no rows destroyed, and reversible by clearing it. Search resolves
through `coalesce(canonical_id, id)`, so a merge takes effect everywhere at once
and a mistaken merge is undone the same way.

---

## 5. Jobs and pools

Three additions to the pipeline, and one of them is not ML at all.

| kind | pool | what it does |
|---|---|---|
| `mlprep` | new **prep** pool (Go, CPU) | writes the ML renditions; samples video frames |
| `vision` | new **vision** pool (Go, GPU via HTTP) | one call to photo-ml; writes embeddings, caption, tags, OCR |
| — | the existing metadata job | reverse-geocodes on ingest |

*(All built, and the `vision` row turned out to be three rows rather than one.
`vision` writes embeddings, `ocr` writes recognised text, `describe` writes the
caption and the tags — three kinds sharing one pool. The reason is the same one
that split `mlprep` off in the first place, one step further along: re-embedding
the library is fifteen minutes and re-captioning it is hours, so a single job
would tie every encoder bench to a full re-captioning. Sharing a pool is right —
it is still one GPU — but the pool now drains its kinds in **priority order**
rather than FIFO, cheapest first, because three passes of wildly different cost
interleaved all finish at the end. See `jobs.ClaimInOrder`.*

*One more thing came out differently and it is the
part of this design that took the most care: the pool is **gated**, not merely
retried. It asks photo-ml whether it is up before it claims anything, so an
absent service means an idle pool rather than sixty thousand failures; and for
the job already in flight when the service is restarted there is `jobs.Defer`,
which returns it to the queue with its **attempt rolled back**. Without that,
`systemctl restart photo-ml` during a backfill would spend five attempts per
asset against a closed socket and mark the library permanently failed. The
`internal/mlclient` boundary is where the two kinds of failure are separated:
5xx and every transport error cost nothing, a 4xx is a bad rendition and burns
attempts as it should. server/README.md § What a photograph shows.)*

`vision` gets its own pool for the reason `signature` got one, and the reason is
stronger here: it is a pass over the whole archive that nothing is waiting for.
Behind the metadata pool it would stall the gallery's thumbnails for hours to
answer a question nobody has typed yet. Concurrency 1–2 — the work is a queue in
front of one GPU, and more claimants would only mean more processes waiting.

`mlprep` is a separate kind from `vision` so that a model swap requeues only
`vision`; the renditions are already on disk and decoding them again would be
the expensive half. It is separate from `metadata` so that getting one new file
per asset does not force a re-run of exiftool and three thumbnail sizes over
23,000 assets — migration 0007 did exactly that, correctly, for a change that
genuinely needed it. This one does not.

**Geocoding rides in the metadata job** for new uploads, plus a
`photobackup geocode` backfill for the 11,045 assets that already exist — a new
case in `cmd/photobackup/main.go` beside `reindex`. It is Go, it is offline (a
GeoNames `cities500` extract plus admin codes, ~30 MB, a k-d tree in memory),
and it is deliberately not ML. Nothing about "photos in Chicago" should be able
to break because a GPU is busy, and nothing about it should have to wait for a
model to be chosen.

*(Built — `internal/geocode`. Two things came out differently from this
sketch. The extract is downloaded into `GEONAMES_DIR` rather than bundled: it is
gitignored, three curls, and photographs simply keep their coordinates and go
without a place name when it is absent. And nearest-centre turned out to be the
wrong rule — it answers the Eiffel Tower with "Paris 16 Passy" — so a place
covers a radius derived from its population and the largest place reaching the
photograph wins. server/README.md § Place names has the trade-offs. The whole
11,045-asset backfill runs in under a second.)*

---

## 6. photo-ml — the service

A systemd unit beside `photod.service`, a `uv`-managed venv, `User=photo-ml`
with **no read access to `/mnt/photos`**. That last part is not belt-and-braces,
it is the enforcement of §3: if the service cannot open the archive, no future
change to it can quietly start reading the vault.

```
POST /embed     images[] → vectors[]           vision encoder, 1152-d   built
                texts[]  → vectors[]           the same space           built
POST /describe  images[] → [{caption, tags[]}] VLM, free-form tags      built
POST /ocr       images[] → [{text, lines[]}]   dedicated text recognition  built
POST /parse     query    → filter JSON         small instruct model     built
GET  /health             → which models are resident                    built
```

*(Every route takes a list and answers per item, which the sketch had as
singular for the middle two. The service holds no state and knows nothing about
assets, so folding three frames of a clip into one video's caption is a decision
for the process that knows what a video is — and the list is also what lets the
captioner batch, which turned out to matter more than anything else in this
file. See below.)*

*(`texts[]` was not in the original sketch and had to be: a query becomes a
vector before it becomes a search, and SigLIP's two towers are one model and one
residency, so splitting them into two routes would have been two names for one
thing. Images arrive base64 in JSON rather than multipart — a third more bytes
over a loopback socket that is not the bottleneck, in exchange for every
endpoint being exercisable with curl and a here-document, which for the piece of
this system that gets debugged at 1am mid-backfill is the better trade.)*

`/parse` lives here rather than in Go because it is the only other thing in the
system that wants a GPU and a tokenizer. Putting it here means `photod` has
exactly one ML dependency to lose, and losing it degrades one code path instead
of two.

### VRAM

16.3 GB on the RTX 5060 Ti, of which the desktop session is already holding
about 1.4 GB.

| model | planned | actual | residency |
|---|---|---|---|
| vision encoder, SigLIP-2 so400m fp16 | 1.8 GB | **2.3 GB** | resident |
| query parser | 4B at 4-bit, 3 GB | **Qwen3-0.6B bf16, 1.2 GB** | resident |
| captioner | 7–8B at 4-bit, 6 GB | **Qwen3-VL-4B bf16, 9.1–10.1 GB** | on demand |
| OCR (ONNX) | 0.5 GB | **CPU, no VRAM at all** | on demand |

*(Two of those moved and both for the same reason: the card is Blackwell, and
the AWQ/GPTQ kernels are the part of that ecosystem most likely to be missing an
architecture this new. The failure is a service that installs, starts, and then
reports "no kernel image is available" on the first forward pass, which is
precisely the 1am debugging session this design spends paragraphs avoiding. 4B
at bf16 costs what 8B at 4-bit would have and needs no extra dependency.*

*The parser shrank to pay for it. 0.6B is small enough to be honest about: the
gates in §7 reject nearly everything it says, and what survives is occasionally
useful. That is the arrangement §11 asks for rather than a disappointment — but
it is worth writing down that the Go grammar is doing essentially all of the
work, and that `PHOTO_ML_PARSER_MODEL` is there for the day the card is not also
holding a captioner.*

*OCR on the CPU was a straight win nobody planned: the pass costs the card
nothing and can run while the captioner is loaded.)*

The two heavy ones load when a `vision` job arrives and unload once the queue
has been dry for a few minutes.

*(The mechanism is `photo_ml/residency.py` and it is built, with only the
encoder registered against it so far. Two details in it turned out to matter
more than the design did. A model in use is never a reap candidate, because the
reaper runs on its own thread and unloading weights another thread is
mid-forward-pass on is a segfault rather than an error. And unloading calls
`torch.cuda.empty_cache()` rather than dropping the last reference — dropping it
returns the blocks to torch's caching allocator, which keeps them reserved
against the driver, so `nvidia-smi` goes on showing 6GB held and NVENC goes on
failing to allocate. That one line is the whole of what §11 is watching for.)* Interactive search then costs about 5 GB
steady-state while the desktop is in use, and the overnight backfill gets the
whole card.

This matters because the archive machine runs `VIDEO_ENCODER=h264_nvenc`. NVENC
is a separate silicon block, so it will not fight the models for SMs — but it
does want VRAM, and the CUDA-side decode in front of it will slow while a
backfill runs. Transcodes getting slower during an ML backfill is acceptable;
transcodes failing to allocate is not, which is what the on-demand unloading is
protecting.

**The model names above are candidates, not commitments.** §9 step 1 is a bench,
because "best quality" is a claim to be measured on this library rather than a
spec sheet to be read.

---

## 7. The query path

```
GET /v1/search?q=phoenix at the beach last summer
```

1. `photo-ml /parse` returns structured JSON:
   `{people: ["Phoenix"], after: "2025-06-01", before: "2025-09-30",
     visual: "at the beach", kind: null}`
2. The structured half becomes a `TimelineFilter`. It already carries `Person`,
   `Kind`, `Favorites`, `AlbumID` and `Category`; it gains a date range, a
   place, and a tag. No second query engine, and every existing index still
   applies.
3. The visual half becomes one text embedding → HNSW over `asset_embeddings`,
   and one `websearch_to_tsquery` over `asset_search`.
4. **Reciprocal-rank fusion** over the two rankings, inside the structured
   `WHERE`. RRF rather than a weighted sum because cosine similarity and
   `ts_rank` are not on comparable scales, and tuning a weight between them is a
   job with no natural end.

**The response echoes the parse back.** This is where the UX is won: the existing
`FilterPill` renders `Phoenix ×` and `Jun–Sep 2025 ×` as removable chips, so a
wrong parse is visible and fixed with one click rather than by retyping the
sentence and hoping. A search is an editable filter, not an oracle.

*(Built, and step 1 came out inverted — which is the change in this document that
matters most. §11 says the parser is the weakest link and the last step; it was
built **first**, in Go, and the model was layered on top of it afterwards rather
than the other way round.*

*`internal/searchquery` is the parser: a deterministic grammar that reads dates,
matches names and places against what this archive actually contains, and hands
the rest to the encoder. photo-ml's `/parse` runs after it and may speak only
where the grammar was silent — and every claim is checked against the query
itself before it is believed. A person only if the query mentions a word of that
name; a place only if the query mentions it; a date range only if the query says
anything temporal at all, and only both ends together. Media kind, category and
favourites are not on the model's contract, because the grammar answers those
completely and a model can only disagree.*

*That last rule was not theoretical. A 0.6B model handed the archive's eleven
people as a spelling hint returns **the whole list on every query**, and every
name on it passes a naive vocabulary check because they are all real people
here. Six ANDed people is a filter that matches nothing — §11's silent exclusion
arriving through the front door on day one. `searchquery.mentions` is what stops
it, and it is also what makes the model useful when it is: "chris" earns
"Chris Morrison" because the word is there.*

*One thing was added that the sketch does not have. When the fuzzy half produces
no candidates but the structured half narrowed something, the filter's own
answer stands in date order — because "Phoenix, last summer, and no caption
mentions a beach" should not be an empty grid. When the filter narrowed nothing,
an unmatched phrase is genuinely no results. server/README.md § Searching has
the whole path.)*

### When photo-ml is down

`/v1/search` still answers. A small Go date-and-name grammar covers the common
phrasings, and full-text search over captions, tags, OCR, filenames and place
names is already sitting in Postgres from the last backfill. What is lost is
fuzzy visual recall — the ability to find a beach nobody named. What is not lost
is the search box, the timeline, the uploads, or any photograph.

---

## 8. Web

`web/src/app/search/page.tsx` currently renders the word "Search". It becomes a
query box, the parsed-query pills, and a **relevance-ordered grid** — the
existing tile components without the day headings, because ranking is the
answer to the question that was asked and chronology is not.

Each tile can say why it matched: the matched tag, or the OCR line with the
match highlighted. With free-form tags this is not a nicety. Being able to see
what the model called a photograph is what makes the cleanup pass in step 9
possible at all.

*(Built, and it came out as two surfaces rather than one. The query box is a
**command palette** — ⌘K from anywhere, or the Search tab, which stopped being a
link — showing six live results on a 300ms debounce with "Search for …" at the
top of the list. A search is a question rather than a destination, and an empty
results page somebody has to walk to before typing is one screen between the
thought and the answer. It is a palette rather than a search box because of what
this file already says is coming: a sentence will name an action as well as a
subject, and that should be a group in a list rather than a second surface.*

*The grid is the gallery's own, which took two props rather than a rewrite:
`lib/layout.headless` already drew a run with no date as a flat wall of tiles —
it was written for "order by length" and turns out to be exactly what a ranking
is — and `ranked` also stops the grid claiming the floating sort-and-filter
pill, because an order nobody chose is not one they can change.*

*The pills are chips and the mechanism behind them is the thing worth writing
down. The URL **is** the request: `?q=` asks the server to read the sentence, and
taking a chip off rewrites the URL into `parse=0` beside the fields that
survived. §7's escape hatch needed one correction to carry it — `visual` is now
read by presence rather than by content, so "phoenix", all of which is a name,
can say it has no phrase for the encoder rather than falling back to the word
the filter already answered exactly. Back is then an undo, and a search is a
link.*

*One thing had to be built that this section does not mention. Every grid in the
app names a selection by position, and a ranking has no positions anything else
can reconstruct — so `useSearchActions` spells them out into ids before they
leave, and travels with no filter and no view, because both exist to make a
position mean something. The evidence per tile is in the palette rather than on
the grid: caption, tags, and the OCR line with the match marked, which is what
step 9 will be read through.)*

*(And one surface after that, which this section did not anticipate at all: the
**viewer's details panel**, which is a search read backwards.
`GET /v1/assets/{id}/analysis` takes a photograph and returns the words the
ranking was built out of — caption, tags with the merge resolved and the model's
own word beside it, the whole of the recognised text, and how many frames the
encoder wrote. Its own route rather than more fields on the detail, because that
load is on every arrow-key press and recognised text is unbounded.*

*Two things in it are worth writing down here rather than in web/README.md. It
carries the **state of each ML job**, because §11's warning has a second reading
one surface out: a photograph with no caption is one the captioner has not
reached, one it failed on, or one nothing has queued, and a panel drawing all
three as an empty box reports them as the same silence. And it keeps §11's seam
visible where somebody can actually see it — a name an import confirmed and a
word a model wrote are drawn as different chips with the source named, because
the panel is exactly where the two would otherwise become one list of things
"in" the photograph. Tags and names are clickable and search the archive; in the
vault nothing is, since a hidden photograph's name in `?q=` is that name in the
URL, the history and the recent-search list.)*

---

## 9. Order of work

Each of steps 3, 5 and 6 ships something searchable on its own.

1. **Bench first, on this library.** Pull a 500-asset stratified sample,
   hand-write ~35 queries with known answers — "the receipt from the ski trip",
   "Phoenix in the snow", "that blue error screenshot" — and score recall@20
   across 2–3 vision encoders and 2 captioners. Half a day, and it turns "best
   quality" into a measurement. Nothing downstream changes if the winner does.
2. ~~Swap the image to `pgvector/pgvector:pg18`; write and run migration
   0016.~~ **Done.**
3. ~~**Geocoder.** GeoNames loader, `photobackup geocode` backfill,
   metadata-job hook.~~ **Done** — "photos in Chicago" works with no GPU in the
   picture.
4. ~~**`mlprep`** job, pool, and the ML renditions.~~ **Done** — a fourth
   worker pool sized by `PREP_CONCURRENCY`, writing `<sha>.ml.webp` and
   `<sha>.ml.0.webp`…`.5`. Backfill ≈ 1–1.5 h, CPU-bound, and it is queued by
   the worker's reconcile rather than by the migration.
5. ~~**`photo-ml` with `/embed` and `/health` only**, plus the `vision` job
   writing embeddings.~~ **Done** — a uv project at `photo-ml/`, a fifth worker
   pool sized by `VISION_CONCURRENCY`, and `deploy/photo-ml.service` running as
   a user that cannot see `/mnt/photos`. Three things came out differently from
   this sketch, each noted where it belongs: `/embed` takes `texts[]` as well as
   `images[]`, because a query has to become a vector and the two towers are one
   model and one residency; the encoder is `siglip2-so400m-patch14-384`,
   committed to rather than benched, since §4 had already pinned the width at
   1152 and `model` in the primary key makes a later bench a delete and a
   requeue; and the pool is gated on the service being up rather than retried
   against it. §6 and §5 have the details.

   Backfilled in **16m04s** over 17,792 assets, and the first thing asked of it
   — `snow` — came back with Christmas Day 2019 in Tahoe Vista and March 2025 in
   Breckenridge, from pixels alone: the encoder has never seen a place name.
   `photo-ml/README.md` and server/README.md § What a photograph shows have the
   query, which is two commands until step 7 gives it an endpoint.
6. ~~**`/describe` and `/ocr`**, the tags and captions tables, the `asset_search`
   tsvector.~~ **Done** — migration `0018_describe.sql`, two more job kinds
   sharing the vision pool in priority order, `Qwen3-VL-4B-Instruct` for the
   captions and rapidocr on the CPU for the text. Three things came out
   differently. The captioner is bf16 rather than 4-bit, because Blackwell and
   quantisation kernels are a bad bet; the backfill is a typed command rather
   than a reconcile, because hours of GPU begun by a service restart is a
   restart with a surprise in it; and photo-ml learned to **batch**, which was
   the difference between 7.3 hours and 2.4 over this library and is the single
   most consequential thing measured in this whole feature. §10 has the numbers.
7. ~~**`/parse`**, `GET /v1/search`, RRF ranking, and the degraded path.~~
   **Done**, and built in the opposite order to the one written here: the
   degraded path first. §7 has why, and why the model ended up being the small
   part.
8. ~~The search page and the pills.~~ **Done** — a command palette on ⌘K with
   live results, and `/search` as a ranked grid whose chips edit the parse
   through `parse=0`. §8 has what came out differently, and §11's paragraph
   about a parse that fails confidently is now answered by something somebody
   can click.
9. The tag-merge UI, run once over the finished vocabulary.

### The tag merge, step 9

Expect 3,000–6,000 distinct strings out of 15,000 stills. That long tail is the
deliberate cost of not guessing a vocabulary in advance.

What keeps it to an evening rather than a week: the text encoder from step 5 is
already resident, so the merge UI embeds the **tag names**, clusters them, and
proposes merges — "dog / puppy / doggo / a dog", accept — instead of presenting
a scrollable list of four thousand words. Accepting writes `canonical_id`, which
is one column and is reversible.

---

## 10. Backfill, end to end

| step | work | wall clock |
|---|---|---|
| geocode | 11,045 fixes against an in-memory k-d tree | seconds — measured |
| mlprep | decode 17,792 originals, sample the videos | ~1 h — measured |
| embed | 31,422 renditions through the vision encoder | **16m04s — measured** |
| ocr | 31,422 renditions, on the CPU | ~30 min |
| describe | ~23,400 images through the VLM | **2.4–7.3 h, and it depends** |

*(The embed row is now a measurement rather than an estimate, and the estimate
was wrong in the useful direction. 61,000 renditions assumed six frames from
every video in the library; the pass actually sends 31,422 — 14,970 stills at
one frame each and 16,452 frames from the videos the timeline shows, the rest
having fallen out as Live Photo halves and overlays before mlprep ever ran. It
held steady at about 1,300 assets a minute on the RTX 5060 Ti, at 2.6GB of VRAM
and 69% utilisation, while photod went on serving the gallery.*

*80 videos came out with no vector: clips mlprep could not sample, which have no
renditions for the encoder to look at. That is the tolerance clipRenditions
applies reaching the far end of the pipe intact rather than turning into 80
photographs marked permanently broken — and it is the case jobs.ReconcileVision
notes it will re-offer on every restart, at a claim and a stat each.)*

*The describe row is a range rather than a number because it is the one figure
in this table that is a choice. The captioner is memory-bandwidth bound — 8.8GB
of weights read per generated token, whether that token is for one image or
twelve — so, measured on this card over these renditions:*

| batch | per image | over the library | peak VRAM |
|---|---|---|---|
| 1 | 1.47 s | 7.3 h | 9.1 GB |
| 4 | 0.78 s | 3.9 h | 9.6 GB |
| 8 | 0.48 s | 2.4 h | 10.1 GB |
| 16 | 0.38 s | 1.9 h | 11.2 GB |

*Threads buy nothing: four concurrent single-image calls take exactly four times
as long as one, because they serialise on the same kernels. Only a real batch
does. So the requests have to meet somewhere, and the only place they can is
inside photo-ml — a job is a claim on one photograph and must stay that way, or
a lease and a failure would be shared between eight unrelated files. One
collector thread and a 30ms window is the whole mechanism.*

*Which retroactively makes `VISION_CONCURRENCY` a throughput knob rather than
the no-op the original design correctly said it was. It is 4 by default and 8 is
worth setting for an overnight run. 16 is not: the peak leaves too little for
NVENC, which is the number §11 says to watch.)*

One overnight run, once per model generation. Re-running everything except
`mlprep` after a model swap is under five hours; re-running `embed` alone is
fifteen minutes.

---

## 11. What to watch

**The parser is the weakest link and it is the last step.** Vector search either
works or is boringly mediocre. A query parser fails *confidently*: it silently
filters out the right answer because it decided "last summer" meant 2024, and
the user sees an empty grid with no evidence of why. The pills in §7 are the
mitigation and they are not optional — a wrong parse has to be visible and
removable, or the search will be trusted exactly once.

*(Acted on, by inverting the order: the grammar was built first and the model
second, and the model may only speak where the grammar was silent and only in
words the query contains. This paragraph earned its place within an hour of the
parser existing — see §7 for the hint-list failure, which was exactly this,
found because the paragraph said to look for it. The pills are now built, and
the × works: a chip removed rewrites the URL into the explicit spelling and asks
again, so a wrong reading costs one click rather than a retyped sentence. The
first thing it caught was its own: "snow in december 2025" reads the date
correctly and returns 79, and taking the date off returns 388 — because only
500 photographs have been through the captioner so far, so a narrow date range
is mostly ranking a filter's leftovers. Which is visible now, and was not.)*

**Free-form tags are a bet on cleanup happening.** `canonical_id` and tag-name
clustering are what make that bet safe. If step 9 slips, search still works
through the embeddings and FTS; the tag browser is what stays messy.

**Faces are deferred, but the seam matters now.** `asset_people` must not become
entangled with `tags`. They are different kinds of claim: one is a name a person
confirmed, the other is a word a model produced. When our own clustering does
arrive, those 4,576 Google labels are the signal that names the clusters
automatically — which only works if they are still a clean, separate table.

**The GPU is shared with the encoder and with the desktop.** The on-demand model
lifecycle in §6 is the mechanism; the number to watch after step 5 is whether
NVENC transcodes ever fail to allocate during a backfill. If they do, the
vision pool needs a pause on transcode pressure rather than a bigger card.

*(Still the number to watch, and now with an actual budget behind it: 2.3GB of
encoder and 1.2 of parser resident, 10.1 more while the captioner is loaded, and
1.4 for the desktop — about 15 of 16.3 at the peak of a captioning pass. The
first lever if NVENC starts failing is `PHOTO_ML_DESCRIBE_BATCH`, 8 → 4, which
gives back half a gigabyte and costs about ninety minutes over the library. The
second is the pause this paragraph asks for, and it is still not built.)*

**Nothing rebuilds the tsvector by itself.** `asset_search` is written by the
describe and OCR jobs, and by nothing else — so a tag merge, a re-geocode, or a
changed recipe in `rebuild_asset_search` leaves every row already written out of
date. `photobackup ml reindex` is the answer and it takes seconds, but it has to
be *remembered*, which is the same shape of bet as the free-form tags above and
is worth naming as one.
