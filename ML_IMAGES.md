# ML_IMAGES — search over what the photographs are of

The plan for the search feature: a plain-text box that answers questions about
dates, places, people, objects and vibes, over a library the archive already
holds. This document is the design and the order of work.

It was a plan and is now partly a record: **steps 2, 3, 4 and 5 of §9 are
built** — pgvector, migrations `0016_search.sql` and `0017_vision.sql`, the
offline geocoder, the `mlprep` job that writes the renditions a model reads, and
photo-ml itself with the `vision` pool feeding it. Semantic search is real; it
just has no endpoint yet. Everything from step 6 onward — captions, tags, OCR,
the query parser and the page — is still design.

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

*(Both built. `vision` writes embeddings only — the caption, tags and OCR
columns of that row are step 6. One thing came out differently and it is the
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
POST /describe  image    → {caption, tags[]}   VLM, free-form tags      step 6
POST /ocr       image    → {text, boxes[]}     dedicated text recognition  step 6
POST /parse     query    → filter JSON         small instruct model     step 7
GET  /health             → which models are resident                    built
```

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

| model | roughly | residency |
|---|---|---|
| vision encoder (SigLIP-2 so400m class), fp16 | 1.8 GB | resident |
| query parser (4B class, 4-bit) | 3 GB | resident |
| captioner (7–8B VLM, AWQ/GPTQ 4-bit) | 6 GB + KV | on demand |
| OCR (ONNX) | 0.5 GB | on demand |

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
6. **`/describe` and `/ocr`**, the tags and captions tables, the `asset_search`
   tsvector. Overnight backfill ≈ 2–4 h.
7. **`/parse`**, `GET /v1/search`, RRF ranking, and the degraded path.
8. The search page and the pills.
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
| ocr | 14,970 stills | 10–15 min |
| describe | ~23,000 images through the VLM | 2–4 h |

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
