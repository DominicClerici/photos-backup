// Package vault keeps the Archive and the Hidden buckets: what is in them, how
// it is encrypted, and how it gets in and out.
//
// The rule the whole package is built around is that **putting something in
// must not require the password, and taking anything out must**. Hiding a
// photograph happens at a right-click in a gallery that has been open all
// afternoon; a password prompt there would teach anybody to leave the vault
// unlocked, which costs more than every property the password was buying. So
// the vault is a keypair rather than a password-derived key: the public half is
// in the clear and anything may encrypt with it, the private half is sealed
// under Argon2id and nothing reads a byte back without it.
//
// What that buys is a fully working "hide this" on a locked vault, and a locked
// vault that is genuinely opaque — the ciphertext on disk, the sealed metadata
// in Postgres, and the public key are the entire attack surface, and none of
// them can be turned into a photograph.
//
// What it costs is stated where it is paid: see the scrub in db/vault.go for
// the two columns that stay in the clear and why.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

var (
	// ErrLocked means an operation needed the private key and the vault does
	// not currently hold it.
	ErrLocked = errors.New("vault: locked")
	// ErrBadPassword means the password did not open the sealed private key.
	// Indistinguishable from a corrupted secret row on purpose: both are "this
	// password does not work", and telling them apart is a service to whoever
	// is guessing.
	ErrBadPassword = errors.New("vault: that password does not open this vault")
	// ErrCorrupt means a sealed payload did not authenticate. The bytes were
	// altered, truncated, or belong to something else.
	ErrCorrupt = errors.New("vault: sealed data did not authenticate")
	// ErrNoVault means no vault has been created on this archive yet.
	ErrNoVault = errors.New("vault: no vault has been set up")
)

// KDF is the cost the password is put through, stored beside the ciphertext it
// protects rather than assumed.
//
// The parameters travel with the secret because raising them later must not
// lock somebody out of a vault sealed under the old ones. Changing these
// changes what a *new* vault costs and what a password change re-seals under;
// an existing vault keeps opening at the cost it was sealed at until then.
type KDF struct {
	Salt    []byte
	Time    uint32
	Memory  uint32
	Threads uint8
}

// Argon2id at 64MiB and three passes.
//
// The threat is somebody who has the disk and all the time in the world, so the
// only defence is making each guess expensive. 64MiB is the memory hardness
// that makes a GPU stop being an advantage; three passes puts a single
// derivation at a few hundred milliseconds on this machine, which is invisible
// once per unlock and ruinous a few billion times.
const (
	defaultKDFTime    = 3
	defaultKDFMemory  = 64 * 1024 // KiB
	defaultKDFThreads = 4
	kdfSaltLen        = 16
	keyLen            = 32
)

// NewKDF mints the parameters for a vault being created now.
func NewKDF() (KDF, error) {
	salt := make([]byte, kdfSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return KDF{}, fmt.Errorf("generate kdf salt: %w", err)
	}
	return KDF{Salt: salt, Time: defaultKDFTime, Memory: defaultKDFMemory, Threads: defaultKDFThreads}, nil
}

// key stretches a password into the 32 bytes that wrap the private key.
func (k KDF) key(password string) []byte {
	return argon2.IDKey([]byte(password), k.Salt, k.Time, k.Memory, k.Threads, keyLen)
}

// Secret is the vault's identity as it is stored: a public key anything may
// encrypt to, and a private key only a password opens.
type Secret struct {
	PublicKey []byte
	KDF       KDF
	// SealedPrivate is the X25519 private key under AES-256-GCM keyed by the
	// password. Its authentication tag is the whole of how a wrong password is
	// recognised — there is no separate verifier that could get out of step
	// with it, and no way to test a guess more cheaply than by paying Argon2id.
	SealedPrivate []byte
}

// Create mints a new vault under a password.
//
// Called exactly once per archive, the first time anything is hidden. There is
// no recovery key and no escrow: a forgotten password is the photographs, and
// saying otherwise would be the lie that makes the rest of this pointless.
func Create(password string) (Secret, *ecdh.PrivateKey, error) {
	if password == "" {
		return Secret{}, nil, errors.New("vault: a password is required")
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Secret{}, nil, fmt.Errorf("generate vault key: %w", err)
	}
	kdf, err := NewKDF()
	if err != nil {
		return Secret{}, nil, err
	}
	sealed, err := wrap(kdf.key(password), priv.Bytes())
	if err != nil {
		return Secret{}, nil, err
	}
	return Secret{PublicKey: priv.PublicKey().Bytes(), KDF: kdf, SealedPrivate: sealed}, priv, nil
}

