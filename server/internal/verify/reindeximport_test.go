package verify_test

import (
	"context"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

const pairContentID = "064002F7-7F1B-41FA-A07C-DB8B0D52E7FF"

// addImported archives a file the way `photobackup import` does: the asset line
// carries the content identifier read out of the export, and the sidecar
// follows on a metadata line of its own.
func (a *archive) addImported(t *testing.T, filename string, content []byte, contentID, sidecar string) db.Asset {
	t.Helper()
	asset := a.add(t, filename, content, captured)

	if contentID != "" {
		if _, _, err := a.store.SetContentID(context.Background(), asset.ID, contentID); err != nil {
			t.Fatalf("set content id: %v", err)
		}
		// The asset line the upload wrote holds it too, which is the half a
		// rebuild reads.
		a.rewriteWithContentID(t, asset, contentID)
	}
	if sidecar != "" {
		entry := manifest.Entry{
			Type:          manifest.KindMetadata,
			SHA256:        asset.SHA256,
			ImportSource:  db.SourceGoogleTakeout,
			ImportSidecar: []byte(sidecar),
			ImportAlbums:  []manifest.AlbumRef{{Title: "Iceland 2025"}},
			StoredAt:      time.Now().UTC(),
		}
		if err := a.manifest.Append(entry); err != nil {
			t.Fatalf("append metadata line: %v", err)
		}
		meta, err := db.ImportMetadataFrom(db.SourceGoogleTakeout, []byte(sidecar),
			[]db.AlbumRef{{Title: "Iceland 2025"}})
		if err != nil {
			t.Fatalf("ImportMetadataFrom: %v", err)
		}
		if err := a.store.ApplyImportMetadata(context.Background(), asset.ID, meta); err != nil {
			t.Fatalf("ApplyImportMetadata: %v", err)
		}
	}

	reloaded, err := a.store.Asset(context.Background(), asset.ID)
	if err != nil {
		t.Fatalf("reload asset: %v", err)
	}
	return reloaded
}

// rewriteWithContentID appends the asset line an import would have written,
// since the helper's own line predates knowing the identifier. A duplicate line
// for one digest is exactly what a re-uploaded file produces and the replay
// handles it the same way.
func (a *archive) rewriteWithContentID(t *testing.T, asset db.Asset, contentID string) {
	t.Helper()
	err := a.manifest.Append(manifest.Entry{
		SHA256: asset.SHA256, MD5: asset.MD5, Size: asset.ByteSize,
		Filename: asset.OriginalFilename, ContentType: asset.ContentType, Ext: asset.Ext,
		CapturedAt: asset.CapturedAt, DeviceID: asset.DeviceID, LocalID: asset.LocalID,
		ContentID: contentID, StoredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("append manifest: %v", err)
	}
}

const reindexSidecar = `{
  "title": "IMG_0001.HEIC",
  "description": "at the border",
  "photoTakenTime": { "timestamp": "1736125085" },
  "geoData": { "latitude": 41.7844, "longitude": -122.5848 },
  "favorited": true,
  "people": [{ "name": "Brody" }]
}`

// The recovery guarantee, for an archive that came from an export rather than
// from a phone. Everything an import contributed lives in the manifest or
// nowhere: the export directory is usually deleted the week after, so a rebuild
// that lost the pairing or the sidecars would be losing them permanently.
func TestReindexRestoresAnImportedLibrary(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()

	still := a.addImported(t, "IMG_0001.HEIC", fixture(t, "iphone-portrait.heic"), pairContentID, reindexSidecar)
	video := a.addImported(t, "IMG_0001.MOV", fixture(t, "clip.mov"), pairContentID, "")

	paired, err := a.store.Asset(ctx, video.ID)
	if err != nil {
		t.Fatalf("load paired video: %v", err)
	}
	if paired.LiveParentAssetID == nil {
		t.Fatal("the two were not paired before the rebuild, so the test proves nothing")
	}

	a.dropIndex(t)

	result, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if result.Described != 1 {
		t.Errorf("Described = %d, want the one sidecar replayed", result.Described)
	}

	rebuiltStill, err := a.store.AssetBySHA256(ctx, still.SHA256)
	if err != nil {
		t.Fatalf("load rebuilt still: %v", err)
	}
	rebuiltVideo, err := a.store.AssetBySHA256(ctx, video.SHA256)
	if err != nil {
		t.Fatalf("load rebuilt video: %v", err)
	}

	if rebuiltVideo.ContentID != pairContentID {
		t.Errorf("ContentID = %q after a rebuild, want it off the manifest line", rebuiltVideo.ContentID)
	}
	if rebuiltVideo.LiveParentAssetID == nil || *rebuiltVideo.LiveParentAssetID != rebuiltStill.ID {
		t.Errorf("LiveParentAssetID = %v, want the rebuilt still %s — the pairing did not survive",
			rebuiltVideo.LiveParentAssetID, rebuiltStill.ID)
	}

	if rebuiltStill.Description != "at the border" {
		t.Errorf("Description = %q, want the sidecar replayed", rebuiltStill.Description)
	}
	if !rebuiltStill.Favorite {
		t.Error("Favorite was lost in the rebuild")
	}
	if rebuiltStill.GPSLat == nil || *rebuiltStill.GPSLat != 41.7844 {
		t.Errorf("GPSLat = %v, want the sidecar's coordinates", rebuiltStill.GPSLat)
	}

	extras, err := a.store.AssetExtras(ctx, rebuiltStill.ID)
	if err != nil {
		t.Fatalf("AssetExtras: %v", err)
	}
	if len(extras.Albums) != 1 || extras.Albums[0] != "Iceland 2025" {
		t.Errorf("Albums = %v, want the album membership restored", extras.Albums)
	}
	if len(extras.People) != 1 || extras.People[0] != "Brody" {
		t.Errorf("People = %v", extras.People)
	}
}

// A metadata line names a digest and records no bytes. Counted as an asset line
// it would look like an archived file that is not on disk, and verify would
// report a blob-missing finding for every sidecar in the log.
func TestVerifyIgnoresMetadataLines(t *testing.T) {
	a := newArchive(t)
	a.addImported(t, "IMG_0001.HEIC", fixture(t, "iphone-portrait.heic"), pairContentID, reindexSidecar)

	if report := a.run(t, verify.Options{Deep: true}); len(report.Findings) != 0 {
		t.Errorf("findings on an intact imported archive: %v", report.Findings)
	}
}
