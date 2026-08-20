package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/imagehash"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

// runSignature reduces one original to the numbers the duplicate scan compares.
//
// It is the third kind of derivative this worker builds and the only one that
// produces no file: a row of integers — two for a still, forty-two for a clip —
// describing what the picture looks like closely enough to recognise another
// copy of it. See internal/imagehash for what they are and internal/merge for
// what is done with them.
//
// Deliberately queued at the end of the metadata job rather than beside it. A
// video cannot be sampled without knowing how long it is, and that number is
// written by the job in front of this one. On a fresh upload the two run
// seconds apart; on a backfill the ordering is the whole difference between
// seven thousand sampled clips and seven thousand sampled across a duration of
// zero.
func (r *Runner) runSignature(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
	}
	switch {
	case vaulted(asset):
		// The same silent success the other jobs give a vaulted asset, and here
		// it is not merely pragmatic. A signature is a description of the
		// photograph, and the vault's whole promise is that this server cannot
		// hold one. See internal/vault.
		return nil
	case asset.DeletedAt != nil:
		// Moved to the trash between the queueing and the claim. Nothing that
		// reads signatures looks at deleted assets.
		return nil
	case asset.IsOverlay, asset.IsLivePair():
		// Components of other assets' pictures rather than pictures. They are
		// absent from every timeline and from the scan's input, so a signature
		// for one would be a decode nobody reads — and for a Live Photo's
		// motion it would also be a near-duplicate of its own still.
		return nil
	}

	src := r.Blobs.Path(asset.SHA256, asset.Ext)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("original missing from the blob store: %w", err)
	}

	sig := db.Signature{Version: merge.SignatureVersion}
	if asset.Width != nil && asset.Height != nil && *asset.Height > 0 {
		sig.Aspect = float64(*asset.Width) / float64(*asset.Height)
	}

	if asset.MediaKind == db.MediaVideo {
		if err := r.clipSignature(ctx, asset, src, &sig); err != nil {
			return err
		}
	} else if err := r.stillSignature(ctx, asset, src, &sig); err != nil {
		return err
	}

	return r.Store.PutSignature(ctx, assetID, sig)
}

// stillSignature hashes a photograph.
//
// Through the caption layer, for a Snapchat memory, because the composite is
// the picture: two copies of one memory are two copies of what was sent, and
// the photograph underneath is a thing nobody ever saw.
func (r *Runner) stillSignature(ctx context.Context, asset db.Asset, src string, sig *db.Signature) error {
	overlay, err := r.overlay(ctx, asset)
	if err != nil {
		return err
	}
	if overlay != nil {
		overlay.Width, overlay.Height = intOr(asset.Width), intOr(asset.Height)
	}

	gray, err := r.Images.Sample(ctx, src, overlay, imagehash.SampleEdge)
	if err != nil {
		return err
	}
	hashes, err := imagehash.Compute(gray)
	if err != nil {
		return err
	}
	sig.Difference, sig.Perceptual = hashes.Difference, hashes.Perceptual
	return nil
}

// clipSignature walks a video and hashes merge.FrameCount frames from along its
// length.
//
// The whole-picture hashes come from the middle of that same sequence rather
// than from the poster thumbnail the metadata job already built, which is the
// less obvious of the two available answers and the right one. The poster is a
// square *crop*; every hash in this feature is taken from a square *squash*.
// Mixing the two would mean a video's cheap pre-filter hash and its sampled
// frames described the picture in two different geometries, and the pre-filter
// would then reject clips the sequence was about to match.
//
// A clip that cannot be sampled is not a failure. Its row is written with no
// frames and no hashes, and merge.Duplicates refuses to compare a video that
// has none — so an unsampled clip is simply never anybody's duplicate. Parking
// the job instead would mark a perfectly good archived video as broken over a
// question nobody asked.
func (r *Runner) clipSignature(ctx context.Context, asset db.Asset, src string, sig *db.Signature) error {
	if r.Video == nil {
		return nil
	}
	info, err := r.Video.Probe(ctx, src)
	if err != nil {
		r.log().Warn("could not probe a video to sample it; it will not be compared to any other",
			"asset", asset.ID, "error", err)
		return nil
	}

	frames, err := r.Video.SampleFrames(ctx, src, merge.FrameCount, imagehash.SampleEdge, info)
	if err != nil {
		r.log().Warn("could not sample a video; it will not be compared to any other",
			"asset", asset.ID, "error", err)
		return nil
	}

	for _, frame := range frames {
		h, err := imagehash.Compute(frame)
		if err != nil {
			return err
		}
		sig.FrameDifference = append(sig.FrameDifference, h.Difference)
		sig.FramePerceptual = append(sig.FramePerceptual, h.Perceptual)
	}
	if n := len(sig.FrameDifference); n > 0 {
		sig.Difference = sig.FrameDifference[n/2]
		sig.Perceptual = sig.FramePerceptual[n/2]
	}
	return nil
}