// Unlock opens the sealed private key. The returned identity is what every read
// path needs and what nothing on the write path is given.
func (s Secret) Unlock(password string) (*ecdh.PrivateKey, error) {
	raw, err := unwrap(s.KDF.key(password), s.SealedPrivate)
	if err != nil {
		return nil, ErrBadPassword
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, ErrBadPassword
	}
	return priv, nil
}

// Rewrap re-seals the same private key under a new password.
//
// The identity does not change, which is the whole point: every file and every
// sealed document in the vault was encrypted to this public key, and changing a
// password must not mean rewriting a hundred gigabytes. New KDF parameters are
// minted, so a password change is also how a vault created under weaker costs
// gets the current ones.
func Rewrap(priv *ecdh.PrivateKey, password string) (Secret, error) {
	if password == "" {
		return Secret{}, errors.New("vault: a password is required")
	}
	kdf, err := NewKDF()
	if err != nil {
		return Secret{}, err
	}
	sealed, err := wrap(kdf.key(password), priv.Bytes())
	if err != nil {
		return Secret{}, err
	}
	return Secret{PublicKey: priv.PublicKey().Bytes(), KDF: kdf, SealedPrivate: sealed}, nil
}

// Recipient is the public half, as the write path holds it.
func (s Secret) Recipient() (*ecdh.PublicKey, error) {
	pub, err := ecdh.X25519().NewPublicKey(s.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("vault: unusable public key: %w", err)
	}
	return pub, nil
}

// wrap and unwrap are plain AES-256-GCM with a random nonce, used only for the
// private key: one small payload, one key, and a fresh nonce every time it is
// written. The streaming construction below exists because a 550MB video is
// none of those things.
func wrap(key, plaintext []byte) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func unwrap(key, sealed []byte) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, ErrCorrupt
	}
	out, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return nil, ErrCorrupt
	}
	return out, nil
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nil
}

// A payload's key is derived per payload, from an ephemeral X25519 exchange
// with the vault's public key. Two consequences worth naming:
//
// Every file and every document gets a key nothing else has, so a nonce scheme
// that would be reckless under a shared key — a counter from zero — is exactly
// right here.
//
// And the context string is in the derivation rather than in an AAD parameter,
// which means a ciphertext moved to another asset's path, or a thumbnail
// renamed to look like an original, derives a different key and fails to open.
// Binding by construction rather than by remembering to pass the right AAD.
func derive(shared, ephPub, vaultPub []byte, context string) ([]byte, error) {
	// Both halves of the exchange write the salt the same way — the ephemeral
	// public key, then the vault's — which is the only thing making the two
	// sides agree on a key.
	salt := make([]byte, 0, len(ephPub)+len(vaultPub))
	salt = append(append(salt, ephPub...), vaultPub...)

	key, err := hkdf.Key(sha256.New, shared, salt, "photobackup/vault/v1\x00"+context+"\x00", keyLen)
	if err != nil {
		return nil, fmt.Errorf("vault: derive key: %w", err)
	}
	return key, nil
}

// sealKey mints an ephemeral keypair and returns the payload key plus the
// ephemeral public key that has to travel with the ciphertext.
func sealKey(to *ecdh.PublicKey, context string) (key, ephPub []byte, err error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: ephemeral key: %w", err)
	}
	shared, err := eph.ECDH(to)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: key exchange: %w", err)
	}
	ephPub = eph.PublicKey().Bytes()
	key, err = derive(shared, ephPub, to.Bytes(), context)
	if err != nil {
		return nil, nil, err
	}
	return key, ephPub, nil
}

// openKey recovers the same payload key from the private half.
func openKey(priv *ecdh.PrivateKey, ephPub []byte, context string) ([]byte, error) {
	eph, err := ecdh.X25519().NewPublicKey(ephPub)
	if err != nil {
		return nil, ErrCorrupt
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return nil, ErrCorrupt
	}
	return derive(shared, ephPub, priv.PublicKey().Bytes(), context)
}

// docMagic and fileMagic make a stray file or a mangled column identifiable
// before anything tries to decrypt it, and make the two formats impossible to
// confuse with each other.
var (
	docMagic  = []byte("PBVDOC01")
	fileMagic = []byte("PBVFIL01")
)

const (
	magicLen  = 8
	ephPubLen = 32
	tagLen    = 16
	// chunkSize is the plaintext a single GCM seal covers.
	//
	// 64KiB is the size at which the per-chunk tag is a rounding error (0.02%)
	// and a range request still only has to decrypt a page's worth to answer.
	// It is part of the on-disk format: changing it makes every existing vault
	// file unreadable.
	chunkSize = 64 * 1024
	// nonceCounterLen leaves the last nonce byte for the final-chunk flag.
	nonceCounterLen = 11
)

