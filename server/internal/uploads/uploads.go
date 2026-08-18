// Package uploads holds partially-received originals so a large video survives
// a dropped connection, an app kill, or a server restart, and resumes from the
// last byte the server promised to have kept.
//
// The unit of state is a file on disk. A session's received length is the size
// of its `.part` file — not a counter in a database that could disagree with
// it — and every chunk is fsynced before its new offset is reported. An offset
// this package hands out is a promise, which is the only property that makes
// resuming safe.
package uploads

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrNotFound means no session exists under that id.
	ErrNotFound = errors.New("uploads: no such upload session")
	// ErrOffsetMismatch means a chunk arrived at an offset other than the one
	// the session is waiting for. Callers answer 409 and include the real
	// offset so the client can seek instead of starting over.
	ErrOffsetMismatch = errors.New("uploads: chunk does not start at the current offset")
	// ErrTooLong means a chunk would push the session past its declared size.
	ErrTooLong = errors.New("uploads: chunk exceeds the declared size")
	// ErrIncomplete means commit was called before all the bytes arrived.
	ErrIncomplete = errors.New("uploads: session is not complete")
)

// Declaration is what the client says it is about to send. It is written to a
// sidecar at session creation so the server can commit the upload even if the
// client never comes back with the metadata again.
type Declaration struct {
	DeviceID   string     `json:"device_id"`
	LocalID    string     `json:"local_id"`
	Filename   string     `json:"filename"`
	MD5        string     `json:"md5"`
	Size       int64      `json:"size"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	// LiveParentLocalID names the still this is the paired video of, when it is
	// one. Deliberately not part of ID: it describes the relationship, not the
	// bytes, and a session must resume under the same id whether or not the
	// client repeats it.
	LiveParentLocalID string `json:"live_parent_local_id,omitempty"`
	// ContentID is Apple's content identifier if the client already knows it.
	// Like LiveParentLocalID it describes the file rather than the bytes, and
	// like it, it is deliberately not part of ID.
	ContentID string    `json:"content_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is a declaration plus how much of it has arrived.
type Session struct {
	ID     string
	Decl   Declaration
	Offset int64
}

// Complete reports whether every declared byte has been received.
func (s Session) Complete() bool { return s.Offset == s.Decl.Size }

type Store struct {
	dir string
}

// New returns a store rooted at dir, which should sit on the same filesystem as
// the blob tree so a completed session commits by rename rather than by copy.
func New(dir string) *Store { return &Store{dir: dir} }

// Dir is where partial uploads live.
func (s *Store) Dir() string { return s.dir }

// ID is the session identifier for a declaration.
//
// It is derived from the content claim rather than allocated, which is what
// makes Create double as resume: a phone that was killed mid-video, and lost
// whatever it knew about the transfer, asks again with the same four facts and
// is handed back its own partial file. Nothing has to be persisted on the
// client for recovery to work.
func ID(d Declaration) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		d.DeviceID, d.LocalID, strings.ToLower(d.MD5), fmt.Sprint(d.Size),
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

// Create opens a session, or returns the existing one for the same declaration.
// It is idempotent by construction and is the call a client makes to resume.
func (s *Store) Create(d Declaration) (Session, error) {
	if err := validate(d); err != nil {
		return Session{}, err
	}
	id := ID(d)

	if existing, err := s.Get(id); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Session{}, err
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Session{}, fmt.Errorf("create upload dir: %w", err)
	}

	d.CreatedAt = time.Now().UTC()
	meta, err := json.Marshal(d)
	if err != nil {
		return Session{}, fmt.Errorf("marshal declaration: %w", err)
	}
	// The sidecar is written and fsynced before the part file exists, so a
	// crash can leave a declaration with no bytes — harmless — but never bytes
	// with no idea what they are.
	if err := writeSync(s.metaPath(id), meta); err != nil {
		return Session{}, err
	}
	f, err := os.OpenFile(s.partPath(id), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Session{}, fmt.Errorf("create part file: %w", err)
	}
	if err := f.Close(); err != nil {
		return Session{}, fmt.Errorf("close part file: %w", err)
	}

	return Session{ID: id, Decl: d, Offset: 0}, nil
}

