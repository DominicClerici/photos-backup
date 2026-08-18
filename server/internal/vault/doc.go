package vault

import (
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"time"
)

// AlbumRef is an album a photograph was in when it was hidden: its id, so a
// restore can find it, and its title, so a vault that has been unlocked can
// draw the album without asking the database about a row it may have deleted.
type AlbumRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Source string `json:"source"`
}

// Doc is the sealed document: an asset row and the memberships that were
// deleted with it.
//
// The row travels as raw JSON rather than as a struct on purpose. It is written
// by `to_jsonb(a)` and read back by `jsonb_populate_record(null::assets, ...)`,
// so the round trip carries every column the schema has — including the ones a
// migration adds while a photograph is sitting in the vault, and including the
// ones this file has never heard of. A struct here would be a list to keep in
// step with the schema, and the failure mode of getting it wrong is a
// photograph that comes back missing something.
//
// The fields this package actually needs are parsed out of it separately, into
// Item, which is a view rather than the record.
type Doc struct {
	Version int             `json:"v"`
	Asset   json.RawMessage `json:"asset"`
	Albums  []AlbumRef      `json:"albums"`
	People  []string        `json:"people"`
}

// row is the handful of columns the vault's own gallery is drawn from. Every
// one of them is encrypted in the database, which is why this exists at all:
// there is no query that could sort, group or count by any of it.
type row struct {
	ID                string     `json:"id"`
	SHA256            string     `json:"sha256"`
	MD5               string     `json:"md5"`
	ByteSize          int64      `json:"byte_size"`
	Ext               string     `json:"ext"`
	ContentType       string     `json:"content_type"`
	OriginalFilename  string     `json:"original_filename"`
	MediaKind         string     `json:"media_kind"`
	Width             *int       `json:"width"`
	Height            *int       `json:"height"`
	SortTime          time.Time  `json:"sort_time"`
	ExifOffsetMinutes *int       `json:"exif_offset_minutes"`
	DurationSeconds   *float64   `json:"duration_seconds"`
	DerivedState      string     `json:"derived_state"`
	PlaybackState     string     `json:"playback_state"`
	LiveState         string     `json:"live_state"`
	LiveParentLocalID string     `json:"live_parent_local_id"`
	LiveParentAssetID *string    `json:"live_parent_asset_id"`
	IsOverlay         bool       `json:"is_overlay"`
	OverlayAssetID    *string    `json:"overlay_asset_id"`
	Favorite          bool       `json:"favorite"`
	Archived          bool       `json:"archived"`
	Subtypes          []string   `json:"subtypes"`
	Description       *string    `json:"description"`
	VaultedAt         *time.Time `json:"vaulted_at"`

	// Everything below is read for the viewer's information panel and nothing
	// else. It is in the sealed document either way — the panel is the reason
	// there is a struct here to name it in.
	CapturedAt  *time.Time `json:"captured_at"`
	UploadedAt  time.Time  `json:"uploaded_at"`
	ByteSizeRaw int64      `json:"-"`
	CameraMake  *string    `json:"camera_make"`
	CameraModel *string    `json:"camera_model"`
	Lens        *string    `json:"lens"`
	GPSLat      *float64   `json:"gps_lat"`
	GPSLon      *float64   `json:"gps_lon"`
}

// Detail is one hidden photograph as the viewer's panel draws it: everything
// the scrub took out of the row, put back for the length of one request.
//
// It exists because the viewer is the same viewer. A photograph in the vault
// opens into the same surface, with the same arrow keys and the same panel, and
// a panel that went blank on hidden photographs would be a second viewer by
// omission.
type Detail struct {
	Filename        string
	MediaKind       string
	SHA256          string
	ByteSize        int64
	Width           *int
	Height          *int
	DurationSeconds *float64
	TakenAt         time.Time
	OffsetMinutes   *int
	ReportedAt      *time.Time
	UploadedAt      time.Time
	CameraMake      string
	CameraModel     string
	Lens            string
	GPSLat          *float64
	GPSLon          *float64
	Description     string
	Favorite        bool
	Archived        bool
	Albums          []string
	People          []string
	HasOverlay      bool
	State           string
	PlaybackState   string
}

