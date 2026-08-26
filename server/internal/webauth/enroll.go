package webauth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/code"
)

// How a session was opened.
const (
	MethodPasskey  = "passkey"
	MethodRecovery = "recovery"
)

// CreateEnrollment mints a code that authorises registering one passkey.
//
// It writes to the database rather than asking photod for one, so enrolling
// works whether or not the daemon is up and there is no admin endpoint to
// protect. Being able to create a credential is exactly the authority that
// filesystem access to this database already carries — the same reasoning
// `photobackup pair` is built on.
func (s *Store) CreateEnrollment(ctx context.Context, ttl time.Duration, label string) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = DefaultEnrollTTL
	}
	c, err := code.New()
	if err != nil {
		return "", time.Time{}, err
	}

	const insert = `
		insert into web_enrollments (code_sha256, expires_at, label)
		values ($1, now() + $2::interval, $3)
		returning expires_at`
	var expiresAt time.Time
	if err := s.pool.QueryRow(ctx, insert, digestOf(c), ttl.String(), truncate(label, 64)).Scan(&expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("store enrollment code: %w", err)
	}
	return c, expiresAt, nil
}

// CheckEnrollment reports whether a code is redeemable, without redeeming it.
//
// The registration ceremony needs this: the code has to be validated before the
// browser is asked to create a credential, because asking somebody for Touch ID
// and only then telling them their code expired is a bad way to find out. The
// claim itself happens in AddPasskey, in the transaction that stores the
// credential.
//
// A code that passes here can still fail there — it could expire in between, or
// be redeemed by another browser — and that is the right way round. This is a
// courtesy; the transaction is the check.
func (s *Store) CheckEnrollment(ctx context.Context, raw string) error {
	normalized, err := code.Normalize(raw)
	if err != nil {
		return err
	}
	const query = `
		select exists (
			select 1 from web_enrollments
			where code_sha256 = $1 and used_at is null and expires_at > now()
		)`
	var ok bool
	if err := s.pool.QueryRow(ctx, query, digestOf(normalized)).Scan(&ok); err != nil {
		return fmt.Errorf("check enrollment code: %w", err)
	}
	if !ok {
		return ErrBadCode
	}
	return nil
}

// SweepEnrollments deletes codes that can never be redeemed again. Nothing
// depends on it — a spent code is inert — so this is housekeeping.
func (s *Store) SweepEnrollments(ctx context.Context) (int64, error) {
	const del = `
		delete from web_enrollments
		where expires_at < now() - interval '1 day' or used_at < now() - interval '1 day'`
	tag, err := s.pool.Exec(ctx, del)
	if err != nil {
		return 0, fmt.Errorf("sweep enrollment codes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MintRecovery replaces the recovery codes with a fresh set and returns them.
//
// Replacing rather than appending is the point: a set of recovery codes is a
// single credential with ten faces, and minting more without retiring the old
// ones would mean a code printed a year ago and long since lost still opens the
// archive. Anyone running this is expected to write the new ones down and
// destroy the old list.
func (s *Store) MintRecovery(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		n = RecoveryCodeCount
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `delete from web_recovery_codes`); err != nil {
		return nil, fmt.Errorf("clear recovery codes: %w", err)
	}

	out := make([]string, 0, n)
	for range n {
		secret, digest, err := newSecret(RecoveryPrefix, recoveryBytes)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `insert into web_recovery_codes (code_sha256) values ($1)`, digest); err != nil {
			return nil, fmt.Errorf("store recovery code: %w", err)
		}
		out = append(out, secret)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	return out, nil
}

// RedeemRecovery spends one recovery code.
//
// The conditional UPDATE is what makes it single use: two requests presenting
// the same code race on one row and exactly one of them matches it. A code that
// has been spent is indistinguishable from one that never existed, which is the
// same answer ErrBadCode gives everywhere else in this package.
func (s *Store) RedeemRecovery(ctx context.Context, raw, from string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrBadCode
	}
	const update = `
		update web_recovery_codes set used_at = now(), used_from = $2
		where code_sha256 = $1 and used_at is null`
	tag, err := s.pool.Exec(ctx, update, digestOf(raw), truncate(from, 64))
	if err != nil {
		return fmt.Errorf("redeem recovery code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBadCode
	}
	return nil
}

// RecoveryRemaining is how many codes are still spendable. Shown on the status
// endpoint so that running low is visible before it matters.
func (s *Store) RecoveryRemaining(ctx context.Context) (int, error) {
	const query = `select count(*) from web_recovery_codes where used_at is null`
	var n int
	if err := s.pool.QueryRow(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("count recovery codes: %w", err)
	}
	return n, nil
}
