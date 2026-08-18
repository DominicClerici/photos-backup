// Package verify audits the archive against itself: the database against the
// blobs, the blobs against the manifest, and the derivative state against the
// files it claims exist.
//
// Everything here is read-only unless Fix is set, and even then the repairs are
// only the ones with a single obvious answer. A blob is never deleted and a
// corrupt one is never "repaired" — bit rot is reported and left alone, because
// the only honest response to an original that no longer matches its hash is a
// human with a second copy.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
)

// Kind classifies a finding. The order here is the order they are reported in,
// worst first: bytes that are gone or wrong come before bookkeeping.
type Kind string

const (
	// BlobMissing is an asset row whose original is not on disk. This is the
	// one that means a photo has been lost.
	BlobMissing Kind = "blob-missing"
	// BlobCorrupt is an original whose bytes no longer hash to its own name.
	BlobCorrupt Kind = "blob-corrupt"
	// BlobWrongSize is a size disagreement between the row and the file, found
	// without hashing.
	BlobWrongSize Kind = "blob-wrong-size"
	// BlobUnindexed is a file in the blob tree with no asset row. The archive
	// has it; the database does not know about it.
	BlobUnindexed Kind = "blob-unindexed"
	// ManifestMissing is an archived asset with no manifest line — the known
	// gap when a crash lands between the rename and the append.
	ManifestMissing Kind = "manifest-missing"
	// ManifestOrphan is a manifest line whose blob is not there.
	ManifestOrphan Kind = "manifest-orphan"
	// DerivativeMissing is an asset claiming a derivative that is not on disk.
	DerivativeMissing Kind = "derivative-missing"
	// DerivativeFailed is a derivative job that gave up permanently.
	DerivativeFailed Kind = "derivative-failed"
	// StaleUpload is an abandoned partial upload.
	StaleUpload Kind = "stale-upload"
	// StaleTemp is a leftover staging file from an interrupted write.
	StaleTemp Kind = "stale-temp"
	// VaultMissing is an asset in the Archive or Hidden bucket whose encrypted
	// original is not on disk. Critical, and for a worse reason than
	// BlobMissing: a plaintext original that goes missing may still exist on
	// the phone that uploaded it, while this one is content nothing else in the
	// world has a copy of in this form.
	VaultMissing Kind = "vault-missing"
)

// Severity separates "a photo is gone or damaged" from "the bookkeeping needs
// a nudge". Only the first kind should ever wake anyone up.
type Severity int

const (
	// Warning is recoverable bookkeeping: rebuildable derivatives, missing
	// index rows, litter.
	Warning Severity = iota
	// Critical means original bytes are missing or no longer match their hash.
	Critical
)

func (k Kind) Severity() Severity {
	switch k {
	case BlobMissing, BlobCorrupt, BlobWrongSize, ManifestOrphan, VaultMissing:
		return Critical
	default:
		return Warning
	}
}

// Finding is one thing wrong with the archive.
type Finding struct {
	Kind   Kind
	SHA256 string
	Path   string
	Detail string
	// Fixed reports whether --fix resolved it on this run.
	Fixed bool
}

func (f Finding) String() string {
	subject := f.SHA256
	if subject == "" {
		subject = f.Path
	}
	if len(subject) > 16 && f.SHA256 != "" {
		subject = subject[:16]
	}

	line := fmt.Sprintf("%-19s %-16s %s", f.Kind, subject, f.Detail)
	if f.Fixed {
		line += "  [fixed]"
	}
	return line
}

// Options configures a run.
type Options struct {
	// Deep re-hashes every original. This is the bit-rot check and the only
	// pass that reads the whole archive, so it is off by default and on in the
	// weekly timer.
	Deep bool
	// Fix applies the unambiguous repairs.
	Fix bool
	// RetryFailed puts jobs that gave up back in the queue. Deliberately not
	// part of Fix: a job reaching this state has already failed every attempt
	// it was given, so retrying it belongs to someone who knows what changed —
	// a new binary, a newly installed codec. Folded into Fix it would run from
	// the weekly timer and grind the same impossible file forever.
	RetryFailed bool
	// StaleAfter is how old an abandoned partial or temp file must be before it
	// is reported. Anything younger could be an upload in flight right now.
	StaleAfter time.Duration
	// Progress is called as the deep pass works through the archive, so a
	// multi-hour verify is not a silent one.
	Progress func(done, total int64)
}

