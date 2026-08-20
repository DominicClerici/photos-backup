package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
)

// The vault's own endpoints: the lock, and the gallery behind it.
//
// They divide exactly along the line the whole feature is built on. Everything
// that *puts something in* works on a locked vault, because hiding a photograph
// is a right-click and a password prompt there would train anybody to leave the
// vault open. Everything that reads answers 423 until somebody unlocks it.
//
// The unlock is server-wide rather than per-caller, which is the honest
// consequence of the gallery's endpoints being unauthenticated on a loopback
// listener — see vault.Keeper.

// maxVaultBody is generous for a password and nothing else.
const maxVaultBody = 8 << 10

// vaultStatus is what the gallery polls to know which of the three states it is
// in: no vault at all, locked, or open.
type vaultStatus struct {
	// Exists is false before anything has ever been hidden, which is the state
	// the "choose a password" flow is for.
	Exists   bool `json:"exists"`
	Unlocked bool `json:"unlocked"`
	// ExpiresAt is when the key will be dropped if nothing touches it. Sent so
	// the gallery can say so rather than locking under somebody's pointer.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	if s.Vault == nil {
		writeJSON(w, http.StatusOK, vaultStatus{})
		return
	}
	exists, err := s.Vault.Exists(r.Context())
	if err != nil {
		s.logger().Error("read the vault secret", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	status := vaultStatus{Exists: exists, Unlocked: s.Vault.Keeper.Unlocked()}
	if status.Unlocked {
		at := s.Vault.Keeper.Expires()
		status.ExpiresAt = &at
	}
	writeJSON(w, http.StatusOK, status)
}

type passwordRequest struct {
	Password string `json:"password"`
	// New is set only by the change-password endpoint, where Password is the
	// old one.
	New string `json:"new,omitempty"`
}

// minPassword is a floor rather than a policy.
//
// There is no recovery, no escrow and no reset: the password is the
// photographs. Rules about punctuation would be theatre against an attacker who
// has the disk and Argon2id to get through, and the one thing actually worth
// refusing is the password somebody types to get past a dialog.
const minPassword = 8

func (s *Server) readPassword(w http.ResponseWriter, r *http.Request) (passwordRequest, bool) {
	var req passwordRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxVaultBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return req, false
	}
	return req, true
}

// handleVaultSetup creates the vault, once.
//
// Reached from the first hide rather than from a settings screen, which is why
// it leaves the vault open: somebody who has just chosen a password and been
// asked for it again would reasonably conclude the first prompt did nothing.
func (s *Server) handleVaultSetup(w http.ResponseWriter, r *http.Request) {
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}
	req, ok := s.readPassword(w, r)
	if !ok {
		return
	}
	if len(req.Password) < minPassword {
		writeError(w, http.StatusBadRequest, "a vault password must be at least 8 characters")
		return
	}

	if err := s.Vault.Setup(r.Context(), req.Password); err != nil {
		s.logger().Error("create the vault", "error", err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.logger().Info("created the vault")
	s.handleVaultStatus(w, r)
}

func (s *Server) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}
	req, ok := s.readPassword(w, r)
	if !ok {
		return
	}

	switch err := s.Vault.Unlock(r.Context(), req.Password); {
	case errors.Is(err, vault.ErrNoVault):
		writeError(w, http.StatusNotFound, "this archive has no vault yet")
		return
	case errors.Is(err, vault.ErrBadPassword):
		// Logged, because a vault somebody is guessing at is a thing to be able
		// to see afterwards. Not rate limited: Argon2id at 64MiB already puts a
		// hard ceiling on attempts per second, and it is a ceiling that does not
		// depend on this process still being the one being talked to.
		s.logger().Warn("rejected a vault password", "remote", clientIP(r))
		writeError(w, http.StatusForbidden, "that password does not open this vault")
		return
	case err != nil:
		s.logger().Error("unlock the vault", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not open the vault")
		return
	}
	s.handleVaultStatus(w, r)
}

func (s *Server) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}
	s.Vault.Keeper.Lock()
	s.handleVaultStatus(w, r)
}

