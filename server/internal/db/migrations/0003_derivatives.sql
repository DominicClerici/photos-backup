-- +goose Up

-- Background work the server owes an asset. Two kinds today:
--
--   metadata  exiftool + the 256px thumbnail (+ a poster frame for video).
--             Fast, and the gallery cannot show the asset until it is done.
--   playback  the H.264 MP4 rendition of a video. Slow, and only the viewer
--             needs it.
--
-- They are separate rows rather than one job so the two worker pools can be
-- sized independently: a single pool would let four 4K transcodes claim every
-- slot and starve every thumbnail queued behind them.
create table jobs (
    id         bigserial   primary key,
    kind       text        not null check (kind in ('metadata', 'playback')),
    asset_id   uuid        not null references assets (id) on delete cascade,
    state      text        not null default 'pending'
                           check (state in ('pending', 'running', 'done', 'failed')),
    attempts   int         not null default 0,
    -- run_after is how backoff is expressed: a failed job is rescheduled by
    -- pushing this forward rather than by sleeping in the worker.
    run_after  timestamptz not null default now(),
    locked_at  timestamptz,
    locked_by  text,
    last_error text,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    unique (asset_id, kind)
);

-- The claim query is the only hot path here: pending rows of a given kind,
-- oldest runnable first. Partial, because done and failed rows stay forever and
-- would otherwise dominate the index.
create index jobs_claim_idx on jobs (kind, run_after, id) where state = 'pending';

-- The lease sweep looks for running jobs whose worker died. Also partial: at
-- most a handful of rows are ever in this state.
create index jobs_lease_idx on jobs (locked_at) where state = 'running';

-- Counts for /health and /v1/jobs.
create index jobs_state_idx on jobs (state, kind);

alter table assets
    -- Split from content_type so queries do not have to pattern-match a MIME
    -- string, and so an unrecognised extension is classified once, on the way in.
    add column media_kind          text not null default 'image'
                                   check (media_kind in ('image', 'video')),
    add column width               int,
    add column height              int,
    add column orientation         int,
    add column duration_seconds    double precision,
    add column camera_make         text,
    add column camera_model        text,
    add column lens                text,
    add column gps_lat             double precision,
    add column gps_lon             double precision,
    -- The capture time read out of the file itself. captured_at, already
    -- present, stays exactly as the phone reported it. Keeping both means a
    -- disagreement is visible instead of destroyed, and it gives the v2 USB
    -- ingest -- which has no phone metadata at all -- a working path.
    add column exif_captured_at    timestamptz,
    -- Minutes east of UTC from OffsetTimeOriginal, so the viewer can say "6pm"
    -- meaning 6pm where the photo was taken rather than 6pm where you are.
    add column exif_offset_minutes int,
    add column derived_state       text not null default 'pending'
                                   check (derived_state in ('pending', 'ready', 'failed')),
    add column playback_state      text not null default 'none'
                                   check (playback_state in ('none', 'pending', 'ready', 'failed'));

-- Existing rows were classified by extension when they were uploaded.
update assets set media_kind = 'video' where content_type like 'video/%';

-- The timeline sorts and paginates on this. A stored generated column keeps
-- keyset pagination a plain index scan instead of an expression evaluated per
-- row, and guarantees the sort key can never drift from its inputs.
alter table assets
    add column sort_time timestamptz not null
        generated always as (coalesce(exif_captured_at, captured_at, uploaded_at)) stored;

create index assets_sort_time_idx on assets (sort_time desc, id desc);

-- assets_timeline_idx ordered by (captured_at, uploaded_at), which sort_time
-- now supersedes.
drop index assets_timeline_idx;

-- Everything already archived needs its derivatives built. These enqueue as
-- normal pending work, so the first run after this migration backfills the
-- library through the same path a fresh upload takes.
insert into jobs (kind, asset_id)
select 'metadata', id from assets
on conflict (asset_id, kind) do nothing;

-- +goose Down
drop index assets_sort_time_idx;
create index assets_timeline_idx on assets (captured_at desc nulls last, uploaded_at desc);
alter table assets
    drop column sort_time,
    drop column playback_state,
    drop column derived_state,
    drop column exif_offset_minutes,
    drop column exif_captured_at,
    drop column gps_lon,
    drop column gps_lat,
    drop column lens,
    drop column camera_model,
    drop column camera_make,
    drop column duration_seconds,
    drop column orientation,
    drop column height,
    drop column width,
    drop column media_kind;
drop table jobs;
