package db

import (
	"context"
	"fmt"
)

// Where an archived asset presently is, from the point of view of somebody
// asking whether it is worth uploading again.
const (
	// ContentInLibrary is the ordinary answer: the photograph is on the timeline.
	ContentInLibrary = "library"
	// ContentInTrash means it is in Recently Deleted and comes back if it is
	// restored, so sending the bytes again is not how you get it back.
	ContentInTrash = "trash"
	// ContentInVault means the content is archived but the row no longer describes
	// it. Deliberately not the bucket: see ArchivedContent.
	ContentInVault = "vault"
	// ContentPurged means the archive held these bytes and destroyed them on purpose.
	ContentPurged = "purged"
)

// ArchivedContent is what the archive already holds under one digest.
//
// Filename is empty for anything but a library or trash match, because a
// vaulted row has had its filename encrypted away and a purged one was never
// more than a content key.
type ArchivedContent struct {
	SHA256   string
	AssetID  string
	Filename string
	Where    string
}

// LookupContent answers "have I got this already?" for a list of sha256
// digests, which is the only question a browser about to send a file can ask
// cheaply — it has the bytes, so it can produce the archive's own content key
// without the archive having to receive them first.
//
// A vault match is reported as ContentInVault and never as which bucket. That the
// answer is given at all is the leak the vault scrub already accepts by design
// — see the note above `scrubbed` in vault.go: the digests stay so that
// sync/check cannot be tricked into re-accepting a photograph somebody hid, and
// the cost is that a person holding a file can test whether it is in there.
// Naming the bucket would be a second disclosure with nothing to buy it.
func (s *Store) LookupContent(ctx context.Context, shas []string) (map[string]ArchivedContent, error) {
	out := make(map[string]ArchivedContent, len(shas))
	if len(shas) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		select sha256, id::text, original_filename, vault, deleted_at is not null
		from assets
		where sha256 = any($1::text[])`, shas)
	if err != nil {
		return nil, fmt.Errorf("look up archived digests: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m ArchivedContent
		var vault string
		var deleted bool
		if err := rows.Scan(&m.SHA256, &m.AssetID, &m.Filename, &vault, &deleted); err != nil {
			return nil, fmt.Errorf("scan archived digest: %w", err)
		}
		switch {
		case vault != "":
			m.Where, m.Filename, m.AssetID = ContentInVault, "", ""
		case deleted:
			m.Where = ContentInTrash
		default:
			m.Where = ContentInLibrary
		}
		out[m.SHA256] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Asked second and recorded only where nothing live answered. A purge
	// removes the asset row, so the two sets do not overlap today; the guard is
	// here because "the archive still has it" is the more useful answer of the
	// two and should win if they ever do.
	purged, err := s.IsPurged(ctx, shas)
	if err != nil {
		return nil, err
	}
	for sha := range purged {
		if _, live := out[sha]; !live {
			out[sha] = ArchivedContent{SHA256: sha, Where: ContentPurged}
		}
	}
	return out, nil
}