// handleVaultPassword changes the password without re-encrypting anything: the
// keypair is unchanged and only its wrapping is rewritten. See
// vault.Service.ChangePassword.
func (s *Server) handleVaultPassword(w http.ResponseWriter, r *http.Request) {
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}
	req, ok := s.readPassword(w, r)
	if !ok {
		return
	}
	if len(req.New) < minPassword {
		writeError(w, http.StatusBadRequest, "a vault password must be at least 8 characters")
		return
	}

	switch err := s.Vault.ChangePassword(r.Context(), req.Password, req.New); {
	case errors.Is(err, vault.ErrNoVault):
		writeError(w, http.StatusNotFound, "this archive has no vault yet")
		return
	case errors.Is(err, vault.ErrBadPassword):
		writeError(w, http.StatusForbidden, "that password does not open this vault")
		return
	case err != nil:
		s.logger().Error("change the vault password", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not change the password")
		return
	}
	s.handleVaultStatus(w, r)
}

// vaultResponse is what an operation that can be undone hands back — the same
// shape the trash uses, for the same reason.
type vaultResponse struct {
	Batch  string `json:"batch"`
	Moved  int    `json:"moved"`
	Albums int    `json:"albums,omitempty"`
	People int    `json:"people,omitempty"`
}

// bucketOf reads the {bucket} path value.
func bucketOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	bucket := r.PathValue("bucket")
	if !db.ValidBucket(bucket) {
		writeError(w, http.StatusNotFound, "no such vault")
		return "", false
	}
	return bucket, true
}

// handleVaultAdd hides a selection of photographs.
//
// No password. The selection is resolved in the library — a vaulted item cannot
// be named by a position in the library's timeline, and a trashed one is out of
// scope — so this endpoint cannot reach across into either.
func (s *Server) handleVaultAdd(w http.ResponseWriter, r *http.Request) {
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}
	sel, ok := readSelection(w, r)
	if !ok {
		return
	}

	candidates, err := s.Store.VaultCandidates(r.Context(), sel)
	if err != nil {
		s.writeSelectionError(w, err, "resolve a selection for the vault")
		return
	}
	result, err := s.Vault.Add(r.Context(), bucket, candidates)
	if err != nil {
		s.writeVaultError(w, err, "hide a selection")
		return
	}
	s.logger().Info("hid a selection", "bucket", bucket, "items", result.Count)
	writeJSON(w, http.StatusOK, vaultResponse{Batch: result.Batch, Moved: result.Count})
}

// handleVaultAlbum hides an album and everything in it.
//
// The two halves share one batch, exactly as deleting an album and its photos
// does, because they undo together: an album restored empty, or photographs
// restored into an album that is still hidden, would be worse than either half.
//
// Unlike the photographs, the album row is not encrypted — see migration 0012.
func (s *Server) handleVaultAlbum(w http.ResponseWriter, r *http.Request) {
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}
	id := r.PathValue("id")

	candidates, err := s.Store.VaultAlbumCandidates(r.Context(), id)
	if err != nil {
		s.writeSelectionError(w, err, "read an album for the vault")
		return
	}

	result, err := s.Vault.Add(r.Context(), bucket, candidates)
	if err != nil && !errors.Is(err, db.ErrEmptySelection) {
		s.writeVaultError(w, err, "hide an album")
		return
	}
	// An empty album is still an album somebody hid, and it goes in on a batch
	// of its own so the Undo has something to name.
	if result.Batch == "" {
		result.Batch, err = db.NewBatch()
		if err != nil {
			s.writeVaultError(w, err, "hide an album")
			return
		}
	}

	if err := s.Store.VaultAlbum(r.Context(), id, bucket, result.Batch); err != nil {
		if errors.Is(err, db.ErrNotFound) || isBadUUID(err) {
			writeError(w, http.StatusNotFound, "no such album")
			return
		}
		s.writeVaultError(w, err, "hide an album")
		return
	}
	writeJSON(w, http.StatusOK, vaultResponse{Batch: result.Batch, Moved: result.Count, Albums: 1})
}

type personRequest struct {
	Name string `json:"name"`
}

