-- +goose Up
-- Migration 0005 pairs a Live Photo from a declaration the phone makes on
-- upload, and that is all it can pair. A Google Takeout export declares
-- nothing, so its paired videos landed as ordinary three-second clips beside
-- the stills they belong to, and the gallery drew the same moment twice.
--
-- The declaration was never the only evidence. Apple stamps a UUID into both
-- halves at capture — the Apple maker note carries it on the still, the
-- QuickTime `com.apple.quicktime.content.identifier` key carries it on the
-- video — and Google's export preserves both. That identifier is what this
-- migration stores, and it pairs anything that has ever been through an iPhone
-- regardless of how it reached the archive.
alter table assets
    -- The Apple content identifier, uppercased, read off the file by the
    -- metadata worker and optionally declared on upload by a client that has
    -- already read it. Empty on everything that never came from an iPhone,
    -- which is most of the ways a photo can enter an archive.
    add column content_id text not null default '';

-- Pairing resolves by asking "what else carries this identifier?", from
-- whichever half landed second. Partial because the answer is only ever wanted
-- for rows that have one.
create index assets_content_id_idx on assets (content_id) where content_id <> '';

-- The timeline hid a video the moment it was declared to be half of a Live
-- Photo, because the phone only ever declares one whose still is on its way.
-- An import has no such guarantee: an export can hold a paired video whose
-- still was deleted years ago, and 44 of the 135 files in the sample export
-- are exactly that. Hiding those would archive them into invisibility.
--
-- So a video is hidden once it is *resolved* to a still, or once a phone has
-- declared one — never merely for carrying an identifier. An orphan stays an
-- ordinary video and silently becomes motion on the day its still is imported.
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null;

-- What a Takeout sidecar knows and the file does not.
--
-- Google writes a `.supplemental-metadata.json` beside each item holding the
-- capture time, coordinates, caption, and flags as Google Photos held them.
-- For anything whose EXIF survived the round trip this is redundant. For a
-- screenshot, a saved image, or anything a messaging app stripped, it is the
-- only metadata that exists — the sample export has a PNG whose entire
-- provenance is its sidecar.
alter table assets
    -- The caption, from the sidecar's description field.
    add column description text,
    -- Google Photos' star and its archive. Stored because they are real user
    -- intent that cannot be recovered once the export is deleted; neither
    -- currently changes what the gallery shows.
    add column favorite boolean not null default false,
    add column archived boolean not null default false,
    -- Where the sidecar came from, e.g. 'google-takeout'. Empty for an asset
    -- the phone delivered.
    add column import_source text not null default '',
    -- The sidecar verbatim. Everything above is a projection of this, and this
    -- is kept so that a field we chose not to model today — view counts,
    -- Google's own URL, the upload origin — is not destroyed by that choice.
    -- The export is deleted after an import; the JSON is the only copy.
    add column import_metadata jsonb,
    -- The sidecar's coordinates, kept apart from gps_lat/gps_lon rather than
    -- written into them. The file is the authority on its own contents and the
    -- metadata worker rewrites those two columns on every run, so a value
    -- merged into them would survive exactly until the next reindex. These
    -- feed the canonical columns only where the file itself carried nothing.
    add column import_gps_lat double precision,
    add column import_gps_lon double precision;

-- Album membership, which lives in the export's directory structure and in a
-- per-directory metadata.json rather than in any file's metadata.
create table albums (
    id          uuid        primary key default gen_random_uuid(),
    -- Namespaced by importer so two exports with a "Favorites" album do not
    -- silently merge into one, and so a re-import of the same export does.
    source      text        not null default '',
    title       text        not null,
    description text        not null default '',
    created_at  timestamptz not null default now(),
    unique (source, title)
);

create table album_assets (
    album_id uuid not null references albums (id) on delete cascade,
    asset_id uuid not null references assets (id) on delete cascade,
    primary key (album_id, asset_id)
);

-- "What albums is this asset in", which is the direction the viewer asks.
create index album_assets_asset_idx on album_assets (asset_id);

-- The people Google tagged, by name only. Not an identity: it is a label a
-- face-grouping model produced and a person confirmed, and the model that
-- produced it is not ours. Stored as text so v2's own face work can be
-- reconciled against it rather than constrained by it.
create table asset_people (
    asset_id uuid not null references assets (id) on delete cascade,
    name     text not null,
    primary key (asset_id, name)
);

create index asset_people_name_idx on asset_people (name);

-- +goose Down
drop table asset_people;
drop table album_assets;
drop table albums;
alter table assets
    drop column import_gps_lon,
    drop column import_gps_lat,
    drop column import_metadata,
    drop column import_source,
    drop column archived,
    drop column favorite,
    drop column description;
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '';
drop index assets_content_id_idx;
alter table assets drop column content_id;
