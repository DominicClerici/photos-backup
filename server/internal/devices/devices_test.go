package devices

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

const (
	adminURL = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	// Its own database, because `go test ./...` runs packages concurrently and a
	// shared one means truncating another package's rows mid-test.
	testDBName = "photobackup_test_devices"
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
	if _, err := store.Pool().Exec(ctx, "truncate table pairing_codes, devices cascade"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return New(store.Pool())
}

func mustCode(t *testing.T, s *Store, ttl time.Duration) string {
	t.Helper()
	code, _, err := s.CreateCode(context.Background(), ttl, "")
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	return code
}

func TestPairingMintsAUsableToken(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	device, token, err := s.Pair(ctx, mustCode(t, s, time.Minute), "a phone", "ios", "192.168.1.5")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Errorf("token = %q, want the %q prefix so a leak is recognisable", token, TokenPrefix)
	}

	authenticated, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authenticated.ID != device.ID {
		t.Errorf("authenticated as %s, want %s", authenticated.ID, device.ID)
	}
	if authenticated.Name != "a phone" {
		t.Errorf("name = %q, want %q", authenticated.Name, "a phone")
	}
}

// The token is stored as a digest, so the database never holds the credential
// itself. A dump of it cannot be replayed to write into the archive.
func TestTheTokenIsNotStored(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, token, err := s.Pair(ctx, mustCode(t, s, time.Minute), "a phone", "ios", "")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	var stored []byte
	if err := s.pool.QueryRow(ctx, "select token_sha256 from devices").Scan(&stored); err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if strings.Contains(string(stored), token) || string(stored) == token {
		t.Fatal("the token itself is in the database")
	}
	if len(stored) != 32 {
		t.Errorf("stored %d bytes, want a 32-byte sha256", len(stored))
	}
}

func TestACodeCannotBeRedeemedTwice(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	code := mustCode(t, s, time.Minute)

	if _, _, err := s.Pair(ctx, code, "first", "ios", ""); err != nil {
		t.Fatalf("first pair: %v", err)
	}
	if _, _, err := s.Pair(ctx, code, "second", "ios", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("second pair error = %v, want ErrBadCode", err)
	}

	// And the loser leaves nothing behind: the device insert is rolled back with
	// the failed claim.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d devices exist, want 1 — a rejected pairing left a row", len(list))
	}
}

// Two phones racing on one code: exactly one wins, and exactly one device is
// created. The claim is a conditional UPDATE inside the same transaction as the
// insert, which is what makes that true rather than usually true.
func TestConcurrentRedemptionCreatesOneDevice(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	code := mustCode(t, s, time.Minute)

	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Pair(ctx, code, fmt.Sprintf("phone-%d", i), "ios", "")
			won[i] = err == nil
		}()
	}
	wg.Wait()

	winners := 0
	for _, ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d racers redeemed the same code, want exactly 1", winners)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("%d devices exist, want 1", len(list))
	}
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	code := mustCode(t, s, time.Minute)
	expire(t, s, code)

	if _, _, err := s.Pair(ctx, code, "a phone", "ios", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("error = %v, want ErrBadCode", err)
	}
}

// expire ages a code past its deadline. CreateCode coerces a non-positive TTL to
// the default — a typo in `--ttl` should not mint a code that is already dead —
// so the only way to get an expired one is to move it.
func expire(t *testing.T, s *Store, code string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`update pairing_codes set expires_at = now() - interval '1 minute' where code_sha256 = $1`,
		digestOf(code)); err != nil {
		t.Fatalf("expire a code: %v", err)
	}
}

func TestRevokingStopsTheToken(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	device, token, err := s.Pair(ctx, mustCode(t, s, time.Minute), "a phone", "ios", "")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if _, err := s.Revoke(ctx, device.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrRevoked) {
		t.Fatalf("error = %v, want ErrRevoked so the phone can say it was unpaired", err)
	}
}

// Revoking twice must not move the timestamp, or `photobackup devices` would
// report the wrong date every time somebody re-ran it.
func TestRevokingIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	device, _, err := s.Pair(ctx, mustCode(t, s, time.Minute), "a phone", "ios", "")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	first, err := s.Revoke(ctx, device.ID)
	if err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	second, err := s.Revoke(ctx, device.ID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if !first.RevokedAt.Equal(*second.RevokedAt) {
		t.Errorf("revoked_at moved from %s to %s", first.RevokedAt, second.RevokedAt)
	}
}

