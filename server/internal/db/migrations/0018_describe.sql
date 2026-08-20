-- +goose Up
-- What a photograph is *of*, in words — and the one index that makes those
-- words findable.
--
-- 0016 built four empty tables for this and said nothing would write to them
-- until the vision pass existed. Two of them — asset_embeddings and the place
-- columns — have been filling since 0017. The other three, asset_descriptions,
-- asset_ocr and tags, are what this migration turns on, along with the
-- asset_search tsvector that ties all of it to the filename and the place name.
--
-- Unlike 0016 and 0017, this one is not inert on the day it lands. The last
-- statement below fills asset_search for the whole library, and it is worth
-- being clear about why that is not the "schema change with a surprise in it"
-- those two migrations refused to be. It queues nothing, starts no GPU, and
-- runs in a couple of seconds over 23,000 rows: what it writes is the filename
-- and the place name, which the archive has held all along and has never been
-- able to search. The captions arrive later and rewrite these rows in place.

-- Two more kinds of background work, and they are separate from each other and
-- from `vision` for the reason `mlprep` is separate from `vision`: so that
-- swapping one model requeues one model's work.
--
--   ocr       a dedicated text recogniser over the same renditions. Minutes
--             over the library, and the whole of what makes a screenshot, a
--             receipt or a road sign findable by what it says.
--   describe  the captioner: one caption and a handful of free-form tags per
--             frame. Hours over the library, and the expensive one.
--
-- Folding these into `vision` would have tied them together the wrong way
-- round. Re-embedding the library is fifteen minutes and re-captioning it is
-- four hours, so one job would mean every encoder bench dragged four hours of
-- VLM behind it — the exact coupling ML_IMAGES.md §5 split mlprep out to avoid,
-- one layer further along.
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge', 'mlprep', 'vision', 'ocr', 'describe'));

-- The merge, read backwards. asset_tags points at whatever word the model
-- actually wrote, and search resolves through coalesce(canonical_id, id) — so
-- finding every tag that has been folded into "dog" is a lookup by canonical_id
-- and wants an index for it. Partial, because the column is null for the
-- overwhelming majority: nothing has been merged into anything on the day a
-- vocabulary is written.
create index tags_canonical_idx on tags (canonical_id) where canonical_id is not null;

-- "photos in Chicago" as an index scan rather than a sequential one, and the
-- distinct-value scans that build the query parser's vocabulary of place names.
-- Partial for the same reason: 38% of this library has no GPS fix and those
-- rows have nothing to offer either query.
create index assets_place_city_idx    on assets (place_city)    where place_city    is not null;
create index assets_place_admin1_idx  on assets (place_admin1)  where place_admin1  is not null;
create index assets_place_country_idx on assets (place_country) where place_country is not null;

-- +goose StatementBegin
-- The recipe for one asset's tsvector, in one place.
--
-- A function rather than a statement pasted into Go, because this is written
-- from four tables and two models and read by exactly one query, and two copies
-- of it would drift the moment somebody added a source. It lives in a migration
-- because what it produces is *stored*: changing the recipe means every row
-- already written is out of date, and a migration is the thing that knows how
-- to say so.
--
-- The weights are ML_IMAGES.md §4's, and the reasoning is about who wrote each
-- source.
--
--   A  caption and tags. A model looked at the photograph and said what was in
--      it. That is the closest thing here to a description of the subject.
--   B  OCR and the imported description. Text that is genuinely *in* the
--      photograph, or that a person typed under it — precise when it matches,
--      and full of interface chrome and street signs when it does not.
--   C  filename and place. Always present, never chosen: every photograph has
--      them, so a match on one is weak evidence and must not outrank a caption.
--
-- One text search configuration for all six sources, and it is `english` even
-- for the two that are proper nouns. Symmetry beats fidelity here: the query
-- side is a single websearch_to_tsquery and it has to be *some* configuration,
-- so anything indexed under another one silently stops matching. `english`
-- stems "Breckenridge" to `breckenridg` — which looks wrong until you notice
-- that a search for "breckenridge" stems to exactly the same thing, and that a
-- `simple` place column beside an `english` query stems only one side and
-- matches nothing at all. That was this function's first bug.
--
-- The filename is exploded on punctuation first, because otherwise
-- `IMG_20190131_123456.jpg` is one token and nobody types that.
--
-- ids null means the whole library. The model names are arguments rather than
-- literals so that a bench comparing two captioners can rebuild against either
-- without a schema change — db.CaptionModel and db.OCRModel are what photod
-- passes.
create function rebuild_asset_search(
    ids           uuid[],
    caption_model text,
    ocr_model     text
) returns bigint
language plpgsql as $$
declare
    touched bigint;
begin
    -- A photograph hidden or trashed since the last rebuild loses its row.
    -- The vault's objection is precisely to a legible, searchable description
    -- of what it is holding, and a tsvector is exactly that; the trash's is
    -- milder but the answer is the same, since nothing searches it.
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
    -- Resolved through the merge on the way in, so a library where "puppy" has
    -- been folded into "dog" answers a search for either without the query
    -- having to know. The raw claim stays on asset_tags, which is what keeps a
    -- mistaken merge undoable.
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

-- And the one part of search that works today, on a machine that has never had
-- a GPU: every filename and every place name in the archive, as a tsvector.
-- Seconds, not hours. See the header.
select rebuild_asset_search(null, 'qwen3-vl-4b-instruct', 'rapidocr');

-- +goose Down
drop function rebuild_asset_search(uuid[], text, text);

drop index assets_place_country_idx;
drop index assets_place_admin1_idx;
drop index assets_place_city_idx;
drop index tags_canonical_idx;

alter table jobs drop constraint jobs_kind_check;
delete from jobs where kind in ('ocr', 'describe');
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge', 'mlprep', 'vision'));

delete from asset_search;
