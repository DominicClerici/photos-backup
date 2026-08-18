-- +goose Up
-- Migration 0003 read twelve tags off an original and stored eleven of them.
-- An audit against the real 15,689-file export says what that costs, as a
-- fraction of the files actually in this archive:
--
--   the exposure — ISO, aperture, shutter, focal length            50%
--   GPS altitude                                                   57%
--   the bearing the camera faced, and the fix's own accuracy       30%
--   a video's codec, frame rate, bitrate and audio layout          32%  (every video)
--   faces something had already found and boxed                     9%
--   a caption the file itself carries                              12%
--   the colour profile a rendition has to be converted from        35%
--
-- None of it was lost — the originals are archived, so a wider tag list and a
-- requeue recovers all of it. What it was, was unqueryable: "photos taken at
-- f/1.8" and "photos on this mountain, above this altitude" were questions the
-- database could not answer about files that plainly knew.
alter table assets
    -- Everything exiftool was asked for, exactly as it answered.
    --
    -- The same bargain the import sidecar makes in 0006: the columns below are
    -- a choice about what deserves an index, that choice is wrong about
    -- something, and this is what makes being wrong survivable. It is not the
    -- disaster-recovery copy — the original is, and it outranks this — it is
    -- what stops a question about the library requiring hours of exiftool.
    add column exif_metadata jsonb,

    -- The rest of the fix. Latitude and longitude have been here since 0003;
    -- these are the other four things a phone writes down at the same instant.
    add column gps_altitude    double precision,
    add column gps_direction   double precision,
    add column gps_accuracy    double precision,
    -- The GPS receiver's own clock, which is UTC by definition. It is the one
    -- timestamp in a photo that needs nothing assumed about a timezone, which
    -- makes it the tiebreaker when exif_captured_at had to guess.
    add column gps_at          timestamptz,

    -- The exposure. Nothing in the archive branches on any of it; it is what
    -- anyone wants to read on a photo they have opened, and what a search for
    -- "the long exposures" needs.
    add column iso              int,
    add column f_number         double precision,
    add column exposure_seconds double precision,
    add column focal_length     double precision,
    add column focal_length_35  int,
    add column flash            int,

    -- The caption the file carries, kept apart from description exactly as the
    -- sidecar's coordinates are kept apart from gps_lat/gps_lon. The metadata
    -- worker rewrites its own columns on every run, so a file caption merged
    -- into description would overwrite a caption typed into Google Photos and
    -- then survive until the next reindex. It fills description only where
    -- nothing else has.
    add column exif_description text,

    -- The ICC profile's name, e.g. "Display P3". A wide-gamut original drawn as
    -- though it were sRGB is visibly wrong, so this is the fact a converter
    -- needs and has never had.
    add column color_profile text,
    -- Apple's ImageCaptureType: 10 for a Live Photo's still, 12 for a portrait.
    -- Recorded rather than interpreted, because the mapping is Apple's and
    -- undocumented, and a number we kept can be interpreted later.
    add column capture_type int,

    -- The video stream, from the container. ffprobe already reports the size
    -- and duration the worker stores; nothing recorded what the file actually
    -- was, so "which of these need re-encoding" had no answer.
    add column video_codec    text,
    add column frame_rate     double precision,
    add column bitrate        bigint,
    add column audio_codec    text,
    add column audio_channels int,

    -- Face boxes as fractions of the image, from XMP's region list — put there
    -- by a phone or by Google, not by us. No names survive in the real export,
    -- so this is geometry and not identity, and it is stored so that v2's own
    -- face work has something to reconcile against rather than start from.
    add column faces jsonb,

    -- What the source called this asset: 'screenshot', 'portrait', 'burst',
    -- 'panorama', 'slomo'. PhotoKit knows all of it about a photo on the phone
    -- and nothing ever asked; a Takeout knows none of it. Kept as text so a
    -- second source can name a kind this one has never heard of.
    add column subtypes text[] not null default '{}';

-- "Photos with faces" is the one question here that pays for an index, and it
-- is the question v2 opens with.
create index assets_faces_idx on assets ((faces is not null)) where faces is not null;

-- Everything above is read by the metadata job, and every asset already in the
-- archive was read by a version of it that did not know these tags existed.
-- Requeueing is how they get filled: the same path a fresh upload takes, over
-- originals that never went anywhere.
insert into jobs (kind, asset_id)
select 'metadata', id from assets
on conflict (asset_id, kind) do update
    set state = 'pending', run_after = now(), attempts = 0, last_error = null;

-- +goose Down
drop index assets_faces_idx;
alter table assets
    drop column subtypes,
    drop column faces,
    drop column audio_channels,
    drop column audio_codec,
    drop column bitrate,
    drop column frame_rate,
    drop column video_codec,
    drop column capture_type,
    drop column color_profile,
    drop column exif_description,
    drop column flash,
    drop column focal_length_35,
    drop column focal_length,
    drop column exposure_seconds,
    drop column f_number,
    drop column iso,
    drop column gps_at,
    drop column gps_accuracy,
    drop column gps_direction,
    drop column gps_altitude,
    drop column exif_metadata;
