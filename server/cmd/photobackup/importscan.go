package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/takeout"
)

// importItem is one media file on its way into the archive, with everything the
// export could tell us about it gathered up beforehand.
type importItem struct {
	path     string
	localID  string // the path relative to the export root
	filename string
	size     int64
	modified time.Time

	isVideo   bool
	contentID string

	// source is the import source this item is described under, and it is what
	// says how the archive should read sidecar. Empty means a Google Takeout,
	// which is the importer this type was written for and still the only one
	// that fills in albums or reads a content identifier.
	source string

	// sidecar is the item's Google JSON, verbatim, or nil. A Live Photo's video
	// half never has one of its own and borrows the still's — see
	// inheritSidecars.
	sidecar json.RawMessage
	takenAt *time.Time
	albums  []takeout.Album
	trashed bool

	// overlayItem is the drawn-on layer this item was exported with, and
	// overlaySHA256 is what that layer was archived as once it has been sent.
	// Only a Snapchat memory has either; see snapchatscan.go.
	//
	// Two fields rather than one because they are known at different times:
	// the pairing comes out of the scan, and the hash only exists after the
	// overlay's bytes are in the archive.
	overlayItem   *importItem
	overlaySHA256 string

	md5     string
	status  string
	assetID string
	// sha256 is what the archive stored the bytes under, learned from the
	// upload's answer. It is how one item names another to the server, since
	// asset ids are the database's and the hash is the file's.
	sha256 string
}

// mediaFile is one candidate file, carrying where it sits inside the export
// rather than only where it sits on this disk.
//
// The two differ as soon as an export arrives as several zips, which is how
// Google delivers anything large. Every path an importer reasons about — which
// sidecar describes this file, which album it is in, what local id it keeps
// across runs — is relative to the top of the export, and unzipping into six
// directories changes only the part in front of that.
type mediaFile struct {
	path string
	// rel is the path from the root it was found under, slash-separated. It is
	// the item's identity: the same file is the same rel whether the export was
	// unzipped into one directory or six.
	rel string
	// relDir is rel's directory, "." at the top of the export. Sidecars and
	// albums are grouped by it, so a sidecar in one delivery finds the media
	// file in another.
	relDir string
}

// scanResult is an export, read.
type scanResult struct {
	items []*importItem
	// pairs is how many stills were matched to a video within this export,
	// reported because it is the number the whole exercise exists to make
	// non-zero.
	pairs int
	// orphanVideos are paired videos whose still is not in this export. They
	// import as ordinary videos and pair themselves later if the still ever
	// turns up.
	orphanVideos int
	// described is how many items found a sidecar.
	described int
	// unmatchedSidecars are sidecars whose media file could not be found, which
	// is the signal that the filename rules have drifted again.
	//
	// Their contents travel with them. Reporting the filename and dropping the
	// body was the wrong half to keep: the name says a rule needs fixing, and
	// the body is the caption, the people, the coordinates and the capture time
	// for a photograph that exists somewhere — and the export it was read from
	// is deleted long before anyone gets to the rule.
	unmatchedSidecars []unmatchedSidecar
	albums            []string
	skippedTrash      int
	// duplicatePaths counts files that appear at the same place in more than one
	// delivery of the same export. The first one read wins; the rest are the
	// same item arriving twice.
	duplicatePaths int
}

// unmatchedSidecar is a sidecar that describes no file this scan could find,
// kept whole so the archive can hold the evidence rather than a filename.
type unmatchedSidecar struct {
	// locator is where it sat inside the export, which is its identity across
	// deliveries and across re-runs.
	locator string
	raw     json.RawMessage
}

