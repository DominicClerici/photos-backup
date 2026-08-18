-- +goose Up
create table assets (
    id                uuid primary key default gen_random_uuid(),
    sha256            text        not null unique,
    md5               text        not null,
    byte_size         bigint      not null,
    original_filename text        not null,
    ext               text        not null,
    content_type      text        not null,
    captured_at       timestamptz,
    uploaded_at       timestamptz not null default now(),
    device_id         text        not null,
    local_id          text        not null
);

-- The gallery orders by capture time, falling back to arrival for assets whose
-- EXIF date never made it across.
create index assets_timeline_idx on assets (captured_at desc nulls last, uploaded_at desc);

-- +goose Down
drop table assets;
