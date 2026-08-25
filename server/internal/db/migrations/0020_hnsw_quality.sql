-- +goose NO TRANSACTION
-- File-level, and it governs both directions: goose reads this annotation
-- wherever it appears and stops wrapping the whole migration. CREATE INDEX
-- CONCURRENTLY cannot run inside a transaction block, and both halves below use
-- it.

-- +goose Up
-- The migration that rebuilds a graph, having measured it.
--
-- 0017 created the HNSW index over asset_embeddings with pgvector's defaults —
-- m = 16, ef_construction = 64 — because those are what you get when you do not
-- say otherwise, and nothing had yet asked what they cost. photo-ml/bench
-- asked. Over the 45 search phrases in bench/queries.json, comparing what the
-- index returns against what an exhaustive scan of the same 31,583 vectors
-- returns:
--
--   build                     ef_search   top-10   top-20   top-60
--   m=16, ef_construction=64      40       0.86     0.85     0.89
--   m=32, ef_construction=200     40       0.94     0.92     0.93
--   m=32, ef_construction=200    200       0.97     0.96     0.96
--   m=48, ef_construction=400    200       0.99     0.99     0.98
--
-- The first row is what the archive was running, and 0.86 is not a rounding
-- error: for "flowers" the graph returned *none* of the true top ten, and for
-- "a birthday cake" it returned three. Those photographs were in the table, at
-- the right distance, correctly embedded, and unreachable — the failure mode
-- with no error message anywhere in it, which looks exactly like a model that
-- does not know what a flower is.
--
-- The obvious first suspect was memory. The old index is 81MB and
-- maintenance_work_mem is 64MB, so the build could not hold its graph and
-- pgvector fell back to its on-disk path. That turned out not to be the cause:
-- rebuilt at the same parameters with 1GB to work in, recall measured 0.82,
-- which is the same number wearing a different hat. HNSW construction is
-- randomised and 0.82 against 0.86 is two samples of one distribution. The
-- parameters were the whole of it.
--
-- m = 48 rather than 32 because it is free here. The index is 122MB either way
-- — the same size the 81MB default grew to at m=32 — the build is 39 seconds
-- against 20, and both are once per model swap. On a library ten times this
-- size the trade would be worth rerunning; at 31,583 vectors it is not a trade.
--
-- CONCURRENTLY is deliberate and it is why this is two statements rather than
-- an ALTER. It does not take the lock that would stop the vision pool writing
-- embeddings, so a backfill that happens to be running when somebody deploys
-- this is slowed rather than blocked. The cost is that goose must not wrap it
-- in a transaction, which is what the annotation at the top of this file is for.

-- Built beside the old one, under a temporary name, because the alternative is
-- a window with no index at all. A search in that window is answered by
-- sequential scan over 31,583 vectors — correct, and 86ms rather than 6ms,
-- which is a search box that feels broken for as long as the build takes.
create index concurrently if not exists asset_embeddings_hnsw_rebuild
    on asset_embeddings using hnsw (embedding halfvec_cosine_ops)
    with (m = 48, ef_construction = 400)
    where model = 'siglip2-so400m-patch14-384';

drop index concurrently if exists asset_embeddings_siglip2_hnsw;

alter index asset_embeddings_hnsw_rebuild rename to asset_embeddings_siglip2_hnsw;

-- +goose Down

-- Back to 0017's index, defaults and all. Reversible in the sense that matters
-- — the name and the predicate are what db.VisionModel and every search depend
-- on, and both are restored — but not in the sense that the old graph comes
-- back: HNSW construction is randomised, so this builds a *different* bad graph
-- rather than the previous one. Nothing reads a graph's identity, only its
-- answers, which is why that is acceptable rather than merely tolerable.
create index concurrently if not exists asset_embeddings_hnsw_rollback
    on asset_embeddings using hnsw (embedding halfvec_cosine_ops)
    where model = 'siglip2-so400m-patch14-384';

drop index concurrently if exists asset_embeddings_siglip2_hnsw;

alter index asset_embeddings_hnsw_rollback rename to asset_embeddings_siglip2_hnsw;
