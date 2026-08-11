package db

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TimelineItem is deliberately the smallest thing the grid can draw: an id to
// fetch a thumbnail with, a time to group under a heading, and enough state to
// know whether the thumbnail exists yet. Around 80 bytes on the wire, so a
// 200-item page is ~16KB.
type TimelineItem struct {
	ID        string    `json:"id"`
	MediaKind string    `json:"kind"`
	TakenAt   time.Time `json:"taken_at"`
	// OffsetMinutes is the file's own UTC offset, sent so the grid can file a
	// photo under the day it was taken rather than the day it falls on in
	// whatever timezone the browser happens to be in. Null means the file
	// recorded no zone, and the client falls back to local time.
	OffsetMinutes   *int     `json:"offset_minutes,omitempty"`
	State           string   `json:"state"`
	PlaybackState   string   `json:"playback_state,omitempty"`
	DurationSeconds *float64 `json:"duration,omitempty"`
	// LiveState is the state of this still's paired video, for the half of an
	// iPhone library that is Live Photos. Omitted when there is no motion, so
	// the field's presence is the whole answer the grid needs.
	LiveState string `json:"live,omitempty"`
}

// visibleAssets is the timeline's view of the library: everything except the
// paired videos, which are shown as their still's motion rather than as items
// of their own.
//
// It filters on the declaration, not on the resolution. A video whose still has
// not arrived yet is still a Live Photo's video and still not a thing anyone
// took, so hiding it does not wait on the pairing being complete.
const visibleAssets = `live_parent_local_id = ''`

// liveJoin attaches a still's motion state. Lateral with a limit rather than a
// plain join: the same still can be reached by two devices' copies of the same
// paired video, and a plain join would draw that photo twice.
const liveJoin = `
	left join lateral (
		select v.live_state from assets v
		where v.live_parent_asset_id = a.id
		order by v.uploaded_at, v.id limit 1
	) live on true`

// TimelinePage is one page of the timeline. NextCursor is empty at the end.
type TimelinePage struct {
	Items      []TimelineItem `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// Cursor is the keyset position between pages: the sort key of the last item
// returned, plus its id to break ties between assets sharing a timestamp.
type Cursor struct {
	SortTime time.Time
	ID       string
}

var ErrBadCursor = errors.New("db: malformed timeline cursor")

// Encode renders a cursor as an opaque token. Opaque because it is a position
// in a result set, not an API: making it look unparseable keeps clients from
// building on its shape.
func (c Cursor) Encode() string {
	raw := c.SortTime.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return Cursor{}, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{SortTime: t, ID: id}, nil
}

// Timeline returns one page of assets, newest first. An empty cursor starts at
// the beginning.
//
// Pagination is keyset rather than OFFSET, so page 400 costs exactly what page
// 1 costs. The row comparison `(sort_time, id) < (...)` maps directly onto the
// (sort_time desc, id desc) index, which is what keeps it a plain index scan.
//
// Assets whose derivatives are still pending or have failed are included. The
// alternative — hiding them until ready — means most of the library is silently
// absent during a backfill, and a permanently failed job produces a photo that
// is archived but unreachable.
func (s *Store) Timeline(ctx context.Context, after *Cursor, limit int) (TimelinePage, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	var (
		cursorTime any
		cursorID   any
	)
	if after != nil {
		cursorTime = after.SortTime
		cursorID = after.ID
	}

	// One extra row tells us whether another page exists without a second query.
	rows, err := s.pool.Query(ctx, `
		select a.id::text, a.media_kind, a.sort_time, a.exif_offset_minutes,
		       a.derived_state, a.playback_state, a.duration_seconds,
		       coalesce(live.live_state, 'none')
		from assets a`+liveJoin+`
		where `+visibleAssets+`
		  and ($1::timestamptz is null
		       or (a.sort_time, a.id) < ($1::timestamptz, $2::uuid))
		order by a.sort_time desc, a.id desc
		limit $3`, cursorTime, cursorID, limit+1)
	if err != nil {
		return TimelinePage{}, fmt.Errorf("query timeline: %w", err)
	}
	defer rows.Close()

	var page TimelinePage
	var last Cursor
	for rows.Next() {
		var it TimelineItem
		var sortTime time.Time
		if err := rows.Scan(&it.ID, &it.MediaKind, &sortTime, &it.OffsetMinutes,
			&it.State, &it.PlaybackState, &it.DurationSeconds, &it.LiveState); err != nil {
			return TimelinePage{}, fmt.Errorf("scan timeline item: %w", err)
		}
		it.TakenAt = sortTime
		it.hideEmptyLiveState()
		page.Items = append(page.Items, it)
		last = Cursor{SortTime: sortTime, ID: it.ID}
	}
	if err := rows.Err(); err != nil {
		return TimelinePage{}, fmt.Errorf("read timeline: %w", err)
	}

	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last = Cursor{SortTime: page.Items[limit-1].TakenAt, ID: page.Items[limit-1].ID}
		page.NextCursor = last.Encode()
	}
	return page, nil
}

// TimelineStates returns the current derivative state of specific assets. The
// gallery polls this for the pending tiles it has on screen, which is far
// cheaper than re-fetching whole pages while a backfill runs.
func (s *Store) TimelineStates(ctx context.Context, ids []string) (map[string]TimelineItem, error) {
	states := make(map[string]TimelineItem, len(ids))
	if len(ids) == 0 {
		return states, nil
	}

	rows, err := s.pool.Query(ctx, `
		select a.id::text, a.media_kind, a.sort_time, a.exif_offset_minutes,
		       a.derived_state, a.playback_state, a.duration_seconds,
		       coalesce(live.live_state, 'none')
		from assets a`+liveJoin+`
		where a.id = any($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("query asset states: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var it TimelineItem
		var sortTime time.Time
		if err := rows.Scan(&it.ID, &it.MediaKind, &sortTime, &it.OffsetMinutes,
			&it.State, &it.PlaybackState, &it.DurationSeconds, &it.LiveState); err != nil {
			return nil, fmt.Errorf("scan asset state: %w", err)
		}
		it.TakenAt = sortTime
		it.hideEmptyLiveState()
		states[it.ID] = it
	}
	return states, rows.Err()
}

// hideEmptyLiveState drops the sentinel so a still with no motion sends no
// field at all rather than a string saying so, across every item in the page.
func (t *TimelineItem) hideEmptyLiveState() {
	if t.LiveState == PlaybackNone {
		t.LiveState = ""
	}
}
