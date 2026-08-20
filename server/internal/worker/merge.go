package worker

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
	"github.com/dominicclerici/photos-backup/server/internal/snapchat"
	"github.com/dominicclerici/photos-backup/server/internal/video"
)

// runMerge joins the pieces of one Snapchat recording into a single archived
// original, and puts the pieces in the trash.
//
// This is the one job in this worker that adds to the archive rather than
// deriving from it, and that is worth being explicit about. Every other file
// under blobs/ arrived from a camera by way of a phone or an export; this one
// was built here, out of six that did. It is content-addressed like any other,
// it gets a manifest line like any other, `verify` covers it like any other,
// and the six it was made from are still on the disk and still verifiable for
// as long as they sit in the trash.
//
// The order below is the upload path's order and for the upload path's reason:
// blob, then manifest, then row. Anything lost after the blob lands is
// recoverable by running the job again, because the join is a deterministic
// function of the same six files.
//
// Nothing is trashed until the joined recording is committed and indexed. A
// crash halfway leaves a duplicate-looking minute of video beside the pieces it
// came from, which somebody can see and undo; the other order would leave six
// pieces in the trash and nothing joined.
func (r *Runner) runMerge(ctx context.Context, assetID string) error {
	group, err := r.Store.PendingGroupWithMember(ctx, merge.KindSegments, assetID)
	if errors.Is(err, db.ErrNotFound) {
		// Dismissed, superseded, or already merged between the queueing and the
		// claim. There is genuinely nothing to do.
		return nil
	}
	if err != nil {
		return err
	}
	if len(group.Members) < 2 {
		return fmt.Errorf("merge %s: %d pieces is not something to join", group.ID, len(group.Members))
	}

	parts, assets, err := r.gatherParts(ctx, group)
	if err != nil {
		return err
	}

	staged, cleanup, err := r.Derivatives.Stage("joined-*.mp4")
	if err != nil {
		return err
	}
	defer cleanup()

	result, err := r.Video.Join(ctx, parts, staged,
		video.JoinOptions{AllowDurationMismatch: group.Forced})
	var mismatch *video.DurationMismatch
	if errors.As(err, &mismatch) {
		// The one join failure that produces a playable file, and the one worth
		// arguing with: the arithmetic cannot tell a part ffmpeg silently
		// dropped from a container whose last frame runs a tenth of a second
		// long, and a person watching it can. So the file this job just refused
		// is kept under the group rather than deleted with the rest of the
		// staging, and the review page offers it beside a button that archives
		// it anyway. The job still fails — nothing is in the library yet.
		r.keepRejected(group, staged)
		return err
	}
	if err != nil {
		return err
	}
	if !result.Copied {
		r.log().Info("joined a recording by re-encoding it",
			"group", group.ID, "parts", len(parts), "reason", result.Why)
	}
	if result.Mismatch {
		r.log().Warn("archived a joined recording whose parts do not add up",
			"group", group.ID, "parts", len(parts),
			"expected", result.Expected, "seconds", result.DurationSeconds)
	}

	joined, err := r.archiveJoined(ctx, group, assets, staged, result)
	if err != nil {
		return err
	}

	merged, err := r.Store.MergeSegments(ctx, group.ID, joined)
	if err != nil {
		return err
	}
	r.log().Info("joined a Snapchat recording that was exported in pieces",
		"group", group.ID, "parts", len(parts), "asset", joined,
		"seconds", result.DurationSeconds, "copied", result.Copied, "trashed", merged.Trashed)

	// The question has been answered, so the rejected attempt at answering it is
	// nobody's. Sweeping catches this too — see db.SegmentPreviews — but a file
	// removed at the moment it stops being wanted is a file nothing has to
	// reason about later.
	r.dropRejected(group)

	// The joined asset was inserted with a metadata job of its own, and there is
	// a transcode behind that.
	r.Nudge()
	return nil
}

// gatherParts turns a group's members into ffmpeg's inputs, in order, and hands
// back the asset rows alongside because everything after the join is built from
// them.
func (r *Runner) gatherParts(ctx context.Context, group db.MergeGroup) ([]video.Part, []db.Asset, error) {
	parts := make([]video.Part, 0, len(group.Members))
	assets := make([]db.Asset, 0, len(group.Members))

	for _, m := range group.Members {
		asset, err := r.Store.Asset(ctx, m.AssetID)
		if err != nil {
			return nil, nil, fmt.Errorf("merge %s: %w", group.ID, err)
		}
		if asset.Vault != "" {
			// A piece was hidden after the group was found. Its plaintext is
			// gone from the disk on purpose and joining it would be defeating
			// the vault with the archive's own tools.
			return nil, nil, fmt.Errorf(
				"merge %s: part %s is in the vault; refusing to join it", group.ID, m.AssetID)
		}

		src := r.Blobs.Path(asset.SHA256, asset.Ext)
		if _, err := os.Stat(src); err != nil {
			return nil, nil, fmt.Errorf("merge %s: part missing from the blob store: %w", group.ID, err)
		}

		part := video.Part{Path: src}
		// Each piece carries its own caption layer, which appears and vanishes
		// at the ten-second boundaries exactly as it did in the app.
		if layer, err := r.overlay(ctx, asset); err != nil {
			return nil, nil, err
		} else if layer != nil {
			part.Overlay = layer.Path
		}

		parts = append(parts, part)
		assets = append(assets, asset)
	}
	return parts, assets, nil
}