func (i *Item) Detail() Detail {
	albums := make([]string, 0, len(i.Doc.Albums))
	for _, ref := range i.Doc.Albums {
		albums = append(albums, ref.Title)
	}
	people := i.Doc.People
	if people == nil {
		people = []string{}
	}
	return Detail{
		Filename:        i.row.OriginalFilename,
		MediaKind:       i.row.MediaKind,
		SHA256:          i.row.SHA256,
		ByteSize:        i.row.ByteSize,
		Width:           i.row.Width,
		Height:          i.row.Height,
		DurationSeconds: i.row.DurationSeconds,
		TakenAt:         i.row.SortTime,
		OffsetMinutes:   i.row.ExifOffsetMinutes,
		ReportedAt:      i.row.CapturedAt,
		UploadedAt:      i.row.UploadedAt,
		CameraMake:      text(i.row.CameraMake),
		CameraModel:     text(i.row.CameraModel),
		Lens:            text(i.row.Lens),
		GPSLat:          i.row.GPSLat,
		GPSLon:          i.row.GPSLon,
		Description:     text(i.row.Description),
		Favorite:        i.row.Favorite,
		Archived:        i.row.Archived,
		Albums:          albums,
		People:          people,
		HasOverlay:      i.row.OverlayAssetID != nil,
		State:           i.row.DerivedState,
		PlaybackState:   i.row.PlaybackState,
	}
}

func text(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// Item is one opened row as the vault's gallery holds it.
//
// Component is what notComponent decides in SQL, decided here instead, over
// exactly the same three facts — a paired video and a caption layer are parts
// of a photograph rather than photographs, in the vault as much as out of it.
type Item struct {
	Bucket string
	Doc    Doc
	row    row

	// Live is the state of this still's motion, filled from the component that
	// points at it once every row has been opened.
	Live string
	// HasOverlay says a caption layer was composed into this picture, which the
	// viewer offers a toggle for.
	HasOverlay bool
}

func (i *Item) ID() string        { return i.row.ID }
func (i *Item) SHA256() string    { return i.row.SHA256 }
func (i *Item) Ext() string       { return i.row.Ext }
func (i *Item) Filename() string  { return i.row.OriginalFilename }
func (i *Item) MediaKind() string { return i.row.MediaKind }
func (i *Item) SortTime() time.Time {
	return i.row.SortTime
}
func (i *Item) ContentType() string { return i.row.ContentType }

// Component reports that this row is part of another item rather than an item.
func (i *Item) Component() bool {
	return i.row.LiveParentLocalID != "" || i.row.LiveParentAssetID != nil || i.row.IsOverlay
}

// Open unseals one document. The context binds it to the asset it belongs to,
// so a sealed blob moved onto another row does not open.
func openDoc(priv *ecdh.PrivateKey, assetID string, sealed []byte) (Doc, row, error) {
	raw, err := OpenDoc(priv, "asset\x00"+assetID, sealed)
	if err != nil {
		return Doc{}, row{}, err
	}
	var doc Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Doc{}, row{}, fmt.Errorf("vault: unreadable sealed document: %w", err)
	}
	var r row
	if err := json.Unmarshal(doc.Asset, &r); err != nil {
		return Doc{}, row{}, fmt.Errorf("vault: unreadable sealed row: %w", err)
	}
	return doc, r, nil
}

// SealAsset seals one asset's document to the vault. Called on the write path,
// which holds only the public key.
func SealAsset(to *ecdh.PublicKey, assetID string, doc []byte) ([]byte, error) {
	return SealDoc(to, "asset\x00"+assetID, doc)
}
