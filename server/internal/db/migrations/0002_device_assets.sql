-- +goose Up
-- One archived asset can be reached from many places: the same bytes saved
-- twice in a library, or the same photo on a second device. assets.device_id
-- and assets.local_id record whoever delivered the bytes first, matching the
-- manifest line. This table records every mapping instead, which is what
-- sync/check needs: a local id whose content is already archived under a
-- different local id must answer "have", not "want".
--
-- modified_at is the local asset's modification time as reported when it was
-- last read. An edit in Photos keeps the PhotoKit local identifier but changes
-- the bytes, so a mapping whose modified_at no longer matches the phone's is
-- treated as unknown and re-checked by content.
create table device_assets (
    device_id   text        not null,
    local_id    text        not null,
    asset_id    uuid        not null references assets (id) on delete cascade,
    modified_at timestamptz,
    first_seen  timestamptz not null default now(),
    primary key (device_id, local_id)
);

create index device_assets_asset_idx on device_assets (asset_id);

-- sync/check looks content up by (md5, byte_size) for local ids it does not
-- recognise. Size is part of the key so an accidental md5 collision cannot make
-- the server claim it holds bytes it has never seen.
create index assets_md5_size_idx on assets (md5, byte_size);

-- Mappings implied by rows that predate this table. modified_at stays null,
-- which reads as "unknown" and costs those assets one content check each.
insert into device_assets (device_id, local_id, asset_id, first_seen)
select device_id, local_id, id, uploaded_at
from assets
where device_id <> ''
  and local_id <> ''
on conflict (device_id, local_id) do nothing;

-- +goose Down
drop index assets_md5_size_idx;
drop table device_assets;
