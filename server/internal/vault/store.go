package vault

import (
	"crypto/cipher"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store is a tree of vault files, laid out exactly as blobstore and derivstore
// lay theirs out: the same two-level hash fanout, the same "the path is a pure
// function of the digest" rule, one suffix further along.
//
// There are two of these in a running server rather than one, and for the same
// reason there are two plaintext trees: an original belongs on the archive
// drive and a rendition belongs on the SSD the gallery is served from, and
// encrypting them did not change where they want to live.
type Store struct {
	root string
}

func NewStore(root string) *Store { return &Store{root: root} }

func (s *Store) Root() string { return s.root }

// Suffix marks a vault file. Present so that a stray one in a blob tree is
// recognisable for what it is, and so nothing can mistake it for an original.
const Suffix = ".enc"

// Path locates one encrypted rendition. `suffix` is empty for the original and
// otherwise one of derivstore's, which is what keeps the vault tree readable
// against the plaintext tree it mirrors.
func (s *Store) Path(sha256hex, suffix string) string {
	return filepath.Join(s.root, sha256hex[0:2], sha256hex[2:4], sha256hex+suffix+Suffix)
}

func (s *Store) Exists(sha256hex, suffix string) bool {
	info, err := os.Stat(s.Path(sha256hex, suffix))
	return err == nil && info.Size() > 0
}

// binding ties a ciphertext to the one place it is allowed to be.
//
// It goes into the key derivation, so a vault file moved to another digest's
// path — or a thumbnail renamed to look like the original beside it — derives a
// different key and does not open. The alternative is remembering to pass the
// right AAD at four call sites, which is a thing that gets forgotten once.
func binding(sha256hex, suffix string) string {
	return sha256hex + "\x00" + suffix
}

// Put encrypts r into the tree, staged and renamed so a crash cannot leave a
// half-written vault file where a whole one should be.
//
// It takes the public key, which is the point: this runs while the vault is
// locked, from the same click that hid the photograph.
func (s *Store) Put(to *ecdh.PublicKey, sha256hex, suffix string, r io.Reader) (int64, error) {
	dir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("create vault staging dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "seal-*")
	if err != nil {
		return 0, fmt.Errorf("create vault staging file: %w", err)
	}
	staged := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(staged)
		}
	}()

	written, err := EncryptFile(to, binding(sha256hex, suffix), r, tmp)
	if err != nil {
		return 0, err
	}
	// Unlike a derivative, a vault file cannot be rebuilt from anything: the
	// plaintext it came from is about to be deleted. So it is fsynced, the way
	// an original is.
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("fsync vault file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("close vault file: %w", err)
	}

	dst := s.Path(sha256hex, suffix)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, fmt.Errorf("create vault dir: %w", err)
	}
	if err := os.Rename(staged, dst); err != nil {
		return 0, fmt.Errorf("commit vault file: %w", err)
	}
	committed = true
	return written, nil
}

// PutFile is Put over a file already on disk, which is every caller: the thing
// being hidden is an archived original or a rendition made from one.
func (s *Store) PutFile(to *ecdh.PublicKey, sha256hex, suffix, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open plaintext to seal: %w", err)
	}
	defer f.Close()
	return s.Put(to, sha256hex, suffix, f)
}

