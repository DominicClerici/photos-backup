-- +goose Up
-- The vocabulary, made readable — ML_IMAGES.md §9, and one stage more than §9
-- planned for.
--
-- §2 chose free-form tags: the captioner writes whatever words it likes, and
-- the cleanup is a data operation over the accumulated vocabulary once the
-- library has been through. That bet has now been paid in full — 2,936 distinct
-- strings out of the first 2,055 photographs, which is on track for the three
-- to six thousand §9 predicted — and this migration is the schema the cleanup
-- runs on.
--
-- §9 describes one operation: cluster the tag names in the encoder's own space
-- and propose merges. Doing it against the real vocabulary turned up a second
-- one that has to come first, because it changes the answer to the first. A
-- vision model looking at a screenshot writes "login", "result", "true",
-- "details" and "post"; looking at a photograph of people it writes "casual",
-- "friendly" and "collaborative"; and looking at anything at all it sometimes
-- writes "photograph". None of those is a word anybody will ever search for,
-- none of them merges into anything, and all of them sit in the weight-A half
-- of every tsvector they are attached to. Clustering with them still in is
-- clustering three thousand words when two thousand are the question.
--
-- So there are two passes and they are ordered:
--
--   triage   every word is junk or not. The captioner judges its own output —
--            see photo_ml/captioner.py judge() — and a person reviews the
--            verdicts in two lists before any of them counts.
--   merge    what survives is embedded, clustered, and proposed as groups of
--            words that mean one thing. Accepting sets canonical_id, which is
--            what 0016 built the column for.
--
-- Both are reversible, and neither destroys a row. asset_tags goes on recording
-- exactly what the model wrote about each photograph; junk and canonical_id are
-- read at every point of use, which is the same property that makes a mistaken
-- merge undoable and is why the merge was designed this way in the first place.

-- What a person and a model each think of one word.
--
-- Four columns rather than one, and the split is ML_IMAGES.md §11's seam — "a
-- name a person confirmed" against "a word a model produced" — applied to the
-- vocabulary itself.
--
--   junk        the verdict in force. Read by everything: the clustering, the
--               tsvector, the query parser's vocabulary, the viewer's panel.
--   junk_score  what the captioner thought, 0 to 1. Advisory, and kept because
--               it is the order the review list is worth reading in: the
--               mistakes worth catching are the confident ones.
--   triaged_at  when the model last judged this word. Null means nothing has,
--               and `junk = false, triaged_at is null` is the safe default a
--               new word arrives with — untouched rather than presumed junk.
--   judged_at   when a *person* decided. This is the column that makes the
--               triage re-runnable: a pass writes junk only where judged_at is
--               null, so re-analysing a grown vocabulary can never overrule an
--               answer somebody gave. Approving a reviewed list is what stamps
--               it in bulk.
alter table tags
    add column junk        boolean     not null default false,
    add column junk_score  real,
    add column triaged_at  timestamptz,
    add column judged_at   timestamptz;

-- "every word still waiting for a verdict" is the triage pass's only question,
-- and "every word in force as junk" is what four other queries subtract.
-- Partial on the second, because junk is the minority and false is the default
-- every new row arrives with.
create index tags_untriaged_idx on tags (id) where triaged_at is null;
create index tags_junk_idx      on tags (id) where junk;

-- The tag names, as vectors, in the same space as the photographs.
--
-- The same shape as asset_embeddings and for the same reasons: model in the
-- primary key so two encoders can be compared without a schema change, halfvec
-- because fp16 costs nothing measurable at this size, and the vector written by
-- whatever actually produced it rather than by whatever was configured.
--
-- Stored rather than computed per request, which is the one place this differs
-- from what §9 sketched. Embedding the whole vocabulary is seven seconds
-- against a warm encoder — cheap enough to do live, until you notice that the
-- review screen's similarity threshold is a control somebody is going to drag,
-- and that re-clustering has to be instant for that control to be worth having.
-- With these stored the clustering is a kNN query and costs about 240ms; with
-- them recomputed it is seven seconds a drag. It also means the merge review
-- goes on working with photo-ml down, which is the rule PROJECT.md §4 applies
-- to everything else here.
create table tag_embeddings (
    tag_id      bigint      not null references tags (id) on delete cascade,
    model       text        not null,
    embedding   halfvec(1152) not null,
    embedded_at timestamptz not null default now(),
    primary key (tag_id, model)
);

