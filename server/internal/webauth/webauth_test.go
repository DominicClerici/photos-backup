package webauth

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/code"
	"github.com/dominicclerici/photos-backup/server/internal/db"
)

const (
	adminURL = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	// Its own database, because `go test ./...` runs packages concurrently and a
	// shared one means truncating another package's rows mid-test.
	testDBName = "photobackup_test_webauth"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()

	admin := adminURL
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ensureDatabase(t, ctx, admin)

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + testDBName

	store, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open database: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const truncate = `truncate table web_sessions, web_recovery_codes, web_enrollments,
	                  web_passkeys, web_identity cascade`
	if _, err := store.Pool().Exec(ctx, truncate); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return New(store.Pool())
}

func ensureDatabase(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to postgres: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx, `select exists (select 1 from pg_database where datname = $1)`,
		testDBName).Scan(&exists); err != nil {
		t.Fatalf("look for test database: %v", err)
	}
	if exists {
		return
	}
	if _, err := conn.Exec(ctx, `create database "`+testDBName+`"`); err != nil {
		t.Fatalf("create test database: %v", err)
	}
}

func mustMint(t *testing.T, s *Store) string {
	t.Helper()
	token, _, err := s.Mint(context.Background(), "", MethodPasskey, "test", "go test")
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	return token
}

func TestASessionAuthenticates(t *testing.T) {
	s := newStore(t)
	token := mustMint(t, s)

	sess, err := s.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if sess.Method != MethodPasskey {
		t.Errorf("method = %q, want %q", sess.Method, MethodPasskey)
	}
}

// The token is never stored, only its digest — so a dump of this database
// cannot be replayed to sign in. This is the same property the devices table
// has, asserted the same way.
func TestTheSessionTokenIsNotStored(t *testing.T) {
	s := newStore(t)
	token := mustMint(t, s)

	var count int
	err := s.pool.QueryRow(context.Background(),
		`select count(*) from web_sessions where encode(token_sha256, 'escape') = $1`, token).Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Fatal("the session token itself is in the database")
	}
}

func TestAnUnknownTokenIsRefused(t *testing.T) {
	s := newStore(t)

	if _, err := s.Authenticate(context.Background(), SessionPrefix+"nope"); !errors.Is(err, ErrBadSession) {
		t.Fatalf("error = %v, want ErrBadSession", err)
	}
	if _, err := s.Authenticate(context.Background(), ""); !errors.Is(err, ErrBadSession) {
		t.Fatalf("empty token error = %v, want ErrBadSession", err)
	}
}

// The absolute cap is what nothing resets. A session that has been busy all day
// still dies at it, which is the difference between this and the idle window —
// so this ages expires_at while leaving last_seen_at at now, and the session
// must die anyway.
func TestTheAbsoluteCapEndsASession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	token := mustMint(t, s)

	if _, err := s.pool.Exec(ctx,
		`update web_sessions set expires_at = now() - interval '1 minute', last_seen_at = now()`); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrBadSession) {
		t.Fatalf("error = %v, want ErrBadSession past the absolute cap", err)
	}
}

// The idle window is the other clock, and it is the one a laptop left open
// falls foul of.
func TestTheIdleWindowEndsASession(t *testing.T) {
	s := newStore(t)
	token := mustMint(t, s)

	// Wound back rather than waited out: the behaviour under test is the
	// comparison, not the passage of time.
	if _, err := s.pool.Exec(context.Background(),
		`update web_sessions set last_seen_at = now() - interval '2 hours'`); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	s.Idle = time.Hour
	if _, err := s.Authenticate(context.Background(), token); !errors.Is(err, ErrBadSession) {
		t.Fatalf("error = %v, want ErrBadSession past the idle window", err)
	}
}

// Touch slides the idle window, which is what keeps somebody browsing from
// being signed out mid-scroll.
func TestTouchSlidesTheIdleWindow(t *testing.T) {
	s := newStore(t)
	s.Idle = time.Hour
	token := mustMint(t, s)
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `update web_sessions set last_seen_at = now() - interval '50 minutes'`); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	sess, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	s.Touch(ctx, sess)

	// Now well past where the original last_seen_at would have put the deadline.
	if _, err := s.pool.Exec(ctx, `update web_sessions set last_seen_at = last_seen_at`); err != nil {
		t.Fatalf("settle: %v", err)
	}
	refreshed, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate after touch: %v", err)
	}
	if time.Since(refreshed.LastSeenAt) > time.Minute {
		t.Errorf("last seen %s ago, want it slid to now", time.Since(refreshed.LastSeenAt))
	}
}