// scanExport reads an export into an ordered list of uploads.
//
// It takes several roots because Google delivers a large export as a stack of
// zips, and it splits an item from its sidecar across them freely: in the real
// export, `20180116_000028.mp4` is in the sixth zip and the JSON describing it
// is in the first. Read one directory at a time, 5,979 of that export's 11,282
// sidecars describe a file that is not there, and the metadata in them is lost
// the moment the export is deleted. Read together, none are. So the unit is the
// export, not the directory, and every root is a delivery of the same one.
//
// The order is the point of doing this as one pass rather than streaming: every
// still is uploaded before any video, so that when a paired video's row is
// inserted the still it belongs to is already there and the pairing resolves in
// that same transaction. Getting it wrong is survivable — the metadata worker
// resolves late pairings too — but it costs a re-run of the video's derivatives,
// and on a library that is a third Live Photos that is a great deal of ffmpeg.
func scanExport(ctx context.Context, exif *exifdata.Reader, roots []string, includeTrash bool) (scanResult, error) {
	var result scanResult

	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		cleaned = append(cleaned, filepath.Clean(root))
	}

	files, sidecarsByDir, albumsByDir, duplicates, err := walkExport(cleaned)
	if err != nil {
		return result, err
	}
	result.duplicatePaths = duplicates

	// One exiftool per root. It is what says which files are media at all,
	// which are video, and which carry a content identifier.
	identified := make(map[string]exifdata.Scanned, len(files))
	for _, root := range cleaned {
		err := exif.ScanTree(ctx, root, func(s exifdata.Scanned) error {
			identified[filepath.Clean(s.Path)] = s
			return nil
		})
		if err != nil {
			return result, fmt.Errorf("read metadata under %s: %w", root, err)
		}
	}

	for _, file := range files {
		scanned, ok := identified[file.path]
		if !ok || !isMedia(scanned.MIMEType) {
			// Not something this archive stores: the sidecars themselves, the
			// export's HTML, anything exiftool could not identify.
			continue
		}
		info, err := os.Stat(file.path)
		if err != nil {
			return result, err
		}

		item := &importItem{
			path:      file.path,
			localID:   file.rel,
			filename:  filepath.Base(file.path),
			size:      info.Size(),
			modified:  info.ModTime().UTC(),
			isVideo:   scanned.IsVideo(),
			contentID: scanned.ContentID,
			albums:    albumsByDir[file.relDir],
		}
		if raw, ok := sidecarsByDir[file.relDir][item.filename]; ok {
			item.attachSidecar(raw)
			delete(sidecarsByDir[file.relDir], item.filename)
		}
		result.items = append(result.items, item)
	}

	result.pairs, result.orphanVideos = inheritSidecars(result.items)

	for relDir, remaining := range sidecarsByDir {
		for name, raw := range remaining {
			result.unmatchedSidecars = append(result.unmatchedSidecars,
				unmatchedSidecar{locator: path.Join(relDir, name), raw: raw})
		}
	}
	sort.Slice(result.unmatchedSidecars, func(i, j int) bool {
		return result.unmatchedSidecars[i].locator < result.unmatchedSidecars[j].locator
	})

	kept := result.items[:0]
	for _, item := range result.items {
		if item.trashed && !includeTrash {
			result.skippedTrash++
			continue
		}
		if item.sidecar != nil {
			result.described++
		}
		kept = append(kept, item)
	}
	result.items = kept

	seen := map[string]bool{}
	for _, item := range result.items {
		for _, album := range item.albums {
			if !seen[album.Title] {
				seen[album.Title] = true
				result.albums = append(result.albums, album.Title)
			}
		}
	}
	sort.Strings(result.albums)

	sortForPairing(result.items)
	return result, nil
}

// importSource is what the archive is told this item's sidecar is written in.
//
// The zero value means a Google Takeout rather than "unspecified", because this
// importer predates there being a second kind and every item it builds without
// saying otherwise is one.
func (it *importItem) importSource() string {
	if it.source == "" {
		return db.SourceGoogleTakeout
	}
	return it.source
}

func (it *importItem) attachSidecar(raw json.RawMessage) {
	it.sidecar = raw
	meta, err := takeout.Normalize(raw)
	if err != nil {
		// The sidecar stays attached regardless: the server parses it too, and
		// it is the server's copy that ends up in the archive. What is lost
		// here is only the capture time this run would have sent as a header.
		return
	}
	it.takenAt = meta.TakenAt
	it.trashed = meta.Trashed
}

