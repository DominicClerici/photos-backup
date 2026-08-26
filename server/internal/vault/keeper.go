package vault

import (
	"crypto/ecdh"
	"sync"
	"time"
)

// DefaultIdle is how long an unlocked vault stays unlocked without being used.
//
// Fifteen minutes is the compromise between the two ways of getting this wrong.
// Shorter and somebody browsing their own archive types a password every few
// minutes, which is how people end up choosing a password they can type
// quickly. Longer and a machine walked away from is an open vault, which is the
// threat this whole feature is about.
//
// It is idle time rather than a fixed session: a vault being read is a vault
// somebody is sitting in front of.
const DefaultIdle = 15 * time.Minute

// Keeper holds the private key while the vault is unlocked, and nothing else.
//
// It is one per server rather than one per client, and that is still a real
// property of this deployment. This comment used to say the reason was that the
// gallery was unauthenticated and "who is asking" was unanswerable; that is no
// longer true — every route is behind a passkey session or a device token now.
// What remains true is that there is one archive and one person, so unlocking
// the vault unlocks it for every signed-in client rather than for the tab that
// typed the password. That is a deliberate simplification and not an oversight:
// scoping the key to a session would mean the phone and the laptop each type
// the password, which is exactly the friction that teaches people to leave a
// vault unlocked.
//
// The idle timeout is what carries the weight instead, and it is why it is idle
// time rather than a fixed session.
//
// What it does not do is write the key anywhere. It is in memory, it is dropped
// on a lock, on an idle timeout, and on a restart, and there is no path from
// the disk to it that does not go through the password.
type Keeper struct {
	mu   sync.Mutex
	priv *ecdh.PrivateKey
	// pub is cached from the last secret seen, so the write path can encrypt
	// without a database round trip per file. It is public; caching it costs
	// nothing.
	pub   *ecdh.PublicKey
	timer *time.Timer
	idle  time.Duration
	// at is when the key was last used, reported so the UI can say how long is
	// left rather than surprising somebody mid-scroll.
	at time.Time
}

func NewKeeper(idle time.Duration) *Keeper {
	if idle <= 0 {
		idle = DefaultIdle
	}
	return &Keeper{idle: idle}
}

// Unlock puts the identity in memory and starts the idle clock.
func (k *Keeper) Unlock(secret Secret, password string) error {
	priv, err := secret.Unlock(password)
	if err != nil {
		return err
	}
	pub, err := secret.Recipient()
	if err != nil {
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	k.priv, k.pub = priv, pub
	k.touch()
	return nil
}

// Hold puts an identity that has just been created in memory, so that creating
// a vault leaves it open rather than asking for the password again one line
// later.
func (k *Keeper) Hold(priv *ecdh.PrivateKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.priv, k.pub = priv, priv.PublicKey()
	k.touch()
}

// Lock drops the key.
func (k *Keeper) Lock() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.drop()
}

// Identity is the private key, or ErrLocked. Every read path in the vault goes
// through here, which is also what keeps the idle clock honest: the timer is
// reset by use rather than by a heartbeat the browser could send while nobody
// is looking.
func (k *Keeper) Identity() (*ecdh.PrivateKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.priv == nil {
		return nil, ErrLocked
	}
	k.touch()
	return k.priv, nil
}

// Unlocked reports the state without extending it. Asked by the status
// endpoint, which the gallery polls — and a poll must not be what keeps a vault
// open on an empty desk.
func (k *Keeper) Unlocked() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.priv != nil
}

// Expires is when the key will be dropped if nothing touches it. Zero while
// locked.
func (k *Keeper) Expires() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.priv == nil {
		return time.Time{}
	}
	return k.at.Add(k.idle)
}

// Recipient is the public half. It is answered from the cache when there is
// one and otherwise from the secret the caller supplies, because the write path
// has to work on a server that has never been unlocked since it started.
func (k *Keeper) Recipient(secret Secret) (*ecdh.PublicKey, error) {
	k.mu.Lock()
	cached := k.pub
	k.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	pub, err := secret.Recipient()
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.pub = pub
	k.mu.Unlock()
	return pub, nil
}

// touch and drop are called with the lock held.
func (k *Keeper) touch() {
	k.at = time.Now()
	if k.timer != nil {
		k.timer.Stop()
	}
	k.timer = time.AfterFunc(k.idle, k.Lock)
}

func (k *Keeper) drop() {
	if k.timer != nil {
		k.timer.Stop()
		k.timer = nil
	}
	k.priv = nil
	// The public key stays: it is public, and dropping it would mean a locked
	// server could not encrypt, which is the one thing a locked server must be
	// able to do.
}
