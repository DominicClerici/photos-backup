package uploads_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/uploads"
)

func decl(size int64) uploads.Declaration {
	return uploads.Declaration{
		DeviceID: "iphone-14-pro",
		LocalID:  "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001",
		Filename: "IMG_8302.MOV",
		MD5:      "d41d8cd98f00b204e9800998ecf8427e",
		Size:     size,
	}
}

func newStore(t *testing.T) *uploads.Store {
	t.Helper()
	return uploads.New(filepath.Join(t.TempDir(), "incoming"))
}

func TestAppendAdvancesOffset(t *testing.T) {
	s := newStore(t)
	session, err := s.Create(decl(9))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if session.Offset != 0 {
		t.Fatalf("fresh session at offset %d, want 0", session.Offset)
	}

	session, err = s.Append(session.ID, 0, strings.NewReader("first"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if session.Offset != 5 {
		t.Errorf("offset = %d, want 5", session.Offset)
	}
	if session.Complete() {
		t.Error("session reported complete at 5 of 9 bytes")
	}

	session, err = s.Append(session.ID, 5, strings.NewReader("4444"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !session.Complete() {
		t.Errorf("offset = %d of %d, want complete", session.Offset, session.Decl.Size)
	}

	got, err := os.ReadFile(s.PartPath(session.ID))
	if err != nil {
		t.Fatalf("read part: %v", err)
	}
	if string(got) != "first4444" {
		t.Errorf("assembled %q, want %q", got, "first4444")
	}
}

// The id is a function of the declaration, so a client that has forgotten
// everything about a transfer recovers it by asking for the same thing again.
// This is the whole resume mechanism.
func TestCreateIsIdempotentAndResumes(t *testing.T) {
	s := newStore(t)

	first, err := s.Create(decl(10))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Append(first.ID, 0, strings.NewReader("1234")); err != nil {
		t.Fatalf("append: %v", err)
	}

	resumed, err := s.Create(decl(10))
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if resumed.ID != first.ID {
		t.Errorf("resumed id = %q, want %q", resumed.ID, first.ID)
	}
	if resumed.Offset != 4 {
		t.Errorf("resumed offset = %d, want 4", resumed.Offset)
	}
}

// A store built fresh over the same directory sees the same sessions, which is
// what makes a session survive a server restart.
func TestSessionsSurviveARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "incoming")

	before := uploads.New(dir)
	session, err := before.Create(decl(8))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := before.Append(session.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatalf("append: %v", err)
	}

	after := uploads.New(dir)
	got, err := after.Get(session.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if got.Offset != 3 {
		t.Errorf("offset = %d, want 3", got.Offset)
	}
	if got.Decl.Filename != "IMG_8302.MOV" {
		t.Errorf("declaration lost: %+v", got.Decl)
	}
}

func TestAppendAtWrongOffsetIsRefusedWithTheRealOne(t *testing.T) {
	s := newStore(t)
	session, _ := s.Create(decl(20))
	if _, err := s.Append(session.ID, 0, strings.NewReader("12345")); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := s.Append(session.ID, 2, strings.NewReader("xx"))
	if !errors.Is(err, uploads.ErrOffsetMismatch) {
		t.Fatalf("error = %v, want ErrOffsetMismatch", err)
	}
	if got.Offset != 5 {
		t.Errorf("reported offset = %d, want 5 so the client can seek", got.Offset)
	}

	// The refused chunk must not have landed.
	data, _ := os.ReadFile(s.PartPath(session.ID))
	if string(data) != "12345" {
		t.Errorf("part file = %q, want %q", data, "12345")
	}
}

// A client that sends more than it declared gets the overrun rolled back
// rather than a part file that can never match its md5.
func TestAppendBeyondDeclaredSizeRollsBack(t *testing.T) {
	s := newStore(t)
	session, _ := s.Create(decl(4))

	got, err := s.Append(session.ID, 0, strings.NewReader("far too many bytes"))
	if !errors.Is(err, uploads.ErrTooLong) {
		t.Fatalf("error = %v, want ErrTooLong", err)
	}
	if got.Offset != 0 {
		t.Errorf("offset = %d, want 0 after rollback", got.Offset)
	}

	info, err := os.Stat(s.PartPath(session.ID))
	if err != nil {
		t.Fatalf("stat part: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("part file is %d bytes, want 0", info.Size())
	}
}

// A chunk cut off mid-flight keeps what arrived. Throwing it away would mean a
// 550MB video restarts every time the connection blinks, which is the exact
// failure resumable upload exists to prevent.
func TestInterruptedChunkKeepsWhatArrived(t *testing.T) {
	s := newStore(t)
	session, _ := s.Create(decl(100))

	got, err := s.Append(session.ID, 0, &cutOffReader{data: []byte("partial payload"), after: 7})
	if err == nil {
		t.Fatal("expected the interrupted read to be reported")
	}
	if got.Offset != 7 {
		t.Errorf("offset = %d, want 7 — the bytes that did arrive", got.Offset)
	}

	reread, err := s.Get(session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reread.Offset != 7 {
		t.Errorf("offset after re-read = %d, want 7", reread.Offset)
	}
}

// cutOffReader hands over `after` bytes and then fails, standing in for a
// connection that dies mid-chunk.
type cutOffReader struct {
	data  []byte
	after int
	sent  int
}

func (r *cutOffReader) Read(p []byte) (int, error) {
	if r.sent >= r.after {
		return 0, errors.New("connection reset by peer")
	}
	n := copy(p, r.data[r.sent:r.after])
	r.sent += n
	return n, nil
}

func TestGetUnknownSession(t *testing.T) {
	s := newStore(t)

	for _, id := range []string{
		"0123456789abcdef0123456789abcdef", // well-formed, absent
		"../../etc/passwd",                 // traversal
		"short",
		"0123456789ABCDEF0123456789ABCDEF", // uppercase is not a derived id
	} {
		if _, err := s.Get(id); !errors.Is(err, uploads.ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want ErrNotFound", id, err)
		}
	}
}

func TestCreateRejectsIncompleteDeclarations(t *testing.T) {
	s := newStore(t)

	for name, mutate := range map[string]func(*uploads.Declaration){
		"no device":   func(d *uploads.Declaration) { d.DeviceID = "" },
		"no local id": func(d *uploads.Declaration) { d.LocalID = "" },
		"no filename": func(d *uploads.Declaration) { d.Filename = "" },
		"no md5":      func(d *uploads.Declaration) { d.MD5 = "" },
		"zero size":   func(d *uploads.Declaration) { d.Size = 0 },
		"negative":    func(d *uploads.Declaration) { d.Size = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			d := decl(10)
			mutate(&d)
			if _, err := s.Create(d); err == nil {
				t.Error("accepted an incomplete declaration")
			}
		})
	}
}

func TestDiscardRemovesEverything(t *testing.T) {
	s := newStore(t)
	session, _ := s.Create(decl(10))
	if _, err := s.Append(session.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.Discard(session.ID); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := s.Get(session.ID); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 0 {
		t.Errorf("directory still holds %d entries", len(entries))
	}
}

func TestSweepRemovesOnlyStaleSessions(t *testing.T) {
	s := newStore(t)

	stale, _ := s.Create(decl(10))
	if _, err := s.Append(stale.ID, 0, strings.NewReader("old")); err != nil {
		t.Fatalf("append: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{s.PartPath(stale.ID), filepath.Join(s.Dir(), stale.ID+".json")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("backdate %s: %v", p, err)
		}
	}

	fresh := decl(10)
	fresh.LocalID = "a-different-local-id"
	live, _ := s.Create(fresh)

	removed, err := s.Sweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d sessions, want 1", removed)
	}
	if _, err := s.Get(stale.ID); !errors.Is(err, uploads.ErrNotFound) {
		t.Error("stale session survived the sweep")
	}
	if _, err := s.Get(live.ID); err != nil {
		t.Errorf("live session was swept: %v", err)
	}
}

func TestSweepOnAnEmptyDirectory(t *testing.T) {
	if removed, err := newStore(t).Sweep(time.Hour); err != nil || removed != 0 {
		t.Errorf("Sweep() = %d, %v; want 0, nil", removed, err)
	}
}