// walkExport collects the media files, the item sidecars, and the albums the
// directory layout implies — all of them keyed by the directory's place inside
// the export rather than on disk, so several deliveries of one export read as
// one export.
func walkExport(roots []string) (
	files []mediaFile,
	sidecars map[string]map[string]json.RawMessage,
	albums map[string][]takeout.Album,
	duplicates int,
	err error,
) {
	sidecars = make(map[string]map[string]json.RawMessage)
	albums = make(map[string][]takeout.Album)

	// Two passes over each directory, because a sidecar can only be matched to
	// a media file once the directory's media filenames are all known — the
	// truncation rules resolve by looking for a name that is actually there.
	// The first pass gathers every root, so the second sees a directory whole
	// even when its contents were split across zips.
	type contents struct {
		media map[string]string // filename -> the absolute path it was found at
		jsons map[string]string // filename -> the absolute path it was found at
	}
	byRelDir := make(map[string]*contents)
	at := func(relDir string) *contents {
		c, ok := byRelDir[relDir]
		if !ok {
			c = &contents{media: map[string]string{}, jsons: map[string]string{}}
			byRelDir[relDir] = c
		}
		return c
	}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") && p != root {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return nil
			}

			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			relDir := path.Dir(filepath.ToSlash(rel))
			c := at(relDir)

			if strings.EqualFold(filepath.Ext(d.Name()), ".json") {
				if _, seen := c.jsons[d.Name()]; !seen {
					c.jsons[d.Name()] = p
				}
				return nil
			}
			if _, seen := c.media[d.Name()]; seen {
				// The same item at the same place in two deliveries. Every
				// observed instance is the identical file zipped twice, and
				// keeping both would give two uploads one local id.
				duplicates++
				return nil
			}
			c.media[d.Name()] = p
			files = append(files, mediaFile{path: p, rel: filepath.ToSlash(rel), relDir: relDir})
			return nil
		})
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("walk %s: %w", root, err)
		}
	}

	for relDir, c := range byRelDir {
		media := make(map[string]bool, len(c.media))
		for name := range c.media {
			media[name] = true
		}

		if album, ok := albumFor(relDir, c.jsons, len(media) > 0); ok {
			albums[relDir] = []takeout.Album{album}
		}

		// In name order, because matching is now order-dependent: the first
		// sidecar to claim a media file keeps it, and which one that is must not
		// come out of Go's map iteration.
		jsonNames := make([]string, 0, len(c.jsons))
		for name := range c.jsons {
			jsonNames = append(jsonNames, name)
		}
		sort.Strings(jsonNames)

		matched := make(map[string]json.RawMessage)
		for _, name := range jsonNames {
			if takeout.IsDirectoryJSON(name) {
				continue
			}
			raw, err := os.ReadFile(c.jsons[name])
			if err != nil {
				return nil, nil, nil, 0, err
			}
			target, ok := matchSidecar(name, raw, media, matched)
			if !ok {
				// Recorded under its own name so the caller can report it.
				matched[name] = raw
				continue
			}
			matched[target] = raw
		}
		sidecars[relDir] = matched
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, sidecars, albums, duplicates, nil
}

// matchSidecar decides which media file a sidecar describes.
//
// The title inside the sidecar is tried first because it is usually the only
// part of this that is data rather than an artifact: it is the filename Google
// Photos actually held the item under. The name-derived candidates come second,
// and a prefix match last, for the exports where the media name itself was
// truncated to fit the sidecar's length cap.
//
// The "usually" is the whole point of this function's shape. Two items in one
// folder with the same title get one media file each — `IMG_1.HEIC` and
// `IMG_1(1).HEIC` — but both of their sidecars say `"title": "IMG_1.HEIC"`,
// because the title is what Photos held and the counter is what the export
// invented to keep the names apart. Believe the title there and both sidecars
// claim `IMG_1.HEIC`: one overwrites the other, and `IMG_1(1).HEIC` imports with
// no capture time, no coordinates, no people and no album. In the sample export
// that is 119 files, and it is silent, because from the outside every sidecar
// found a home. So a counter in the sidecar's own name disqualifies the title
// entirely — it is the only record of which of the two items this is.
//
// taken is what the sidecars already read in this directory have claimed. A
// media file is described by exactly one sidecar, so a second claim on the same
// file is always a mistake — and letting it through does double damage, since it
// drops the document already sitting there as well as mis-attaching this one.
func matchSidecar(sidecarName string, raw []byte, media map[string]bool, taken map[string]json.RawMessage) (string, bool) {
	free := func(name string) bool { return media[name] && taken[name] == nil }

	byName := takeout.MediaNameFor(sidecarName)
	byTitle := func() (string, bool) {
		// A counter does not merely outrank the title, it makes the title
		// unusable: it says this sidecar is the second item under a shared
		// title, so the title is the *other* sibling's filename, and following
		// it would take the file that sibling's own sidecar is entitled to.
		// Better to fall through and be reported than to be confidently wrong
		// about which photograph this is.
		if takeout.HasCollisionCounter(sidecarName) {
			return "", false
		}
		parsed, err := takeout.Parse(raw)
		if err != nil || !free(parsed.Title) {
			return "", false
		}
		return parsed.Title, true
	}

	if name, ok := byTitle(); ok {
		return name, true
	}
	for _, candidate := range byName {
		if free(candidate) {
			return candidate, true
		}
	}

	// Last resort. Only accepted when exactly one media file could be meant:
	// attaching one item's location and caption to another would be worse than
	// attaching them to nothing.
	stem := strings.TrimSuffix(sidecarName, ".json")
	var only string
	for name := range media {
		if !strings.HasPrefix(name, stem) || !free(name) {
			continue
		}
		if only != "" {
			return "", false
		}
		only = name
	}
	return only, only != ""
}

