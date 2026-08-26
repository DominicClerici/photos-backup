// Package webauth owns the credential a browser signs in with, and the session
// it holds afterwards.
//
// It is the replacement for what Phase 12 built and removed. That design was one
// shared password with no accounts, no roles and no revocation, and the reason
// it came out is the reason this looks nothing like it: a password is a secret
// the server has to hold a verifier for, that can be typed into anything that
// asks, and that is the same on every device. A passkey is none of those. The
// authenticator keeps the private half and this database sees only the public
// one, so there is nothing here to replay, nothing to phish, and revoking one
// device's key leaves the others working.
//
// The shape is deliberately close to internal/devices, which solves the same
// problem for the phone: credentials handed out once, stored as sha256 digests,
// revocable from the command line, and never distinguishing "unknown" from
// "expired" in what it tells a caller. Where the two differ is lifetime. A
// device token has none, because a backup that stops working overnight is worse
// than one that runs until it is revoked. A browser session has two — an idle
// window and an absolute cap — because a machine walked away from is the threat
// a session is actually about.
package webauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dominicclerici/photos-backup/server/internal/code"
)

const (
	// SessionPrefix marks a session token wherever one turns up. It lives in a
	// cookie rather than an env file, so it is less likely to be pasted
	// somewhere than a device token is — but it can still end up in a log, and
	// recognising a live credential on sight is worth four characters.
	SessionPrefix = "pbs_"
	// RecoveryPrefix does the same for a recovery code, which unlike the other
	// two is expected to be written down on paper.
	RecoveryPrefix = "pbr_"
)

const (
	// sessionBytes is 256 bits, for the same reason a device token is: there is
	// no dictionary to attack, so a digest is the right way to store it and a
	// password KDF would be slowing down nothing.
	sessionBytes = 32
	// recoveryBytes is 128 bits. Smaller than a session token because it is
	// transcribed by hand, and still far past anything guessable — a code is
	// one of 2^128, single use, and rate limited on top.
	recoveryBytes = 16
	// RecoveryCodeCount is how many are minted at once. Ten is enough to keep a
	// couple in a wallet and a couple in a safe without becoming a list nobody
	// keeps track of.
	RecoveryCodeCount = 10
)

const (
	// DefaultEnrollTTL is how long a passkey enrollment code stands. Shorter
	// than a pairing code's ten minutes: enrolling happens at the keyboard the
	// code was printed on, or on a phone in the same room.
	DefaultEnrollTTL = 5 * time.Minute
	// DefaultIdle ends a session that has not been used. An hour is long enough
	// to read a page, answer the door and come back, and short enough that a
	// laptop left open in a café is not an open archive all afternoon.
	DefaultIdle = time.Hour
	// DefaultLifetime is the absolute cap, which nothing resets. Twelve hours
	// means a session cannot outlive the day it was created in, however busy
	// the browser keeps it.
	DefaultLifetime = 12 * time.Hour
	// touchInterval throttles the write that slides the idle window. A grid of
	// thumbnails is a few hundred authenticated requests in a second, and
	// updating one row for each would be several hundred writes to record
	// something the idle timeout reads at minute resolution.
	touchInterval = time.Minute
)

var (
	// ErrBadSession means no live session holds that token. Unknown, expired,
	// idled out and revoked are not distinguished, for the reason devices does
	// not distinguish them: telling a caller which one it was is telling them
	// something about credentials they do not hold.
	ErrBadSession = errors.New("webauth: session is not valid")
	// ErrBadCode means an enrollment or recovery code is well-formed but not
	// redeemable — unknown, expired, or already used.
	ErrBadCode = errors.New("webauth: that code is not valid")
	// ErrMalformedCode means the input could not be a code at all. The same
	// value internal/code returns, so errors.Is holds across both.
	ErrMalformedCode = code.ErrMalformed
	// ErrNoPasskey means no passkey has been registered on this archive yet.
	// The server is closed in this state rather than open: see
	// api.Server.handleAuthStatus.
	ErrNoPasskey = errors.New("webauth: no passkey is registered")
	// ErrNoSuchPasskey means no passkey exists under that id.
	ErrNoSuchPasskey = errors.New("webauth: no such passkey")
	// ErrCloned means an authenticator presented a signature counter at or
	// below the one already recorded, which is how a duplicated credential
	// announces itself. See Store.RecordLogin.
	ErrCloned = errors.New("webauth: authenticator signature counter went backwards")
)

