package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// item is one file on its way to the archive, in the same shape the phone's
// SQLite queue holds a PhotoKit asset.
type item struct {
	path     string
	localID  string
	filename string
	size     int64
	md5      string

	capturedAt *time.Time
	modifiedAt *time.Time

	status  string
	assetID string
}

// scan walks a directory into a queue.
//
// The local id is the path relative to the root, which gives the same property
// PhotoKit's identifiers have: stable across runs, opaque to the server, and
// unique within one device.
func scan(root string, limit int) ([]*item, error) {
	var items []*item

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFile(d.Name()) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		modified := info.ModTime().UTC()

		items = append(items, &item{
			path:       path,
			localID:    filepath.ToSlash(rel),
			filename:   d.Name(),
			size:       info.Size(),
			capturedAt: capturedAt(path, modified),
			modifiedAt: &modified,
			status:     "pending",
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// skipFile drops everything that is not media a phone would have produced:
// Takeout's metadata sidecars, and the junk a Mac leaves in every directory.
func skipFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return strings.EqualFold(filepath.Ext(name), ".json")
}

// takeoutMeta is the part of a Google Takeout sidecar worth reading. The rest
// is album membership and view counts, which this archive has no concept of.
type takeoutMeta struct {
	PhotoTakenTime struct {
		Timestamp string `json:"timestamp"`
	} `json:"photoTakenTime"`
}

var numbered = regexp.MustCompile(`^(.*)\((\d+)\)(\.[^.]*)?$`)

// capturedAt reads the real capture time out of the Takeout sidecar, falling
// back to the file's modification time.
//
// The server extracts EXIF itself and prefers what it finds there, so this only
// has to be good enough to sort the assets whose EXIF is missing — which for an
// export of scanned and downloaded photos is a lot of them.
func capturedAt(path string, fallback time.Time) *time.Time {
	for _, candidate := range sidecarPaths(path) {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var meta takeoutMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		secs, err := strconv.ParseInt(meta.PhotoTakenTime.Timestamp, 10, 64)
		if err != nil || secs <= 0 {
			continue
		}
		t := time.Unix(secs, 0).UTC()
		return &t
	}
	return &fallback
}

// sidecarPaths lists where Takeout might have put the metadata for a file.
//
// The duplicate-name case is the awkward one: the second copy of a photo is
// `name(1).jpg`, but its sidecar is `name.jpg.supplemental-metadata(1).json` —
// the counter moves from the media name to the sidecar name.
func sidecarPaths(path string) []string {
	dir, name := filepath.Split(path)
	paths := []string{
		filepath.Join(dir, name+".supplemental-metadata.json"),
		filepath.Join(dir, name+".json"),
	}

	if m := numbered.FindStringSubmatch(name); m != nil {
		stem, counter, ext := m[1], m[2], m[3]
		paths = append(paths,
			filepath.Join(dir, fmt.Sprintf("%s%s.supplemental-metadata(%s).json", stem, ext, counter)),
			filepath.Join(dir, fmt.Sprintf("%s%s(%s).json", stem, ext, counter)),
		)
	}
	return paths
}

// hash computes the digest the phone would compute natively.
func hash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := md5.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