func TestRevokingSomethingThatIsNotADevice(t *testing.T) {
	s := newStore(t)

	for _, id := range []string{"not-a-uuid", "9f1c2e4a-0000-4000-8000-000000000000"} {
		if _, err := s.Revoke(context.Background(), id); !errors.Is(err, ErrNoDevice) {
			t.Errorf("revoke %q error = %v, want ErrNoDevice", id, err)
		}
	}
}

func TestAnUnknownTokenIsRefused(t *testing.T) {
	s := newStore(t)

	for _, token := range []string{"", "nonsense", TokenPrefix + "AAAA"} {
		if _, err := s.Authenticate(context.Background(), token); !errors.Is(err, ErrBadToken) {
			t.Errorf("authenticate %q error = %v, want ErrBadToken", token, err)
		}
	}
}

func TestTouchIsThrottled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	device, _, err := s.Pair(ctx, mustCode(t, s, time.Minute), "a phone", "ios", "")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	s.Touch(ctx, device.ID)
	first := lastSeen(t, s, device.ID)
	if first == nil {
		t.Fatal("last_seen_at is null after a touch")
	}

	// A large video authenticates a few hundred times; only the first should
	// write.
	for range 50 {
		s.Touch(ctx, device.ID)
	}
	if second := lastSeen(t, s, device.ID); !second.Equal(*first) {
		t.Errorf("last_seen_at moved from %s to %s; the throttle is not holding", first, second)
	}
}

func TestSweepRemovesSpentCodes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	mustCode(t, s, time.Minute)
	stale := mustCode(t, s, time.Minute)
	if _, err := s.pool.Exec(ctx,
		`update pairing_codes set expires_at = now() - interval '2 days' where code_sha256 = $1`,
		digestOf(stale)); err != nil {
		t.Fatalf("age a code: %v", err)
	}

	removed, err := s.SweepCodes(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d codes, want 1 — the live one must survive", removed)
	}
}

func TestCodesAreTypeable(t *testing.T) {
	// Crockford base32: nothing that can be confused with a digit, so a code read
	// off a terminal and typed on a phone survives the trip.
	for range 200 {
		code, err := newCode()
		if err != nil {
			t.Fatalf("new code: %v", err)
		}
		if len(code) != codeChars {
			t.Fatalf("code %q is %d characters, want %d", code, len(code), codeChars)
		}
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("code %q contains a character the alphabet excludes", code)
		}
	}
}

func TestNormalizeCodeForgivesHowItWasTyped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"K7QM3XRJ", "K7QM3XRJ"},
		{"K7QM-3XRJ", "K7QM3XRJ"},
		{"k7qm 3xrj", "K7QM3XRJ"},
		{" K7QM-3XRJ ", "K7QM3XRJ"},
		// The three characters the alphabet leaves out, folded onto the digits
		// they look like on a screen.
		{"O7QM-3XRJ", "07QM3XRJ"},
		{"I7QM-3XRJ", "17QM3XRJ"},
		{"l7qm-3xrj", "17QM3XRJ"},
	} {
		got, err := NormalizeCode(tc.in)
		if err != nil {
			t.Errorf("normalize %q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalize %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeCodeRejectsTheWrongLength(t *testing.T) {
	for _, in := range []string{"", "K7QM", "K7QM-3XRJ-EXTRA", "!!!!!!!!"} {
		if _, err := NormalizeCode(in); !errors.Is(err, ErrMalformedCode) {
			t.Errorf("normalize %q error = %v, want ErrMalformedCode", in, err)
		}
	}
}

func TestFormatCodeGroupsIt(t *testing.T) {
	if got := FormatCode("K7QM3XRJ"); got != "K7QM-3XRJ" {
		t.Errorf("FormatCode = %q, want %q", got, "K7QM-3XRJ")
	}
}

// Round trip: whatever is printed normalizes back to what was stored.
func TestAPrintedCodeRedeems(t *testing.T) {
	s := newStore(t)
	code := mustCode(t, s, time.Minute)

	if _, _, err := s.Pair(context.Background(), FormatCode(code), "a phone", "ios", ""); err != nil {
		t.Fatalf("pairing with the printed form failed: %v", err)
	}
}

func lastSeen(t *testing.T, s *Store, id string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := s.pool.QueryRow(context.Background(),
		"select last_seen_at from devices where id = $1", id).Scan(&at); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	return at
}

func ensureDatabase(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		"select exists (select 1 from pg_database where datname = $1)", testDBName).Scan(&exists); err != nil {
		t.Fatalf("look up test database: %v", err)
	}
	if exists {
		return
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("create database %s", pgx.Identifier{testDBName}.Sanitize())); err != nil {
		t.Fatalf("create test database: %v", err)
	}
}
