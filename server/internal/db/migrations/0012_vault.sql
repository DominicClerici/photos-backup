-- +goose Up
-- The archive learns to keep a secret.
--
-- Everything up to here has been about what the gallery *shows*. The trash was
-- the first thing that took a photograph out of the timeline, and it did it
-- with one nullable column, because the only party being kept away from those
-- rows was a SELECT. This is the first thing that takes a photograph away from
-- somebody holding the disk.
--
-- Two buckets, Archive and Hidden, which are the same mechanism twice. They are
-- separate because the reasons are separate — one is "I have seen enough of
-- this", the other is "this is nobody else's business" — and folding them into
-- one destination with a flag would be the kind of tidiness that makes a
-- product worse. Everything below says `vault` when it means either of them.
--
-- What is encrypted is the picture and everything that describes it: the
-- original, every rendition made from it, and the metadata that would let
-- somebody reconstruct what the picture was without ever seeing it. What is
-- not, deliberately, is the content key — see the note on the scrub below.

-- The bucket a row is in, or empty for the library.
--
-- Text rather than a boolean pair or an enum: two buckets today, the same code
-- path for both, and a third would be a value rather than a migration. An enum
-- would be the same thing with a type to alter.
alter table assets
    add column vault text not null default '' check (vault in ('', 'archive', 'hidden')),
    -- When it went in. Not a retention — nothing expires out of the vault —
    -- it is what the vault's own timeline orders by when the capture time it
    -- would rather use has been encrypted away.
    add column vaulted_at timestamptz,
    -- The operation that put it there, for the same reason delete_batch exists:
    -- one click can hide four hundred photographs across nine years, and the
    -- Undo in the toast has to put back exactly those.
    add column vault_batch uuid;

-- Every visibility predicate in this schema is a partial index predicate too,
-- so a new term means rebuilding them. Fourth time: 0005, 0006, 0009, 0011.
--
-- The vault is not the trash and cannot be reached through it. An item in the
-- vault is out of the library *and* out of Recently Deleted, which is why the
-- term goes on both indexes rather than only the first — a hidden photograph
-- that turned up in the trash would be a hole in the whole point of it.
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null
      and not is_overlay and deleted_at is null and vault = '';

drop index assets_trash_idx;
create index assets_trash_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null
      and not is_overlay and deleted_at is not null and vault = '';

-- The vault's own scope. Ordered by when it was hidden rather than by
-- sort_time, because sort_time is derived from a capture time this row no
-- longer has: the encrypted copy knows when the photograph was taken, and the
-- row does not.
--
-- Which is also why this index is only ever used to *find* the rows. What
-- orders them for a grid is decided after they are decrypted, in memory, by
-- something holding the key. See internal/vault.
create index assets_vault_idx on assets (vault, vaulted_at desc, id desc) where vault <> '';
create index assets_vault_batch_idx on assets (vault_batch) where vault_batch is not null;

-- The vault's identity, and the one row in this schema that is a secret rather
-- than a fact about a photograph.
--
-- A keypair rather than a password-derived key directly, because adding to the
-- vault and reading it are not the same permission. Hiding a photograph has to
-- work at the moment somebody decides to hide it, which is a right-click in a
-- gallery that has been open all afternoon — asking for a password there would
-- teach people to leave the vault unlocked, which is worse than every property
-- the password was buying. So the public key is in the clear and anything can
-- encrypt with it; the private key is sealed under the password and nothing
-- can read a single byte back without it.
--
-- One row, enforced by the check. Two vaults would be two passwords, and the
-- two buckets deliberately share one.
create table vault_secret (
    id smallint primary key default 1 check (id = 1),
    -- The X25519 public key. Public in the ordinary sense: it is what makes
    -- "hide this" work while the vault is locked, and it is useless for
    -- reading anything.
    public_key bytea not null,
    -- Argon2id, with the parameters it was run with rather than the parameters
    -- the current build prefers. Raising the cost later must not lock somebody
    -- out of a vault sealed under the old ones, and re-deriving under the new
    -- ones is something the password change already does.
    kdf_salt    bytea not null,
    kdf_time    int   not null,
    kdf_memory  int   not null,
    kdf_threads int   not null,
    -- The X25519 private key under AES-256-GCM, keyed by the password. The
    -- authentication tag is the whole of how a wrong password is recognised —
    -- there is no separate verifier to get out of step with the ciphertext, and
    -- no way to test a guess more cheaply than by doing the Argon2id.
    sealed_private bytea       not null,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now()
);

-- Everything the row above no longer says about a photograph.
--
-- One sealed document per asset, holding the filename, the capture time, the
-- camera, the coordinates, the caption, the albums it was in, the people in it,
-- and the raw EXIF — the whole of what the scrub takes out of `assets`. It is
-- sealed to the vault's public key, so this table can be written by something
-- that cannot read it.
--
-- It is also the restore. Putting a photograph back is not a matter of
-- remembering that it used to be in an album: it is this document, opened, and
-- written back into the columns and the membership tables it came out of.
create table vault_items (
    asset_id uuid primary key references assets (id) on delete cascade,
    sealed   bytea       not null,
    added_at timestamptz not null default now()
);

-- An album or a person moved into the vault is not encrypted, and that is a
-- deliberate asymmetry rather than an oversight.
--
-- What is being protected is the photographs. An album is a title and a
-- membership list; the membership went into the sealed documents above with the
-- photographs themselves, and the title is a word. Encrypting the word would
-- mean the vault could not draw its own collections page without being
-- unlocked, which it has to be anyway to draw a single thumbnail — so it would
-- buy nothing and cost the one thing that has to work while locked, which is
-- hiding an album in the first place.
--
-- The cost is stated plainly: somebody with the database can read the titles of
-- hidden albums and the names of hidden people. Not which photographs are in
-- them, not what they look like, not when they were taken.
alter table albums
    add column vault text not null default '' check (vault in ('', 'archive', 'hidden')),
    add column vaulted_at timestamptz,
    add column vault_batch uuid;

create index albums_vault_idx on albums (vault) where vault <> '';
create index albums_vault_batch_idx on albums (vault_batch) where vault_batch is not null;

-- A person has never had a table — a name is a row in asset_people and nothing
-- else — so hiding one needs somewhere to say so.
--
-- Without it, hiding a person would be indistinguishable from every one of
-- their photographs happening to be hidden, and the day an import tagged that
-- name onto a new photograph they would silently reappear in the library. The
-- row is what makes "this person is hidden" a decision that outlives the
-- photographs it was made about.
create table vault_people (
    name       text        primary key,
    vault      text        not null check (vault in ('archive', 'hidden')),
    vaulted_at timestamptz not null default now(),
    vault_batch uuid
);

create index vault_people_batch_idx on vault_people (vault_batch) where vault_batch is not null;

-- +goose Down
drop table vault_people;
drop index albums_vault_batch_idx;
drop index albums_vault_idx;
alter table albums
    drop column vault_batch,
    drop column vaulted_at,
    drop column vault;
drop table vault_items;
drop table vault_secret;
drop index assets_vault_batch_idx;
drop index assets_vault_idx;
drop index assets_trash_idx;
create index assets_trash_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null
      and not is_overlay and deleted_at is not null;
drop index assets_timeline_visible_idx;
create index assets_timeline_visible_idx on assets (sort_time desc, id desc)
    where live_parent_local_id = '' and live_parent_asset_id is null
      and not is_overlay and deleted_at is null;
alter table assets
    drop column vault_batch,
    drop column vaulted_at,
    drop column vault;