// albumFor decides whether a directory is an album, and what it is called.
//
// An export records album membership nowhere but in its directory structure, so
// this is the whole of the recovery. A directory qualifies when it holds media
// directly and is not one of the chronological buckets loose photos are filed
// into. Its metadata.json names it when there is one; the directory name does
// when there is not.
func albumFor(relDir string, jsons map[string]string, hasMedia bool) (takeout.Album, bool) {
	if relDir == "." || !hasMedia {
		return takeout.Album{}, false
	}
	name := path.Base(relDir)
	if takeout.IsDatedFolder(name) {
		return takeout.Album{}, false
	}

	for jsonName, at := range jsons {
		if !takeout.IsAlbumDescriptor(jsonName) {
			continue
		}
		raw, err := os.ReadFile(at)
		if err != nil {
			break
		}
		var album takeout.Album
		if err := json.Unmarshal(raw, &album); err == nil && strings.TrimSpace(album.Title) != "" {
			return album, true
		}
		break
	}
	return takeout.Album{Title: name}, true
}

// inheritSidecars gives a Live Photo's video half the metadata Google wrote
// against the still.
//
// Google exports a Live Photo as two files and one sidecar, because to Google
// it is one item. Without this the video would arrive with no capture time, no
// coordinates, and no album — which matters exactly when the pairing does not
// resolve and the video has to stand on its own as an item in the timeline.
//
// It reports how many complete pairs the export holds and how many videos are
// halves of a pair whose still is somewhere else.
func inheritSidecars(items []*importItem) (pairs, orphanVideos int) {
	stills := make(map[string]*importItem)
	for _, item := range items {
		if item.contentID == "" || item.isVideo {
			continue
		}
		if existing, ok := stills[item.contentID]; !ok || item.path < existing.path {
			stills[item.contentID] = item
		}
	}

	paired := make(map[string]bool)
	for _, item := range items {
		if item.contentID == "" || !item.isVideo {
			continue
		}
		still, ok := stills[item.contentID]
		if !ok {
			orphanVideos++
			continue
		}
		if !paired[item.contentID] {
			paired[item.contentID] = true
			pairs++
		}
		if item.sidecar == nil && still.sidecar != nil {
			item.attachSidecar(still.sidecar)
			item.albums = still.albums
		}
	}
	return pairs, orphanVideos
}

// sortForPairing puts every still ahead of every video. Within each half the
// order is the path, so a run is reproducible and a resumed one covers the
// same ground in the same sequence.
func sortForPairing(items []*importItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].isVideo != items[j].isVideo {
			return !items[i].isVideo
		}
		return items[i].path < items[j].path
	})
}

func isMedia(mimeType string) bool {
	mimeType = strings.ToLower(mimeType)
	return strings.HasPrefix(mimeType, "image/") || strings.HasPrefix(mimeType, "video/")
}

// hashItem fills in the md5 the server checks against, reading the file once.
func hashItem(it *importItem) error {
	f, err := os.Open(it.path)
	if err != nil {
		return err
	}
	defer f.Close()

	sum := md5.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	it.md5 = hex.EncodeToString(sum.Sum(nil))
	return nil
}