// handleVaultPerson hides everyone a name is tagged on, and the name itself.
//
// The name travels in the body rather than in the path because it is somebody's
// name: it can hold a slash, an emoji, or a right-to-left mark, and a path
// segment is the wrong place to find that out.
func (s *Server) handleVaultPerson(w http.ResponseWriter, r *http.Request) {
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return
	}

	var req personRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxVaultBody))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name the person to hide")
		return
	}

	candidates, err := s.Store.VaultPersonCandidates(r.Context(), req.Name)
	if err != nil {
		s.writeSelectionError(w, err, "read a person for the vault")
		return
	}

	result, err := s.Vault.Add(r.Context(), bucket, candidates)
	if err != nil && !errors.Is(err, db.ErrEmptySelection) {
		s.writeVaultError(w, err, "hide a person")
		return
	}
	if result.Batch == "" {
		result.Batch, err = db.NewBatch()
		if err != nil {
			s.writeVaultError(w, err, "hide a person")
			return
		}
	}

	if err := s.Store.VaultPerson(r.Context(), req.Name, bucket, result.Batch); err != nil {
		s.writeVaultError(w, err, "hide a person")
		return
	}
	writeJSON(w, http.StatusOK, vaultResponse{Batch: result.Batch, Moved: result.Count, People: 1})
}

// vaultRestoreRequest names what to bring back out, three ways.
//
// Ids for a selection inside the vault's own grid, a batch for the Undo in a
// toast, and an album or a person for the grouping's own menu. There are no
// ranges: positions in the vault are positions in an index this server holds in
// memory, so they are resolved here rather than sent as intervals — see
// handleVaultRestore.
type vaultRestoreRequest struct {
	IDs    []string `json:"ids,omitempty"`
	Batch  string   `json:"batch,omitempty"`
	Album  string   `json:"album,omitempty"`
	Person string   `json:"person,omitempty"`
	// Ranges are positions in the vault timeline the client is looking at,
	// resolved against the same index that drew it.
	Ranges []db.Range `json:"ranges,omitempty"`
	Bucket string     `json:"bucket,omitempty"`
	// Filter is the whole description of the timeline those positions were
	// counted in: which collection, narrowed how, in what order. All of it
	// matters — position 2 of a grid sorted oldest-first, or of one showing
	// only videos, is a different photograph from position 2 of the bucket.
	Filter struct {
		Album     string `json:"album,omitempty"`
		Person    string `json:"person,omitempty"`
		Category  string `json:"category,omitempty"`
		Sort      string `json:"sort,omitempty"`
		Kind      string `json:"kind,omitempty"`
		Favorites bool   `json:"favorites,omitempty"`
		Unalbumed bool   `json:"unalbumed,omitempty"`
	} `json:"filter"`
}

type vaultRestoreResponse struct {
	Restored int `json:"restored"`
	Albums   int `json:"albums,omitempty"`
	People   int `json:"people,omitempty"`
}

// handleVaultRestore takes things back out.
//
// This one needs the password, and that is the asymmetry the whole feature
// rests on: anything can put a photograph into the vault, and only the password
// takes one out. A restore has to decrypt the bytes to write them back, so it
// could not be otherwise even if somebody wanted it to be.
func (s *Server) handleVaultRestore(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}

	var req vaultRestoreRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSelectionBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	ids, albums, people, ok := s.resolveRestore(w, r, req)
	if !ok {
		return
	}

	// The groupings come out first, and the order is load-bearing rather than
	// tidy. A photograph rejoins an album only if that album is back in the
	// library — see CommitUnvault — so restoring an album's photographs while
	// the album row is still marked hidden puts them in the library and leaves
	// the album empty, which is exactly the wrong half of "it goes back where
	// it was".
	//
	// The same check is what makes the *other* case right: restoring one
	// photograph out of an album that stays hidden must not quietly put it back
	// into a hidden album, and it does not.
	response := vaultRestoreResponse{Albums: len(albums), People: len(people)}
	for _, id := range albums {
		if err := s.Store.UnvaultAlbum(r.Context(), id); err != nil {
			s.writeVaultError(w, err, "take an album out of the vault")
			return
		}
	}
	for _, name := range people {
		if err := s.Store.UnvaultPerson(r.Context(), name); err != nil {
			s.writeVaultError(w, err, "take a person out of the vault")
			return
		}
	}

	if len(ids) > 0 {
		restored, err := s.Vault.Remove(r.Context(), ids)
		if err != nil {
			s.writeVaultError(w, err, "take a selection out of the vault")
			return
		}
		response.Restored = restored
	}

	s.logger().Info("took items out of the vault",
		"items", response.Restored, "albums", response.Albums, "people", response.People)
	writeJSON(w, http.StatusOK, response)
}

