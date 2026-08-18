// Package websession is the browser's credential: one shared password, and the
// short-lived cookies handed out in exchange for it.
//
// It exists because a browser cannot carry the credential the phone does. A
// device token is a bearer header, and there is no way to put a header on an
// <img> or a <video> — which is the whole reason the gallery's read path stayed
// open on a loopback listener for five phases. A cookie is the one credential
// the browser attaches to a subresource on its own, so the gallery gets a cookie
// and the phone keeps its token.
//
// # Deliberately small, and deliberately removable
//
// This is a house key, not an identity system. There are no accounts, no
// usernames, no roles, no password reset, and no record of a session anywhere
// but this process's memory. Anyone holding the password is "the owner", which
// is true of the archive already: the machine has one user.
//
// If it is ever replaced by something real — a proper identity provider, or per
// user accounts — the whole of what has to be removed is:
//
//   - this package
//   - internal/api/websession.go, and the four marked lines in api.go it needs
//   - the GALLERY_PASSWORD / GALLERY_SESSION_TTL / WEB_URL settings in config
//   - web/src/hooks/useSession.ts and web/src/components/SignIn.tsx
//
// Nothing else in the server knows this package exists. In particular no
// database table, no migration and no file on disk records a session, so
// deleting the code deletes the feature completely.
//
// # What it is not
//
// Not a defence against someone who already has a shell on the machine, or the
// disk. Not multi-user: two browsers signed in with the same password are
// indistinguishable to everything downstream, which is why the vault's unlock
// stays server-wide rather than pretending to be per-session.
package websession

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

var (
	// ErrNotConfigured is "no GALLERY_PASSWORD is set". Sessions cannot be
	// created at all, which leaves the server exactly as it was before this
	// package existed: token-only.
	ErrNotConfigured = errors.New("websession: no gallery password is configured")
	ErrBadPassword   = errors.New("websession: wrong password")
	// ErrTooManyAttempts is the rate limiter. A shared password is a
	// guessable-in-principle credential in a way a 256-bit device token is not,
	// so unlike pairing this limit is load-bearing rather than hygiene.
	ErrTooManyAttempts = errors.New("websession: too many attempts")
)

const (
	// tokenBytes is the cookie's entropy. 256 bits, from crypto/rand, so the
	// cookie is not guessable and needs no expiry to be safe from search —
	// the expiry is about a borrowed laptop, not about brute force.
	tokenBytes = 32

	// maxSessions bounds the map. A household has a handful of browsers; this
	// is high enough never to be reached in use and low enough that nothing
	// can grow the map without the password.
	maxSessions = 64

	// Attempt limits, per window. Deliberately tighter than pairing's: this is
	// a password somebody chose, not forty bits of random.
	maxAttempts    = 10
	attemptWindow  = 5 * time.Minute
	maxAttemptKeys = 256
)

// Store holds the configured password and the sessions issued against it.
//
// Sessions live in memory only. A photod restart signs every browser out, which
// is the correct trade for a home server: the alternative is a table, a
// migration, and rows that outlive the process that could have revoked them.
type Store struct {
	password string
	ttl      time.Duration

	mu sync.Mutex
	// Keyed by the SHA-256 of the token rather than the token, so the cookies
	// currently valid cannot be read back out of this process — a core dump or
	// a heap profile yields hashes. Cheap, and it costs one line.
	live     map[[32]byte]time.Time
	attempts map[string][]time.Time
}

// New builds the store. An empty password disables the whole feature; see
// Configured.
func New(password string, ttl time.Duration) *Store {
	return &Store{password: password, ttl: ttl}
}

// Configured reports whether a password was set. Callers use it to decide
// whether to offer the sign-in route at all, so that a server with no gallery
// password says "not configured" rather than "wrong password" forever.
func (s *Store) Configured() bool { return s != nil && s.password != "" }

// TTL is how long a session lasts without being used. Reported to the browser
// so it can say when it will need the password again.
func (s *Store) TTL() time.Duration { return s.ttl }

