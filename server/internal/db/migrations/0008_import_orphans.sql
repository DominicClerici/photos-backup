-- +goose Up
-- What an import read and could not attach to anything.
--
-- Two things landed here, and they are the same kind of loss: a fact about a
-- photograph that exists nowhere but in an export which is about to be deleted,
-- dropped on the floor because the code had nowhere to put it.
--
--   A sidecar whose media file is not in the export. Google splits an item from
--   the JSON describing it across numbered zips freely, so this is ordinary
--   rather than exceptional, and importing all six deliveries together is what
--   makes it rare. What was left was a printed filename and a discarded body:
--   the caption, the people, the coordinates and the capture time went with it.
--
--   Albums for an item with no sidecar. Album membership exists nowhere but the
--   export's directory layout, and the request that carries it to the archive
--   is shaped around a sidecar, so an item in an album folder whose sidecar did
--   not match lost the album silently.
--
-- Neither is applied to an asset. That is deliberate: an orphan is evidence,
-- not a decision, and what to do about one — match it by hand, wait for the
-- delivery that holds its file, decide it was junk — is a judgement this table
-- exists to make possible rather than to pre-empt.
create table import_orphans (
    id uuid primary key default gen_random_uuid(),

    -- The importer that read it, namespacing everything below: 'google-takeout'
    -- or 'ios-photokit'.
    source text not null,
    -- 'sidecar' or 'album'.
    kind text not null,
    -- Where it sat inside the export, slash-separated and relative to the top —
    -- the same identity the scan gives a media file, so it is stable whether
    -- the export was unzipped once or six times.
    locator text not null,

    -- Set on an album orphan, which unlike a sidecar orphan does have an asset;
    -- what it lacks is a way to have said so. Null on a sidecar orphan, whose
    -- whole problem is that no asset is known.
    asset_id uuid references assets(id) on delete cascade,

    -- The sidecar verbatim, and the albums the directory layout implied. Raw
    -- for the same reason import_metadata is raw: re-reading it with a later
    -- parser must be possible, because a better parser is the most likely way
    -- one of these ever stops being an orphan.
    sidecar jsonb,
    albums  jsonb,

    -- Why it could not be attached, in words.
    reason text not null default '',

    first_seen timestamptz not null default now(),
    last_seen  timestamptz not null default now(),

    -- Re-running an import is how anyone recovers a half-finished one, and it
    -- re-reads every sidecar it could not place last time. One row per thing,
    -- not one per attempt.
    unique (source, kind, locator)
);

create index import_orphans_kind_idx on import_orphans (kind, source);

-- +goose Down
drop table import_orphans;