// Open decrypts one vault file. The identity is required, which is the whole of
// what "locked" means here.
func (s *Store) Open(priv *ecdh.PrivateKey, sha256hex, suffix string) (*Reader, error) {
	if priv == nil {
		return nil, ErrLocked
	}
	f, err := os.Open(s.Path(sha256hex, suffix))
	if err != nil {
		return nil, err
	}
	r, err := NewReader(priv, binding(sha256hex, suffix), f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

// Remove deletes one vault file. A missing one is not an error, for the same
// reason it is not one in blobstore: the point is to reach a state.
func (s *Store) Remove(sha256hex, suffix string) (int64, error) {
	path := s.Path(sha256hex, suffix)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat vault file: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("remove vault file: %w", err)
	}
	return info.Size(), nil
}

// RemoveAll deletes every vault file for one original, across a list of
// suffixes the caller supplies — the empty string for the original, then
// whatever renditions exist.
func (s *Store) RemoveAll(sha256hex string, suffixes []string) (files int, bytes int64, err error) {
	for _, suffix := range suffixes {
		n, rmErr := s.Remove(sha256hex, suffix)
		if rmErr != nil {
			err = rmErr
			continue
		}
		if n > 0 {
			files++
			bytes += n
		}
	}
	return files, bytes, err
}

// Reader reads a vault file as though it were the plaintext.
//
// It is an io.ReadSeeker on purpose. The chunked construction means the
// plaintext at any offset lives in exactly one chunk, at a position arithmetic
// gives without reading anything before it — so a seek is a seek, and a vaulted
// video gets a working scrub bar and Range support from http.ServeContent for
// free. A whole-file streaming cipher would have made every seek a decrypt from
// byte zero.
type Reader struct {
	f      *os.File
	gcm    cipher.AEAD
	size   int64 // of the plaintext
	chunks int64
	pos    int64
	// held is the index of the chunk currently decrypted into buf, or -1.
	held int64
	buf  []byte
	raw  []byte
}

// NewReader opens a vault file from an already-open handle. The handle is the
// reader's from here: Close closes it.
func NewReader(priv *ecdh.PrivateKey, binding string, f *os.File) (*Reader, error) {
	if priv == nil {
		return nil, ErrLocked
	}
	header := make([]byte, magicLen+ephPubLen)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, ErrCorrupt
	}
	if string(header[:magicLen]) != string(fileMagic) {
		return nil, ErrCorrupt
	}
	key, err := openKey(priv, header[magicLen:], "file\x00"+binding)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat vault file: %w", err)
	}
	size, err := PlaintextSize(info.Size())
	if err != nil {
		return nil, err
	}

	body := info.Size() - int64(magicLen+ephPubLen)
	chunks := body / int64(chunkSize+tagLen)
	if body%int64(chunkSize+tagLen) != 0 {
		chunks++
	}
	reader := &Reader{
		f: f, gcm: gcm, size: size, chunks: chunks, held: -1,
		buf: make([]byte, 0, chunkSize),
		raw: make([]byte, chunkSize+tagLen),
	}
	// The first chunk is decrypted here rather than on the first Read, so that
	// opening a vault file either authenticates or fails.
	//
	// Without it the wrong key is a mid-stream error: http.ServeContent has
	// already written a 200 and half a photograph by the time anything notices,
	// and the browser draws a corrupt image rather than showing the failure. The
	// cost is one AES-GCM open of 64KiB, which a sequential read was going to
	// pay anyway.
	if err := reader.load(0); err != nil {
		return nil, err
	}
	return reader, nil
}

// Size is the length of the plaintext, which is what a Content-Length has to
// say and what a Range request is resolved against.
func (r *Reader) Size() int64 { return r.size }

func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	index := r.pos / chunkSize
	if err := r.load(index); err != nil {
		return 0, err
	}
	n := copy(p, r.buf[r.pos-index*chunkSize:])
	r.pos += int64(n)
	return n, nil
}

func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, errors.New("vault: invalid whence")
	}
	if next < 0 {
		return 0, errors.New("vault: seek before the start of the file")
	}
	r.pos = next
	return next, nil
}

func (r *Reader) Close() error { return r.f.Close() }

// load decrypts one chunk into buf, if it is not already there.
//
// The final-chunk flag is part of the nonce, so a truncated file fails here
// rather than decrypting into a shorter, perfectly valid-looking photograph.
func (r *Reader) load(index int64) error {
	if r.held == index {
		return nil
	}
	at := int64(magicLen+ephPubLen) + index*int64(chunkSize+tagLen)
	if _, err := r.f.Seek(at, io.SeekStart); err != nil {
		return fmt.Errorf("seek vault file: %w", err)
	}
	n, err := io.ReadFull(r.f, r.raw)
	if err != nil && err != io.ErrUnexpectedEOF {
		return ErrCorrupt
	}

	nonce := make([]byte, r.gcm.NonceSize())
	writeNonce(nonce, uint64(index), index == r.chunks-1)
	out, err := r.gcm.Open(r.buf[:0], nonce, r.raw[:n], nil)
	if err != nil {
		return ErrCorrupt
	}
	r.buf = out
	r.held = index
	return nil
}
