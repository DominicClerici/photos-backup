-- +goose Up
-- What the browser signs in with, and what it holds afterwards.
--
-- Phase 12 authenticated the gallery with one shared password and was removed
-- for it: a house key with no revocation and no identity. This is the
-- replacement. The credential is a WebAuthn passkey — a keypair held by the
-- authenticator, of which this database sees only the public half — so unlike
-- the password there is nothing here that can be replayed against the server,
-- and unlike the password there is nothing to phish.
--
-- The same rule the devices table follows applies to everything here that is a
-- secret: sessions, enrollment codes and recovery codes are stored as sha256
-- digests of values handed out exactly once. A dump of this database cannot be
-- replayed to sign in.

-- The WebAuthn user handle, and the reason this table has exactly one row.
--
-- A discoverable credential is bound to a (rp id, user handle) pair, and the
-- handle has to outlive any individual passkey — losing it would orphan every
-- credential registered under it. It is therefore generated once, on first use,
-- and never rewritten. The archive has one user by design (PROJECT.md §1), so a
-- singleton is the honest shape rather than a users table with a check
-- constraint pretending otherwise.
create table web_identity (
    -- The singleton guard: `true` is the only value that satisfies the check,
    -- and it is the primary key, so a second row cannot be inserted.
    only_row     boolean     primary key default true check (only_row),
    user_handle  bytea       not null,
    display_name text        not null default 'photobackup',
    created_at   timestamptz not null default now()
);

-- Registered passkeys. More than one row is expected and wanted: a phone, a
-- laptop, and a hardware key are three authenticators for one identity, and
-- having a second is what makes losing the first survivable.
create table web_passkeys (
    id            uuid        primary key default gen_random_uuid(),
    -- The raw credential id the authenticator returns. Unique because it is
    -- what a login is resolved by, and a collision would be an ambiguity in
    -- whose key just signed.
    credential_id bytea       not null unique,
    -- The library's webauthn.Credential, marshalled. Stored whole rather than
    -- decomposed into columns because it is opaque to every query here — only
    -- the WebAuthn library ever interprets it — and because the shape belongs
    -- to that library rather than to this schema. Splitting it out would mean a
    -- migration every time it gained a field.
    --
    -- The signature counter lives inside it, and is rewritten on every login:
    -- see webauth.Store.RecordLogin. A counter that goes backwards is how a
    -- cloned authenticator announces itself.
    credential    jsonb       not null,
    label         text        not null default '',
    -- What the authenticator said it speaks: "internal" for a platform
    -- credential, "usb"/"nfc" for a security key. Recorded and never acted on —
    -- the browser decides how to reach a key, and a stored hint that disagreed
    -- with reality would be worse than none. It is here so that `photobackup
    -- passkey list` can tell a Touch ID key from a YubiKey.
    transports    text        not null default '',
    created_at    timestamptz not null default now(),
    last_used_at  timestamptz,
    -- Revocation rather than deletion, matching devices: a passkey that was
    -- withdrawn is exactly what somebody auditing this wants to still be able
    -- to see, and the sessions it minted reference it.
    revoked_at    timestamptz
);

-- Enrollment codes: the bootstrap. Registering a passkey requires proving
-- filesystem access to this database by running `photobackup passkey add`,
-- which is the same authority `photobackup pair` already carries and the same
-- reasoning devices.Store.CreateCode is built on.
--
-- Without this there is no way to register the first credential that is not
-- either an open endpoint or a chicken-and-egg problem. With it, an
-- unenrolled server is closed rather than open: see api.Server.handleAuthStatus.
create table web_enrollments (
    code_sha256 bytea       primary key,
    expires_at  timestamptz not null,
    created_at  timestamptz not null default now(),
    used_at     timestamptz,
    used_by     uuid        references web_passkeys (id) on delete set null,
    label       text        not null default ''
);

create index web_enrollments_open_idx on web_enrollments (expires_at) where used_at is null;

-- Recovery codes. The passkey this archive will actually be used with is a
-- platform credential synced through iCloud Keychain, so the realistic way to
-- lose it is losing the Apple account rather than losing a device. These are
-- the way back in when that happens.
--
-- 128 bits of randomness each, which is why sha256 is the right digest here for
-- the same reason it is right for a device token: there is no dictionary to
-- attack and nothing for a password KDF to slow down. Single use, and redeeming
-- one is expected to be followed immediately by registering a new passkey.
create table web_recovery_codes (
    code_sha256 bytea       primary key,
    created_at  timestamptz not null default now(),
    used_at     timestamptz,
    used_from   text        not null default ''
);

-- Browser sessions. Kept in Postgres rather than in the serving process's
-- memory — which is where Phase 12 kept them — for two things that memory
-- cannot give: `photobackup web --revoke` can end one from another terminal,
-- and a photod restart during an evening's browsing does not sign you out.
--
-- The security properties are unchanged by that choice: the cookie's value is
-- 256 random bits and only its digest is here, so this table is as unreplayable
-- as devices is.
create table web_sessions (
    token_sha256 bytea       primary key,
    -- Null when the session was minted by a recovery code, and null again if
    -- the passkey behind it is later deleted. Nothing authenticates against
    -- this column; it is here so a session can be explained.
    passkey_id   uuid        references web_passkeys (id) on delete set null,
    -- 'passkey' or 'recovery'. A recovery session is a real session and is
    -- deliberately not weaker, because the thing it exists to let you do —
    -- register a replacement passkey — needs the same authority.
    method       text        not null default 'passkey',
    created_at   timestamptz not null default now(),
    -- Both clocks, because they answer different questions. expires_at is the
    -- absolute cap and never moves; last_seen_at drives the idle timeout and is
    -- rewritten as the session is used. A session dies at whichever comes
    -- first. See webauth.Store.Authenticate.
    expires_at   timestamptz not null,
    last_seen_at timestamptz not null default now(),
    revoked_at   timestamptz,
    created_from text        not null default '',
    user_agent   text        not null default ''
);

-- What the sweep looks for, and what every authenticated request probes.
create index web_sessions_live_idx on web_sessions (expires_at) where revoked_at is null;

-- +goose Down
drop table web_sessions;
drop table web_recovery_codes;
drop table web_enrollments;
drop table web_passkeys;
drop table web_identity;
