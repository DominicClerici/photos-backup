package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// CeremonyTTL is how long a half-finished registration or login stands. Two
// minutes is comfortably longer than a Touch ID prompt and comfortably shorter
// than walking away from the machine.
const CeremonyTTL = 2 * time.Minute

// Ceremonies holds the state between the two halves of a WebAuthn exchange:
// the challenge the server issued, and what it expects back.
//
// In memory rather than in Postgres, which is the same call vault.Keeper makes
// and for the same reason: this is state with a two-minute life that a restart
// is allowed to lose. Somebody whose photod restarted between tapping "sign in"
// and touching the sensor taps it again. There is no table, no migration and
// nothing on disk.
//
// What it must not be is guessable or reusable. The id is 256 random bits, it
// is handed to the browser in a cookie that outlives nothing else, and Take
// removes it — so a captured challenge response cannot be replayed even inside
// the two minutes.
type Ceremonies struct {
	mu   sync.Mutex
	live map[string]ceremony
	ttl  time.Duration
}

type ceremony struct {
	data webauthn.SessionData
	kind string
	// note carries the enrollment code a registration was authorised by, so the
	// code that was checked before the Touch ID prompt is the same one claimed
	// after it. Sending it twice from the browser would let the two disagree.
	note    string
	expires time.Time
}

// Ceremony kinds. A registration challenge must not be redeemable as a login
// and the reverse, so the kind is checked on the way out — otherwise the two
// endpoints would be one oracle with two doors.
const (
	KindRegister = "register"
	KindLogin    = "login"
)

func NewCeremonies(ttl time.Duration) *Ceremonies {
	if ttl <= 0 {
		ttl = CeremonyTTL
	}
	return &Ceremonies{live: make(map[string]ceremony), ttl: ttl}
}

// Put files the session data and returns the opaque id that names it.
func (c *Ceremonies) Put(kind, note string, data *webauthn.SessionData) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate ceremony id: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	c.live[id] = ceremony{data: *data, kind: kind, note: note, expires: time.Now().Add(c.ttl)}
	return id, nil
}

// Take removes a ceremony and returns it with its note, if it is live and of
// the kind asked for. Single use: a second call with the same id finds nothing,
// which is what stops a captured response being replayed inside the two minutes.
func (c *Ceremonies) Take(id, kind string) (webauthn.SessionData, string, bool) {
	if id == "" {
		return webauthn.SessionData{}, "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.live[id]
	if !ok {
		return webauthn.SessionData{}, "", false
	}
	delete(c.live, id)
	if held.kind != kind || time.Now().After(held.expires) {
		return webauthn.SessionData{}, "", false
	}
	return held.data, held.note, true
}

// sweepLocked drops what has expired. Called on the write path rather than from
// a goroutine: the map only grows when somebody starts a ceremony, so the moment
// one starts is exactly the moment worth tidying, and a server nobody is signing
// in to needs no timer running.
func (c *Ceremonies) sweepLocked() {
	now := time.Now()
	for id, held := range c.live {
		if now.After(held.expires) {
			delete(c.live, id)
		}
	}
}
