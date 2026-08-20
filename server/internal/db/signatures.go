package db

import (
	"context"
	"fmt"

	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

// Signature is what the signature job produces for one asset: the numbers
// internal/merge compares, on their way into the row that holds them.
//
// It is not merge.Signature. That one carries the file's size and age as well,
// because ranking a group needs them, and those come off the asset row rather
// than out of a decoder — the job that writes this one has no business
// restating them.
type Signature struct {
	Version    int
	Difference uint64
	Perceptual uint64
	Aspect     float64
	// FrameDifference and FramePerceptual are a video sampled along its length.
	// Empty for a still.
	FrameDifference []uint64
	FramePerceptual []uint64
}

// PutSignature writes what an asset looks like, replacing whatever was there.
//
// An upsert rather than an insert because the version is the whole point: a
// changed algorithm requeues every asset in the archive, and each of those jobs
// arrives at a row that already exists.
//
// Postgres has no unsigned integers, so every hash crosses this boundary
// reinterpreted as a signed one. Nothing downstream cares — the only operations
// these ever undergo are XOR and a population count, and both are blind to how
// the top bit is read — but the cast has to be on purpose in both directions or
// half the archive's signatures silently become negative and stay that way.
func (s *Store) PutSignature(ctx context.Context, assetID string, sig Signature) error {
	const upsert = `
		insert into asset_signatures
		    (asset_id, version, dhash, phash, aspect, frame_dhashes, frame_phashes, computed_at)
		values ($1::uuid, $2, $3, $4, $5, $6, $7, now())
		on conflict (asset_id) do update set
		    version = excluded.version,
		    dhash = excluded.dhash,
		    phash = excluded.phash,
		    aspect = excluded.aspect,
		    frame_dhashes = excluded.frame_dhashes,
		    frame_phashes = excluded.frame_phashes,
		    computed_at = excluded.computed_at`

	_, err := s.pool.Exec(ctx, upsert,
		assetID, sig.Version,
		int64(sig.Difference), int64(sig.Perceptual), sig.Aspect,
		signed(sig.FrameDifference), signed(sig.FramePerceptual))
	if err != nil {
		return fmt.Errorf("store signature for %s: %w", assetID, err)
	}
	return nil
}

func signed(in []uint64) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		out[i] = int64(v)
	}
	return out
}

func unsigned(in []int64) []uint64 {
	out := make([]uint64, len(in))
	for i, v := range in {
		out[i] = uint64(v)
	}
	return out
}

// scannable is the predicate every read in this file shares: an asset that is
// in the library, is an item rather than a component of one, and has something
// on disk to have been read.
//
// The vault is excluded here rather than filtered later, and it is the one
// exclusion that is not about tidiness. A signature is a description of a
// photograph. Computing one for something in the vault would be this server
// writing down what the picture looks like, which is precisely the thing the
// vault exists to stop it knowing — see internal/vault. An asset on its way in
// has its signature deleted with the rest of its plaintext.
const scannable = visibleAssets

// SignaturesForScan reads every signature the duplicate scan should consider.
//
// Members of a pending video-segment group are excluded. Those are about to
// become one file and stop existing separately, and offering somebody the six
// pieces of one recording as "duplicates" would be asking them to approve
// throwing away five sixths of a minute of video. It is the one place the two
// halves of this feature have to know about each other, and it is a join.
func (s *Store) SignaturesForScan(ctx context.Context) ([]merge.Signature, error) {
	const query = `
		select a.id, a.media_kind, sig.dhash, sig.phash, sig.aspect,
		       sig.frame_dhashes, sig.frame_phashes,
		       coalesce(a.duration_seconds, 0), coalesce(a.width, 0), coalesce(a.height, 0),
		       a.byte_size, a.sort_time
		from assets a
		join asset_signatures sig on sig.asset_id = a.id
		where ` + scannable + `
		  and sig.version = $1
		  and not exists (
		      select 1 from merge_members m
		      join merge_groups g on g.id = m.group_id
		      where m.asset_id = a.id and g.kind = $2 and g.state = 'pending'
		  )`

	rows, err := s.pool.Query(ctx, query, merge.SignatureVersion, merge.KindSegments)
	if err != nil {
		return nil, fmt.Errorf("read signatures: %w", err)
	}
	defer rows.Close()

	var out []merge.Signature
	for rows.Next() {
		var sig merge.Signature
		var dhash, phash int64
		var frameD, frameP []int64
		if err := rows.Scan(&sig.ID, &sig.MediaKind, &dhash, &phash, &sig.Aspect,
			&frameD, &frameP,
			&sig.DurationSeconds, &sig.Width, &sig.Height, &sig.ByteSize, &sig.SortTime); err != nil {
			return nil, fmt.Errorf("read signatures: %w", err)
		}
		sig.Difference, sig.Perceptual = uint64(dhash), uint64(phash)
		sig.FrameDifference, sig.FramePerceptual = unsigned(frameD), unsigned(frameP)
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read signatures: %w", err)
	}
	return out, nil
}

