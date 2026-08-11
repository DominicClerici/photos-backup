-- +goose Up
-- Two credentials with opposite lifetimes: a pairing code that is typed once
-- and dies, and a device token that lives until it is revoked.
--
-- Neither is stored as the value that was handed out — both are sha256 digests.
-- A dump of this database cannot be replayed to write into the archive.

create table devices (
    id           uuid        primary key default gen_random_uuid(),
    name         text        not null,
    platform     text        not null default '',
    token_sha256 bytea       not null unique,
    created_at   timestamptz not null default now(),
    -- Written at most once a minute per device. A 3GB video is ~375
    -- authenticated chunk requests, and updating this on each would be 375
    -- writes to one row to record something nothing reads more precisely than
    -- "today".
    last_seen_at timestamptz,
    revoked_at   timestamptz,
    paired_from  text        not null default ''
);

-- assets.device_id and device_assets.device_id now hold a devices.id, and
-- deliberately carry no foreign key to this table. `photobackup reindex`
-- replays manifest.jsonl into an empty database and has to restore the mapping
-- for whichever device uploaded each blob — including one since revoked and
-- deleted. A constraint here would make recovering the archive depend on rows
-- the manifest never recorded.

create table pairing_codes (
    code_sha256 bytea       primary key,
    expires_at  timestamptz not null,
    created_at  timestamptz not null default now(),
    used_at     timestamptz,
    used_by     uuid        references devices (id) on delete set null,
    label       text        not null default ''
);

-- Redemption claims a row by this predicate, so it wants an index once a few
-- hundred expired codes have accumulated.
create index pairing_codes_open_idx on pairing_codes (expires_at) where used_at is null;

-- +goose Down
drop table pairing_codes;
drop table devices;
