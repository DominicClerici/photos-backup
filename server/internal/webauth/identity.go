package webauth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/code"
)

// handleBytes is the size of the WebAuthn user handle. The spec caps it at 64
// and recommends using all of it; nothing reads it but the authenticator, and
// it is generated once per archive.
const handleBytes = 64

// Identity returns the archive's user, creating it on first call.
//
// Creation is an upsert that loses a race rather than winning one: two requests
// arriving at an empty table both insert, one conflicts, and both then read the
// row that landed. Which of the two handles survives does not matter — what
// matters is that it never changes afterwards, because every passkey registered
// under it would stop resolving.
func (s *Store) Identity(ctx context.Context) (Identity, error) {
	handle := make([]byte, handleBytes)
	if _, err := rand.Read(handle); err != nil {
		return Identity{}, fmt.Errorf("generate user handle: %w", err)
	}

	const upsert = `
		insert into web_identity (user_handle) values ($1)
		on conflict (only_row) do update set user_handle = web_identity.user_handle
		returning user_handle, display_name`

	var id Identity
	if err := s.pool.QueryRow(ctx, upsert, handle).Scan(&id.Handle, &id.DisplayName); err != nil {
		return Identity{}, fmt.Errorf("resolve web identity: %w", err)
	}

	creds, err := s.credentials(ctx)
	if err != nil {
		return Identity{}, err
	}
	id.Creds = creds
	return id, nil
}

// credentials loads the live passkeys as the WebAuthn library's own type.
//
// Revoked ones are excluded, which is what makes revocation mean anything: a
// withdrawn credential is absent from the allow list a registration excludes
// against, and absent from the set a login is resolved against.
func (s *Store) credentials(ctx context.Context) ([]webauthn.Credential, error) {
	const query = `select credential from web_passkeys where revoked_at is null order by created_at`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("load passkeys: %w", err)
	}
	defer rows.Close()

	var out []webauthn.Credential
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan passkey: %w", err)
		}
		var cred webauthn.Credential
		if err := json.Unmarshal(raw, &cred); err != nil {
			return nil, fmt.Errorf("decode passkey: %w", err)
		}
		out = append(out, cred)
	}
	return out, rows.Err()
}

// HasPasskey reports whether this archive has any live credential at all.
//
// The whole of the bootstrap decision rests on this: false means nothing can
// sign in, which is why an unenrolled server serves the enrollment page and
// refuses everything else rather than falling open.
func (s *Store) HasPasskey(ctx context.Context) (bool, error) {
	const query = `select exists (select 1 from web_passkeys where revoked_at is null)`
	var any bool
	if err := s.pool.QueryRow(ctx, query).Scan(&any); err != nil {
		return false, fmt.Errorf("count passkeys: %w", err)
	}
	return any, nil
}

