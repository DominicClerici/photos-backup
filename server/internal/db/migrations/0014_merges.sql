-- +goose Up
-- Two archives' worth of the same photographs, and one recording in six files.
--
-- This library was assembled from four sources — a phone, a Google Takeout, a
-- Snapchat export in two halves — and the same moment reached it by more than
-- one road. Content addressing already caught every case where the *bytes*
-- agreed: `assets.sha256` is unique, and across 23,000 items there is not one
-- md5 collision left to find. What it cannot catch is the same photograph
-- recompressed, resized, or re-saved, which is what an export does to
-- everything it touches. Those are different bytes and the same picture, and
-- nothing here could see it.
--
-- The other half is narrower and stranger. Snapchat caps a memory at ten
-- seconds, so a longer recording is exported as several files with no marking
-- of any kind to say they were once one thing — no shared identifier, no
-- sequence number, nothing in the filename. What gives them away is
-- memories_history.json: consecutive segments of one recording are written
-- exactly ten seconds apart, to the second, while two memories that merely
-- happened near each other are not. In this library that is 287 links at
-- exactly 10.00s against single digits at every other spacing.
--
-- Both end in the same place — several rows that should be one item — so both
-- are recorded here, in one pair of tables, and differ only in how the group
-- was found and what resolving it does.

-- What a photograph looks like, in 64 bits, twice.
--
-- Two independent hashes rather than one, because they fail differently. A
-- difference hash is a gradient: it survives compression and resizing and is
-- blind to overall brightness, which also means a dark frame and a slightly
-- less dark frame of the same nothing agree completely. A perceptual hash is
-- the low frequencies of a DCT: it describes structure, and it is the one that
-- notices two flat images are flat in different ways. Requiring both to agree
-- is what keeps a wall and a different wall out of the same group.
--
-- Neither is stored to be looked at. They exist to be XORed against each other
-- and popcounted, which is why they are bigints and not bit(64) — Postgres
-- will happily hold either, and Go only ever wants the integer.
create table asset_signatures (
    asset_id uuid primary key references assets (id) on delete cascade,

    -- Which algorithm produced these. The whole point of writing it down is
    -- that the thresholds below are guesses that will be adjusted: bump this
    -- and every signature is stale, requeueable, and recomputable from the
    -- originals, which are the only thing here that is not derived.
    version int not null,

    dhash bigint not null,
    phash bigint not null,

    -- Width over height, after orientation. Cheap, and it settles the most
    -- common false positive there is: two photographs whose 32x32 grey
    -- reductions agree because both were reduced from shapes that share
    -- nothing.
    aspect double precision not null,

    -- A video, sampled. See internal/merge: twenty frames at even fractions of
    -- the running time, each hashed exactly as a still is, compared position
    -- for position against another video of the same length.
    --
    -- Time-based rather than frame-based, which is the whole reason it works
    -- across a re-encode: a clip at 15fps and the same clip at 30fps put
    -- different frames at index seven and the same picture at 35% of the way
    -- through. Empty for a still, and the two columns above are then the
    -- entirety of its signature.
    frame_dhashes bigint[] not null default '{}',
    frame_phashes bigint[] not null default '{}',

    computed_at timestamptz not null default now()
);

-- The scan reads every signature of a live asset in one pass and clusters them
-- in memory — twenty thousand 64-bit integers is a rounding error of RAM, and
-- there is no predicate here worth an index. What this one is for is the
-- opposite question, asked per asset by the review page: what does this look
-- like, and is it stale.
create index asset_signatures_version_idx on asset_signatures (version);