// archiveJoined commits the joined file as an original and returns its asset id.
func (r *Runner) archiveJoined(
	ctx context.Context,
	group db.MergeGroup,
	parts []db.Asset,
	staged string,
	result video.JoinResult,
) (string, error) {
	file, err := os.Open(staged)
	if err != nil {
		return "", fmt.Errorf("merge %s: reopen the joined file: %w", group.ID, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("merge %s: measure the joined file: %w", group.ID, err)
	}

	// Put insists on a declaration it can check the bytes against, because every
	// other caller has a client that made one. There is no client here, so the
	// file is hashed first and the check becomes what it should be for a local
	// file: a guard against the disk changing under us between the join and the
	// commit.
	//
	// Put rather than Adopt, which is the method for committing a file that is
	// already complete on disk and would be the obvious choice. Adopt renames,
	// and this file is staged under DERIVATIVES_ROOT — the SSD — while the blob
	// tree is on the archive drive. A rename across two filesystems fails, so
	// the bytes have to be streamed. Encoding on the fast disk and copying once
	// to the slow one is the right way round anyway.
	md5sum, err := stagedDigest(staged)
	if err != nil {
		return "", fmt.Errorf("merge %s: %w", group.ID, err)
	}

	const ext = ".mp4"
	contentType := mediatype.FromExt(ext)
	stored, err := r.Blobs.Put(file, ext, blobstore.Expected{MD5: md5sum, Size: info.Size()})
	if err != nil {
		return "", fmt.Errorf("merge %s: store the joined recording: %w", group.ID, err)
	}

	first := parts[0]
	name := joinedName(first.OriginalFilename)
	localID := joinedLocalID(first)

	source, err := r.Store.ImportSidecar(ctx, first.ID)
	if err != nil {
		return "", fmt.Errorf("merge %s: %w", group.ID, err)
	}
	sidecar, err := joinedSidecar(first, source, parts, result)
	if err != nil {
		return "", fmt.Errorf("merge %s: %w", group.ID, err)
	}

	if stored.Created {
		if err := r.Manifest.Append(manifest.Entry{
			SHA256:      stored.SHA256,
			MD5:         stored.MD5,
			Size:        stored.Size,
			Filename:    name,
			ContentType: contentType,
			Ext:         ext,
			CapturedAt:  first.CapturedAt,
			DeviceID:    first.DeviceID,
			LocalID:     localID,
			StoredAt:    time.Now().UTC(),
		}); err != nil {
			return "", fmt.Errorf("merge %s: append manifest: %w", group.ID, err)
		}
		// A second line, of the kind an import writes, carrying the document
		// that says what this file is. Without it a rebuild from the log alone
		// would recover a minute of Snapchat video with no indication that it
		// was ever six files.
		if err := r.Manifest.Append(manifest.Entry{
			Type:          manifest.KindMetadata,
			SHA256:        stored.SHA256,
			ImportSource:  db.SourceSnapchat,
			ImportSidecar: sidecar,
			StoredAt:      time.Now().UTC(),
		}); err != nil {
			return "", fmt.Errorf("merge %s: append manifest metadata: %w", group.ID, err)
		}
	}

	id, _, err := r.Store.RecordAsset(ctx, db.Asset{
		SHA256:           stored.SHA256,
		MD5:              stored.MD5,
		ByteSize:         stored.Size,
		OriginalFilename: name,
		Ext:              ext,
		ContentType:      contentType,
		MediaKind:        db.MediaVideo,
		CapturedAt:       first.CapturedAt,
		DeviceID:         first.DeviceID,
		LocalID:          localID,
	})
	if err != nil {
		return "", fmt.Errorf("merge %s: index the joined recording: %w", group.ID, err)
	}

	meta, err := db.ImportMetadataFrom(db.SourceSnapchat, sidecar, nil)
	if err != nil {
		return "", fmt.Errorf("merge %s: %w", group.ID, err)
	}
	if err := r.Store.ApplyImportMetadata(ctx, id, meta); err != nil {
		return "", fmt.Errorf("merge %s: describe the joined recording: %w", group.ID, err)
	}
	return id, nil
}

// stagedDigest is the MD5 of a file this worker has just written, for the
// declaration Put checks it against.
func stagedDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("reopen the joined file: %w", err)
	}
	defer f.Close()

	sum := md5.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("hash the joined file: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// joinedName is what the joined file is called: the first piece's name with the
// role suffix replaced.
//
// Snapchat's own convention, followed rather than invented, because the name is
// the one part of this that somebody reads. `2018-10-07_c6a325c9-…-main.mp4`
// becomes `2018-10-07_c6a325c9-…-joined.mp4`, which sorts beside the pieces it
// came from and says what it is.
func joinedName(first string) string {
	ext := path.Ext(first)
	stem := strings.TrimSuffix(first, ext)
	stem = strings.TrimSuffix(stem, "-main")
	return stem + "-joined.mp4"
}

// joinedLocalID is the joined recording's identity within the import, which is
// what device_assets is keyed on.
//
// Derived from the first piece's local id rather than generated, so that
// re-running a merge that was undone and re-proposed lands on the same mapping
// instead of accumulating one per attempt.
func joinedLocalID(first db.Asset) string {
	if first.LocalID == "" {
		return ""
	}
	ext := path.Ext(first.LocalID)
	stem := strings.TrimSuffix(first.LocalID, ext)
	stem = strings.TrimSuffix(stem, "-main")
	return stem + "-joined.mp4"
}

// joinedSidecar composes the document that describes the joined recording.
//
// The first piece's history row is carried across verbatim, which is what gives
// the joined file its capture instant and its coordinates: the recording began
// when its first piece began, and Snapchat's record of that is the row it wrote
// for that piece. Everything this archive worked out — that there were six
// pieces, which ones, and whether the join copied or re-encoded — sits beside it
// under `joined`, never inside it. Same rule the importer follows: Snapchat's
// words are Snapchat's, ours are ours.
func joinedSidecar(
	first db.Asset,
	firstSidecar json.RawMessage,
	parts []db.Asset,
	result video.JoinResult,
) (json.RawMessage, error) {
	var source snapchat.Sidecar
	if len(firstSidecar) > 0 {
		if err := json.Unmarshal(firstSidecar, &source); err != nil {
			return nil, fmt.Errorf("read the first piece's sidecar: %w", err)
		}
	}

	method := "stream-copy"
	if !result.Copied {
		method = "re-encode"
	}
	joined := &snapchat.Joined{
		Method:          method,
		Reason:          result.Why,
		DurationSeconds: result.DurationSeconds,
		JoinedAt:        time.Now().UTC(),
	}
	if result.Mismatch {
		joined.ExpectedSeconds = result.Expected
	}
	for _, p := range parts {
		part := snapchat.JoinedPart{File: p.OriginalFilename, SHA256: p.SHA256}
		if p.CapturedAt != nil {
			part.CapturedAt = *p.CapturedAt
		}
		if p.DurationSeconds != nil {
			part.DurationSeconds = *p.DurationSeconds
		}
		joined.Parts = append(joined.Parts, part)
	}

	out := snapchat.Sidecar{
		Export:           snapchat.Source,
		Kind:             "memories",
		Delivery:         source.Delivery,
		File:             joinedName(first.OriginalFilename),
		MediaID:          source.MediaID,
		Role:             snapchat.RoleMain,
		CapturedAt:       first.CapturedAt,
		CapturedAtSource: source.CapturedAtSource,
		History:          source.History,
		HistoryMatch:     source.HistoryMatch,
		Subtypes:         []string{snapchat.SubtypeMemory, snapchat.SubtypeJoined},
		Joined:           joined,
	}
	return json.Marshal(out)
}

// keepRejected files a join this worker refused to archive under the group it
// came from, so it can be watched.
//
// Under the group's fingerprint rather than any asset's digest, because it is
// not a rendition of an asset: it is what six of them would be if they were
// one. That makes it the only file in the derivative tree that no per-asset
// cleanup can reach, which is why db.SegmentPreviews and the sweep that reads
// it exist.
//
// Never fatal. The job has already failed and is about to say so; failing to
// keep the evidence costs the review page a button, not the archive anything.
func (r *Runner) keepRejected(group db.MergeGroup, staged string) {
	if r.Derivatives == nil {
		return
	}
	if err := r.Derivatives.Commit(group.Fingerprint, derivstore.JoinPreview, staged); err != nil {
		r.log().Warn("could not keep the rejected join for review",
			"error", err, "group", group.ID)
		return
	}
	r.log().Info("kept a rejected join for review", "group", group.ID)
}

// dropRejected removes an earlier attempt's evidence once the join has landed.
func (r *Runner) dropRejected(group db.MergeGroup) {
	if r.Derivatives == nil {
		return
	}
	if err := r.Derivatives.Remove(group.Fingerprint, derivstore.JoinPreview); err != nil {
		r.log().Warn("could not remove a rejected join", "error", err, "group", group.ID)
	}
}

// EnqueueSegmentMerges queues a join for every recording found and not yet put
// back together.
//
// Idempotent by way of the jobs table's unique (asset_id, kind): a group whose
// job is already queued or running is left alone, and one whose job finished
// against a group that has since been superseded is requeued, because the head
// asset is the same and the work is not.
func (r *Runner) EnqueueSegmentMerges(ctx context.Context) (int, error) {
	heads, err := r.Store.PendingGroupHeads(ctx, merge.KindSegments)
	if err != nil {
		return 0, err
	}
	for _, head := range heads {
		if err := jobs.Requeue(ctx, r.Store.Pool(), jobs.KindMerge, head); err != nil {
			return 0, err
		}
	}
	if len(heads) > 0 {
		r.Nudge()
	}
	return len(heads), nil
}