// resolveRestore turns any of the four ways of naming a restore into the ids,
// albums and names it means.
func (s *Server) resolveRestore(w http.ResponseWriter, r *http.Request, req vaultRestoreRequest) (ids, albums, people []string, ok bool) {
	switch {
	case req.Batch != "":
		assets, albumIDs, names, err := s.Store.VaultBatch(r.Context(), req.Batch)
		if err != nil {
			if isBadUUID(err) {
				writeError(w, http.StatusBadRequest, "malformed batch")
				return nil, nil, nil, false
			}
			s.writeVaultError(w, err, "read a vault batch")
			return nil, nil, nil, false
		}
		return assets, albumIDs, names, true

	case req.Album != "":
		index, ok := s.vaultIndex(w, r, req.Bucket)
		if !ok {
			return nil, nil, nil, false
		}
		for _, it := range index.Select(vault.Filter{AlbumID: req.Album}) {
			ids = append(ids, it.ID())
		}
		return ids, []string{req.Album}, nil, true

	case req.Person != "":
		index, ok := s.vaultIndex(w, r, req.Bucket)
		if !ok {
			return nil, nil, nil, false
		}
		for _, it := range index.Select(vault.Filter{Person: req.Person}) {
			ids = append(ids, it.ID())
		}
		return ids, nil, []string{req.Person}, true

	case len(req.Ranges) > 0:
		index, ok := s.vaultIndex(w, r, req.Bucket)
		if !ok {
			return nil, nil, nil, false
		}
		// A sort this does not recognise is the default one, exactly as
		// vaultFilter treats it: a restore that has already been authorised
		// should not fail on the spelling of an ordering.
		sort, err := db.ParseSort(req.Filter.Sort)
		if err != nil {
			sort = db.SortNewest
		}
		items := index.Select(narrowing(db.TimelineFilter{
			AlbumID: req.Filter.Album, Person: req.Filter.Person, Category: req.Filter.Category,
			Sort: sort, Kind: req.Filter.Kind, Favorites: req.Filter.Favorites,
			Unalbumed: req.Filter.Unalbumed,
		}))
		for _, run := range req.Ranges {
			for i := max(run.Start, 0); i < run.End && i < len(items); i++ {
				ids = append(ids, items[i].ID())
			}
		}
		return append(ids, req.IDs...), nil, nil, true

	case len(req.IDs) > 0:
		return req.IDs, nil, nil, true
	}

	writeError(w, http.StatusBadRequest, "name what to restore")
	return nil, nil, nil, false
}

// vaultIndex builds one bucket's opened contents.
func (s *Server) vaultIndex(w http.ResponseWriter, r *http.Request, bucket string) (*vault.Index, bool) {
	if bucket != "" && !db.ValidBucket(bucket) {
		writeError(w, http.StatusNotFound, "no such vault")
		return nil, false
	}
	index, err := s.Vault.Index(r.Context(), bucket)
	if err != nil {
		s.writeVaultError(w, err, "open the vault")
		return nil, false
	}
	return index, true
}

// handleVaultTimeline, handleVaultDays and handleVaultLocate are the library's
// three timeline endpoints, over a bucket.
//
// They answer in exactly the shapes the gallery's own do — same page, same
// cursor, same day table — which is what lets the vault reuse the grid, the
// virtualization, the zoom and the viewer wholesale rather than getting a
// second, lesser gallery of its own. What differs is entirely underneath: these
// are computed in Go over decrypted rows, because there is nothing left in the
// database for SQL to sort by.
func (s *Server) handleVaultTimeline(w http.ResponseWriter, r *http.Request) {
	// The bucket is checked before the lock, and the order matters: a name that
	// is not a vault is a 404 whether or not the vault is open, and answering
	// 423 for it would tell a caller that /v1/vault/secret is a real place
	// somebody has shut.
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if !s.vaultReady(w) {
		return
	}
	index, ok := s.vaultIndex(w, r, bucket)
	if !ok {
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))

	var after *db.Cursor
	if token := r.URL.Query().Get("cursor"); token != "" {
		cursor, err := db.DecodeCursor(token)
		if err != nil {
			writeError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		after = &cursor
	}

	writeJSON(w, http.StatusOK, index.Page(vaultFilter(r), after, skip, limit))
}

func (s *Server) handleVaultDays(w http.ResponseWriter, r *http.Request) {
	// The bucket is checked before the lock, and the order matters: a name that
	// is not a vault is a 404 whether or not the vault is open, and answering
	// 423 for it would tell a caller that /v1/vault/secret is a real place
	// somebody has shut.
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if !s.vaultReady(w) {
		return
	}
	index, ok := s.vaultIndex(w, r, bucket)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, index.Days(vaultFilter(r), r.URL.Query().Get("tz")))
}

