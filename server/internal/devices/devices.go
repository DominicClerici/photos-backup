// Package devices owns the two credentials a phone needs to write to the
// archive: the pairing code it is shown once, and the long-lived token it
// authenticates every upload with.
//
// The shape is deliberately boring. A code is typed in, redeemed exactly once,
// and exchanged for a token; the token is a bearer credential with no expiry and
// no refresh, revocable from the command line. A backup that stops working
// because a token quietly aged out is a worse failure than one that keeps
// working until somebody says otherwise.
package devices

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCodeTTL is how long a pairing code stands. Long enough to walk to the
// phone and type it, short enough that a code left on a terminal overnight is
// worth nothing.
const DefaultCodeTTL = 10 * time.Minute

// touchInterval is how often a device's last_seen_at is allowed to be written.
const touchInterval = time.Minute

var (
	// ErrBadCode means the code is well-formed but not redeemable: unknown,
	// expired, or already used. The three are not distinguished, because telling
	// a caller which one it was is telling them something about codes they do
	// not hold.
	ErrBadCode = errors.New("devices: pairing code is not valid")
	// ErrBadToken means no device holds that token.
	ErrBadToken = errors.New("devices: token is not recognised")
	// ErrRevoked means the token was valid and has been withdrawn. Separate from
	// ErrBadToken so the phone can say "unpaired" instead of "not recognised" —
	// whoever is holding the token already knows they hold it, so the
	// distinction gives nothing away.
	ErrRevoked = errors.New("devices: device has been unpaired")
	// ErrNoDevice means no device exists under that id.
	ErrNoDevice = errors.New("devices: no such device")
)

// Device is one paired client.
type Device struct {
	ID         string
	Name       string
	Platform   string
	CreatedAt  time.Time
	LastSeenAt *time.Time
	RevokedAt  *time.Time
	PairedFrom string
}

// Revoked reports whether this device's token has been withdrawn.
func (d Device) Revoked() bool { return d.RevokedAt != nil }

