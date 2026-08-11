-- +goose Up
-- A Live Photo arrives as two uploads: the still, and the ~3s video PhotoKit
-- pairs with it. Until now both landed as ordinary assets and the gallery drew
-- the same moment twice, once as a photo and once as a silent clip.
--
-- The phone is the only party that knows they belong together — the two files
-- share nothing but a capture time, and matching on that would pair a photo
-- with whatever video happened to be taken in the same second. So the pairing
-- is declared on the video's upload, and these columns are where it lands.
--
-- Two columns rather than one, because the declaration and its resolution
-- answer different questions and become true at different moments:
--
--   live_parent_local_id  what the phone said, known the instant the video
--                         arrives. It is what makes an asset a paired video at
--                         all, so it decides which derivatives get built and
--                         what the timeline hides.
--   live_parent_asset_id  which archived still it belongs to. It cannot be
--                         filled in until that still exists, and the still may
--                         arrive after its video, so it is resolved from
--                         whichever side lands second.
alter table assets
    add column live_parent_local_id text not null default '',
    add column live_parent_asset_id uuid references assets (id) on delete set null,
    -- The 256px motion rendition the grid plays on hover, tracked exactly like
    -- derived_state and playback_state. 'none' on everything that is not a
    -- paired video.
    add column live_state text not null default 'none'
        check (live_state in ('none', 'pending', 'ready', 'failed'));

-- Resolving from the still's side: "is any video waiting for this local id?"
create index assets_live_parent_local_idx on assets (device_id, live_parent_local_id)
    where live_parent_local_id <> '';

-- Resolving from the timeline's side: "what motion does this still have?"
create index assets_live_parent_asset_idx on assets (live_parent_asset_id)
    where live_parent_asset_id is not null;

-- Every timeline page filters paired videos out, and roughly a third of an
-- iPhone library is one. Better that they are absent from the index the scan
-- walks than filtered out of it a page at a time.
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '';

-- +goose Down
drop index assets_timeline_visible_idx;
drop index assets_live_parent_asset_idx;
drop index assets_live_parent_local_idx;
alter table assets
    drop column live_state,
    drop column live_parent_asset_id,
    drop column live_parent_local_id;