func (s *Server) handleVaultLocate(w http.ResponseWriter, r *http.Request) {
	// The bucket is checked before the lock, and the order matters: a name that
	// is not a vault is a 404 whether or not the vault is open, and answering
	// 423 for it would tell a caller that /v1/vault/secret is a real place
	// somebody has shut.
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if !s.vaultReady(w) {
		return
	}
	index, ok := s.vaultIndex(w, r, bucket)
	if !ok {
		return
	}
	at := index.Locate(vaultFilter(r), r.URL.Query().Get("id"))
	if at < 0 {
		writeError(w, http.StatusNotFound, "that item is not in this collection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"index": at})
}

// handleVaultCollections is the vault's own collections page: the albums, the
// people and the categories, over one bucket.
func (s *Server) handleVaultCollections(w http.ResponseWriter, r *http.Request) {
	// The bucket is checked before the lock, and the order matters: a name that
	// is not a vault is a 404 whether or not the vault is open, and answering
	// 423 for it would tell a caller that /v1/vault/secret is a real place
	// somebody has shut.
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if !s.vaultReady(w) {
		return
	}
	index, ok := s.vaultIndex(w, r, bucket)
	if !ok {
		return
	}

	collections := index.Collections()
	// The total is what the header counts and what tells an empty vault from an
	// unopened one, so it travels with the page rather than costing a second
	// request that would have to rebuild the same index.
	writeJSON(w, http.StatusOK, struct {
		db.Collections
		Total int `json:"total"`
	}{Collections: collections, Total: len(index.Items)})
}

// vaultFilter reads the collection a vault request is narrowed to, the facets
// it narrows by and the order it wants, in the same query parameters the
// library's timeline uses.
//
// An unreadable sort or kind falls back to the default rather than answering
// 400, which is the opposite of what the library's timeline does with the same
// parameter and is the right call here for one reason: this filter is read by
// three handlers that have already opened a vault, and refusing the request at
// that point would spend a password on an error message. The gallery is the
// only client and it does not send either of these wrong.
func vaultFilter(r *http.Request) vault.Filter {
	q := r.URL.Query()
	sort, err := db.ParseSort(q.Get("sort"))
	if err != nil {
		sort = db.SortNewest
	}
	kind := q.Get("kind")
	if kind != db.MediaImage && kind != db.MediaVideo {
		kind = ""
	}
	return vault.Filter{
		AlbumID:   q.Get("album"),
		Person:    q.Get("person"),
		Category:  q.Get("category"),
		Sort:      sort,
		Kind:      kind,
		Favorites: truthy(q.Get("favorites")),
		Unalbumed: truthy(q.Get("unalbumed")),
	}
}

// narrowing is the library's description of a timeline, read as the vault's.
//
// The two structs say the same thing about the same grid and are separate only
// because one of them is turned into SQL and the other into a walk over a slice
// in memory. This is the one place that translates, so a facet added to the
// timeline cannot be silently dropped on the way to a vault selection —
// resolving a range against a differently-ordered index would restore the wrong
// photographs.
func narrowing(f db.TimelineFilter) vault.Filter {
	return vault.Filter{
		AlbumID:   f.AlbumID,
		Person:    f.Person,
		Category:  f.Category,
		Sort:      f.Sort,
		Kind:      f.Kind,
		Favorites: f.Favorites,
		Unalbumed: f.Unalbumed,
	}
}

// writeVaultError maps a vault failure onto a status.
func (s *Server) writeVaultError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, vault.ErrLocked):
		writeError(w, http.StatusLocked, "the vault is locked")
	case errors.Is(err, vault.ErrNoVault):
		// 428: the request was well-formed and refused because something has to
		// happen first, which is exactly what the gallery turns into "choose a
		// password for your archive".
		writeError(w, http.StatusPreconditionRequired, "this archive has no vault yet")
	case errors.Is(err, db.ErrEmptySelection):
		writeError(w, http.StatusBadRequest, "the selection names no items")
	case errors.Is(err, db.ErrBadBucket):
		writeError(w, http.StatusNotFound, "no such vault")
	case isBadUUID(err):
		writeError(w, http.StatusBadRequest, "malformed id")
	default:
		s.logger().Error(what, "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not "+what)
	}
}
