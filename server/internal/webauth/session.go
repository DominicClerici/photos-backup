package webauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Mint opens a session and returns the token that names it. The token is
// returned here and never again; only its digest is kept.
func (s *Store) Mint(ctx context.Context, passkeyID, method, from, userAgent string) (string, Session, error) {
	token, digest, err := newSecret(SessionPrefix, sessionBytes)
	if err != nil {
		return "", Session{}, err
	}
	if method == "" {
		method = MethodPasskey
	}

	// A nil uuid parameter rather than an empty string: passkey_id is null for
	// a recovery session, and "" is not a uuid.
	var passkey *string
	if passkeyID != "" {
		passkey = &passkeyID
	}

	const insert = `
		insert into web_sessions (token_sha256, passkey_id, method, expires_at, created_from, user_agent)
		values ($1, $2, $3, now() + $4::interval, $5, $6)
		returning coalesce(passkey_id::text, ''), method, created_at, expires_at, last_seen_at,
		          created_from, user_agent`
	sess, err := scanSession(s.pool.QueryRow(ctx, insert, digest, passkey, method,
		s.lifetime().String(), truncate(from, 64), truncate(userAgent, 256)))
	if err != nil {
		return "", Session{}, fmt.Errorf("open session: %w", err)
	}
	sess.token = token
	return token, sess, nil
}

// Authenticate resolves a session cookie to the session holding it.
//
// The lookup is by digest, so no secret-dependent comparison happens in this
// process at all — there is no string to compare in variable time, only an index
// probe for a value the caller already supplied. The same property
// devices.Store.Authenticate has, and for the same reason.
//
// Every way a session can be dead answers ErrBadSession: unknown, revoked, past
// its absolute cap, or idle too long. They are not distinguished because the
// difference is only ever interesting to somebody who does not hold a live one.
func (s *Store) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrBadSession
	}

	const query = `
		select coalesce(passkey_id::text, ''), method, created_at, expires_at, last_seen_at,
		       created_from, user_agent, revoked_at
		from web_sessions where token_sha256 = $1`

	var revokedAt *time.Time
	row := s.pool.QueryRow(ctx, query, digestOf(token))

	var sess Session
	err := row.Scan(&sess.PasskeyID, &sess.Method, &sess.CreatedAt, &sess.ExpiresAt,
		&sess.LastSeenAt, &sess.CreatedFrom, &sess.UserAgent, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrBadSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("look up session: %w", err)
	}

	now := time.Now()
	switch {
	case revokedAt != nil:
		return Session{}, ErrBadSession
	case !now.Before(sess.ExpiresAt):
		return Session{}, ErrBadSession
	case !now.Before(sess.LastSeenAt.Add(s.idle())):
		return Session{}, ErrBadSession
	}

	sess.token = token
	return sess, nil
}

// Touch slides the idle window, at most once a minute.
//
// Throttled for the reason devices.Store.Touch is, and more so: a zoomed-out
// grid is a few hundred authenticated thumbnail requests in a second, and a
// write per request would be several hundred updates to one row to record
// something read at minute resolution.
//
// It reports no error. Failing to record liveness is not a reason to fail a
// request, and the next one tries again — the cost of losing the write is that
// the idle window is a minute shorter than it should have been.
func (s *Store) Touch(ctx context.Context, sess Session) {
	if sess.token == "" {
		return
	}
	key := string(digestOf(sess.token))

	now := time.Now()
	if last, ok := s.touched.Load(key); ok {
		if now.Sub(last.(time.Time)) < touchInterval {
			return
		}
	}

	const update = `update web_sessions set last_seen_at = now() where token_sha256 = $1 and revoked_at is null`
	if _, err := s.pool.Exec(ctx, update, digestOf(sess.token)); err != nil {
		return
	}
	// Only after it landed, so a failed write is retried on the next request
	// rather than suppressed for a minute.
	s.touched.Store(key, now)
}

// Revoke ends one session, by the token that names it. This is what signing out
// does, and it is a real revocation rather than a discarded cookie: the row is
// dead on the server whether or not the browser cooperated.
func (s *Store) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	const update = `update web_sessions set revoked_at = now() where token_sha256 = $1 and revoked_at is null`
	if _, err := s.pool.Exec(ctx, update, digestOf(token)); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	s.touched.Delete(string(digestOf(token)))
	return nil
}

// RevokeAll ends every live session. What `photobackup web --revoke-all` calls,
// and the right first move when a laptop has gone missing.
func (s *Store) RevokeAll(ctx context.Context) (int64, error) {
	const update = `update web_sessions set revoked_at = now() where revoked_at is null`
	tag, err := s.pool.Exec(ctx, update)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	s.touched.Clear()
	return tag.RowsAffected(), nil
}

// Sessions lists the live ones, newest first. Dead sessions are excluded here
// and not in Passkeys, because a revoked passkey explains why a phone stopped
// working and an expired session explains nothing.
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	const query = `
		select coalesce(passkey_id::text, ''), method, created_at, expires_at, last_seen_at,
		       created_from, user_agent
		from web_sessions
		where revoked_at is null and expires_at > now() and last_seen_at > now() - $1::interval
		order by created_at desc`
	rows, err := s.pool.Query(ctx, query, s.idle().String())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Sweep deletes sessions that can never be used again.
//
// Housekeeping rather than correctness — Authenticate already refuses every row
// this removes — but a table that only grows is a table somebody eventually has
// to explain. The window is generous so that `photobackup web` can still show
// what happened this morning.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	const del = `
		delete from web_sessions
		where expires_at < now() - interval '7 days'
		   or revoked_at < now() - interval '7 days'`
	tag, err := s.pool.Exec(ctx, del)
	if err != nil {
		return 0, fmt.Errorf("sweep sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanSession(sc scanner) (Session, error) {
	var sess Session
	err := sc.Scan(&sess.PasskeyID, &sess.Method, &sess.CreatedAt, &sess.ExpiresAt,
		&sess.LastSeenAt, &sess.CreatedFrom, &sess.UserAgent)
	return sess, err
}
