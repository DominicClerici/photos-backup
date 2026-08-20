-- +goose Up
-- The schema search is built on: what a photograph looks like, what a model
-- said about it, and where it was taken.
--
-- All of it is inert on the day it lands. Nothing writes to these tables until
-- the vision pass exists, and the place columns stay null until the geocoder
-- fills them — which is deliberate. This is the one migration in the feature
-- that has to run against a live archive, so it runs first and on its own,
-- while the only thing that can go wrong with it is a missing extension.
--
-- `vector` is that missing extension, and it is why the compose image changed:
-- postgres:18-alpine does not ship it. See docker-compose.yml and the swap
-- procedure in deploy/README.md.
create extension if not exists vector;

-- One row per frame. A still has frame 0; a video has one per sampled frame,
-- which is what lets a clip that goes from the beach to a restaurant be found
-- as both. Ranking takes max(similarity) across an asset's frames — averaging
-- them into one vector per video would make it neither.
--
-- `model` is part of the key rather than a column beside it, so a model swap is
-- `delete where model = <old>` plus a requeue: never a migration, never a
-- truncate, and the old and the new can sit here together while somebody
-- measures one against the other.
--
-- halfvec rather than vector: fp16 halves the storage, and at 61,000 rows of
-- 1152 dimensions the recall difference is not measurable. Widening to vector
-- later does not change a query.
--
-- The HNSW index is deliberately absent. Its predicate is `where model = '...'`
-- and the model is not chosen until the bench in ML_IMAGES.md §9 step 1 has
-- run, so it belongs to the migration that names one. An index built now would
-- either name a model nobody has measured or cover every model at once, which
-- is the thing the predicate exists to avoid.
create table asset_embeddings (
    asset_id  uuid    not null references assets (id) on delete cascade,
    frame     int     not null default 0,
    model     text    not null,
    embedding halfvec(1152) not null,
    primary key (asset_id, frame, model)
);

-- What the captioner and the text recogniser produced. Keyed by model for the
-- same reason the embeddings are: two of them can coexist while being compared,
-- and dropping one is a delete rather than a schema change.
create table asset_descriptions (
    asset_id     uuid        not null references assets (id) on delete cascade,
    model        text        not null,
    caption      text        not null,
    generated_at timestamptz not null default now(),
    primary key (asset_id, model)
);

create table asset_ocr (
    asset_id     uuid        not null references assets (id) on delete cascade,
    model        text        not null,
    text         text        not null,
    generated_at timestamptz not null default now(),
    primary key (asset_id, model)
);

-- Free-form vocabulary, with the merge built in from the start.
--
-- The vision model writes whatever words it wants — expect three to six
-- thousand distinct strings out of fifteen thousand stills, which is the
-- deliberate cost of not guessing a vocabulary in advance and then finding it
-- missing the half of the archive that is Snapchat.
--
-- canonical_id is the whole of the cleanup plan. Merging "puppy" into "dog"
-- sets one column: no re-run, no rows destroyed, and reversible by clearing it.
-- Search resolves through coalesce(canonical_id, id), so a merge takes effect
-- everywhere at once and a mistaken one is undone the same way.
create table tags (
    id           bigserial primary key,
    name         text not null unique,
    canonical_id bigint references tags (id)
);

create table asset_tags (
    asset_id   uuid   not null references assets (id) on delete cascade,
    tag_id     bigint not null references tags (id) on delete cascade,
    confidence real,
    primary key (asset_id, tag_id)
);

-- "every asset carrying this tag" is the tag browser's only question, and the
-- primary key above answers the other direction.
create index asset_tags_tag_idx on asset_tags (tag_id);

-- One tsvector per asset: caption and tags weighted A, ocr B, filename and
-- place C. It is a table rather than a generated column on assets because what
-- feeds it lives in four other tables, and because full-text search is the half
-- of the query path that keeps working when photo-ml is down.
create table asset_search (
    asset_id uuid primary key references assets (id) on delete cascade,
    tsv      tsvector not null
);
create index asset_search_tsv_idx on asset_search using gin (tsv);

-- Where the photograph was taken, in words.
--
-- Not ML, and on the assets row rather than in a table of its own, because it
-- is a fact about the asset in exactly the way camera_make is: one value, known
-- at ingest, from coordinates the row already holds. An offline GeoNames
-- extract resolves it — no network, no per-photo API call, no coordinates
-- leaving the machine. See internal/geocode.
--
-- place_source names what did the resolving ('geonames' today), so a later
-- source — a hand-typed "the cabin", a better extract — is distinguishable from
-- this one without re-running anything. geocoded_at is what makes the backfill
-- resumable and re-runnable: null means nobody has looked yet.
alter table assets
    add column place_city    text,
    add column place_admin1  text,
    add column place_country text,
    add column place_source  text,
    add column geocoded_at   timestamptz;

-- A fifth kind of background work.
--
--   mlprep  write the renditions a vision model reads: the whole photograph,
--           uncropped, at 512px, and several frames out of a video. Go decodes
--           and Python does tensors, so photo-ml is handed image bytes over
--           loopback and never opens a file under /mnt/photos.
--
-- Its own kind rather than part of the vision job that will read its output,
-- because swapping the model has to requeue only the model's half: the
-- renditions are already on disk, and decoding them again is the expensive
-- part. Its own kind rather than part of `metadata`, because one new file per
-- asset is not worth re-running exiftool and three thumbnail sizes over 23,000
-- of them.
--
-- No backfill here, unlike 0014's. The jobs are queued by the worker's
-- reconcile on the next start instead, because this one is an hour of CPU and a
-- migration that starts an hour of work is a schema change with a surprise in
-- it. See jobs.ReconcileMLPrep for the predicate, which is the timeline's own.
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge', 'mlprep'));

-- +goose Down
alter table jobs drop constraint jobs_kind_check;
delete from jobs where kind = 'mlprep';
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge'));

alter table assets
    drop column geocoded_at,
    drop column place_source,
    drop column place_country,
    drop column place_admin1,
    drop column place_city;

drop index asset_search_tsv_idx;
drop table asset_search;
drop index asset_tags_tag_idx;
drop table asset_tags;
drop table tags;
drop table asset_ocr;
drop table asset_descriptions;
drop table asset_embeddings;

drop extension vector;