// Deps are the pieces of the archive a run inspects.
type Deps struct {
	Store       *db.Store
	Blobs       *blobstore.Store
	Derivatives *derivstore.Store
	// VaultBlobs is the encrypted tree. Optional: without it the vault is
	// skipped rather than reported as missing, which is the right answer for a
	// deployment where the vault has never been used.
	VaultBlobs *vault.Store
	Uploads    *uploads.Store
	Queue      *jobs.Queue
	PhotosRoot string
}

// Report is the outcome of a run.
type Report struct {
	Findings []Finding
	Checked  int64
	Bytes    int64
	Hashed   int64
	Fixed    int
	Elapsed  time.Duration
}

// Critical reports whether anything found means lost or damaged originals.
func (r Report) Critical() bool {
	for _, f := range r.Findings {
		if f.Kind.Severity() == Critical && !f.Fixed {
			return true
		}
	}
	return false
}

// Unresolved counts findings still outstanding after any repairs.
func (r Report) Unresolved() int {
	n := 0
	for _, f := range r.Findings {
		if !f.Fixed {
			n++
		}
	}
	return n
}

// Run performs every pass and returns what it found.
func Run(ctx context.Context, d Deps, opt Options) (Report, error) {
	started := time.Now()
	if opt.StaleAfter <= 0 {
		opt.StaleAfter = 24 * time.Hour
	}

	r := &Report{}
	counts, err := d.Store.Counts(ctx)
	if err != nil {
		return *r, err
	}

	// The set of hashes the database knows about, built during the first pass
	// and consumed by the second. One hex string per asset: about 26MB for a
	// 400,000-item library, which is worth not walking the tree twice.
	indexed := make(map[string]struct{}, counts.Assets)
	// And the subset of those that are in the vault, which every pass after the
	// first has to know about: their bytes are deliberately not in the blob
	// tree, so a manifest line pointing at one is a fact rather than an orphan.
	sealed := make(map[string]struct{})

	if err := checkAssets(ctx, d, opt, r, indexed, sealed, counts); err != nil {
		return *r, err
	}
	if err := checkManifest(ctx, d, opt, r, indexed, sealed); err != nil {
		return *r, err
	}
	if err := checkBlobTree(ctx, d, opt, r, indexed); err != nil {
		return *r, err
	}
	if err := checkFailedJobs(ctx, d, opt, r); err != nil {
		return *r, err
	}
	checkLitter(d, opt, r)

	sort.SliceStable(r.Findings, func(i, j int) bool {
		return r.Findings[i].Kind.Severity() > r.Findings[j].Kind.Severity()
	})
	for _, f := range r.Findings {
		if f.Fixed {
			r.Fixed++
		}
	}
	r.Elapsed = time.Since(started)
	return *r, nil
}

// checkAssets walks the database and confirms each row's original is present,
// the right length, and — with Deep — still hashes to its own name.
func checkAssets(ctx context.Context, d Deps, opt Options, r *Report, indexed, sealed map[string]struct{}, counts db.Counts) error {
	return d.Store.EachAsset(ctx, func(a db.Asset) error {
		r.Checked++
		indexed[a.SHA256] = struct{}{}
		if opt.Progress != nil {
			opt.Progress(r.Checked, counts.Assets)
		}

		// An asset in the vault has no plaintext anywhere, by design, and no
		// extension left on its row to look for one under. What can still be
		// checked without the password is that the ciphertext is there and has
		// a plausible length — the digest cannot be, because the file on disk
		// is not the bytes that digest names.
		//
		// Deep verification of the vault would mean holding the private key,
		// which is a thing `verify` deliberately does not do: it runs from a
		// systemd timer at four in the morning, and a nightly job that can
		// decrypt the hidden photographs is a nightly job that has to be
		// trusted with them.
		if a.Vault != "" {
			sealed[a.SHA256] = struct{}{}
			checkVaulted(d, r, a)
			return nil
		}

		path := d.Blobs.Path(a.SHA256, a.Ext)
		info, err := os.Stat(path)
		switch {
		case os.IsNotExist(err):
			r.Findings = append(r.Findings, Finding{
				Kind: BlobMissing, SHA256: a.SHA256, Path: path,
				Detail: fmt.Sprintf("%s is indexed but not on disk", a.OriginalFilename),
			})
			return nil
		case err != nil:
			return fmt.Errorf("stat blob %s: %w", path, err)
		}

		r.Bytes += info.Size()
		if info.Size() != a.ByteSize {
			r.Findings = append(r.Findings, Finding{
				Kind: BlobWrongSize, SHA256: a.SHA256, Path: path,
				Detail: fmt.Sprintf("row says %d bytes, file is %d", a.ByteSize, info.Size()),
			})
			return nil
		}

		if opt.Deep {
			sum, err := hashFile(path)
			if err != nil {
				return fmt.Errorf("hash blob %s: %w", path, err)
			}
			r.Hashed += info.Size()
			if sum != a.SHA256 {
				// Deliberately not repairable. The bytes on disk are not the
				// bytes that were archived, and only a second copy can say
				// which one is right.
				r.Findings = append(r.Findings, Finding{
					Kind: BlobCorrupt, SHA256: a.SHA256, Path: path,
					Detail: fmt.Sprintf("hashes to %s — restore this file from a backup", sum[:16]),
				})
				return nil
			}
		}

		checkDerivatives(ctx, d, opt, r, a)
		return nil
	})
}

