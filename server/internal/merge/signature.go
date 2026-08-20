package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/imagehash"
)

// FrameCount is how many frames a video is reduced to.
//
// Twenty, spread evenly across the running time. It is enough that a clip and a
// re-encode of it agree on all twenty while two different clips of the same
// room do not — the whole discriminating power here is in the *sequence*, and
// twenty samples of one is a far stronger statement than any single frame.
// Beyond that the marginal frame buys very little and costs a row of eight
// bytes and a little decoding on seven thousand videos.
const FrameCount = 20

// SignatureVersion stamps every stored signature with the recipe that produced
// it, so that changing the recipe makes the archive's signatures stale rather
// than wrong.
//
// It is not imagehash.Version on its own. That number moves when a bit moves;
// this one also has to move when the *sampling* changes, because twenty frames
// and thirty frames are not comparable sequences however the frames themselves
// were hashed. Anything that changes what a stored signature means belongs in
// this arithmetic.
const SignatureVersion = imagehash.Version*1000 + FrameCount

// Fingerprint identifies a group by its members, so that a scan run twice
// recognises the question it already asked.
//
// Sorted before hashing, because the *set* is the identity: a segment group
// carries an order and the same six pieces in the same order are the same
// group, but so are the same six pieces the scan happened to walk differently
// after a reindex. The kind is included so that a hypothetical pair of assets
// found both alike and consecutive would be two questions rather than one.
func Fingerprint(kind string, ids []string) string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)

	sum := sha256.Sum256([]byte(kind + "\x00" + strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:])
}