// AddPasskey stores a credential the registration ceremony has just verified,
// and claims the enrollment code that authorised it in the same transaction.
//
// The two are one write because they authorise each other: a credential stored
// without claiming a code would be a passkey nobody approved, and a code
// claimed without storing a credential would burn the only way to register one.
func (s *Store) AddPasskey(ctx context.Context, cred *webauthn.Credential, label, enrollCode string) (Passkey, error) {
	raw, err := json.Marshal(cred)
	if err != nil {
		return Passkey{}, fmt.Errorf("encode passkey: %w", err)
	}
	normalized, err := code.Normalize(enrollCode)
	if err != nil {
		return Passkey{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Passkey{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insert = `
		insert into web_passkeys (credential_id, credential, label, transports)
		values ($1, $2, $3, $4)
		returning id::text, label, created_at, last_used_at, revoked_at, transports`
	p, err := scanPasskey(tx.QueryRow(ctx, insert, cred.ID, raw,
		truncate(label, 64), transportsOf(cred)))
	if err != nil {
		return Passkey{}, fmt.Errorf("store passkey: %w", err)
	}

	// Conditional UPDATE rather than a read-then-write, so two browsers racing
	// on one code cannot both register: the second matches no row and its
	// insert is rolled back with it. The same shape devices.Store.Pair uses.
	const claim = `
		update web_enrollments set used_at = now(), used_by = $2
		where code_sha256 = $1 and used_at is null and expires_at > now()`
	tag, err := tx.Exec(ctx, claim, digestOf(normalized), p.ID)
	if err != nil {
		return Passkey{}, fmt.Errorf("claim enrollment code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Passkey{}, ErrBadCode
	}

	if err := tx.Commit(ctx); err != nil {
		return Passkey{}, fmt.Errorf("commit transaction: %w", err)
	}
	return p, nil
}

// AddPasskeyAuthorized stores a credential without a code, for a browser that
// is already signed in.
//
// Registering a second authenticator from an open session is the normal way to
// add the laptop after the phone, and requiring a trip to the terminal for it
// would be ceremony: whoever holds a live session can already read and delete
// the entire archive, so they can already do strictly more than register a key.
func (s *Store) AddPasskeyAuthorized(ctx context.Context, cred *webauthn.Credential, label string) (Passkey, error) {
	raw, err := json.Marshal(cred)
	if err != nil {
		return Passkey{}, fmt.Errorf("encode passkey: %w", err)
	}

	const insert = `
		insert into web_passkeys (credential_id, credential, label, transports)
		values ($1, $2, $3, $4)
		returning id::text, label, created_at, last_used_at, revoked_at, transports`
	p, err := scanPasskey(s.pool.QueryRow(ctx, insert, cred.ID, raw,
		truncate(label, 64), transportsOf(cred)))
	if err != nil {
		return Passkey{}, fmt.Errorf("store passkey: %w", err)
	}
	return p, nil
}

// RecordLogin writes back what the assertion changed: the signature counter,
// and the fact that this key was used.
//
// The counter is the reason this is not merely bookkeeping. An authenticator
// increments it on every assertion, so a value at or below the stored one means
// two copies of a private key that was supposed to exist once. Platform
// passkeys synced through iCloud legitimately report zero always — Apple does
// not implement the counter — so a zero counter is not evidence of anything and
// is left alone. A counter that was moving and then went backwards is.
func (s *Store) RecordLogin(ctx context.Context, cred *webauthn.Credential) error {
	raw, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("encode passkey: %w", err)
	}

	const update = `
		update web_passkeys
		set credential = $2, last_used_at = now()
		where credential_id = $1 and revoked_at is null
		returning id::text`
	var id string
	err = s.pool.QueryRow(ctx, update, cred.ID, raw).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchPasskey
	}
	if err != nil {
		return fmt.Errorf("record passkey login: %w", err)
	}
	if cred.Authenticator.CloneWarning {
		return ErrCloned
	}
	return nil
}

// PasskeyIDFor resolves a credential to the row it is stored in, so a session
// can name what opened it.
func (s *Store) PasskeyIDFor(ctx context.Context, credentialID []byte) (string, error) {
	const query = `select id::text from web_passkeys where credential_id = $1`
	var id string
	err := s.pool.QueryRow(ctx, query, credentialID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSuchPasskey
	}
	if err != nil {
		return "", fmt.Errorf("look up passkey: %w", err)
	}
	return id, nil
}

// Passkeys lists every registered credential, newest first, revoked ones
// included — a revoked key is exactly what somebody auditing this wants to see.
func (s *Store) Passkeys(ctx context.Context) ([]Passkey, error) {
	const query = `
		select id::text, label, created_at, last_used_at, revoked_at, transports
		from web_passkeys order by created_at desc`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()

	var out []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan passkey: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevokePasskey withdraws a credential and, in the same transaction, every
// session it opened.
//
// Revoking the key without the sessions would be theatre: the browser that
// signed in with it holds a cookie that outlives the revocation by up to twelve
// hours, which is exactly the window somebody revoking a lost laptop's key is
// trying to close.
//
// Idempotent: revoking an already-revoked passkey keeps the original timestamp.
func (s *Store) RevokePasskey(ctx context.Context, id string) (Passkey, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Passkey{}, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const update = `
		update web_passkeys set revoked_at = coalesce(revoked_at, now())
		where id = $1
		returning id::text, label, created_at, last_used_at, revoked_at, transports`
	p, err := scanPasskey(tx.QueryRow(ctx, update, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Passkey{}, 0, ErrNoSuchPasskey
	}
	if err != nil {
		// An id that is not a uuid arrives as a cast error rather than no rows,
		// and reporting that as a database failure would be a lie.
		if isBadUUID(err) {
			return Passkey{}, 0, ErrNoSuchPasskey
		}
		return Passkey{}, 0, fmt.Errorf("revoke passkey: %w", err)
	}

	const kill = `
		update web_sessions set revoked_at = now()
		where passkey_id = $1 and revoked_at is null`
	tag, err := tx.Exec(ctx, kill, p.ID)
	if err != nil {
		return Passkey{}, 0, fmt.Errorf("revoke sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Passkey{}, 0, fmt.Errorf("commit transaction: %w", err)
	}
	return p, tag.RowsAffected(), nil
}

// transportsOf flattens what the authenticator said it speaks into one column.
// Reported, never acted on: the browser decides how to reach a key, and a
// stored hint that disagreed with reality would be worse than none.
func transportsOf(cred *webauthn.Credential) string {
	if len(cred.Transport) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		if s := strings.TrimSpace(string(t)); s != "" {
			parts = append(parts, s)
		}
	}
	return truncate(strings.Join(parts, ","), 64)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPasskey(s scanner) (Passkey, error) {
	var p Passkey
	err := s.Scan(&p.ID, &p.Label, &p.CreatedAt, &p.LastUsedAt, &p.RevokedAt, &p.Transports)
	return p, err
}

func isBadUUID(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid input syntax for type uuid") ||
		strings.Contains(msg, "invalid UUID")
}
