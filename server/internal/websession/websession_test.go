package websession

import (
	"errors"
	"testing"
	"time"
)

const password = "correct horse battery"

func TestUnconfiguredIssuesNothing(t *testing.T) {
	s := New("", time.Hour)
	if s.Configured() {
		t.Fatal("a store with no password reports itself configured")
	}
	if _, _, err := s.Create("", "10.0.0.2"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Create with no password configured = %v, want ErrNotConfigured", err)
	}
	// The property the whole feature rests on: an unconfigured server cannot be
	// signed in to, so every guarded route falls back to the device token.
	if _, ok := s.Valid("anything"); ok {
		t.Fatal("an unconfigured store validated a token")
	}
}

func TestCreateAndValidate(t *testing.T) {
	s := New(password, time.Hour)

	token, expires, err := s.Create(password, "10.0.0.2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" {
		t.Fatal("Create returned an empty token")
	}
	if time.Until(expires) < 55*time.Minute {
		t.Fatalf("expiry %v is not about an hour out", expires)
	}

	if _, ok := s.Valid(token); !ok {
		t.Fatal("a token that was just issued did not validate")
	}
	if _, ok := s.Valid(token + "x"); ok {
		t.Fatal("a token that was never issued validated")
	}
	if _, ok := s.Valid(""); ok {
		t.Fatal("the empty token validated")
	}

	second, _, err := s.Create(password, "10.0.0.3")
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second == token {
		t.Fatal("two sessions were issued the same token")
	}
}

func TestWrongPassword(t *testing.T) {
	s := New(password, time.Hour)
	if _, _, err := s.Create("correct horse batter", "10.0.0.2"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("Create with a wrong password = %v, want ErrBadPassword", err)
	}
	// A prefix and a longer string, because the comparison is over digests
	// precisely so that neither is distinguishable from any other wrong answer.
	if _, _, err := s.Create(password+"!", "10.0.0.2"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("Create with a longer wrong password = %v, want ErrBadPassword", err)
	}
}

func TestSignOut(t *testing.T) {
	s := New(password, time.Hour)
	token, _, err := s.Create(password, "10.0.0.2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.End(token)
	if _, ok := s.Valid(token); ok {
		t.Fatal("a token still validated after being ended")
	}
	// Idempotent: a stale cookie asking to be forgotten has got what it wanted.
	s.End(token)
}

func TestExpiryIsIdleTime(t *testing.T) {
	s := New(password, 50*time.Millisecond)
	token, _, err := s.Create(password, "10.0.0.2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Used inside the window twice over: a browser being scrolled must not be
	// signed out at the TTL, only after it stops.
	for range 3 {
		time.Sleep(30 * time.Millisecond)
		if _, ok := s.Valid(token); !ok {
			t.Fatal("an idle timer expired a session that was still being used")
		}
	}

	time.Sleep(80 * time.Millisecond)
	if _, ok := s.Valid(token); ok {
		t.Fatal("a session outlived its idle window")
	}
}

func TestRateLimitIsPerAddress(t *testing.T) {
	s := New(password, time.Hour)

	for i := range maxAttempts {
		if _, _, err := s.Create("wrong", "10.0.0.2"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d = %v, want ErrBadPassword", i, err)
		}
	}
	if _, _, err := s.Create("wrong", "10.0.0.2"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("attempt past the limit = %v, want ErrTooManyAttempts", err)
	}
	// Even with the right password: past the limit the store stops answering
	// the question at all, which is what makes the limit worth having.
	if _, _, err := s.Create(password, "10.0.0.2"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("a correct password past the limit = %v, want ErrTooManyAttempts", err)
	}

	// The reason the limiter is keyed by address: otherwise anyone on the
	// network could lock the owner out of their own gallery by guessing wrong
	// ten times.
	if _, _, err := s.Create(password, "10.0.0.9"); err != nil {
		t.Fatalf("another address was locked out by the first one's attempts: %v", err)
	}
}

func TestSessionsAreBounded(t *testing.T) {
	s := New(password, time.Hour)

	var first string
	for i := range maxSessions + 4 {
		token, _, err := s.Create(password, "10.0.0.2")
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		if i == 0 {
			first = token
		}
	}

	s.mu.Lock()
	held := len(s.live)
	s.mu.Unlock()
	if held > maxSessions {
		t.Fatalf("holding %d sessions, want at most %d", held, maxSessions)
	}
	if _, ok := s.Valid(first); ok {
		t.Fatal("the oldest session survived the bound rather than being evicted")
	}
}

func TestEndAll(t *testing.T) {
	s := New(password, time.Hour)
	a, _, _ := s.Create(password, "10.0.0.2")
	b, _, _ := s.Create(password, "10.0.0.3")

	s.EndAll()
	if _, ok := s.Valid(a); ok {
		t.Fatal("a session survived EndAll")
	}
	if _, ok := s.Valid(b); ok {
		t.Fatal("a session survived EndAll")
	}
}