// checkVaulted is everything that can be said about a hidden photograph from
// the outside.
func checkVaulted(d Deps, r *Report, a db.Asset) {
	if d.VaultBlobs == nil {
		return
	}
	path := d.VaultBlobs.Path(a.SHA256, "")
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		r.Findings = append(r.Findings, Finding{
			Kind: VaultMissing, SHA256: a.SHA256, Path: path,
			Detail: fmt.Sprintf("in the %s vault but the sealed original is not on disk", a.Vault),
		})
		return
	case err != nil:
		r.Findings = append(r.Findings, Finding{
			Kind: VaultMissing, SHA256: a.SHA256, Path: path,
			Detail: fmt.Sprintf("could not read the sealed original: %v", err),
		})
		return
	}

	r.Bytes += info.Size()
	// The ciphertext is longer than the plaintext by a header and a tag per
	// chunk, and by exactly that much — so a file that does not decode to the
	// length the row records is truncated or is not a vault file at all, and
	// that is knowable without the key.
	if plain, err := vault.PlaintextSize(info.Size()); err != nil || plain != a.ByteSize {
		r.Findings = append(r.Findings, Finding{
			Kind: VaultMissing, SHA256: a.SHA256, Path: path,
			Detail: fmt.Sprintf("the sealed original does not hold %d bytes", a.ByteSize),
		})
	}
}

// checkDerivatives confirms an asset that claims a thumbnail or a playback
// rendition actually has one. A missing derivative is repairable by re-running
// the job that built it, which is what --fix does.
func checkDerivatives(ctx context.Context, d Deps, opt Options, r *Report, a db.Asset) {
	if d.Derivatives == nil {
		return
	}

	// A paired video has no still thumbnail by design — the tile it appears in
	// belongs to the photo it is paired with. Its motion renditions are checked
	// instead.
	if a.IsLivePair() {
		if a.LiveState == db.DerivedReady {
			if missing, path := missingSizes(d, a.SHA256, derivstore.LiveSuffix); len(missing) > 0 {
				r.Findings = append(r.Findings, Finding{
					Kind: DerivativeMissing, SHA256: a.SHA256, Path: path,
					Detail: fmt.Sprintf("marked ready but the Live Photo rendition is gone at %s", sizeList(missing)),
					Fixed:  opt.Fix && requeue(ctx, d, jobs.KindMetadata, a.ID) == nil,
				})
			}
		}
		return
	}

	if a.DerivedState == db.DerivedReady {
		if missing, path := missingSizes(d, a.SHA256, derivstore.ThumbSuffix); len(missing) > 0 {
			r.Findings = append(r.Findings, Finding{
				Kind: DerivativeMissing, SHA256: a.SHA256, Path: path,
				Detail: fmt.Sprintf("marked ready but the thumbnail is gone at %s", sizeList(missing)),
				Fixed:  opt.Fix && requeue(ctx, d, jobs.KindMetadata, a.ID) == nil,
			})
		}
	}

	if a.PlaybackState != db.DerivedReady {
		return
	}
	if !d.Derivatives.Exists(a.SHA256, derivstore.Playback) {
		r.Findings = append(r.Findings, Finding{
			Kind: DerivativeMissing, SHA256: a.SHA256,
			Path:   d.Derivatives.Path(a.SHA256, derivstore.Playback),
			Detail: "marked ready but the playback rendition is gone",
			Fixed:  opt.Fix && requeue(ctx, d, jobs.KindPlayback, a.ID) == nil,
		})
		return
	}

	// A video with a caption layer keeps a second rendition without it, which
	// is the only thing the viewer's overlay toggle can show. Checked here
	// rather than backfilled by hand, so a library transcoded before the layer
	// was linked — or before this rendition existed at all — is repaired by the
	// `verify --fix` that already runs weekly.
	if a.OverlayAssetID != nil && !d.Derivatives.Exists(a.SHA256, derivstore.PlaybackPlain) {
		r.Findings = append(r.Findings, Finding{
			Kind: DerivativeMissing, SHA256: a.SHA256,
			Path:   d.Derivatives.Path(a.SHA256, derivstore.PlaybackPlain),
			Detail: "carries an overlay but has no rendition without it",
			Fixed:  opt.Fix && requeue(ctx, d, jobs.KindPlayback, a.ID) == nil,
		})
	}
}

