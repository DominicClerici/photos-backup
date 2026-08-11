package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// A Live Photo is two files, and this file is everything that decides they are
// two halves of one thing.
//
// There are two independent kinds of evidence, and the archive needs both.
//
//	the declaration    The phone names the still's local id on the video's
//	                   upload. It is known before a byte is sent, so a pairing
//	                   is complete the moment the second half commits. It is
//	                   also unavailable to anything that is not the phone.
//
//	the content id     Apple stamps a UUID into both halves at capture. It
//	                   survives a Google Takeout round trip, so it pairs an
//	                   export, a restored backup, and a file copied off a Mac —
//	                   everything the declaration cannot reach. It is not known
//	                   until something has read the file's metadata, which for
//	                   an upload is after the bytes are already committed.
//
// Neither subsumes the other, so both run, and both are written to the same two
// columns. Nothing downstream has to know which one paired a given asset.

// linkLivePair joins a Live Photo's two halves from the phone's declaration,
// from whichever of them just landed.
//
// Both directions are needed because either can arrive first. The phone queues
// the still and its paired video with the same capture time, and the queue
// orders by capture time, so which of the two goes up first is not decided
// anywhere — and a run interrupted between them can deliver them minutes apart.
//
// Nothing here fails a commit. A pairing that does not resolve costs a hover
// animation; refusing the upload over it would cost the photo.
func linkLivePair(ctx context.Context, tx pgx.Tx, a Asset, id, liveParent string) error {
	if liveParent != "" {
		// The video: find the still, if the archive has it.
		const link = `
			update assets set live_parent_asset_id = d.asset_id
			from device_assets d
			where assets.id = $1::uuid
			  and assets.live_parent_asset_id is null
			  and d.device_id = $2 and d.local_id = $3`
		if _, err := tx.Exec(ctx, link, id, a.DeviceID, liveParent); err != nil {
			return fmt.Errorf("link paired video to its still: %w", err)
		}
		return nil
	}

	if a.DeviceID == "" || a.LocalID == "" {
		return nil
	}

	// The still: adopt any video that was already waiting for it.
	const adopt = `
		update assets set live_parent_asset_id = $1::uuid
		where device_id = $2
		  and live_parent_local_id = $3
		  and live_parent_asset_id is null`
	if _, err := tx.Exec(ctx, adopt, id, a.DeviceID, a.LocalID); err != nil {
		return fmt.Errorf("link still to its paired video: %w", err)
	}
	return nil
}