// SealDoc encrypts a small payload — a sealed metadata document — to the vault.
//
// Layout: magic, ephemeral public key, nonce, ciphertext. Single-shot because
// these are kilobytes and live in a bytea column, where streaming would buy
// nothing.
func SealDoc(to *ecdh.PublicKey, context string, plaintext []byte) ([]byte, error) {
	key, ephPub, err := sealKey(to, "doc\x00"+context)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, magicLen+ephPubLen+gcm.NonceSize()+len(plaintext)+tagLen)
	out = append(out, docMagic...)
	out = append(out, ephPub...)

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, nil), nil
}

// OpenDoc reverses SealDoc. A context that does not match the one it was sealed
// under fails exactly as a wrong key does, which is the binding described above.
func OpenDoc(priv *ecdh.PrivateKey, context string, sealed []byte) ([]byte, error) {
	if len(sealed) < magicLen+ephPubLen || string(sealed[:magicLen]) != string(docMagic) {
		return nil, ErrCorrupt
	}
	key, err := openKey(priv, sealed[magicLen:magicLen+ephPubLen], "doc\x00"+context)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	body := sealed[magicLen+ephPubLen:]
	if len(body) < gcm.NonceSize() {
		return nil, ErrCorrupt
	}
	out, err := gcm.Open(nil, body[:gcm.NonceSize()], body[gcm.NonceSize():], nil)
	if err != nil {
		return nil, ErrCorrupt
	}
	return out, nil
}

// EncryptFile writes r to w as a vault file.
//
// The construction is STREAM: the plaintext is cut into fixed chunks, each
// sealed under the file's own key with the chunk index as its nonce, and the
// last chunk is marked as last in the nonce itself.
//
// The chunk index is what makes a reordered or duplicated chunk fail, and the
// final flag is what makes a *truncated* file fail — which is the failure a
// naive "encrypt each block" scheme misses, and the one that matters here,
// because a truncated video decrypts perfectly and is simply a shorter video.
//
// Nothing is held in memory but one chunk, so a 4GB original costs 64KiB.
func EncryptFile(to *ecdh.PublicKey, context string, r io.Reader, w io.Writer) (written int64, err error) {
	key, ephPub, err := sealKey(to, "file\x00"+context)
	if err != nil {
		return 0, err
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return 0, err
	}

	header := make([]byte, 0, magicLen+ephPubLen)
	header = append(header, fileMagic...)
	header = append(header, ephPub...)
	n, err := w.Write(header)
	written += int64(n)
	if err != nil {
		return written, fmt.Errorf("write vault header: %w", err)
	}

	plain := make([]byte, chunkSize)
	sealed := make([]byte, 0, chunkSize+tagLen)
	nonce := make([]byte, gcm.NonceSize())

	for index := uint64(0); ; index++ {
		read, readErr := io.ReadFull(r, plain)
		last := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if readErr != nil && !last {
			return written, fmt.Errorf("read plaintext: %w", readErr)
		}
		// A file whose length is an exact multiple of the chunk size ends with
		// an empty final chunk rather than with an unmarked full one. Costs 16
		// bytes; buys the truncation guarantee at every length.
		writeNonce(nonce, index, last)
		sealed = gcm.Seal(sealed[:0], nonce, plain[:read], nil)
		n, err := w.Write(sealed)
		written += int64(n)
		if err != nil {
			return written, fmt.Errorf("write vault chunk: %w", err)
		}
		if last {
			return written, nil
		}
	}
}

// writeNonce lays out the 12-byte nonce: an 11-byte big-endian chunk index and
// a final-chunk flag.
//
// Safe as a counter from zero only because the key is unique to this file —
// see exchange. Under a shared key this would be the classic catastrophe.
func writeNonce(nonce []byte, index uint64, last bool) {
	for i := range nonce {
		nonce[i] = 0
	}
	binary.BigEndian.PutUint64(nonce[nonceCounterLen-8:nonceCounterLen], index)
	if last {
		nonce[nonceCounterLen] = 1
	}
}

// PlaintextSize is the length of the original inside a vault file of this size.
//
// Exact rather than approximate, and computable without reading anything: every
// chunk but the last is full, so the number of chunks follows from the file
// size and the tags come off arithmetically. It is what lets a vaulted video be
// served with a Content-Length and a working seek bar.
func PlaintextSize(encrypted int64) (int64, error) {
	body := encrypted - int64(magicLen+ephPubLen)
	if body < tagLen {
		return 0, ErrCorrupt
	}
	chunks := body / int64(chunkSize+tagLen)
	if body%int64(chunkSize+tagLen) != 0 {
		chunks++
	}
	size := body - chunks*tagLen
	if size < 0 {
		return 0, ErrCorrupt
	}
	return size, nil
}