-- The index that makes the clustering a query rather than a wait.
--
-- Measured on this vocabulary: a brute-force self-join over 3,000 halfvec(1152)
-- rows is 7.7 seconds, and the same neighbours through this index are 240ms.
-- The difference is what lets the threshold be a live control.
--
-- Partial, with the model spelled out, exactly as 0017's is and with the same
-- consequence: a query only reaches it by repeating the predicate literally, so
-- db.VisionModel appears in the SQL as well as in the parameter list. An index
-- covering every model at once would put two incomparable spaces in one graph.
create index tag_embeddings_siglip2_hnsw on tag_embeddings
    using hnsw (embedding halfvec_cosine_ops)
    where model = 'siglip2-so400m-patch14-384';

-- Two words somebody has said are not the same word.
--
-- The tag vocabulary's version of db.BlockedPairs, and it exists for the same
-- reason: without it, rejecting a proposal accomplishes nothing, because the
-- next clustering run computes the same distances over the same vectors and
-- proposes it again. A rejection has to be written down somewhere or it is not
-- a rejection.
--
-- Pairs rather than groups, and ordered so that (a, b) and (b, a) are one row.
-- A proposal is a group, but what a person disagrees with inside one is usually
-- a single member — "mountain, mountains, mountain range, and no, not
-- mountaineering" — and blocking the group would also block the four merges
-- they had just agreed to.
create table tag_merge_blocks (
    tag_id     bigint      not null references tags (id) on delete cascade,
    other_id   bigint      not null references tags (id) on delete cascade,
    blocked_at timestamptz not null default now(),
    primary key (tag_id, other_id),
    -- Canonical order, enforced rather than trusted: every writer normalises,
    -- and this is what makes "have these two been rejected" a primary-key
    -- lookup instead of two.
    constraint tag_merge_blocks_ordered check (tag_id < other_id)
);

-- +goose StatementBegin
-- The tsvector recipe, with junk taken out of it.
--
-- Replaced rather than added to, and this is the case migration 0018's own
-- header and ML_IMAGES.md §11 both warned about: "a changed recipe in
-- rebuild_asset_search leaves every row already written out of date". So the
-- statement after this one rebuilds the whole library, which is seconds, and
-- the same obligation is discharged inside the transaction that writes a merge
-- or a junk verdict — see db.refreshForTags. §11 asks for that to be
-- *remembered*; from here it is not something anybody has to remember.
--
-- One clause changed, in the tag lateral. Everything else is 0018's verbatim.
--
-- The junk test reads through the merge, in that order, because both can be
-- true of one claim: a word that was folded into another is searched as the
-- other, so what decides whether it belongs in a tsvector is the *canonical*
-- word's verdict, not its own. "puppy" merged into "dog" is kept because "dog"
-- is kept; "attendee" merged into "people" would drop the day somebody marks
-- "people" junk, which is right — the claim is still on asset_tags, and
-- unmarking it brings the word back everywhere at once.
create or replace function rebuild_asset_search(
    ids           uuid[],
    caption_model text,
    ocr_model     text
) returns bigint
language plpgsql as $$
declare
    touched bigint;