// SignatureCoverage is how much of the library the scan can actually see.
//
// Reported rather than assumed, because "no duplicates found" and "no
// signatures computed yet" look identical on a review page and mean opposite
// things. The overview says which one it is.
type SignatureCoverage struct {
	Assets int64 `json:"assets"`
	Signed int64 `json:"signed"`
}

func (s *Store) SignatureCoverage(ctx context.Context) (SignatureCoverage, error) {
	const query = `
		select count(*)::bigint,
		       count(sig.asset_id) filter (where sig.version = $1)::bigint
		from assets a
		left join asset_signatures sig on sig.asset_id = a.id
		where ` + scannable

	var out SignatureCoverage
	if err := s.pool.QueryRow(ctx, query, merge.SignatureVersion).Scan(&out.Assets, &out.Signed); err != nil {
		return SignatureCoverage{}, fmt.Errorf("count signature coverage: %w", err)
	}
	return out, nil
}

// SegmentCandidates reads the clips the split-video scan runs over.
//
// Narrow on purpose, and every term in the predicate is load-bearing:
//
//   - Snapchat's memories half. Chat media has no history document, so its
//     capture times come from file modification times that a whole zip shares —
//     470 of 1,213 chat videos claim an instant another one also claims, and
//     ten-second spacing among them means nothing at all.
//   - A capture time that came from the history document. That is the number
//     that is exactly ten seconds apart between consecutive pieces. The EXIF
//     time on the same files drifts by up to eighty seconds across one
//     recording, which is why this reads captured_at and not sort_time.
//   - Not already merged. A group that has been resolved has its pieces in the
//     trash, and `scannable` excludes those; this is about the ones that were
//     dismissed, which stay in the library and must not be proposed again.
func (s *Store) SegmentCandidates(ctx context.Context) ([]merge.VideoSegment, error) {
	const query = `
		select a.id, a.captured_at, a.duration_seconds,
		       coalesce(a.width, 0), coalesce(a.height, 0)
		from assets a
		where ` + scannable + `
		  and a.media_kind = 'video'
		  and a.import_source = $1
		  and a.captured_at is not null
		  and a.duration_seconds is not null
		  and a.import_metadata->>'kind' = 'memories'
		  and a.import_metadata->>'capturedAtSource' = 'history'
		  and not exists (
		      select 1 from merge_members m
		      join merge_groups g on g.id = m.group_id
		      where m.asset_id = a.id and g.kind = $2 and g.state = 'dismissed'
		  )`

	rows, err := s.pool.Query(ctx, query, SourceSnapchat, merge.KindSegments)
	if err != nil {
		return nil, fmt.Errorf("read segment candidates: %w", err)
	}
	defer rows.Close()

	var out []merge.VideoSegment
	for rows.Next() {
		var v merge.VideoSegment
		if err := rows.Scan(&v.ID, &v.At, &v.DurationSeconds, &v.Width, &v.Height); err != nil {
			return nil, fmt.Errorf("read segment candidates: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read segment candidates: %w", err)
	}
	return out, nil
}