// Get reports the current state of a session.
func (s *Store) Get(id string) (Session, error) {
	if !validID(id) {
		return Session{}, ErrNotFound
	}

	meta, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("read declaration: %w", err)
	}
	var d Declaration
	if err := json.Unmarshal(meta, &d); err != nil {
		return Session{}, fmt.Errorf("parse declaration: %w", err)
	}

	info, err := os.Stat(s.partPath(id))
	switch {
	case os.IsNotExist(err):
		return Session{ID: id, Decl: d, Offset: 0}, nil
	case err != nil:
		return Session{}, fmt.Errorf("stat part file: %w", err)
	}
	return Session{ID: id, Decl: d, Offset: info.Size()}, nil
}

// Append writes a chunk at offset and returns the new offset.
//
// The write is appended and fsynced before returning, so the offset a caller
// reports back to a client is one the server can honour after a power loss.
// That fsync is the whole cost of resumability and it is paid per chunk, not
// per byte.
func (s *Store) Append(id string, offset int64, r io.Reader) (Session, error) {
	session, err := s.Get(id)
	if err != nil {
		return Session{}, err
	}
	if offset != session.Offset {
		return session, fmt.Errorf("%w: at %d, chunk starts at %d", ErrOffsetMismatch, session.Offset, offset)
	}

	f, err := os.OpenFile(s.partPath(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Session{}, fmt.Errorf("open part file: %w", err)
	}
	defer f.Close()

	// Bounded by what is still owed, so a client that keeps sending cannot fill
	// the disk with one runaway request. A chunk that hits the limit is a
	// protocol error and the session is left exactly as long as it should be.
	remaining := session.Decl.Size - session.Offset
	written, copyErr := io.Copy(f, io.LimitReader(r, remaining+1))

	if written > remaining {
		if truncErr := f.Truncate(session.Offset); truncErr != nil {
			return Session{}, fmt.Errorf("%w (and could not roll back: %v)", ErrTooLong, truncErr)
		}
		return Session{ID: id, Decl: session.Decl, Offset: session.Offset}, ErrTooLong
	}

	// A short or interrupted body is not an error worth discarding progress
	// over: the bytes that did arrive are good, and the client resumes from the
	// offset this returns. Only the fsync is non-negotiable.
	if syncErr := f.Sync(); syncErr != nil {
		return Session{}, fmt.Errorf("fsync part file: %w", syncErr)
	}

	next := Session{ID: id, Decl: session.Decl, Offset: session.Offset + written}
	if copyErr != nil {
		return next, fmt.Errorf("receive chunk: %w", copyErr)
	}
	return next, nil
}

// PartPath is where a session's received bytes are, for a caller that is about
// to commit them.
func (s *Store) PartPath(id string) string { return s.partPath(id) }

// Discard removes a session and anything it has received.
func (s *Store) Discard(id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	partErr := os.Remove(s.partPath(id))
	metaErr := os.Remove(s.metaPath(id))
	if metaErr != nil && !os.IsNotExist(metaErr) {
		return metaErr
	}
	if partErr != nil && !os.IsNotExist(partErr) {
		return partErr
	}
	return nil
}

// Sweep removes sessions untouched for longer than maxAge and reports how many
// went. A phone that gives up on a video leaves its partial behind, and without
// this the archive slowly accumulates dead bytes that nothing references.
func (s *Store) Sweep(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".part") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".part")

		// The sidecar's own timestamp matters too: a session created but never
		// written to has a part file that has not been touched since creation.
		if meta, err := os.Stat(s.metaPath(id)); err == nil && meta.ModTime().After(cutoff) {
			continue
		}
		if err := s.Discard(id); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *Store) partPath(id string) string { return filepath.Join(s.dir, id+".part") }
func (s *Store) metaPath(id string) string { return filepath.Join(s.dir, id+".json") }

func validate(d Declaration) error {
	switch {
	case strings.TrimSpace(d.DeviceID) == "":
		return errors.New("uploads: deviceId is required")
	case strings.TrimSpace(d.LocalID) == "":
		return errors.New("uploads: localId is required")
	case strings.TrimSpace(d.Filename) == "":
		return errors.New("uploads: filename is required")
	case strings.TrimSpace(d.MD5) == "":
		return errors.New("uploads: md5 is required")
	case d.Size <= 0:
		return errors.New("uploads: size must be positive")
	}
	return nil
}

// validID rejects anything that is not a derived id, which is also what keeps a
// crafted id from escaping the directory.
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func writeSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", filepath.Base(path), err)
	}
	return nil
}
