-- +goose Up
-- A Snapchat memory is two files and one photograph.
--
-- The `-main.jpg` is the frame the camera captured. The `-overlay.png` beside
-- it is a transparent layer holding everything the person then put on top:
-- the caption, the drawings, the stickers, the timestamp, the temperature. The
-- image anybody actually saw and saved is the two composed, and it exists in
-- neither file — Snapchat's export ships the layers, not the picture.
--
-- Nothing in the archive could express that. A Live Photo's two halves are
-- related by an Apple content identifier stamped into both files; these two
-- are related only by sharing a stem in a filename in an export that is about
-- to be deleted. So the relationship is recorded here, once, at import.
alter table assets
    -- The overlay's own asset row. The overlay is archived as an ordinary
    -- asset — same blob store, same manifest line, same `verify` — because a
    -- second place for bytes to live is a second place for them to go missing,
    -- and because the composite has to be rebuildable from the originals for
    -- as long as the archive exists.
    --
    -- What keeps it out of the gallery is the `archived` flag from 0006 and
    -- the `snapchat:overlay` subtype, not its absence from anywhere.
    --
    -- Null for every asset that is not half of a Snapchat memory, which is
    -- almost all of them.
    add column overlay_asset_id uuid references assets (id) on delete set null,

    -- Set on the overlay itself, and the whole of what keeps it out of the
    -- gallery.
    --
    -- A boolean rather than the reverse lookup it duplicates, because the
    -- alternative is `not exists (select 1 from assets p where
    -- p.overlay_asset_id = assets.id)` evaluated per row on the timeline's hot
    -- path, and because the timeline's index is partial — a predicate it can
    -- be built on has to be a column.
    --
    -- Deliberately not the `archived` flag from 0006. That one records a
    -- source's opinion and the gallery is documented as not acting on it; this
    -- one is structural, and means "this asset is part of another asset's
    -- picture, not a picture". Overlays get both: archived because Snapchat
    -- never showed the layer on its own, and this because the timeline must
    -- not either.
    add column is_overlay boolean not null default false;

-- "Which photo does this overlay belong to", asked from the overlay's side —
-- by the derivative worker, which is handed an asset and must find out whether
-- anything upstream needs rebuilding when it changes.
create index assets_overlay_asset_idx on assets (overlay_asset_id)
    where overlay_asset_id is not null;

-- The timeline's index carries its visibility rule as a partial predicate, so
-- adding a term to the rule means rebuilding it. Same shape as 0005 and 0006
-- did for the Live Photo halves, and for the same reason: an overlay is a
-- component of another asset's presentation rather than an item of its own,
-- which is exactly what a paired video is.
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null and not is_overlay;

-- +goose Down
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null;
drop index assets_overlay_asset_idx;
alter table assets
    drop column is_overlay,
    drop column overlay_asset_id;