-- A set of assets that ought to be one asset.
--
-- Two kinds, one table, because everything after the finding is shared: a group
-- is proposed, it is resolved or refused, resolving it elects one asset and
-- moves the rest to the trash, and the trash batch it used is the undo. Only
-- the finding differs, and only in the obvious way — one kind is found by
-- looking at pixels and the other by looking at timestamps.
--
-- What is deliberately not shared is consent. A `video-segments` group is
-- resolved by the worker without anyone being asked, because the evidence is a
-- ten-second grid in a document Snapchat wrote and the answer is not a matter
-- of taste. A `duplicate` group is a judgement about which of four nearly
-- identical photographs is the one worth keeping, and nothing here is entitled
-- to make it.
create table merge_groups (
    id uuid primary key default gen_random_uuid(),

    kind  text not null check (kind in ('duplicate', 'video-segments')),
    state text not null default 'pending'
                    check (state in ('pending', 'merged', 'dismissed')),

    -- The member set, hashed. It is what makes a rescan idempotent: the scan
    -- runs over the whole library every time, proposes the same groups it
    -- proposed last week, and this constraint turns all but the first into a
    -- no-op instead of a second copy of the same question.
    --
    -- It also makes a dismissal stick, but only exactly: a group somebody
    -- refused and a group with one more photograph in it are different
    -- fingerprints, and the second would come back. The scan therefore refuses
    -- to link two assets that are already together in a dismissed group, which
    -- is the rule that actually holds — see merge.Scan. This column is the
    -- cheap half of the same idea.
    fingerprint text not null unique,

    detected_at timestamptz not null default now(),
    resolved_at timestamptz,

    -- What the group became. For a duplicate it is one of the members: the copy
    -- that was kept, and the row every other member's albums, favourite and
    -- caption were carried onto. For a set of video segments it is an asset
    -- that did not exist until the merge ran — the joined recording, archived
    -- like any other original — and it is not a member of the group at all.
    --
    -- Null while the group is pending, and null forever on a dismissal.
    keeper_asset_id uuid references assets (id) on delete set null,

    -- The delete batch the resolution used, which is how it is undone: exactly
    -- the rows that operation trashed, no more, restored by db.RestoreBatch. It
    -- is the same handle the toast under a delete holds, for the same reason —
    -- between the merge and the regret, every position in the timeline moved.
    delete_batch uuid
);

-- The review page's one query: the pending groups, oldest first. Partial,
-- because a library that has been tidied has thousands of resolved rows here
-- and nothing wants to read any of them.
create index merge_groups_pending_idx on merge_groups (kind, detected_at)
    where state = 'pending';

create table merge_members (
    group_id uuid not null references merge_groups (id) on delete cascade,
    asset_id uuid not null references assets (id) on delete cascade,

    -- Where this asset sits in the group.
    --
    -- For video segments it is load-bearing and it is chronological: it is the
    -- order the parts are concatenated in, and getting it wrong produces a
    -- minute of video in which time runs backwards twice. For duplicates it is
    -- the order they are offered in, best first, which is a suggestion.
    position int not null,

    primary key (group_id, asset_id)
);

-- "Is this photograph already spoken for", asked once per candidate by the
-- scan, and asked again by the review page to draw a group.
create index merge_members_asset_idx on merge_members (asset_id);

-- Two more kinds of background work.
--
--   signature  decode one original and write the row above. Its own pool, for
--              the reason the other two have their own pools: it is a full
--              decode of every file in the archive, it is nobody's blocker,
--              and behind a queue shared with thumbnails it would stall the
--              gallery for an hour to answer a question nobody has asked yet.
--   merge      concatenate a set of video segments into one archived original.
--              ffmpeg, so it belongs with the transcodes.
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback', 'signature', 'merge'));

-- Every asset already in the archive was ingested by a worker that had never
-- heard of a signature. Same path a fresh upload takes, over originals that
-- never went anywhere — exactly what 0007 did when the tag list grew.
--
-- Not the vault. A hidden photograph has no plaintext on disk to decode, and
-- the whole point of it is that this server cannot look; a signature is a
-- description of the picture, and computing one would be the server writing
-- down what it promised not to know.
insert into jobs (kind, asset_id)
select 'signature', id from assets where vault = '' and deleted_at is null
on conflict (asset_id, kind) do nothing;

-- +goose Down
delete from jobs where kind in ('signature', 'merge');
alter table jobs drop constraint jobs_kind_check;
alter table jobs add constraint jobs_kind_check
    check (kind in ('metadata', 'playback'));
drop table merge_members;
drop table merge_groups;
drop table asset_signatures;
