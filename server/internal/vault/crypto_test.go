package vault

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A vault password is the photographs, so the tests that matter here are the
// ones about the ways it can be wrong: the wrong password, the altered
// ciphertext, and the file that was cut short. The happy path is table stakes.

func TestPasswordOpensTheVaultAndNothingElseDoes(t *testing.T) {
	secret, priv, err := Create("correct horse battery staple")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	opened, err := secret.Unlock("correct horse battery staple")
	if err != nil {
		t.Fatalf("unlock with the right password: %v", err)
	}
	if !bytes.Equal(opened.Bytes(), priv.Bytes()) {
		t.Fatal("unlocked a different key than was created")
	}

	if _, err := secret.Unlock("correct horse battery stapl"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("one character short: got %v, want ErrBadPassword", err)
	}
	if _, err := secret.Unlock(""); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("empty password: got %v, want ErrBadPassword", err)
	}
}

// Changing the password must not change the identity, because every file in the
// vault is encrypted to it. This is the test that would fail if somebody
// "simplified" Rewrap into Create.
func TestChangingThePasswordKeepsTheIdentity(t *testing.T) {
	secret, _, err := Create("first password")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	priv, err := secret.Unlock("first password")
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}

	rewrapped, err := Rewrap(priv, "second password")
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if !bytes.Equal(rewrapped.PublicKey, secret.PublicKey) {
		t.Fatal("the public key changed; everything already in the vault would be unreadable")
	}
	if bytes.Equal(rewrapped.KDF.Salt, secret.KDF.Salt) {
		t.Fatal("the salt was reused; a password change should mint new parameters")
	}
	if _, err := rewrapped.Unlock("first password"); !errors.Is(err, ErrBadPassword) {
		t.Fatal("the old password still opens the vault")
	}
	if _, err := rewrapped.Unlock("second password"); err != nil {
		t.Fatalf("the new password does not open the vault: %v", err)
	}
}