// Signing out is a revocation on the server, not a discarded cookie: the token
// stops working whether or not the browser cooperated.
func TestRevokingEndsASession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	token := mustMint(t, s)

	if err := s.Revoke(ctx, token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrBadSession) {
		t.Fatalf("error = %v, want ErrBadSession after revoking", err)
	}
}

func TestRevokeAllEndsEverySession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first, second := mustMint(t, s), mustMint(t, s)

	n, err := s.RevokeAll(ctx)
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d, want 2", n)
	}
	for _, token := range []string{first, second} {
		if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrBadSession) {
			t.Errorf("error = %v, want ErrBadSession", err)
		}
	}
}

// A recovery code opens the archive once. The second attempt finds nothing,
// which is the whole of what single use means.
func TestARecoveryCodeIsSpentOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	codes, err := s.MintRecovery(ctx, 4)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(codes) != 4 {
		t.Fatalf("minted %d codes, want 4", len(codes))
	}

	if err := s.RedeemRecovery(ctx, codes[0], "test"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if err := s.RedeemRecovery(ctx, codes[0], "test"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("second redemption = %v, want ErrBadCode", err)
	}

	remaining, err := s.RecoveryRemaining(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 3 {
		t.Errorf("remaining = %d, want 3", remaining)
	}
}

// Minting replaces rather than extends. A set of recovery codes is one
// credential with ten faces, and a list printed a year ago must stop working
// when a new one is printed.
func TestMintingRecoveryCodesRetiresTheOldSet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	old, err := s.MintRecovery(ctx, 3)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := s.MintRecovery(ctx, 3); err != nil {
		t.Fatalf("re-mint: %v", err)
	}

	if err := s.RedeemRecovery(ctx, old[0], "test"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("an old code redeemed with error %v, want ErrBadCode", err)
	}
}

// An enrollment code is checked before the Touch ID prompt and claimed after
// it. This is the check half; the claim is exercised through AddPasskey, which
// needs a real credential and is covered by the API package.
func TestAnEnrollmentCodeIsCheckedAndExpires(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	live, _, err := s.CreateEnrollment(ctx, time.Minute, "laptop")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Accepted however it was typed: the grouping dash from Format costs
	// nothing, and neither does lower case.
	if err := s.CheckEnrollment(ctx, code.Format(live)); err != nil {
		t.Errorf("a live code was refused: %v", err)
	}

	if _, err := s.pool.Exec(ctx,
		`update web_enrollments set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("age the code: %v", err)
	}
	if err := s.CheckEnrollment(ctx, live); !errors.Is(err, ErrBadCode) {
		t.Errorf("an expired code = %v, want ErrBadCode", err)
	}

	// Too short to be a code at all, which is a different answer from "not one
	// this archive holds". Note that most typo-ish strings are *not* malformed:
	// Normalize drops punctuation and folds O onto 0, so "not-a-code" is eight
	// valid characters and comes back as ErrBadCode instead.
	if err := s.CheckEnrollment(ctx, "abc"); !errors.Is(err, ErrMalformedCode) {
		t.Errorf("a malformed code = %v, want ErrMalformedCode", err)
	}
}

// The user handle has to survive, because every discoverable credential is
// bound to it: minting a new one on the second call would orphan every passkey
// already registered.
func TestTheIdentityHandleIsStable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first, err := s.Identity(ctx)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	second, err := s.Identity(ctx)
	if err != nil {
		t.Fatalf("identity again: %v", err)
	}

	if string(first.Handle) != string(second.Handle) {
		t.Fatal("the user handle changed between calls; every registered passkey would be orphaned")
	}
	if len(first.Handle) != handleBytes {
		t.Errorf("handle is %d bytes, want %d", len(first.Handle), handleBytes)
	}
}

// A fresh archive is closed, not open. This is the fact requireAuth's bootstrap
// behaviour rests on.
func TestAFreshArchiveHasNoPasskey(t *testing.T) {
	s := newStore(t)

	any, err := s.HasPasskey(context.Background())
	if err != nil {
		t.Fatalf("has passkey: %v", err)
	}
	if any {
		t.Fatal("a freshly truncated archive reported a registered passkey")
	}
}