type Store struct {
	pool *pgxpool.Pool

	// touched throttles last_seen_at writes. Per process rather than per row,
	// which is right for the one photod that owns this database and would be
	// wrong the moment there were two.
	touched sync.Map
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// CreateCode mints a pairing code and returns it in normalized form. This is
// the only time the code exists outside the caller's terminal: the database
// keeps a digest.
//
// A 40-bit code hashed with sha256 is brute-forceable offline, so the digest
// protects it from a database dump only for as long as the code is live. That is
// the right trade for something that expires in ten minutes and can be redeemed
// once — the credential worth protecting properly is the token it becomes, and
// that one is 256 bits.
func (s *Store) CreateCode(ctx context.Context, ttl time.Duration, label string) (code string, expiresAt time.Time, err error) {
	if ttl <= 0 {
		ttl = DefaultCodeTTL
	}
	code, err = newCode()
	if err != nil {
		return "", time.Time{}, err
	}

	const insert = `
		insert into pairing_codes (code_sha256, expires_at, label)
		values ($1, now() + $2::interval, $3)
		returning expires_at`
	if err := s.pool.QueryRow(ctx, insert, digestOf(code), ttl.String(), label).Scan(&expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("store pairing code: %w", err)
	}
	return code, expiresAt, nil
}

// Pair redeems a code and returns the new device with its token. The token is
// returned here and never again; only its digest is kept.
//
// The code is claimed in the same transaction that creates the device, and the
// claim is a conditional UPDATE, so two phones racing on one code cannot both
// win: the second one's UPDATE matches no row and its device insert is rolled
// back with it.
func (s *Store) Pair(ctx context.Context, rawCode, name, platform, from string) (Device, string, error) {
	code, err := NormalizeCode(rawCode)
	if err != nil {
		return Device{}, "", err
	}
	token, digest, err := newToken()
	if err != nil {
		return Device{}, "", err
	}
	if name == "" {
		name = "unnamed device"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Device{}, "", fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var d Device
	const insert = `
		insert into devices (name, platform, token_sha256, paired_from)
		values ($1, $2, $3, $4)
		returning id::text, created_at`
	if err := tx.QueryRow(ctx, insert, name, platform, digest, from).Scan(&d.ID, &d.CreatedAt); err != nil {
		return Device{}, "", fmt.Errorf("create device: %w", err)
	}
	d.Name, d.Platform, d.PairedFrom = name, platform, from

	const claim = `
		update pairing_codes set used_at = now(), used_by = $2
		where code_sha256 = $1 and used_at is null and expires_at > now()`
	tag, err := tx.Exec(ctx, claim, digestOf(code), d.ID)
	if err != nil {
		return Device{}, "", fmt.Errorf("redeem pairing code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Device{}, "", ErrBadCode
	}

	if err := tx.Commit(ctx); err != nil {
		return Device{}, "", fmt.Errorf("commit transaction: %w", err)
	}
	return d, token, nil
}

// Authenticate resolves a bearer token to the device holding it.
//
// The lookup is by digest, so no secret-dependent comparison happens in this
// process at all — there is no string to compare in variable time, only an index
// probe for a value the caller already supplied.
func (s *Store) Authenticate(ctx context.Context, token string) (Device, error) {
	if token == "" {
		return Device{}, ErrBadToken
	}

	const query = `
		select id::text, name, platform, created_at, last_seen_at, revoked_at, paired_from
		from devices where token_sha256 = $1`
	d, err := scanDevice(s.pool.QueryRow(ctx, query, digestOf(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrBadToken
	}
	if err != nil {
		return Device{}, fmt.Errorf("look up device token: %w", err)
	}
	if d.Revoked() {
		return d, ErrRevoked
	}
	return d, nil
}

// Touch records that a device is alive, at most once a minute.
//
// Throttled because this runs on the authenticated path, which a single large
// video hits a few hundred times. It reports no error: failing to record
// liveness is not a reason to fail an upload, and the next request tries again.
func (s *Store) Touch(ctx context.Context, id string) {
	now := time.Now()
	if last, ok := s.touched.Load(id); ok {
		if now.Sub(last.(time.Time)) < touchInterval {
			return
		}
	}

	const update = `update devices set last_seen_at = now() where id = $1`
	if _, err := s.pool.Exec(ctx, update, id); err != nil {
		return
	}
	// Only after it landed, so a failed write is retried on the next request
	// rather than suppressed for a minute.
	s.touched.Store(id, now)
}

// List returns every device, newest first, revoked ones included — a revoked
// device is exactly what somebody running this wants to see.
func (s *Store) List(ctx context.Context) ([]Device, error) {
	const query = `
		select id::text, name, platform, created_at, last_seen_at, revoked_at, paired_from
		from devices order by created_at desc`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Revoke withdraws a device's token. The row is kept rather than deleted, so
// `photobackup devices` can still explain why a phone stopped uploading, and so
// the assets it delivered still name something.
//
// Idempotent: revoking an already-revoked device keeps the original timestamp.
func (s *Store) Revoke(ctx context.Context, id string) (Device, error) {
	const update = `
		update devices set revoked_at = coalesce(revoked_at, now())
		where id = $1
		returning id::text, name, platform, created_at, last_seen_at, revoked_at, paired_from`
	d, err := scanDevice(s.pool.QueryRow(ctx, update, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNoDevice
	}
	if err != nil {
		// An id that is not a uuid arrives as a cast error rather than no rows,
		// and reporting that as a database failure would be a lie.
		if isBadUUID(err) {
			return Device{}, ErrNoDevice
		}
		return Device{}, fmt.Errorf("revoke device: %w", err)
	}
	return d, nil
}

// SweepCodes deletes pairing codes that can never be redeemed again. Nothing
// depends on it — a spent code is inert — so this is housekeeping, not
// correctness.
func (s *Store) SweepCodes(ctx context.Context) (int64, error) {
	const del = `delete from pairing_codes where expires_at < now() - interval '1 day' or used_at < now() - interval '1 day'`
	tag, err := s.pool.Exec(ctx, del)
	if err != nil {
		return 0, fmt.Errorf("sweep pairing codes: %w", err)
	}
	return tag.RowsAffected(), nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDevice(s scanner) (Device, error) {
	var d Device
	err := s.Scan(&d.ID, &d.Name, &d.Platform, &d.CreatedAt, &d.LastSeenAt, &d.RevokedAt, &d.PairedFrom)
	return d, err
}

func isBadUUID(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid input syntax for type uuid") ||
		strings.Contains(msg, "invalid UUID")
}
