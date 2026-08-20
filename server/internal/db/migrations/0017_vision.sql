-- +goose Up
-- The migration that names a model.
--
-- 0016 built the search schema and deliberately left one thing out: the HNSW
-- index over asset_embeddings, whose predicate is `where model = '<current>'`
-- and whose model had not been chosen. This is that migration. Everything 0016
-- created has been sitting inert since it landed; from here something writes to
-- it.
--
-- siglip2-so400m-patch14-384 — google/siglip2-so400m-patch14-384 as it is
-- recorded on the row. Two strings on purpose: the row records what produced a
-- vector and will outlive whatever the weights were called on whichever mirror
-- they came from, so the identity is ours. db.VisionModel holds the same
-- constant on the Go side, and photo-ml's encoder.MODEL_NAME on the Python one.
--
-- 1152 dimensions because §4 committed halfvec(1152) before the model was
-- picked, and 1152 is this model's width. The bench in §9 step 1 has still not
-- been run, and this schema is what makes running it later cheap: `model` is
-- part of the embeddings' primary key, so a second encoder's vectors sit in the
-- same table beside these ones while somebody measures the two against each
-- other, and losing the loser is a delete rather than a migration.

-- The predicate is the point.
--
-- An index covering every model at once would mix two incomparable spaces in
-- one graph: a neighbour search would walk through vectors from a model that
-- did not produce the query, and the results would be wrong in a way no error
-- reports. Naming one model is what keeps the graph honest, and it is why this
-- could not be written until one had been chosen.
--
-- A query only reaches this index if it repeats the predicate literally, so
-- every search says `where model = 'siglip2-so400m-patch14-384'` even when it
-- is the only model in the table. db.VisionModel is that literal.
--
-- halfvec_cosine_ops because the vectors arrive unit length — photo-ml
-- normalises before it answers — which makes cosine distance 1 - dot and lets
-- similarities from two frames of one video be compared without either having
-- been scaled by how bright the photograph was.
create index asset_embeddings_siglip2_hnsw on asset_embeddings
    using hnsw (embedding halfvec_cosine_ops)
    where model = 'siglip2-so400m-patch14-384';

-- A sixth kind of background work.
--
--   vision  one call to photo-ml per asset; writes the embeddings.
--
-- Its own kind and its own pool, for the reason `signature` got one and with a
-- stronger case: it is a pass over the whole archive that nothing is waiting
-- for, and it is a queue in front of one GPU. Behind the metadata pool it would
-- stall the gallery's thumbnails for hours to answer a question nobody has
-- typed yet.
--
-- Separate from `mlprep` — which it reads the output of — so that swapping the
-- model requeues only the model's half. The renditions are already on disk and
-- decoding them again is the expensive part: re-running `vision` alone is
-- fifteen minutes, and re-running both is an hour and a half.
--
-- No backfill here, and for the same reason 0016 had none: jobs.ReconcileVision
-- queues these on the worker's next start. A migration that queued sixty
-- thousand GPU calls would be a schema change with a surprise in it — and this
-- one has a second surprise available, because the service it needs is optional
-- and may not be installed on the machine running the migration.
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge', 'mlprep', 'vision'));

-- +goose Down
alter table jobs drop constraint jobs_kind_check;
delete from jobs where kind = 'vision';
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge', 'mlprep'));

drop index asset_embeddings_siglip2_hnsw;