begin
    delete from asset_search s
    using assets a
    where s.asset_id = a.id
      and (ids is null or s.asset_id = any (ids))
      and (a.vault <> '' or a.deleted_at is not null);

    insert into asset_search (asset_id, tsv)
    select a.id,
           setweight(to_tsvector('english', coalesce(d.caption, '')), 'A')
        || setweight(to_tsvector('english', coalesce(t.names, '')), 'A')
        || setweight(to_tsvector('english', coalesce(o.text, '')), 'B')
        || setweight(to_tsvector('english', coalesce(a.description, '')), 'B')
        || setweight(to_tsvector('english',
               regexp_replace(a.original_filename, '[^a-zA-Z0-9]+', ' ', 'g')), 'C')
        || setweight(to_tsvector('english',
               concat_ws(' ', a.place_city, a.place_admin1, a.place_country)), 'C')
    from assets a
    left join asset_descriptions d on d.asset_id = a.id and d.model = caption_model
    left join asset_ocr          o on o.asset_id = a.id and o.model = ocr_model
    left join lateral (
        select string_agg(distinct coalesce(canonical.name, tag.name), ' ') as names
        from asset_tags at
        join tags tag on tag.id = at.tag_id
        left join tags canonical on canonical.id = tag.canonical_id
        where at.asset_id = a.id
          and not coalesce(canonical.junk, tag.junk)
    ) t on true
    where (ids is null or a.id = any (ids))
      and a.vault = '' and a.deleted_at is null
    on conflict (asset_id) do update set tsv = excluded.tsv;

    get diagnostics touched = row_count;
    return touched;
end;
$$;
-- +goose StatementEnd

-- The obligation, discharged. Nothing is junk on the day this lands, so this
-- rewrites every row to exactly what it already said — which is the point: the
-- recipe changed, and a migration that changes a stored recipe and leaves the
-- store alone is the stale-index bug §11 names, shipped deliberately.
select rebuild_asset_search(null, 'qwen3-vl-4b-instruct', 'rapidocr');

-- +goose Down
-- The function first, because the version below reads columns the next
-- statement drops.
-- +goose StatementBegin
create or replace function rebuild_asset_search(
    ids           uuid[],
    caption_model text,
    ocr_model     text
) returns bigint
language plpgsql as $$
declare
    touched bigint;
begin
    delete from asset_search s
    using assets a
    where s.asset_id = a.id
      and (ids is null or s.asset_id = any (ids))
      and (a.vault <> '' or a.deleted_at is not null);

    insert into asset_search (asset_id, tsv)
    select a.id,
           setweight(to_tsvector('english', coalesce(d.caption, '')), 'A')
        || setweight(to_tsvector('english', coalesce(t.names, '')), 'A')
        || setweight(to_tsvector('english', coalesce(o.text, '')), 'B')
        || setweight(to_tsvector('english', coalesce(a.description, '')), 'B')
        || setweight(to_tsvector('english',
               regexp_replace(a.original_filename, '[^a-zA-Z0-9]+', ' ', 'g')), 'C')
        || setweight(to_tsvector('english',
               concat_ws(' ', a.place_city, a.place_admin1, a.place_country)), 'C')
    from assets a
    left join asset_descriptions d on d.asset_id = a.id and d.model = caption_model
    left join asset_ocr          o on o.asset_id = a.id and o.model = ocr_model
    left join lateral (
        select string_agg(distinct coalesce(canonical.name, tag.name), ' ') as names
        from asset_tags at
        join tags tag on tag.id = at.tag_id
        left join tags canonical on canonical.id = tag.canonical_id
        where at.asset_id = a.id
    ) t on true
    where (ids is null or a.id = any (ids))
      and a.vault = '' and a.deleted_at is null
    on conflict (asset_id) do update set tsv = excluded.tsv;

    get diagnostics touched = row_count;
    return touched;
end;
$$;
-- +goose StatementEnd

drop table tag_merge_blocks;
drop index tag_embeddings_siglip2_hnsw;
drop table tag_embeddings;

drop index tags_junk_idx;
drop index tags_untriaged_idx;
alter table tags
    drop column judged_at,
    drop column triaged_at,
    drop column junk_score,
    drop column junk;

select rebuild_asset_search(null, 'qwen3-vl-4b-instruct', 'rapidocr');