// Store is every query this package makes.
type Store struct {
	pool *pgxpool.Pool

	// Idle and Lifetime are the two session clocks. Zero means the default.
	Idle     time.Duration
	Lifetime time.Duration

	// touched throttles last_seen_at writes, per process. The same tradeoff
	// devices.Store makes, and correct for the same reason: one photod owns
	// this database.
	touched sync.Map
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) idle() time.Duration {
	if s.Idle <= 0 {
		return DefaultIdle
	}
	return s.Idle
}

func (s *Store) lifetime() time.Duration {
	if s.Lifetime <= 0 {
		return DefaultLifetime
	}
	return s.Lifetime
}

// newSecret mints a prefixed random credential and the digest to store for it.
func newSecret(prefix string, n int) (secret string, digest []byte, err error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate credential: %w", err)
	}
	secret = prefix + base64.RawURLEncoding.EncodeToString(buf)
	return secret, digestOf(secret), nil
}

// digestOf is how every secret in this package is stored, and why a dump of
// this database cannot be replayed to sign in. sha256 rather than a password
// hash for the reason devices gives: these are 128- and 256-bit random values,
// so there is no guessing attack for a slow KDF to frustrate.
//
// The one credential in this archive that *is* password-derived — the vault —
// uses Argon2id, and does so precisely because a human chose it. See
// internal/vault/crypto.go.
func digestOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Identity is the archive's single WebAuthn user, and the credentials
// registered to it.
//
// It implements webauthn.User. The handle is stable for the life of the archive
// because discoverable credentials are bound to it: minting a new one would
// orphan every passkey already registered.
type Identity struct {
	Handle      []byte
	DisplayName string
	Creds       []webauthn.Credential
}

func (i Identity) WebAuthnID() []byte                         { return i.Handle }
func (i Identity) WebAuthnName() string                       { return i.DisplayName }
func (i Identity) WebAuthnDisplayName() string                { return i.DisplayName }
func (i Identity) WebAuthnCredentials() []webauthn.Credential { return i.Creds }

// Passkey is one registered authenticator.
type Passkey struct {
	ID         string
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	// Transports is what the authenticator said it speaks — "internal" for a
	// platform credential, "usb"/"nfc" for a security key. Reported so
	// `photobackup web` can tell a Touch ID key from a YubiKey.
	Transports string
}

// Revoked reports whether this passkey has been withdrawn.
func (p Passkey) Revoked() bool { return p.RevokedAt != nil }

// Session is one signed-in browser.
type Session struct {
	// PasskeyID is which credential opened it, or empty for a recovery-code
	// session. Nothing authenticates against it; it is here so a session can be
	// explained in `photobackup web`.
	PasskeyID  string
	Method     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	CreatedFrom string
	UserAgent   string

	// token is kept only long enough for Authenticate to throttle its touch
	// against it. It is never returned to a caller other than the one that
	// minted it.
	token string
}

// Deadline is when this session dies if nothing touches it — the earlier of the
// idle window and the absolute cap. Reported so the browser can say how long is
// left rather than logging somebody out mid-scroll.
func (s *Store) Deadline(sess Session) time.Time {
	idleOut := sess.LastSeenAt.Add(s.idle())
	if idleOut.Before(sess.ExpiresAt) {
		return idleOut
	}
	return sess.ExpiresAt
}

// truncate bounds a user-supplied string before it reaches a text column.
// Nothing here is interpreted — a user agent is only ever printed back to
// whoever runs the CLI — but an unbounded header is still an unbounded write.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
