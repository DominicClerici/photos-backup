package vault

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

// Service is the vault as the rest of the server uses it: the four places one
// hidden photograph lives, and the order they have to be touched in.
//
// That order is the whole design, and it is the mirror image of purge's. A
// purge destroys, so it commits the database first and takes the bytes last,
// because a file left behind is a finding and a row without a file is a loss.
// Hiding *creates* before it destroys, so it goes the other way:
//
//   - The ciphertext is written first, while the plaintext is still there. A
//     crash here has cost nothing: a stray .enc file that `verify` can find and
//     a library that never noticed.
//   - The database commits second, in one transaction — the sealed document,
//     the scrubbed row, and the album and people rows that go with it.
//   - The plaintext is deleted last, because it is the only step that cannot be
//     undone by rerunning the ones before it.
//
// The window that leaves is between the second and third steps: a crash there
// means a photograph the gallery treats as hidden whose original is still
// readable on the archive drive. That is the one failure here that is a
// security bug rather than an inconvenience, so it is not left to chance —
// Reconcile sweeps for it, and it runs on the same hourly timer the trash's
// expiry does.
type Service struct {
	Store       *db.Store
	Blobs       *blobstore.Store
	Derivatives *derivstore.Store
	// VaultBlobs and VaultDerivatives are the encrypted trees, one per disk, so
	// that hiding a photograph does not move it from the archive drive to the
	// SSD or the other way round.
	VaultBlobs       *Store
	VaultDerivatives *Store
	Manifest         *manifest.Log
	Keeper           *Keeper
	Log              *slog.Logger
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Secret reads the vault's identity.
func (s *Service) Secret(ctx context.Context) (Secret, error) {
	row, err := s.Store.VaultSecret(ctx)
	if errors.Is(err, db.ErrNoVaultSecret) {
		return Secret{}, ErrNoVault
	}
	if err != nil {
		return Secret{}, err
	}
	return Secret{
		PublicKey: row.PublicKey,
		KDF: KDF{
			Salt: row.Salt, Time: uint32(row.Time),
			Memory: uint32(row.Memory), Threads: uint8(row.Threads),
		},
		SealedPrivate: row.SealedPrivate,
	}, nil
}

// Exists reports whether this archive has a vault at all.
func (s *Service) Exists(ctx context.Context) (bool, error) {
	_, err := s.Secret(ctx)
	if errors.Is(err, ErrNoVault) {
		return false, nil
	}
	return err == nil, err
}

// Setup creates the vault, once, and leaves it open.
//
// Leaving it open matters: this is reached from the first time somebody hides
// something, and asking for the password they just chose, one line after they
// chose it, is the kind of correctness that reads as a bug.
func (s *Service) Setup(ctx context.Context, password string) error {
	secret, priv, err := Create(password)
	if err != nil {
		return err
	}
	if err := s.Store.CreateVaultSecret(ctx, db.VaultSecretRow{
		PublicKey: secret.PublicKey, Salt: secret.KDF.Salt,
		Time: int32(secret.KDF.Time), Memory: int32(secret.KDF.Memory),
		Threads: int32(secret.KDF.Threads), SealedPrivate: secret.SealedPrivate,
	}); err != nil {
		return err
	}
	s.Keeper.Hold(priv)
	return nil
}

// Unlock opens the vault for as long as somebody keeps using it.
func (s *Service) Unlock(ctx context.Context, password string) error {
	secret, err := s.Secret(ctx)
	if err != nil {
		return err
	}
	return s.Keeper.Unlock(secret, password)
}

// ChangePassword re-seals the same keypair.
//
// The identity is deliberately unchanged. Every file on disk and every sealed
// document was encrypted to this public key, so re-keying would mean rewriting
// the whole vault — and a password change that can fail halfway through a
// hundred gigabytes is not a password change anybody should be offered.
func (s *Service) ChangePassword(ctx context.Context, old, next string) error {
	secret, err := s.Secret(ctx)
	if err != nil {
		return err
	}
	priv, err := secret.Unlock(old)
	if err != nil {
		return err
	}
	rewrapped, err := Rewrap(priv, next)
	if err != nil {
		return err
	}
	if err := s.Store.SaveVaultSecret(ctx, db.VaultSecretRow{
		PublicKey: rewrapped.PublicKey, Salt: rewrapped.KDF.Salt,
		Time: int32(rewrapped.KDF.Time), Memory: int32(rewrapped.KDF.Memory),
		Threads: int32(rewrapped.KDF.Threads), SealedPrivate: rewrapped.SealedPrivate,
	}); err != nil {
		return err
	}
	s.Keeper.Hold(priv)
	return nil
}

// recipient is the public key the write path encrypts to, whether or not the
// vault is open.
func (s *Service) recipient(ctx context.Context) (*ecdh.PublicKey, error) {
	secret, err := s.Secret(ctx)
	if err != nil {
		return nil, err
	}
	return s.Keeper.Recipient(secret)
}

// Add hides a set of candidates. It needs no password — see the package
// comment — which is the whole reason "Archive" is a right-click and not a
// dialog.
func (s *Service) Add(ctx context.Context, bucket string, candidates []db.VaultCandidate) (db.VaultResult, error) {
	if !db.ValidBucket(bucket) {
		return db.VaultResult{}, fmt.Errorf("%w: %q", db.ErrBadBucket, bucket)
	}
	if len(candidates) == 0 {
		return db.VaultResult{}, db.ErrEmptySelection
	}
	to, err := s.recipient(ctx)
	if err != nil {
		return db.VaultResult{}, err
	}

	sealed := make([]db.SealedItem, 0, len(candidates))
	for _, c := range candidates {
		doc, err := SealAsset(to, c.AssetID, c.Doc)
		if err != nil {
			return db.VaultResult{}, err
		}
		// Nil in, nil out: a photograph nothing has described seals no second
		// document, and the column is left null rather than holding an
		// encrypted empty answer.
		var analysis []byte
		if len(c.Analysis) > 0 {
			if analysis, err = SealAnalysis(to, c.AssetID, c.Analysis); err != nil {
				return db.VaultResult{}, err
			}
		}
		if err := s.sealFiles(to, c); err != nil {
			return db.VaultResult{}, err
		}
		sealed = append(sealed, db.SealedItem{
			AssetID: c.AssetID, Sealed: doc, SealedAnalysis: analysis,
		})
	}

	result, err := s.Store.CommitVault(ctx, bucket, sealed)
	if err != nil {
		// The ciphertext already written is not cleaned up here. It is
		// unreferenced, unreadable without the password, and `verify` reports
		// it — which is a better outcome than a cleanup path that runs after a
		// failure and gets to delete files.
		return db.VaultResult{}, err
	}

	for _, c := range candidates {
		s.dropPlaintext(c.SHA256, c.Ext)
		s.record(manifest.KindVault, bucket, c.SHA256)
	}
	return result, nil
}

// sealFiles encrypts one asset's original and every rendition made from it.
//
// The thumbnails are not an afterthought. A 256px thumbnail *is* the
// photograph, at a size that is perfectly legible on a phone — encrypting the
// original and leaving the grid's copy of it on the SSD would be a vault with a
// window in it.
func (s *Service) sealFiles(to *ecdh.PublicKey, c db.VaultCandidate) error {
	if s.Blobs != nil && s.VaultBlobs != nil {
		// A blob that is already gone is not a reason to refuse. The row is
		// being hidden either way, and an asset with no file on disk is a
		// finding `verify` already knows how to report — checking first rather
		// than unwrapping the error afterwards keeps that case from being told
		// apart from a real one by string matching.
		path := s.Blobs.Path(c.SHA256, c.Ext)
		if _, err := os.Stat(path); err == nil {
			if _, err := s.VaultBlobs.PutFile(to, c.SHA256, "", path); err != nil {
				return fmt.Errorf("seal the original of %s: %w", c.AssetID, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("seal the original of %s: %w", c.AssetID, err)
		}
	}
	if s.Derivatives == nil || s.VaultDerivatives == nil {
		return nil
	}
	for _, suffix := range derivstore.Suffixes() {
		if !s.Derivatives.Exists(c.SHA256, suffix) {
			continue
		}
		if _, err := s.VaultDerivatives.PutFile(to, c.SHA256, suffix,
			s.Derivatives.Path(c.SHA256, suffix)); err != nil {
			return fmt.Errorf("seal %s%s: %w", c.SHA256, suffix, err)
		}
	}
	return nil
}

// dropPlaintext is the last step of hiding, and the only irreversible one.
func (s *Service) dropPlaintext(sha, ext string) {
	if s.Blobs != nil {
		if _, err := s.Blobs.Remove(sha, ext); err != nil {
			s.logger().Error("could not remove the plaintext of a hidden photo",
				"error", err, "sha256", sha)
		}
	}
	if s.Derivatives != nil {
		if _, _, err := s.Derivatives.RemoveAll(sha); err != nil {
			s.logger().Warn("could not remove a hidden photo's renditions",
				"error", err, "sha256", sha)
		}
	}
}

// Remove takes items back out of the vault, decrypting them into the library.
//
// This one does need the password, and that asymmetry is the point.
func (s *Service) Remove(ctx context.Context, ids []string) (int, error) {
	priv, err := s.Keeper.Identity()
	if err != nil {
		return 0, err
	}
	rows, err := s.Store.VaultSealed(ctx, ids)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, db.ErrEmptySelection
	}

	restorations := make([]db.Restoration, 0, len(rows))
	type restored struct{ sha, ext string }
	files := make([]restored, 0, len(rows))

	for _, r := range rows {
		doc, parsed, err := openDoc(priv, r.AssetID, r.Sealed)
		if err != nil {
			return 0, fmt.Errorf("open the sealed metadata of %s: %w", r.AssetID, err)
		}
		// Fatal rather than skipped, the same as the row's document. A restore
		// that quietly dropped the caption would be indistinguishable from a
		// photograph that never had one, and the whole point of sealing it was
		// that coming back out of the vault should not cost anything.
		analysis, err := openAnalysis(priv, r.AssetID, r.SealedAnalysis)
		if err != nil {
			return 0, fmt.Errorf("open the sealed analysis of %s: %w", r.AssetID, err)
		}
		if err := s.unsealFiles(priv, parsed); err != nil {
			return 0, err
		}

		albumIDs := make([]string, 0, len(doc.Albums))
		for _, ref := range doc.Albums {
			albumIDs = append(albumIDs, ref.ID)
		}
		restorations = append(restorations, db.Restoration{
			AssetID:  r.AssetID,
			Asset:    doc.Asset,
			AlbumIDs: albumIDs,
			People:   doc.People,
			Analysis: analysis,
			Item:     !isComponent(parsed),
		})
		files = append(files, restored{sha: parsed.SHA256, ext: parsed.Ext})
	}

	count, err := s.Store.CommitUnvault(ctx, restorations)
	if err != nil {
		return 0, err
	}

	for _, f := range files {
		if s.VaultBlobs != nil {
			if _, err := s.VaultBlobs.Remove(f.sha, ""); err != nil {
				s.logger().Warn("could not remove a restored vault file", "error", err, "sha256", f.sha)
			}
		}
		if s.VaultDerivatives != nil {
			if _, _, err := s.VaultDerivatives.RemoveAll(f.sha, derivstore.Suffixes()); err != nil {
				s.logger().Warn("could not remove restored vault renditions", "error", err, "sha256", f.sha)
			}
		}
		s.record(manifest.KindUnvault, "", f.sha)
	}
	return count, nil
}

// unsealFiles writes the plaintext back where the archive expects it.
//
// The original goes through blobstore.Put rather than being written directly,
// which buys the one check worth having on this path for free: Put verifies the
// bytes against the digest and length the sealed document recorded, so a
// restore either produces the original photograph or refuses. Renditions are
// written without that check because they are derivatives — a wrong one is a
// requeue, not a loss.
func (s *Service) unsealFiles(priv *ecdh.PrivateKey, r row) error {
	if s.VaultBlobs != nil && s.Blobs != nil {
		reader, err := s.VaultBlobs.Open(priv, r.SHA256, "")
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("open the sealed original of %s: %w", r.ID, err)
		}
		if err == nil {
			result, putErr := s.Blobs.Put(reader, r.Ext, blobstore.Expected{MD5: r.MD5, Size: r.ByteSize})
			reader.Close()
			if putErr != nil {
				return fmt.Errorf("restore the original of %s: %w", r.ID, putErr)
			}
			if result.SHA256 != r.SHA256 {
				return fmt.Errorf("restore the original of %s: it decrypted to %s", r.ID, result.SHA256)
			}
		}
	}
	if s.VaultDerivatives == nil || s.Derivatives == nil {
		return nil
	}
	for _, suffix := range derivstore.Suffixes() {
		if !s.VaultDerivatives.Exists(r.SHA256, suffix) {
			continue
		}
		reader, err := s.VaultDerivatives.Open(priv, r.SHA256, suffix)
		if err != nil {
			return fmt.Errorf("open the sealed rendition %s%s: %w", r.SHA256, suffix, err)
		}
		err = s.Derivatives.Write(r.SHA256, suffix, func(w io.Writer) error {
			_, copyErr := io.Copy(w, reader)
			return copyErr
		})
		reader.Close()
		if err != nil {
			return fmt.Errorf("restore the rendition %s%s: %w", r.SHA256, suffix, err)
		}
	}
	return nil
}

func isComponent(r row) bool {
	return r.LiveParentLocalID != "" || r.LiveParentAssetID != nil || r.IsOverlay
}

// SetAlbum adds or removes one album on items that are already in the vault.
//
// It needs the password, and unavoidably so. Album membership went into the
// sealed document when the photograph was hidden — see db.CommitVault — so
// there is no column to update and no query that could do it: the only way to
// add a line to a sealed document is to open it. That makes this the one
// "filing" operation in the whole feature that a locked vault cannot do, which
// is honest rather than awkward. Putting something *into* the vault never needs
// the key; rearranging what is already in there is reading it.
//
// Rows in another bucket are skipped rather than refused. A selection cannot
// span the two buckets — each has its own page and its own index — so this only
// fires on something stale, and the right answer to "add this archived photo to
// a hidden album" is not to do it.
//
// The count is of documents actually rewritten, so adding photographs already
// in the album reports nothing done rather than claiming otherwise.
func (s *Service) SetAlbum(ctx context.Context, bucket string, ids []string, ref AlbumRef, member bool) (int, error) {
	if !db.ValidBucket(bucket) {
		return 0, fmt.Errorf("%w: %q", db.ErrBadBucket, bucket)
	}
	priv, err := s.Keeper.Identity()
	if err != nil {
		return 0, err
	}
	to, err := s.recipient(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.Store.VaultSealed(ctx, ids)
	if err != nil {
		return 0, err
	}

	changed := make([]db.SealedItem, 0, len(rows))
	for _, r := range rows {
		if r.Bucket != bucket {
			continue
		}
		doc, _, err := openDoc(priv, r.AssetID, r.Sealed)
		if err != nil {
			return 0, fmt.Errorf("open the sealed metadata of %s: %w", r.AssetID, err)
		}
		next, moved := withAlbum(doc.Albums, ref, member)
		if !moved {
			continue
		}
		doc.Albums = next

		// Re-marshalled from the same struct it was read into, which is why
		// Doc.Asset is raw JSON: the row travels through this untouched,
		// including the columns this package has never heard of.
		raw, err := json.Marshal(doc)
		if err != nil {
			return 0, fmt.Errorf("rebuild the sealed metadata of %s: %w", r.AssetID, err)
		}
		resealed, err := SealAsset(to, r.AssetID, raw)
		if err != nil {
			return 0, err
		}
		changed = append(changed, db.SealedItem{AssetID: r.AssetID, Sealed: resealed})
	}

	if err := s.Store.ResealVault(ctx, changed); err != nil {
		return 0, err
	}
	return len(changed), nil
}

// withAlbum returns the membership list with one album added or taken out, and
// whether it differs from what went in.
//
// The second return value is what keeps this from rewriting — and re-encrypting
// — every document in a selection that was already where it was being put.
func withAlbum(albums []AlbumRef, ref AlbumRef, member bool) ([]AlbumRef, bool) {
	out := make([]AlbumRef, 0, len(albums)+1)
	var held *AlbumRef
	for i := range albums {
		if albums[i].ID == ref.ID {
			held = &albums[i]
			if member {
				// Kept as the caller spelled it, so a title edited while the
				// photograph was hidden does not stay stale in the document.
				out = append(out, ref)
			}
			continue
		}
		out = append(out, albums[i])
	}

	if !member {
		return out, held != nil
	}
	if held == nil {
		return append(out, ref), true
	}
	return out, *held != ref
}

// Index builds one bucket's gallery. Requires the password, which is the whole
// of what makes a locked vault opaque rather than merely unlinked.
func (s *Service) Index(ctx context.Context, bucket string) (*Index, error) {
	priv, err := s.Keeper.Identity()
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.VaultRows(ctx, bucket)
	if err != nil {
		return nil, err
	}
	albums, err := s.Store.VaultedAlbums(ctx, bucket)
	if err != nil {
		return nil, err
	}
	people, err := s.Store.VaultedPeople(ctx, bucket)
	if err != nil {
		return nil, err
	}
	return Build(priv, bucket, rows, albums, people)
}

// Reconcile removes plaintext that a crash left behind.
//
// The one window in which a hidden photograph is still readable on disk is
// between the transaction committing and the unlink running, and it is small
// enough that it will probably never happen. "Probably never" is not the
// standard this feature is held to, so the sweep runs anyway, hourly, beside
// the trash's expiry — and it is cheap, because the question is only asked
// about rows that are already in the vault.
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	files, err := s.Store.VaultFiles(ctx)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, f := range files {
		if s.Blobs == nil {
			break
		}
		// The extension is one of the columns the scrub emptied, so the file
		// this is looking for has to be found by digest rather than named. A
		// blob tree with a matching digest under any extension is plaintext
		// that should not be there.
		removed, err := s.dropStrayPlaintext(f.SHA256)
		if err != nil {
			s.logger().Error("could not sweep plaintext left by a vault operation",
				"error", err, "sha256", f.SHA256)
			continue
		}
		if removed {
			cleaned++
			s.logger().Warn("removed plaintext left behind by an interrupted vault operation",
				"sha256", f.SHA256, "bucket", f.Bucket)
		}
	}
	return cleaned, nil
}

// ReconcileAnalysis takes the words away from photographs that are already
// hidden.
//
// Reconcile's counterpart, and the reason migration 0023 adds a column and
// nothing else. Sealing needs the vault's public key and an X25519 exchange,
// which SQL cannot do, so a migration could only have deleted — and deleting
// the only record of what a model said about every photograph already in the
// vault, with nothing to give back on a restore, is not a migration anybody
// should run. Here instead: seal first, delete second, the same order Add
// takes, on the same hourly timer.
//
// No password, deliberately. Putting something into the vault has never needed
// one — see the package comment — and this is that operation arriving late for
// photographs that were hidden before it existed. A sweep that waited for
// somebody to unlock would be a sweep that never ran on the archives that need
// it most.
//
// An archive with no vault at all has nothing to do and says so by returning
// zero rather than an error: the sweep runs on a timer on every deployment,
// including the ones where nobody has ever hidden anything.
func (s *Service) ReconcileAnalysis(ctx context.Context) (int, error) {
	left, err := s.Store.VaultAnalysisLeftBehind(ctx)
	if err != nil {
		return 0, err
	}
	if len(left) == 0 {
		return 0, nil
	}
	to, err := s.recipient(ctx)
	if errors.Is(err, ErrNoVault) {
		// Rows in the vault and no keypair to seal them to. That is a broken
		// archive rather than a quiet one, so it is said out loud and nothing
		// is deleted.
		s.logger().Error("hidden photos still carry their captions and there is no vault key to seal them with",
			"assets", len(left))
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	sealed := make([]db.SealedItem, 0, len(left))
	for _, c := range left {
		// Nil when the only thing left behind is the tsvector, which is the
		// state of a hidden photograph that was in the library long enough to
		// get a filename and a place into the search index and never a caption.
		// There is nothing to seal — the recipe is in migration 0018 and a
		// restore rebuilds the row from the columns — so this seals nothing and
		// the sweep goes on to delete it.
		var doc []byte
		if len(c.Analysis) > 0 {
			if doc, err = SealAnalysis(to, c.AssetID, c.Analysis); err != nil {
				return 0, err
			}
		}
		sealed = append(sealed, db.SealedItem{AssetID: c.AssetID, SealedAnalysis: doc})
	}
	return s.Store.CommitVaultAnalysis(ctx, sealed)
}

// dropStrayPlaintext removes any blob and any rendition sharing a vaulted
// digest.
func (s *Service) dropStrayPlaintext(sha string) (bool, error) {
	removed := false
	matches, err := s.Blobs.PathsFor(sha)
	if err != nil {
		return false, err
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed = true
	}
	if s.Derivatives != nil {
		files, _, err := s.Derivatives.RemoveAll(sha)
		if err != nil {
			return removed, err
		}
		if files > 0 {
			removed = true
		}
	}
	return removed, nil
}

// record appends the manifest line. Best effort and logged rather than fatal:
// the operation has already committed, and the log is the recovery story rather
// than the operation.
func (s *Service) record(kind, bucket, sha string) {
	if s.Manifest == nil {
		return
	}
	if err := s.Manifest.Append(manifest.Entry{
		Type: kind, SHA256: sha, Bucket: bucket, StoredAt: time.Now().UTC(),
	}); err != nil {
		s.logger().Error("could not record a vault operation in the manifest",
			"error", err, "sha256", sha, "kind", kind)
	}
}

// Item opens the sealed document of one asset.
//
// The media endpoints need it and the index would be the wrong way to get it:
// serving one thumbnail must not cost decrypting the whole vault. One row, one
// exchange, one AES-GCM open — the same cost the row itself was written at.
func (s *Service) Item(ctx context.Context, assetID string) (*Item, error) {
	priv, err := s.Keeper.Identity()
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.VaultSealed(ctx, []string{assetID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, db.ErrNotFound
	}
	doc, parsed, err := openDoc(priv, rows[0].AssetID, rows[0].Sealed)
	if err != nil {
		return nil, err
	}
	return &Item{Bucket: rows[0].Bucket, Doc: doc, row: parsed}, nil
}

// OpenOriginal and OpenDerivative are the read side of the two trees.
//
// The digest is deliberately enough to find a file: it is one of the columns
// the scrub leaves in the clear, so a thumbnail can be served without first
// opening the sealed document that would say what the photograph is. Which is
// the difference between a vault that draws a grid at the speed the library
// does and one that decrypts a JSON blob per tile.
func (s *Service) OpenOriginal(sha string) (*Reader, error) {
	priv, err := s.Keeper.Identity()
	if err != nil {
		return nil, err
	}
	return s.VaultBlobs.Open(priv, sha, "")
}

func (s *Service) OpenDerivative(sha, suffix string) (*Reader, error) {
	priv, err := s.Keeper.Identity()
	if err != nil {
		return nil, err
	}
	return s.VaultDerivatives.Open(priv, sha, suffix)
}

// Materialize writes a decrypted copy to a temporary file and hands back its
// path.
//
// It exists for the three renditions this server makes on demand rather than
// storing — the 2048px preview, a Live Photo's 1080p clip, and a composited
// caption layer — because ImageMagick and ffmpeg want a seekable path and one
// of them rewrites its output's header at the end. Piping is not an option for
// those, and re-plumbing them to take a reader would be a larger change to the
// derivative pipeline than this feature has any business making.
//
// The copy is plaintext, on disk, for as long as the render takes. It goes in
// the derivative staging directory, which is the SSD rather than the archive
// drive, and the cleanup is deferred by the caller. That is a real, if brief,
// hole in the promise this package makes, and it is the reason the *stored*
// renditions — the thumbnails, the playback file, the hover clip, which is
// everything the grid ever touches — are served by decrypting straight to the
// response instead.
func (s *Service) Materialize(sha, suffix string) (path string, cleanup func(), err error) {
	reader, err := s.openAny(sha, suffix)
	if err != nil {
		return "", func() {}, err
	}
	defer reader.Close()

	staged, cleanup, err := s.Derivatives.Stage("vault-*")
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.OpenFile(staged, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("open the staging file: %w", err)
	}
	_, copyErr := io.Copy(f, reader)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("decrypt to a staging file: %w", errors.Join(copyErr, closeErr))
	}
	return staged, cleanup, nil
}

func (s *Service) openAny(sha, suffix string) (*Reader, error) {
	if suffix == "" {
		return s.OpenOriginal(sha)
	}
	return s.OpenDerivative(sha, suffix)
}
