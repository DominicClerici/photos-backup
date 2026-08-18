-- +goose Up
-- The archive learns to forget.
--
-- Every decision before this one rested on the archive being append-only: the
-- server never deletes, so no upload can race a removal, no sync can undo
-- itself, and the worst a bug can do is store something twice. That is still
-- true of the sync path, which is where the guarantee was actually worth
-- having. What changes here is that the *gallery* can delete, deliberately, by
-- hand — and it does so in two steps, because the property worth keeping is not
-- "nothing is ever removed" but "nothing is removed by accident".
--
-- Step one is this column. A deleted asset keeps its row, its blob, its
-- derivatives and its place in every table that references it; it is merely
-- invisible everywhere except the trash. Nothing has been lost and a restore is
-- one UPDATE. Step two is the purge, which happens 365 days later or when
-- somebody asks for it explicitly, and that one is real: rows gone, bytes gone,
-- and a tombstone left behind so the phone does not helpfully upload it again.
alter table assets
    -- When this asset was moved to the trash. Null is the whole library.
    add column deleted_at timestamptz,
    -- When it becomes eligible for the purge. Written at delete time rather
    -- than derived from deleted_at, so the retention an item was deleted under
    -- is the retention it gets — changing the window later cannot silently
    -- shorten the life of something already in the trash.
    add column purge_after timestamptz,
    -- The operation that deleted it. One click can trash forty thousand items
    -- across a hundred days, and the undo in the toast has to put back exactly
    -- those and nothing else — including the paired videos and caption layers
    -- that were carried along because their parent went.
    add column delete_batch uuid;

-- The gallery's index carries its visibility rule as a partial predicate, so a
-- new term means rebuilding it. Same shape and same reason as 0005, 0006 and
-- 0009: everything the timeline will never return is better absent from the
-- index than filtered out of it a page at a time.
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null
      and not is_overlay and deleted_at is null;

-- The trash is the same timeline over the other half of that predicate: same
-- ordering, same keyset cursor, same day table. It is a scope rather than a
-- collection precisely so it can reuse all of it.
create index assets_trash_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null
      and not is_overlay and deleted_at is not null;

-- The sweep asks one question every hour — "is anything due?" — and the answer
-- is nearly always no. Partial, so that question costs an index probe over the
-- trash rather than a scan of the library.
create index assets_purge_due_idx on assets (purge_after) where deleted_at is not null;

-- Undo, from the batch the delete handed back.
create index assets_delete_batch_idx on assets (delete_batch) where delete_batch is not null;

-- An album is deleted the same way, and by the same batch, so that "delete this
-- album and its photos" is one operation with one undo rather than two that can
-- half-succeed.
--
-- Soft here too, though nothing browses deleted albums: the alternative is
-- recreating the row and its entire membership from something the client held
-- in memory, which is an undo that gets worse the bigger the album is.
alter table albums
    add column deleted_at   timestamptz,
    add column purge_after  timestamptz,
    add column delete_batch uuid;

create index albums_purge_due_idx on albums (purge_after) where deleted_at is not null;
create index albums_delete_batch_idx on albums (delete_batch) where delete_batch is not null;

-- What the archive used to hold and deliberately does not any more.
--
-- Without this, purging is a suggestion. The phone still has the photograph,
-- sync/check asks "do you have these bytes", the archive truthfully says no,
-- and the next backup restores what was just destroyed — the delete would
-- survive exactly until the next time the app was opened.
--
-- So a purge leaves the one thing that answers that question: the content key.
-- It is a few dozen bytes against the many megabytes it stands for, which is
-- what makes keeping it forever the cheap option. The filename is here for the
-- benefit of whoever later reads this table wondering what they threw away.
create table purged_content (
    sha256            text        primary key,
    md5               text        not null default '',
    byte_size         bigint      not null default 0,
    original_filename text        not null default '',
    purged_at         timestamptz not null default now()
);

-- sync/check looks these up exactly the way it looks up live content: by the
-- (md5, size) pair the phone can produce without uploading anything.
create index purged_content_md5_size_idx on purged_content (md5, byte_size)
    where md5 <> '';

-- +goose Down
drop table purged_content;
drop index albums_delete_batch_idx;
drop index albums_purge_due_idx;
alter table albums
    drop column delete_batch,
    drop column purge_after,
    drop column deleted_at;
drop index assets_delete_batch_idx;
drop index assets_purge_due_idx;
drop index assets_trash_idx;
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null and not is_overlay;
alter table assets
    drop column delete_batch,
    drop column purge_after,
    drop column deleted_at;
