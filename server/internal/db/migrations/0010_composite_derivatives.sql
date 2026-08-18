-- +goose Up
-- 0009 recorded which photograph each Snapchat caption layer belongs to. This
-- is the moment the archive starts drawing them.
--
-- Every rendition of a memory is now built from the two files composed rather
-- than from the photograph alone: the thumbnails in the grid, the poster on a
-- video tile, the 2048px preview, and — for video, where nothing on the client
-- can lay a PNG over a playing frame — the playback rendition itself. Which
-- means every one of those already on disk is of half a picture.
--
-- Nothing on disk can be inspected to tell the two apart. A thumbnail built
-- from the photograph alone is a valid thumbnail of the right asset at the
-- right size, so `verify` cannot find these the way it finds a missing file;
-- the only thing that knows they are stale is the fact that this migration ran.
-- So the requeue happens here, once, rather than being left to a command
-- somebody has to remember to type.
--
-- Requeue rather than enqueue: these assets have a metadata job already, and it
-- is marked done.
insert into jobs (kind, asset_id)
select 'metadata', id from assets where overlay_asset_id is not null
on conflict (asset_id, kind) do update
set state = 'pending', attempts = 0, run_after = now(),
    locked_at = null, locked_by = null, last_error = null, updated_at = now();

-- And the transcode separately, because the metadata job will not ask for it.
-- Its own enqueue ignores a kind that is already queued, which for a video that
-- has been through the pipeline once means the burned-in rendition and the
-- plain one beside it would never be built.
insert into jobs (kind, asset_id)
select 'playback', id
from assets
where overlay_asset_id is not null and media_kind = 'video'
on conflict (asset_id, kind) do update
set state = 'pending', attempts = 0, run_after = now(),
    locked_at = null, locked_by = null, last_error = null, updated_at = now();

-- +goose Down
-- Nothing to undo. The work this queued either ran or did not, and the
-- renditions it produced are valid either way — an older binary reads a
-- composited thumbnail as an ordinary one.
select 1;
