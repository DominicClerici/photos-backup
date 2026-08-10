// Package blobstore stores immutable, content-addressed originals on disk.
package blobstore

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrChecksumMismatch means the bytes that arrived do not match the digest
	// the client declared, so the upload is discarded rather than committed.
	ErrChecksumMismatch = errors.New("blobstore: declared md5 does not match received bytes")
	// ErrSizeMismatch means the body was shorter or longer than declared.
	ErrSizeMismatch = errors.New("blobstore: declared size does not match received bytes")
)

// Expected carries the digest and length the client declared for an upload.
type Expected struct {
	MD5  string
	Size int64
}

// Result describes a blob after it has been committed to the store.
type Result struct {
	SHA256 string
	MD5    string
	Size   int64
	// Created is false when an identical blob was already present.
	Created bool
}

type Store struct {
	root string
}

func New(root string) *Store {
	return &Store{root: root}
}

// Path returns the location of a blob. It is a pure function of the digest and
// extension, so it stays valid across restarts and rebuilds.
func (s *Store) Path(sha256hex, ext string) string {
	return filepath.Join(s.root, "blobs", sha256hex[0:2], sha256hex[2:4], sha256hex+normalizeExt(ext))
}

func normalizeExt(ext string) string {
	ext = strings.ToLower(ext)
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

// Put streams r into the store and commits it under its SHA-256. The write is
// staged in a temp file on the same filesystem and renamed into place, so a
// crash can leave a stray temp file but never a partial blob.
func (s *Store) Put(r io.Reader, ext string, expect Expected) (Result, error) {
	tmpDir := filepath.Join(s.root, "blobs", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create temp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpName)
		}
	}()

	shaSum := sha256.New()
	md5Sum := md5.New()
	size, err := io.Copy(io.MultiWriter(tmp, shaSum, md5Sum), r)
	if err != nil {
		return Result{}, fmt.Errorf("stream body: %w", err)
	}

	res := Result{
		SHA256: hex.EncodeToString(shaSum.Sum(nil)),
		MD5:    hex.EncodeToString(md5Sum.Sum(nil)),
		Size:   size,
	}

	if res.Size != expect.Size {
		return Result{}, fmt.Errorf("%w: declared %d, received %d", ErrSizeMismatch, expect.Size, res.Size)
	}
	if !strings.EqualFold(res.MD5, expect.MD5) {
		return Result{}, fmt.Errorf("%w: declared %s, received %s", ErrChecksumMismatch, expect.MD5, res.MD5)
	}

	if err := tmp.Sync(); err != nil {
		return Result{}, fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("close temp file: %w", err)
	}

	dst := s.Path(res.SHA256, ext)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Result{}, fmt.Errorf("create blob dir: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		return res, nil
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return Result{}, fmt.Errorf("commit blob: %w", err)
	}
	committed = true
	res.Created = true
	return res, nil
}

// Adopt commits a file that is already complete on disk, verifying it against
// the same declaration a streamed upload is held to and moving it into the blob
// tree.
//
// This is how a resumable upload finishes. The digests are computed by reading
// the file back rather than by carrying hash state across the chunks that built
// it: a session can span a server restart, and a re-read of even a 550MB video
// costs well under a second, where persisting and restoring the internal state
// of two hashes is a serialization format with nothing to gain.
//
// The source file is consumed. On success it has been renamed into the tree; on
// a digest mismatch it is left where it is for the caller to discard, because a
// store is not the right place to decide that someone's bytes should be
// deleted.
func (s *Store) Adopt(path, ext string, expect Expected) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("open staged upload: %w", err)
	}

	shaSum := sha256.New()
	md5Sum := md5.New()
	size, err := io.Copy(io.MultiWriter(shaSum, md5Sum), f)
	closeErr := f.Close()
	if err != nil {
		return Result{}, fmt.Errorf("read staged upload: %w", err)
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close staged upload: %w", closeErr)
	}

	res := Result{
		SHA256: hex.EncodeToString(shaSum.Sum(nil)),
		MD5:    hex.EncodeToString(md5Sum.Sum(nil)),
		Size:   size,
	}
	if res.Size != expect.Size {
		return Result{}, fmt.Errorf("%w: declared %d, received %d", ErrSizeMismatch, expect.Size, res.Size)
	}
	if !strings.EqualFold(res.MD5, expect.MD5) {
		return Result{}, fmt.Errorf("%w: declared %s, received %s", ErrChecksumMismatch, expect.MD5, res.MD5)
	}

	dst := s.Path(res.SHA256, ext)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Result{}, fmt.Errorf("create blob dir: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		// Already archived. The staged copy is redundant, not precious.
		os.Remove(path)
		return res, nil
	}
	if err := os.Rename(path, dst); err != nil {
		return Result{}, fmt.Errorf("commit blob: %w", err)
	}
	res.Created = true
	return res, nil
}

// Open returns a reader over a stored blob.
func (s *Store) Open(sha256hex, ext string) (*os.File, error) {
	return os.Open(s.Path(sha256hex, ext))
}