// Create exchanges the password for a session token.
//
// ip keys the rate limiter and is used for nothing else — it is never stored
// with the session, because which address a browser signed in from has no
// bearing on whether the cookie it holds is valid, and pinning to it would
// break every laptop that moves between Wi-Fi and Ethernet.
func (s *Store) Create(password, ip string) (token string, expires time.Time, err error) {
	if !s.Configured() {
		return "", time.Time{}, ErrNotConfigured
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if s.tooMany(ip, now) {
		return "", time.Time{}, ErrTooManyAttempts
	}

	// Constant time, and over digests rather than the strings themselves so
	// that the comparison does not leak the password's length.
	want := sha256.Sum256([]byte(s.password))
	got := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		s.failed(ip, now)
		return "", time.Time{}, ErrBadPassword
	}
	// Only failures are counted, and a success forgets them: the limit is aimed
	// at guessing, and a household that signs four browsers in one after another
	// is not guessing.
	delete(s.attempts, ip)

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)

	s.sweep(now)
	if s.live == nil {
		s.live = make(map[[32]byte]time.Time)
	}
	if len(s.live) >= maxSessions {
		// Unreachable without the password, so the oldest session losing its
		// place is the right failure: it is one person's laptop being signed
		// out, not a refusal to sign in.
		s.evictOldest()
	}
	expires = now.Add(s.ttl)
	s.live[sha256.Sum256([]byte(token))] = expires
	return token, expires, nil
}

// Valid reports whether a token names a live session, and extends it.
//
// Idle time rather than a fixed session length, for the reason the vault's
// keeper uses the same rule: a gallery being scrolled is a gallery somebody is
// sitting in front of, and signing them out mid-scroll teaches nothing except
// to pick a shorter password.
func (s *Store) Valid(token string) (time.Time, bool) {
	if !s.Configured() || token == "" {
		return time.Time{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := sha256.Sum256([]byte(token))
	expires, ok := s.live[key]
	now := time.Now()
	if !ok || !now.Before(expires) {
		delete(s.live, key)
		return time.Time{}, false
	}
	expires = now.Add(s.ttl)
	s.live[key] = expires
	return expires, true
}

// End signs one browser out. Unknown tokens are not an error: signing out is
// idempotent, and a stale cookie asking to be forgotten has got what it wanted.
func (s *Store) End(token string) {
	if s == nil || token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, sha256.Sum256([]byte(token)))
}

// EndAll drops every session. Nothing calls it today; it is the one lever this
// package owes a password change, and it is here so that whoever adds that
// endpoint does not have to reach into the map.
func (s *Store) EndAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = nil
}

// sweep drops expired sessions. Called on the way in to Create rather than on a
// timer: the map is tiny, and a goroutine to tidy sixty-four entries would be a
// worse trade than the microsecond this costs at sign-in.
//
// Callers hold the lock.
func (s *Store) sweep(now time.Time) {
	for key, expires := range s.live {
		if !now.Before(expires) {
			delete(s.live, key)
		}
	}
}

// Callers hold the lock.
func (s *Store) evictOldest() {
	var oldestKey [32]byte
	var oldest time.Time
	for key, expires := range s.live {
		if oldest.IsZero() || expires.Before(oldest) {
			oldestKey, oldest = key, expires
		}
	}
	delete(s.live, oldestKey)
}

// tooMany reports whether this address has spent its wrong guesses. Callers
// hold the lock.
//
// Per address only. A global limit is what pairing wants, because a pairing code
// is spent once and a stranger spraying it is the whole risk; here a global
// limit would let anyone on the network lock the owner out of their own gallery
// by guessing wrong ten times.
func (s *Store) tooMany(ip string, now time.Time) bool {
	cutoff := now.Add(-attemptWindow)
	kept := s.attempts[ip][:0]
	for _, at := range s.attempts[ip] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(s.attempts, ip)
		return false
	}
	s.attempts[ip] = kept
	return len(kept) >= maxAttempts
}

// failed records a wrong guess. Callers hold the lock.
func (s *Store) failed(ip string, now time.Time) {
	if s.attempts == nil {
		s.attempts = make(map[string][]time.Time)
	}
	// Bounds the map. Reached only by something spraying from many addresses,
	// and dropping the whole thing costs those attackers nothing they had not
	// already spent — the window is five minutes either way.
	if len(s.attempts) > maxAttemptKeys {
		s.attempts = make(map[string][]time.Time)
	}
	s.attempts[ip] = append(s.attempts[ip], now)
}
