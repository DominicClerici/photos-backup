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
	"path/filepath"
	"sort"
	"strings"
	"time"

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

	// sidecar is the item's Google JSON, verbatim, or nil. A Live Photo's video
	// half never has one of its own and borrows the still's — see
	// inheritSidecars.
	sidecar json.RawMessage
	takenAt *time.Time
	albums  []takeout.Album
	trashed bool

	md5     string
	status  string
	assetID string
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
	unmatchedSidecars []string
	albums            []string
	skippedTrash      int
}

// scanExport reads an export directory into an ordered list of uploads.
//
// The order is the point of doing this as one pass rather than streaming: every
// still is uploaded before any video, so that when a paired video's row is
// inserted the still it belongs to is already there and the pairing resolves in
// that same transaction. Getting it wrong is survivable — the metadata worker
// resolves late pairings too — but it costs a re-run of the video's derivatives,
// and on a library that is a third Live Photos that is a great deal of ffmpeg.
func scanExport(ctx context.Context, exif *exifdata.Reader, root string, includeTrash bool) (scanResult, error) {
	root = filepath.Clean(root)
	var result scanResult

	files, sidecarsByDir, albumsByDir, err := walkExport(root)
	if err != nil {
		return result, err
	}

	// One exiftool over the whole tree. It is what says which files are media
	// at all, which are video, and which carry a content identifier.
	identified := make(map[string]exifdata.Scanned, len(files))
	err = exif.ScanTree(ctx, root, func(s exifdata.Scanned) error {
		identified[filepath.Clean(s.Path)] = s
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("read metadata under %s: %w", root, err)
	}

	for _, path := range files {
		scanned, ok := identified[path]
		if !ok || !isMedia(scanned.MIMEType) {
			// Not something this archive stores: the sidecars themselves, the
			// export's HTML, anything exiftool could not identify.
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return result, err
		}

		dir := filepath.Dir(path)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return result, err
		}

		item := &importItem{
			path:      path,
			localID:   filepath.ToSlash(rel),
			filename:  filepath.Base(path),
			size:      info.Size(),
			modified:  info.ModTime().UTC(),
			isVideo:   scanned.IsVideo(),
			contentID: scanned.ContentID,
			albums:    albumsByDir[dir],
		}
		if raw, ok := sidecarsByDir[dir][item.filename]; ok {
			item.attachSidecar(raw)
			delete(sidecarsByDir[dir], item.filename)
		}
		result.items = append(result.items, item)
	}

	result.pairs, result.orphanVideos = inheritSidecars(result.items)

	for dir, remaining := range sidecarsByDir {
		for name := range remaining {
			result.unmatchedSidecars = append(result.unmatchedSidecars,
				filepath.Join(filepath.Base(dir), name))
		}
	}
	sort.Strings(result.unmatchedSidecars)

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

// walkExport collects the media paths, the item sidecars grouped by directory,
// and the albums the directory layout implies.
func walkExport(root string) (files []string, sidecars map[string]map[string]json.RawMessage, albums map[string][]takeout.Album, err error) {
	sidecars = make(map[string]map[string]json.RawMessage)
	albums = make(map[string][]takeout.Album)

	// Two passes over each directory, because a sidecar can only be matched to
	// a media file once the directory's media filenames are all known — the
	// truncation rules resolve by looking for a name that is actually there.
	byDir := make(map[string][]string)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], d.Name())
		return nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("walk %s: %w", root, err)
	}

	for dir, names := range byDir {
		media := make(map[string]bool, len(names))
		var jsons []string
		for _, name := range names {
			if strings.EqualFold(filepath.Ext(name), ".json") {
				jsons = append(jsons, name)
				continue
			}
			media[name] = true
			files = append(files, filepath.Join(dir, name))
		}

		if album, ok := albumFor(dir, root, jsons, len(media) > 0); ok {
			albums[dir] = []takeout.Album{album}
		}

		matched := make(map[string]json.RawMessage)
		for _, name := range jsons {
			if takeout.IsDirectoryJSON(name) {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, nil, nil, err
			}
			target, ok := matchSidecar(name, raw, media)
			if !ok {
				// Recorded under its own name so the caller can report it.
				matched[name] = raw
				continue
			}
			matched[target] = raw
		}
		sidecars[dir] = matched
	}

	sort.Strings(files)
	return files, sidecars, albums, nil
}

// matchSidecar decides which media file a sidecar describes.
//
// The title inside the sidecar is tried first because it is the only part of
// this that is data rather than an artifact: it is the filename Google Photos
// actually held the item under. The name-derived candidates come second, and a
// prefix match last, for the exports where the media name itself was truncated
// to fit the sidecar's length cap.
func matchSidecar(sidecarName string, raw []byte, media map[string]bool) (string, bool) {
	if parsed, err := takeout.Parse(raw); err == nil && media[parsed.Title] {
		return parsed.Title, true
	}
	for _, candidate := range takeout.MediaNameFor(sidecarName) {
		if media[candidate] {
			return candidate, true
		}
	}

	// Last resort. Only accepted when exactly one media file could be meant:
	// attaching one item's location and caption to another would be worse than
	// attaching them to nothing.
	stem := strings.TrimSuffix(sidecarName, ".json")
	var only string
	for name := range media {
		if !strings.HasPrefix(name, stem) {
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
func albumFor(dir, root string, jsons []string, hasMedia bool) (takeout.Album, bool) {
	if dir == root || !hasMedia {
		return takeout.Album{}, false
	}
	name := filepath.Base(dir)
	if takeout.IsDatedFolder(name) {
		return takeout.Album{}, false
	}

	for _, jsonName := range jsons {
		if !takeout.IsAlbumDescriptor(jsonName) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, jsonName))
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