// resolveByContentID joins the two halves by the identifier they both carry,
// from whichever of them the caller just touched. It returns the ids of videos
// that became paired as a result and had already been through the derivative
// pipeline, which are the ones that now need putting back through it.
//
// The video side takes the first still archived under the identifier rather
// than failing on ambiguity. More than one is a real case — the sample export
// holds a HEIC and a JPEG re-export of the same capture, both stamped with the
// same UUID — and refusing to pair would lose the motion on a photo that
// plainly has some. Picking deterministically means the choice does not change
// under a reindex.
func resolveByContentID(ctx context.Context, tx pgx.Tx, id, contentID, mediaKind string) ([]string, error) {
	if contentID == "" {
		return nil, nil
	}

	// A row that reached here from a duplicate upload was not written by the
	// insert above it. Filling the gap and not overwriting is deliberate: the
	// value already there was read off the file, and this one was only claimed
	// by a client.
	const remember = `update assets set content_id = $2 where id = $1::uuid and content_id = ''`
	if _, err := tx.Exec(ctx, remember, id, contentID); err != nil {
		return nil, fmt.Errorf("record content id: %w", err)
	}

	if mediaKind == MediaVideo {
		const link = `
			update assets v
			set live_parent_asset_id = still.id,
			    live_state = case when v.live_state = 'none' then 'pending' else v.live_state end
			from (
				select id from assets
				where content_id = $2 and media_kind = $3
				order by uploaded_at, id
				limit 1
			) still
			where v.id = $1::uuid and v.live_parent_asset_id is null`
		if _, err := tx.Exec(ctx, link, id, contentID, MediaImage); err != nil {
			return nil, fmt.Errorf("pair video to its still by content id: %w", err)
		}
		return nil, nil
	}

	// The still: adopt every unpaired video carrying the same identifier.
	//
	// derived_state comes back because it decides what happens next. A video
	// whose metadata job has not run yet will see the pairing when it does, and
	// needs nothing. One that already ran was treated as an ordinary video —
	// it has a poster frame and no motion rendition — and has to run again.
	const adopt = `
		update assets v
		set live_parent_asset_id = $1::uuid,
		    live_state = case when v.live_state = 'none' then 'pending' else v.live_state end
		where v.content_id = $2
		  and v.media_kind = $3
		  and v.live_parent_asset_id is null
		  and v.id <> $1::uuid
		returning v.id::text, v.derived_state`

	rows, err := tx.Query(ctx, adopt, id, contentID, MediaVideo)
	if err != nil {
		return nil, fmt.Errorf("pair still to its videos by content id: %w", err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var videoID, derivedState string
		if err := rows.Scan(&videoID, &derivedState); err != nil {
			return nil, fmt.Errorf("scan adopted video: %w", err)
		}
		if derivedState != DerivedPending {
			stale = append(stale, videoID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read adopted videos: %w", err)
	}
	return stale, nil
}

// SetContentID records the identifier the metadata worker read off an original,
// pairs whatever it resolves to, and returns the asset as it now stands.
//
// The file wins over anything a client declared, which is the point of doing
// this here at all: a header can be wrong or absent, and the bytes cannot.
//
// requeued names videos this call turned into paired videos after they had
// already been through the pipeline as ordinary ones. Their metadata jobs are
// reset here; the caller only has to wake a worker.
func (s *Store) SetContentID(ctx context.Context, assetID, contentID string) (asset Asset, requeued []string, err error) {
	contentID = exifdata.NormalizeContentID(contentID)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Asset{}, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Replacing the identifier invalidates whatever it paired. The clause that
	// keeps this safe is live_parent_local_id = '': a phone's declaration is
	// evidence in its own right and survives, while a pairing that rested
	// entirely on an identifier the file turns out not to carry does not. It is
	// the only way an asset that was wrongly hidden gets back onto the timeline.
	const unpair = `
		update assets set live_parent_asset_id = null, live_state = 'none'
		where id = $1::uuid
		  and content_id <> $2
		  and live_parent_local_id = ''
		  and live_parent_asset_id is not null`
	if _, err := tx.Exec(ctx, unpair, assetID, contentID); err != nil {
		return Asset{}, nil, fmt.Errorf("clear stale pairing on %s: %w", assetID, err)
	}

	const update = `
		update assets set content_id = $2 where id = $1::uuid
		returning media_kind`
	var mediaKind string
	if err := tx.QueryRow(ctx, update, assetID, contentID).Scan(&mediaKind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, nil, ErrNotFound
		}
		return Asset{}, nil, fmt.Errorf("record content id on %s: %w", assetID, err)
	}

	stale, err := resolveByContentID(ctx, tx, assetID, contentID, mediaKind)
	if err != nil {
		return Asset{}, nil, err
	}
	for _, videoID := range stale {
		// Requeue rather than Enqueue: these assets have a completed metadata
		// job already, and Enqueue would collide with it and do nothing.
		if err := jobs.Requeue(ctx, tx, jobs.KindMetadata, videoID); err != nil {
			return Asset{}, nil, err
		}
	}

	row := tx.QueryRow(ctx, `select `+assetColumns+` from assets where id = $1::uuid`, assetID)
	if asset, err = scanAsset(row); err != nil {
		return Asset{}, nil, fmt.Errorf("reload asset %s: %w", assetID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Asset{}, nil, fmt.Errorf("commit transaction: %w", err)
	}
	return asset, stale, nil
}

// LiveVideoFor returns the paired video belonging to a still, which is what the
// gallery's hover animation and the viewer's press-and-hold play.
func (s *Store) LiveVideoFor(ctx context.Context, stillID string) (Asset, error) {
	// Ordered and limited because two devices delivering the same still resolve
	// to one asset and can each attach their own copy of the video to it. Both
	// show the same three seconds; the first one archived is as good an answer
	// as the question has.
	row := s.pool.QueryRow(ctx, `select `+assetColumns+`
		from assets where live_parent_asset_id = $1::uuid
		order by uploaded_at, id limit 1`, stillID)
	a, err := scanAsset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, fmt.Errorf("load paired video: %w", err)
	}
	return a, nil
}
