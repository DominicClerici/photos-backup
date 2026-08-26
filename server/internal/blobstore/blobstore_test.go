package blobstore

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func digests(b []byte) (sha, m string) {
	s := sha256.Sum256(b)
	d := md5.Sum(b)
	return hex.EncodeToString(s[:]), hex.EncodeToString(d[:])
}

// blobTreeFiles lists every file under blobs/, including staged temp files, so
// tests can assert that a rejected upload left nothing behind.
func blobTreeFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			found = append(found, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk blob tree: %v", err)
	}
	return found
}

func TestPutRejectsMD5Mismatch(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("bytes that arrive corrupted")

	_, err := s.Put(bytes.NewReader(content), ".heic", Expected{
		MD5:  "00000000000000000000000000000000",
		Size: int64(len(content)),
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Put error = %v, want ErrChecksumMismatch", err)
	}
	if files := blobTreeFiles(t, root); len(files) != 0 {
		t.Errorf("rejected upload left files behind: %v", files)
	}
}

func TestPutRejectsSizeMismatch(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("a truncated body")
	_, m := digests(content)

	_, err := s.Put(bytes.NewReader(content), ".heic", Expected{MD5: m, Size: 9999})
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("Put error = %v, want ErrSizeMismatch", err)
	}
	if files := blobTreeFiles(t, root); len(files) != 0 {
		t.Errorf("rejected upload left files behind: %v", files)
	}
}

func TestPutSameBytesTwiceStoresOneBlob(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("a photo uploaded twice")
	_, m := digests(content)
	expect := Expected{MD5: m, Size: int64(len(content))}

	first, err := s.Put(bytes.NewReader(content), ".heic", expect)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := s.Put(bytes.NewReader(content), ".heic", expect)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if !first.Created {
		t.Errorf("first Put Created = false, want true")
	}
	if second.Created {
		t.Errorf("second Put Created = true, want false for identical bytes")
	}
	if second.SHA256 != first.SHA256 {
		t.Errorf("digests differ across identical uploads")
	}
	if files := blobTreeFiles(t, root); len(files) != 1 {
		t.Errorf("blob tree holds %d files, want exactly 1: %v", len(files), files)
	}
}

func TestOpenReturnsStoredBytes(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("bytes to read back")
	_, m := digests(content)

	res, err := s.Put(bytes.NewReader(content), ".HEIC", Expected{MD5: m, Size: int64(len(content))})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	f, err := s.Open(res.SHA256, ".HEIC")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("Open returned %q, want %q", got, content)
	}
}

func TestPutStoresBlobAtFanoutPath(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("the original bytes of a photo")
	sha, m := digests(content)

	res, err := s.Put(bytes.NewReader(content), ".HEIC", Expected{MD5: m, Size: int64(len(content))})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := filepath.Join(root, "blobs", sha[0:2], sha[2:4], sha+".heic")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("blob not at expected path %s: %v", want, err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("stored bytes differ from input")
	}
	if res.SHA256 != sha {
		t.Errorf("SHA256 = %q, want %q", res.SHA256, sha)
	}
	if res.MD5 != m {
		t.Errorf("MD5 = %q, want %q", res.MD5, m)
	}
	if res.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", res.Size, len(content))
	}
	if !res.Created {
		t.Errorf("Created = false, want true for a first write")
	}
}

// A client declares whichever digest it can produce, and every one it declares
// is checked. The phone has an md5 and the browser gallery has a sha256; the
// store does not care which, and cares very much that what arrives matches.

func TestPutAcceptsASHA256Instead(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("what the browser sends")
	sha, _ := digests(content)

	res, err := s.Put(bytes.NewReader(content), ".jpg", Expected{
		SHA256: sha,
		Size:   int64(len(content)),
	})
	if err != nil {
		t.Fatalf("Put with only a sha256: %v", err)
	}
	if res.SHA256 != sha || !res.Created {
		t.Errorf("Put stored %+v, want a fresh blob under %s", res, sha)
	}
}

func TestPutRejectsSHA256Mismatch(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("bytes that arrive corrupted")

	_, err := s.Put(bytes.NewReader(content), ".jpg", Expected{
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		Size:   int64(len(content)),
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Put error = %v, want ErrChecksumMismatch", err)
	}
	if files := blobTreeFiles(t, root); len(files) != 0 {
		t.Errorf("rejected upload left files behind: %v", files)
	}
}

// Declaring both is not an either/or: a client that gets one of them wrong is
// wrong, whichever one it is.
func TestPutChecksEveryDeclaredDigest(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("half-right")
	sha, _ := digests(content)

	_, err := s.Put(bytes.NewReader(content), ".jpg", Expected{
		SHA256: sha,
		MD5:    "00000000000000000000000000000000",
		Size:   int64(len(content)),
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Put error = %v, want ErrChecksumMismatch", err)
	}
}

// Declaring neither leaves the length as the whole of the check, which is what
// catches the failure a digest cannot tell apart from a different file: a body
// that was cut short.
func TestPutWithNoDigestStillChecksLength(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	content := []byte("no digest declared")

	if _, err := s.Put(bytes.NewReader(content), ".jpg", Expected{Size: int64(len(content))}); err != nil {
		t.Fatalf("Put with no digest: %v", err)
	}
	_, err := s.Put(bytes.NewReader(content), ".jpg", Expected{Size: int64(len(content)) + 1})
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("Put error = %v, want ErrSizeMismatch", err)
	}
}