// A document sealed for one asset must not open under another's name. This is
// what stops a sealed blob being moved onto a different row.
func TestASealedDocumentIsBoundToItsAsset(t *testing.T) {
	secret, priv, err := Create("a password for the vault")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	to, err := secret.Recipient()
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}

	sealed, err := SealAsset(to, "asset-one", []byte(`{"filename":"IMG_0001.HEIC"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	opened, err := OpenDoc(priv, "asset\x00asset-one", sealed)
	if err != nil {
		t.Fatalf("open under its own name: %v", err)
	}
	if string(opened) != `{"filename":"IMG_0001.HEIC"}` {
		t.Fatalf("round trip changed the document: %s", opened)
	}

	if _, err := OpenDoc(priv, "asset\x00asset-two", sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a document opened under another asset's name: %v", err)
	}

	// One flipped bit anywhere in the payload.
	altered := bytes.Clone(sealed)
	altered[len(altered)-1] ^= 1
	if _, err := OpenDoc(priv, "asset\x00asset-one", altered); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("an altered document opened: %v", err)
	}
}

// Sizes either side of the chunk boundary, because the format's two awkward
// cases are the empty file and the one whose length is an exact multiple of the
// chunk — the second is why an empty final chunk is written at all.
func TestFilesRoundTripAtEveryAwkwardLength(t *testing.T) {
	secret, priv, err := Create("a password for the vault")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	to, _ := secret.Recipient()

	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		plain := make([]byte, size)
		if _, err := rand.Read(plain); err != nil {
			t.Fatalf("random bytes: %v", err)
		}

		var sealed bytes.Buffer
		written, err := EncryptFile(to, "blob\x00test", bytes.NewReader(plain), &sealed)
		if err != nil {
			t.Fatalf("encrypt %d bytes: %v", size, err)
		}
		if written != int64(sealed.Len()) {
			t.Fatalf("%d bytes: reported %d written, buffer holds %d", size, written, sealed.Len())
		}

		if got, err := PlaintextSize(written); err != nil || got != int64(size) {
			t.Fatalf("%d bytes: PlaintextSize said %d (%v)", size, got, err)
		}

		reader := readerOver(t, priv, "blob\x00test", sealed.Bytes())
		back, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("decrypt %d bytes: %v", size, err)
		}
		if !bytes.Equal(back, plain) {
			t.Fatalf("%d bytes: round trip changed the contents", size)
		}
		reader.Close()
	}
}

// The failure a naive block cipher misses. A truncated video decrypts perfectly
// and is simply a shorter video, which is why the final chunk is marked as
// final in the nonce rather than inferred from the file's length.
func TestATruncatedFileDoesNotDecrypt(t *testing.T) {
	secret, priv, err := Create("a password for the vault")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	to, _ := secret.Recipient()

	plain := make([]byte, 3*chunkSize)
	if _, err := rand.Read(plain); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	var sealed bytes.Buffer
	if _, err := EncryptFile(to, "blob\x00test", bytes.NewReader(plain), &sealed); err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// Cut the last chunk off entirely: the remaining bytes are a complete,
	// well-formed prefix, and the only thing wrong with them is that the chunk
	// they now end on does not claim to be the last.
	cut := sealed.Bytes()[:magicLen+ephPubLen+2*(chunkSize+tagLen)]
	reader := readerOver(t, priv, "blob\x00test", cut)
	defer reader.Close()

	if _, err := io.ReadAll(reader); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a truncated file decrypted: %v", err)
	}
}

// The whole reason the reader is a Seeker: a Range request on a vaulted video
// has to decrypt the chunks it covers rather than the file up to them.
func TestSeekingReadsFromTheRightChunk(t *testing.T) {
	secret, priv, err := Create("a password for the vault")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	to, _ := secret.Recipient()

	plain := make([]byte, 5*chunkSize+123)
	for i := range plain {
		plain[i] = byte(i * 7)
	}
	var sealed bytes.Buffer
	if _, err := EncryptFile(to, "blob\x00test", bytes.NewReader(plain), &sealed); err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	reader := readerOver(t, priv, "blob\x00test", sealed.Bytes())
	defer reader.Close()

	if reader.Size() != int64(len(plain)) {
		t.Fatalf("Size said %d, want %d", reader.Size(), len(plain))
	}

	for _, at := range []int64{0, 1, chunkSize - 1, chunkSize, 4*chunkSize + 500, int64(len(plain)) - 10} {
		if _, err := reader.Seek(at, io.SeekStart); err != nil {
			t.Fatalf("seek to %d: %v", at, err)
		}
		want := plain[at:min64(at+64, int64(len(plain)))]
		got := make([]byte, len(want))
		if _, err := io.ReadFull(reader, got); err != nil {
			t.Fatalf("read at %d: %v", at, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("read at %d returned the wrong bytes", at)
		}
	}
}

// A file sealed under one digest's path must not open under another's, which is
// what stops a thumbnail being renamed to look like the original beside it.
func TestAFileIsBoundToItsPath(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	secret, priv, err := Create("a password for the vault")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	to, _ := secret.Recipient()

	const (
		mine  = "aaaaaaaabbbbbbbbccccccccdddddddd11111111222222223333333344444444"
		yours = "eeeeeeeeffffffff0000000099999999aaaaaaaabbbbbbbbccccccccdddddddd"
	)
	if _, err := store.Put(to, mine, "", bytes.NewReader([]byte("the photograph"))); err != nil {
		t.Fatalf("put: %v", err)
	}

	reader, err := store.Open(priv, mine, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	back, _ := io.ReadAll(reader)
	reader.Close()
	if string(back) != "the photograph" {
		t.Fatalf("round trip changed the file: %q", back)
	}

	// Move it to another digest's path, exactly as a confused restore or a
	// malicious hand would.
	moved := store.Path(yours, "")
	if err := os.MkdirAll(filepath.Dir(moved), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(store.Path(mine, ""), moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := store.Open(priv, yours, ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a relocated vault file opened: %v", err)
	}
}

// Locked means locked: no identity, no read, and no partial answer.
func TestALockedVaultReadsNothing(t *testing.T) {
	keeper := NewKeeper(0)
	if keeper.Unlocked() {
		t.Fatal("a fresh keeper is unlocked")
	}
	if _, err := keeper.Identity(); !errors.Is(err, ErrLocked) {
		t.Fatalf("Identity on a locked keeper: %v", err)
	}

	secret, _, err := Create("a password for the vault")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := keeper.Unlock(secret, "wrong"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("unlock with the wrong password: %v", err)
	}
	if keeper.Unlocked() {
		t.Fatal("a failed unlock left the vault open")
	}
	if err := keeper.Unlock(secret, "a password for the vault"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if !keeper.Unlocked() {
		t.Fatal("a successful unlock left the vault shut")
	}

	// The public key survives a lock, because hiding a photograph has to work
	// on a locked vault and that is the key it needs.
	keeper.Lock()
	if keeper.Unlocked() {
		t.Fatal("Lock did not lock")
	}
	if _, err := keeper.Recipient(secret); err != nil {
		t.Fatalf("a locked vault cannot encrypt: %v", err)
	}
}

func readerOver(t *testing.T, priv *ecdh.PrivateKey, binding string, sealed []byte) *Reader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sealed")
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	r, err := NewReader(priv, binding, f)
	if err != nil {
		f.Close()
		t.Fatalf("new reader: %v", err)
	}
	return r
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