// missingSizes reports which stored sizes an asset is short of, and the path of
// the first one, so an asset with several gaps is still one finding rather than
// one per size — which is the difference between a report and a wall of text
// the first time a library is checked after a new size is introduced.
//
// A library built before a size existed is exactly this case, and it is how the
// backfill is driven: `photobackup verify --fix` finds every asset that has
// only the sizes it was ingested with and requeues the job that renders the
// rest.
func missingSizes(d Deps, sha string, suffix func(int) string) ([]int, string) {
	var missing []int
	var first string
	for _, size := range derivstore.ThumbSizes {
		if d.Derivatives.Exists(sha, suffix(size)) {
			continue
		}
		if first == "" {
			first = d.Derivatives.Path(sha, suffix(size))
		}
		missing = append(missing, size)
	}
	return missing, first
}

func sizeList(sizes []int) string {
	parts := make([]string, len(sizes))
	for i, size := range sizes {
		parts[i] = fmt.Sprintf("%dpx", size)
	}
	switch len(parts) {
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// checkManifest confirms every line has a blob, and every archived asset has a
// line.
//
// The second direction is the known gap in the commit ordering: a crash between
// the rename and the append leaves a blob with no line. It is the one finding
// --fix can resolve completely, because the database still holds everything the
// line needs.
func checkManifest(ctx context.Context, d Deps, opt Options, r *Report, indexed, sealed map[string]struct{}) error {
	path := filepath.Join(d.PhotosRoot, "manifest.jsonl")
	logged := make(map[string]struct{}, len(indexed))

	err := manifest.Scan(path, func(e manifest.Entry) error {
		// A metadata line names a digest but records no bytes. Counting it as
		// a logged blob would satisfy the second check below with a line a
		// rebuild could not restore the asset from, and looking for its blob
		// would report an orphan for a file that is present and fine.
		if !e.IsAsset() {
			return nil
		}
		logged[e.SHA256] = struct{}{}

		// A vaulted original is not in the blob tree, on purpose, and the line
		// above it in this log still describes it correctly. Reporting it as an
		// orphan would be a Critical finding — the kind that is meant to wake
		// somebody up — about a photograph that is exactly where it was put.
		//
		// The manifest also carries its own `vault` line, which is what a
		// rebuild with no database reads. This pass has a database, so it uses
		// the cheaper answer.
		if _, hidden := sealed[e.SHA256]; hidden {
			return nil
		}

		blob := d.Blobs.Path(e.SHA256, e.Ext)
		if _, err := os.Stat(blob); os.IsNotExist(err) {
			r.Findings = append(r.Findings, Finding{
				Kind: ManifestOrphan, SHA256: e.SHA256, Path: blob,
				Detail: fmt.Sprintf("%s is in the manifest but not in the blob tree", e.Filename),
			})
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read manifest: %w", err)
	}

	log := manifest.New(path)
	for sha := range indexed {
		if _, ok := logged[sha]; ok {
			continue
		}
		finding := Finding{
			Kind: ManifestMissing, SHA256: sha,
			Detail: "archived with no manifest line; a rebuild would not find it",
		}
		if opt.Fix {
			finding.Fixed = appendMissingLine(ctx, d, log, sha) == nil
		}
		r.Findings = append(r.Findings, finding)
	}
	return nil
}

// appendMissingLine reconstructs a manifest line from the database row.
func appendMissingLine(ctx context.Context, d Deps, log *manifest.Log, sha string) error {
	a, err := d.Store.AssetBySHA256(ctx, sha)
	if err != nil {
		return err
	}

	return log.Append(manifest.Entry{
		SHA256:      a.SHA256,
		MD5:         a.MD5,
		Size:        a.ByteSize,
		Filename:    a.OriginalFilename,
		ContentType: a.ContentType,
		Ext:         a.Ext,
		CapturedAt:  a.CapturedAt,
		ModifiedAt:  a.ModifiedAt,
		DeviceID:    a.DeviceID,
		LocalID:     a.LocalID,
		ContentID:   a.ContentID,
		// Not the original storage time, which is gone. The row's arrival time
		// is the closest true thing available.
		StoredAt: a.UploadedAt,
	})
}

// checkBlobTree walks the archive itself, looking for originals the database
// has never heard of.
//
// This is the direction that catches a lost database rather than a lost file,
// and it is why `reindex` exists: the bytes are safe, the index is not.
func checkBlobTree(ctx context.Context, d Deps, opt Options, r *Report, indexed map[string]struct{}) error {
	root := filepath.Join(d.PhotosRoot, "blobs")

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			// Staging is not the archive; it is checked by checkLitter.
			if entry.Name() == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}

		sha := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if len(sha) != 64 {
			r.Findings = append(r.Findings, Finding{
				Kind: BlobUnindexed, Path: path,
				Detail: "not a content-addressed filename",
			})
			return nil
		}
		if _, ok := indexed[sha]; !ok {
			r.Findings = append(r.Findings, Finding{
				Kind: BlobUnindexed, SHA256: sha, Path: path,
				Detail: "on disk with no asset row; `photobackup reindex` will adopt it",
			})
		}
		return nil
	})
}

func checkFailedJobs(ctx context.Context, d Deps, opt Options, r *Report) error {
	if d.Queue == nil {
		return nil
	}
	failed, err := d.Queue.Failed(ctx, 50)
	if err != nil {
		return fmt.Errorf("list failed jobs: %w", err)
	}
	for _, j := range failed {
		finding := Finding{
			Kind:   DerivativeFailed,
			Detail: fmt.Sprintf("%s job for asset %s gave up: %s", j.Kind, j.AssetID, firstLine(j.Error)),
		}
		if opt.RetryFailed {
			finding.Fixed = requeue(ctx, d, j.Kind, j.AssetID) == nil
		}
		r.Findings = append(r.Findings, finding)
	}
	return nil
}

// checkLitter finds bytes nothing references: abandoned partial uploads, and
// staging files from writes that were interrupted.
func checkLitter(d Deps, opt Options, r *Report) {
	if d.Uploads != nil {
		cutoff := time.Now().Add(-opt.StaleAfter)
		entries, _ := os.ReadDir(d.Uploads.Dir())
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".part") {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			r.Findings = append(r.Findings, Finding{
				Kind: StaleUpload, Path: filepath.Join(d.Uploads.Dir(), entry.Name()),
				Detail: fmt.Sprintf("%s abandoned since %s", byteCount(info.Size()), info.ModTime().Format(time.DateOnly)),
			})
		}
		if opt.Fix {
			if removed, err := d.Uploads.Sweep(opt.StaleAfter); err == nil && removed > 0 {
				markFixed(r, StaleUpload)
			}
		}
	}

	tmp := filepath.Join(d.PhotosRoot, "blobs", "tmp")
	cutoff := time.Now().Add(-opt.StaleAfter)
	entries, _ := os.ReadDir(tmp)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(tmp, entry.Name())
		finding := Finding{
			Kind: StaleTemp, Path: path,
			Detail: fmt.Sprintf("%s left by an interrupted upload", byteCount(info.Size())),
		}
		if opt.Fix {
			finding.Fixed = os.Remove(path) == nil
		}
		r.Findings = append(r.Findings, finding)
	}
}

func markFixed(r *Report, kind Kind) {
	for i := range r.Findings {
		if r.Findings[i].Kind == kind {
			r.Findings[i].Fixed = true
		}
	}
}

func requeue(ctx context.Context, d Deps, kind jobs.Kind, assetID string) error {
	if d.Queue == nil {
		return errors.New("no queue")
	}
	return jobs.Requeue(ctx, d.Store.Pool(), kind, assetID)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func byteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
