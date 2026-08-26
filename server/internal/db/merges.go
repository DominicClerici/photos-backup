package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

// The states a group moves through. Pending is a question, and the other two
// are the two ways of answering it.
const (
	MergePending   = "pending"
	MergeMerged    = "merged"
	MergeDismissed = "dismissed"
)

// MergeFailedState is a fourth thing to ask for and not a fourth state.
//
// A group whose join failed is `pending` and stays `pending`: the question is
// still open, the worker will try again if it is requeued, and nothing about
// the photographs has changed. What is different is that the last attempt gave
// up, and that lives in the jobs table rather than here. So it is a filter over
// the pending groups rather than a value the state column can hold — and it is
// spelled here anyway, because the review page asks for it by name.
const MergeFailedState = "failed"

var (
	// ErrNotPending means an operation was asked of a group that has already
	// been answered. Refused rather than repeated: merging a merged group would
	// trash the copy that was kept the first time.
	ErrNotPending = errors.New("db: that group has already been resolved")
	// ErrNotAMember means the asset chosen as the keeper is not in the group.
	ErrNotAMember = errors.New("db: that asset is not in this group")
	// ErrNotMerged means an operation that only makes sense for a merge that
	// happened was asked of a group where none did. Approving one is the only
	// such operation: it says "I have read this entry of the log", and a
	// pending group is not an entry of the log yet.
	ErrNotMerged = errors.New("db: that group has not been merged")
)

// MergeGroup is a set of assets that ought to be one, with enough of each
// member on it to draw the comparison somebody is being asked to make.
type MergeGroup struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	DetectedAt time.Time `json:"detected_at"`
	// KeeperAssetID is the copy that was kept, or the joined recording that was
	// built. Null while the group is pending.
	KeeperAssetID *string `json:"keeper_asset_id,omitempty"`
	// Fingerprint is the member set, hashed — the column that makes a rescan
	// idempotent. It is not on the wire: no client has any use for it, and it
	// is here only because it is the name a group's rejected join is filed
	// under on disk. See derivstore.JoinPreview.
	Fingerprint string `json:"-"`
	// ApprovedAt is when somebody read this entry of the log and was content
	// with it. Only ever set on a joined recording, which is the only half of
	// this feature that resolves itself and therefore the only one that
	// accumulates entries nobody has looked at.
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	// Forced is true when the join was archived over the objection of the
	// duration check. A fact about how this recording came to exist, kept where
	// the page that lists it can say so.
	Forced bool `json:"forced,omitempty"`
	// Failure is the merge job that gave up on this group, when one has. Never
	// set on a resolved group: a merge that happened has no failure, and one
	// that was refused is not owed any work.
	Failure *MergeFailure `json:"failure,omitempty"`

	Members []MergeMember `json:"members"`
}

// MergeFailure is the last attempt at a join, as much of it as is worth
// showing beside the recording it did not produce.
//
// The same row the status page draws under "needs attention", offered here
// instead in the one place where something can be done about it: the error text
// says the running time did not add up, the rejected file is on disk, and the
// two buttons beside this are "watch it" and "archive it anyway".
type MergeFailure struct {
	JobID    int64     `json:"job"`
	Attempts int       `json:"attempts"`
	Error    string    `json:"error"`
	FailedAt time.Time `json:"failed_at"`
}

// MergeMember is one candidate, described in the terms the choice is actually
// made in: how big it is, how heavy, where it came from, and what would be lost
// by discarding it.
type MergeMember struct {
	AssetID   string `json:"id"`
	Position  int    `json:"position"`
	Filename  string `json:"filename"`
	MediaKind string `json:"kind"`
	Width     *int   `json:"width,omitempty"`
	Height    *int   `json:"height,omitempty"`
	ByteSize  int64  `json:"byte_size"`
	// DurationSeconds is null for a still.
	DurationSeconds *float64  `json:"duration,omitempty"`
	TakenAt         time.Time `json:"taken_at"`
	// ImportSource is what put it here — a Takeout, a Snapchat export, the
	// phone. On a page about duplicates it is often the whole explanation.
	ImportSource string `json:"import_source,omitempty"`
	Favorite     bool   `json:"favorite,omitempty"`
	// Albums and People are what a discarded copy would take with it, if the
	// merge did not carry them across. It does — see MergeDuplicate — and they
	// are shown anyway, because "this one is in three albums" is exactly the
	// sort of thing that makes somebody pick a different keeper.
	Albums []string `json:"albums,omitempty"`
	People []string `json:"people,omitempty"`
	State  string   `json:"state"`
}

// RecordGroups writes what a scan found, and is safe to call after every scan.
//
// A group whose members are already a pending group is a no-op, by fingerprint.
// A group that *overlaps* a pending one replaces it: the scan has changed its
// mind about the same photographs — usually because a new copy of them turned
// up — and leaving both would offer somebody two overlapping questions whose
// answers contradict each other.
//
// Nothing here touches a group that has been resolved. A merged group's losers
// are in the trash and out of `scannable`; a dismissed one is a decision, and
// re-proposing it is what merge.Blocked exists to prevent.
func (s *Store) RecordGroups(ctx context.Context, groups []merge.Group) (created int, err error) {
	if len(groups) == 0 {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, g := range groups {
		if len(g.IDs) < 2 {
			continue
		}
		fingerprint := merge.Fingerprint(g.Kind, g.IDs)

		// Already asked, and not yet answered.
		var exists bool
		if err := tx.QueryRow(ctx,
			`select true from merge_groups where fingerprint = $1`, fingerprint).Scan(&exists); err == nil {
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("look up group fingerprint: %w", err)
		}

		// The overlap rule. Deleting cascades to merge_members.
		if _, err := tx.Exec(ctx, `
			delete from merge_groups g
			where g.state = 'pending' and g.kind = $1
			  and exists (select 1 from merge_members m
			              where m.group_id = g.id and m.asset_id = any($2::uuid[]))`,
			g.Kind, g.IDs); err != nil {
			return 0, fmt.Errorf("clear superseded groups: %w", err)
		}

		var id string
		if err := tx.QueryRow(ctx,
			`insert into merge_groups (kind, fingerprint) values ($1, $2) returning id`,
			g.Kind, fingerprint).Scan(&id); err != nil {
			return 0, fmt.Errorf("insert merge group: %w", err)
		}
		for position, assetID := range g.IDs {
			if _, err := tx.Exec(ctx,
				`insert into merge_members (group_id, asset_id, position) values ($1::uuid, $2::uuid, $3)`,
				id, assetID, position); err != nil {
				return 0, fmt.Errorf("insert merge member: %w", err)
			}
		}
		created++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return created, nil
}

// BlockedPairs is every pair of assets somebody has already said are not the
// same photograph.
//
// Every pair within a dismissed group, rather than the group itself, because a
// group is not stable: dismissing {a, b} and then finding {a, b, c} would be a
// different fingerprint and the same rejected question with a stranger in it.
// The pairs survive that.
//
// Quadratic in the size of a dismissed group, which is fine — these are the
// groups somebody has looked at by hand, so there are as many of them as there
// are minutes anybody has spent on this page.
func (s *Store) BlockedPairs(ctx context.Context) (merge.BlockedPairs, error) {
	const query = `
		select g.id, m.asset_id
		from merge_groups g
		join merge_members m on m.group_id = g.id
		where g.state = 'dismissed'
		order by g.id`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read dismissed groups: %w", err)
	}
	defer rows.Close()

	byGroup := map[string][]string{}
	for rows.Next() {
		var group, asset string
		if err := rows.Scan(&group, &asset); err != nil {
			return nil, fmt.Errorf("read dismissed groups: %w", err)
		}
		byGroup[group] = append(byGroup[group], asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read dismissed groups: %w", err)
	}

	blocked := merge.BlockedPairs{}
	for _, members := range byGroup {
		for i := range members {
			for j := i + 1; j < len(members); j++ {
				blocked.Add(members[i], members[j])
			}
		}
	}
	return blocked, nil
}

// MergeCounts is what the overview card says.
type MergeCounts struct {
	// PendingDuplicates is groups, not photographs: it is the number of
	// decisions waiting, which is what the card is offering to spend somebody's
	// time on.
	PendingDuplicates int64 `json:"pending_duplicates"`
	// DuplicateItems is the photographs in them, which is the number that says
	// how much is at stake.
	DuplicateItems int64 `json:"duplicate_items"`
	// PendingSegments is recordings the worker has not joined yet, and
	// MergedSegments is how many it has. Neither counts anything somebody has
	// finished with: a failed join is counted separately below, and an approved
	// one is not counted at all — the point of approving is to stop being told
	// about it.
	PendingSegments int64 `json:"pending_segments"`
	MergedSegments  int64 `json:"merged_segments"`
	// FailedSegments is recordings whose join gave up. Its own figure because
	// it is the only one on this card that is not going to resolve itself:
	// "still being joined" comes right in a minute, and this waits forever.
	FailedSegments int64 `json:"failed_segments"`
	// ApprovedSegments is how many have been read and signed off, which is what
	// the review page's "show approved" is offering to put back on screen.
	ApprovedSegments int64 `json:"approved_segments"`
	// MergedDuplicates is how many groups have been resolved by hand, which is
	// the only progress indicator this page has.
	MergedDuplicates int64 `json:"merged_duplicates"`

	Coverage SignatureCoverage `json:"coverage"`
}

func (s *Store) MergeCounts(ctx context.Context) (MergeCounts, error) {
	const query = `
		select
		  count(*) filter (where kind = $1 and state = 'pending')::bigint,
		  coalesce(sum(members) filter (where kind = $1 and state = 'pending'), 0)::bigint,
		  count(*) filter (where kind = $2 and state = 'pending' and not failed)::bigint,
		  count(*) filter (where kind = $2 and state = 'pending' and failed)::bigint,
		  count(*) filter (where kind = $2 and state = 'merged' and not approved)::bigint,
		  count(*) filter (where kind = $2 and state = 'merged' and approved)::bigint,
		  count(*) filter (where kind = $1 and state = 'merged')::bigint
		from (
		  select g.kind, g.state,
		         g.approved_at is not null as approved,
		         exists (` + failedJob + `) as failed,
		         count(m.asset_id) as members
		  from merge_groups g left join merge_members m on m.group_id = g.id
		  group by g.id
		) x`

	var out MergeCounts
	err := s.pool.QueryRow(ctx, query, merge.KindDuplicate, merge.KindSegments).Scan(
		&out.PendingDuplicates, &out.DuplicateItems,
		&out.PendingSegments, &out.FailedSegments,
		&out.MergedSegments, &out.ApprovedSegments, &out.MergedDuplicates)
	if err != nil {
		return MergeCounts{}, fmt.Errorf("count merge groups: %w", err)
	}

	if out.Coverage, err = s.SignatureCoverage(ctx); err != nil {
		return MergeCounts{}, err
	}
	return out, nil
}

// headAsset is the member a group's work is queued against: its first, by
// position.
//
// The jobs table names an asset and nothing else — it is a queue of work owed
// to assets and has been since before any of this existed — so a merge is
// queued against the first piece of the recording. Three queries have to agree
// about which piece that is, and disagreeing about it later would be a bug
// nobody notices: the group would simply never look like it had failed.
const headAsset = `(select m.asset_id from merge_members m
                    where m.group_id = g.id order by m.position limit 1)`

// failedJob is "the join of this group gave up", as a correlated existence
// test over the queue.
const failedJob = `select 1 from jobs j
                   where j.kind = 'merge' and j.state = 'failed'
                     and j.asset_id = ` + headAsset

// groupColumns is the group half of every read, in the order every Scan below
// expects them.
const groupColumns = `id, kind, state, detected_at, keeper_asset_id,
                      fingerprint, approved_at, force_join`

// scanGroup reads one row of groupColumns.
func scanGroup(row pgx.Row) (MergeGroup, error) {
	var g MergeGroup
	err := row.Scan(&g.ID, &g.Kind, &g.State, &g.DetectedAt, &g.KeeperAssetID,
		&g.Fingerprint, &g.ApprovedAt, &g.Forced)
	return g, err
}

// memberColumns is the member half of every group read, written once because
// the review page and the worker want the same thing about an asset and
// disagreeing about it later would be a bug nobody notices.
const memberColumns = `
	select m.group_id, m.asset_id, m.position,
	       a.original_filename, a.media_kind, a.width, a.height, a.byte_size,
	       a.duration_seconds, a.sort_time, a.import_source, a.favorite,
	       coalesce(al.titles, '{}'), coalesce(pp.names, '{}'),
	       case when a.deleted_at is not null then 'deleted' else 'live' end
	from merge_members m
	join assets a on a.id = m.asset_id
	left join lateral (
	    select array_agg(albums.title order by albums.title) as titles
	    from album_assets join albums on albums.id = album_assets.album_id
	    where album_assets.asset_id = a.id and albums.deleted_at is null
	) al on true
	left join lateral (
	    select array_agg(name order by name) as names
	    from asset_people where asset_id = a.id
	) pp on true`

// MergeQuery is one page of the review, asked for.
//
// A struct rather than four positional arguments because two of them are
// booleans and `Groups(ctx, kind, state, false, true, 60)` is a line nobody can
// read. The zero value is the pending duplicates, which is what the page opens
// on.
type MergeQuery struct {
	Kind string
	// State is one of the three a group can be in, or MergeFailedState, which
	// is pending-and-the-join-gave-up. Empty means pending.
	State string
	// Approved includes joined recordings somebody has signed off. Off by
	// default, which is the whole point of approving one: the list is what is
	// left to look at, not everything that ever happened.
	Approved bool
	Limit    int
}

// Groups reads groups of one kind and state, newest question last.
//
// Members come back in one second query rather than in a join against the
// group rows, because a group of forty duplicates would otherwise repeat its
// own row forty times and the paging would be over the wrong thing entirely.
// The failure, when there is one, comes back in a third for the same reason it
// is not a column: it lives in the queue.
func (s *Store) Groups(ctx context.Context, q MergeQuery) ([]MergeGroup, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	kind := q.Kind
	if kind == "" {
		kind = merge.KindDuplicate
	}

	state, where := q.State, ""
	if state == "" {
		state = MergePending
	}
	if state == MergeFailedState {
		state = MergePending
		where += ` and exists (` + failedJob + `)`
	}
	if !q.Approved {
		where += ` and g.approved_at is null`
	}

	rows, err := s.pool.Query(ctx, `
		select `+groupColumns+`
		from merge_groups g
		where kind = $1 and state = $2`+where+`
		order by detected_at, id
		limit $3`, kind, state, limit)
	if err != nil {
		return nil, fmt.Errorf("read merge groups: %w", err)
	}
	defer rows.Close()

	var groups []MergeGroup
	index := map[string]int{}
	var ids []string
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("read merge groups: %w", err)
		}
		index[g.ID] = len(groups)
		ids = append(ids, g.ID)
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read merge groups: %w", err)
	}
	if len(groups) == 0 {
		return nil, nil
	}

	// Only the segments have a job that can give up on them: a duplicate group
	// is resolved by a person and nothing is queued against it. Asking anyway
	// would cost a query per page and could hang a stranger's failure on a
	// duplicate card, through a head asset that happens to be in both.
	if kind == merge.KindSegments {
		if err := s.attachFailures(ctx, groups, index, ids); err != nil {
			return nil, err
		}
	}

	members, err := s.pool.Query(ctx,
		memberColumns+` where m.group_id = any($1::uuid[]) order by m.group_id, m.position`, ids)
	if err != nil {
		return nil, fmt.Errorf("read merge members: %w", err)
	}
	defer members.Close()

	for members.Next() {
		var group string
		var m MergeMember
		if err := members.Scan(&group, &m.AssetID, &m.Position,
			&m.Filename, &m.MediaKind, &m.Width, &m.Height, &m.ByteSize,
			&m.DurationSeconds, &m.TakenAt, &m.ImportSource, &m.Favorite,
			&m.Albums, &m.People, &m.State); err != nil {
			return nil, fmt.Errorf("read merge members: %w", err)
		}
		at, ok := index[group]
		if !ok {
			continue
		}
		groups[at].Members = append(groups[at].Members, m)
	}
	if err := members.Err(); err != nil {
		return nil, fmt.Errorf("read merge members: %w", err)
	}
	return groups, nil
}

// attachFailures hangs the merge job that gave up on each group that has one.
//
// One query for the page rather than one per row, and a left join would not do:
// a group's failure is a row in another subsystem's table, found through the
// member its work happens to be queued against.
func (s *Store) attachFailures(ctx context.Context, groups []MergeGroup, index map[string]int, ids []string) error {
	rows, err := s.pool.Query(ctx, `
		select g.id, j.id, j.attempts, coalesce(j.last_error, ''), j.updated_at
		from merge_groups g
		join jobs j on j.asset_id = `+headAsset+`
		            and j.kind = 'merge' and j.state = 'failed'
		where g.id = any($1::uuid[])`, ids)
	if err != nil {
		return fmt.Errorf("read merge failures: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var group string
		var f MergeFailure
		if err := rows.Scan(&group, &f.JobID, &f.Attempts, &f.Error, &f.FailedAt); err != nil {
			return fmt.Errorf("read merge failures: %w", err)
		}
		if at, ok := index[group]; ok {
			groups[at].Failure = &f
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read merge failures: %w", err)
	}
	return nil
}

// Group reads one, whatever state it is in.
func (s *Store) Group(ctx context.Context, id string) (MergeGroup, error) {
	g, err := scanGroup(s.pool.QueryRow(ctx,
		`select `+groupColumns+` from merge_groups g where id = $1::uuid`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return MergeGroup{}, ErrNotFound
	case err != nil:
		return MergeGroup{}, fmt.Errorf("read merge group %s: %w", id, err)
	}

	rows, err := s.pool.Query(ctx, memberColumns+` where m.group_id = $1::uuid order by m.position`, id)
	if err != nil {
		return MergeGroup{}, fmt.Errorf("read merge members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var groupID string
		var m MergeMember
		if err := rows.Scan(&groupID, &m.AssetID, &m.Position,
			&m.Filename, &m.MediaKind, &m.Width, &m.Height, &m.ByteSize,
			&m.DurationSeconds, &m.TakenAt, &m.ImportSource, &m.Favorite,
			&m.Albums, &m.People, &m.State); err != nil {
			return MergeGroup{}, fmt.Errorf("read merge members: %w", err)
		}
		g.Members = append(g.Members, m)
	}
	if err := rows.Err(); err != nil {
		return MergeGroup{}, fmt.Errorf("read merge members: %w", err)
	}
	return g, nil
}

// MergeResult is what resolving a group did.
type MergeResult struct {
	// Keeper is the asset that survived: one of the members for a duplicate,
	// the joined recording for a set of segments.
	Keeper string `json:"keeper"`
	// Batch undoes it, and is the same handle the toast under an ordinary
	// delete holds.
	Batch string `json:"batch"`
	// Trashed is how many items went, counted the way the gallery counts them —
	// components carried along are not items.
	Trashed int `json:"trashed"`
}

// MergeDuplicate keeps one copy and trashes the rest.
//
// The interesting half is not the delete. It is that everything the discarded
// copies knew moves onto the one that stays first: the albums they were in, the
// people tagged on them, a caption typed into Google Photos years ago, and the
// heart somebody put on whichever copy they happened to be looking at. Without
// that, keeping the higher-resolution copy of a photograph would silently drop
// it out of the three albums the lower-resolution one was in, and this would be
// a delete wearing the word "merge".
//
// The keeper is the caller's choice rather than this function's. What the
// review page preselects is merge.Rank's opinion, and it is only an opinion.
func (s *Store) MergeDuplicate(ctx context.Context, groupID, keeperID string) (MergeResult, error) {
	group, err := s.Group(ctx, groupID)
	if err != nil {
		return MergeResult{}, err
	}
	if group.State != MergePending {
		return MergeResult{}, ErrNotPending
	}

	var losers []string
	member := false
	for _, m := range group.Members {
		if m.AssetID == keeperID {
			member = true
			continue
		}
		losers = append(losers, m.AssetID)
	}
	if !member {
		return MergeResult{}, ErrNotAMember
	}
	if len(losers) == 0 {
		return MergeResult{}, fmt.Errorf("merge %s: a group of one is not a merge", groupID)
	}

	return s.resolve(ctx, group, keeperID, losers)
}

// MergeSegments records that a joined recording has been archived and puts its
// pieces in the trash.
//
// The joined asset is not a member of the group and was not there when the
// group was found: the worker built it a moment ago, out of the members. So
// unlike a duplicate, every one of the members is a loser, and what the keeper
// inherits it inherits from all of them.
func (s *Store) MergeSegments(ctx context.Context, groupID, joinedAssetID string) (MergeResult, error) {
	group, err := s.Group(ctx, groupID)
	if err != nil {
		return MergeResult{}, err
	}
	if group.State != MergePending {
		return MergeResult{}, ErrNotPending
	}

	losers := make([]string, 0, len(group.Members))
	for _, m := range group.Members {
		if m.AssetID == joinedAssetID {
			return MergeResult{}, fmt.Errorf("merge %s: the joined recording cannot be one of its own parts", groupID)
		}
		losers = append(losers, m.AssetID)
	}
	if len(losers) < 2 {
		return MergeResult{}, fmt.Errorf("merge %s: %d parts is not something to join", groupID, len(losers))
	}

	return s.resolve(ctx, group, joinedAssetID, losers)
}

// resolve is the half both merges share: carry everything onto the keeper,
// trash the rest, and write the group down as answered.
//
// One transaction, because the three are one decision. A crash between the
// carry-over and the delete would leave the keeper holding albums it had not
// earned and the copies still in the library; a crash between the delete and
// the group update would leave a pending group whose members are in the trash,
// which the next scan would propose again.
func (s *Store) resolve(ctx context.Context, group MergeGroup, keeperID string, losers []string) (MergeResult, error) {
	batch, err := newBatch()
	if err != nil {
		return MergeResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MergeResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := carryOver(ctx, tx, keeperID, losers); err != nil {
		return MergeResult{}, err
	}

	// The same statement Trash runs, over an explicit list rather than a
	// selection: `family` so a discarded Live Photo takes its motion and a
	// discarded memory takes its caption layer, and `deleted_at is null` so a
	// copy that was already in the trash keeps the batch it went in with.
	var trashed int
	err = tx.QueryRow(ctx, `
		with sel as (select unnest($3::uuid[]) as id),`+family+`,
		done as (
			update assets a set deleted_at = now(),
			                    purge_after = now() + make_interval(days => $2),
			                    delete_batch = $1::uuid
			where a.id in (select id from fam) and a.deleted_at is null
			returning a.id
		)
		select count(*)::int from sel join done on done.id = sel.id`,
		batch, int32(TrashRetentionDays), losers).Scan(&trashed)
	if err != nil {
		return MergeResult{}, fmt.Errorf("trash the merged copies: %w", err)
	}

	// Guarded on the state so two clicks on the same button cannot both take
	// effect. The second finds nothing pending and the transaction unwinds,
	// which is the only place the double-trash could otherwise happen.
	tag, err := tx.Exec(ctx, `
		update merge_groups
		set state = $2, keeper_asset_id = $3::uuid, delete_batch = $4::uuid, resolved_at = now()
		where id = $1::uuid and state = 'pending'`,
		group.ID, MergeMerged, keeperID, batch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("record the merge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return MergeResult{}, ErrNotPending
	}

	if err := tx.Commit(ctx); err != nil {
		return MergeResult{}, fmt.Errorf("commit transaction: %w", err)
	}
	return MergeResult{Keeper: keeperID, Batch: batch, Trashed: trashed}, nil
}

// carryOver moves everything the discarded copies knew onto the one being kept.
//
// Additive in every case, and never subtractive: a keeper that was already in
// an album stays in it, a keeper that was already a favourite stays one, and a
// caption on the keeper is never replaced by a caption from a copy. What is
// being merged is a set of records of one photograph, and the union of what
// they knew is the only reading that cannot lose something.
func carryOver(ctx context.Context, tx pgx.Tx, keeperID string, losers []string) error {
	if _, err := tx.Exec(ctx, `
		insert into album_assets (album_id, asset_id)
		select distinct album_id, $1::uuid from album_assets where asset_id = any($2::uuid[])
		on conflict do nothing`, keeperID, losers); err != nil {
		return fmt.Errorf("carry albums onto the kept copy: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		insert into asset_people (asset_id, name)
		select distinct $1::uuid, name from asset_people where asset_id = any($2::uuid[])
		on conflict do nothing`, keeperID, losers); err != nil {
		return fmt.Errorf("carry people onto the kept copy: %w", err)
	}

	// A caption only where there is not one already, and the oldest of the
	// candidates so that repeating the merge with the copies in another order
	// reaches the same answer.
	if _, err := tx.Exec(ctx, `
		update assets k set
		    favorite = k.favorite or coalesce((
		        select bool_or(l.favorite) from assets l where l.id = any($2::uuid[])
		    ), false),
		    description = coalesce(nullif(k.description, ''), (
		        select l.description from assets l
		        where l.id = any($2::uuid[]) and coalesce(l.description, '') <> ''
		        order by l.sort_time, l.id limit 1
		    ))
		where k.id = $1::uuid`, keeperID, losers); err != nil {
		return fmt.Errorf("carry the caption and the heart onto the kept copy: %w", err)
	}
	return nil
}

// DismissGroup records that somebody looked and said no.
//
// It is not a delete. The row stays, and every pair of assets inside it becomes
// a pair the scan will never link again — see BlockedPairs. A dismissal that
// removed the row would be forgotten by the next scan, which would ask the same
// question a week later, which is how a review page stops being used.
//
// What it does delete is the work: a set of segments nobody wants joined is not
// owed a join, and a failed row left in the queue would go on being reported on
// the status page as a job that gave up.
func (s *Store) DismissGroup(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update merge_groups set state = $2, resolved_at = now()
		where id = $1::uuid and state = 'pending'`, id, MergeDismissed)
	if err != nil {
		return fmt.Errorf("dismiss group %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotPending
	}
	if err := discardMergeJob(ctx, tx, id); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// headOfGroup names the member a group's work is queued against, standing
// alone. Same rule as headAsset, which is correlated and cannot be.
const headOfGroup = `select m.asset_id from merge_members m
                     where m.group_id = $1::uuid order by m.position limit 1`

// discardMergeJob takes a join off the queue, because there is no longer one
// owed.
//
// A group with no members left is not an error to find here: its parts have
// been purged out from under it, and there is nothing in the queue naming them
// either.
func discardMergeJob(ctx context.Context, tx pgx.Tx, groupID string) error {
	var head string
	switch err := tx.QueryRow(ctx, headOfGroup, groupID).Scan(&head); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("find the head of group %s: %w", groupID, err)
	}
	return jobs.Discard(ctx, tx, jobs.KindMerge, head)
}

// ApproveGroup records that somebody has read this entry of the log and is
// content with it, or takes that back.
//
// Only a merged group can be approved, and in practice only a joined recording
// ever is. A duplicate group was resolved by somebody choosing a keeper, which
// is already a stronger statement than approval; a set of segments was resolved
// by a worker at three in the morning, and this is the only place anybody gets
// to say they have seen it.
//
// It changes nothing about the photographs. That is what makes it safe to be a
// single click with no confirmation: the pieces are still in the trash, the
// joined recording is still in the library, and "split back up" does exactly
// what it did before.
func (s *Store) ApproveGroup(ctx context.Context, id string, approved bool) error {
	tag, err := s.pool.Exec(ctx, `
		update merge_groups set approved_at = case when $2 then now() else null end
		where id = $1::uuid and state = $3`, id, approved, MergeMerged)
	if err != nil {
		return fmt.Errorf("approve group %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMerged
	}
	return nil
}

// ForceJoin says to archive this recording even though its parts do not add up,
// and puts the join back on the queue.
//
// The flag lives on the group rather than on the job because the job is
// retried, requeued and reclaimed by machinery that has never heard of any of
// this, and every one of those attempts has to make the same choice the first
// one did. It stays set afterwards, where MergeGroup.Forced reads it: an
// original this archive built and had doubts about should say so.
func (s *Store) ForceJoin(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	switch err := tx.QueryRow(ctx,
		`select state from merge_groups where id = $1::uuid for update`, id).Scan(&state); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("read merge group %s: %w", id, err)
	}
	if state != MergePending {
		return ErrNotPending
	}

	var head string
	switch err := tx.QueryRow(ctx, headOfGroup, id).Scan(&head); {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("force join %s: its parts are no longer in the library", id)
	case err != nil:
		return fmt.Errorf("find the head of group %s: %w", id, err)
	}

	if _, err := tx.Exec(ctx,
		`update merge_groups set force_join = true where id = $1::uuid`, id); err != nil {
		return fmt.Errorf("record the override on group %s: %w", id, err)
	}
	// Requeue rather than Enqueue: the row is there and it is failed, which is
	// the only state this is ever called from.
	if err := jobs.Requeue(ctx, tx, jobs.KindMerge, head); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// SegmentPreviews is the fingerprint of every group entitled to hold a rejected
// join on disk.
//
// The list a sweep reconciles against — see derivstore.JoinPreview, which is
// the one file in that tree named after a group rather than an asset and so the
// one nothing else can clean up. A group qualifies while it is still a question
// with parts to answer it: resolved, refused, or stripped of its members by a
// purge, and the file it was keeping is nobody's.
func (s *Store) SegmentPreviews(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		select g.fingerprint from merge_groups g
		where g.kind = $1 and g.state = 'pending'
		  and (select count(*) from merge_members m where m.group_id = g.id) >= 2`,
		merge.KindSegments)
	if err != nil {
		return nil, fmt.Errorf("list joinable groups: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			return nil, fmt.Errorf("list joinable groups: %w", err)
		}
		out = append(out, fingerprint)
	}
	return out, rows.Err()
}

// Unmerged is what an undo put back.
type Unmerged struct {
	Restored int `json:"restored"`
	// Keeper is the asset the merge produced and this has now sent to the
	// trash, for a set of joined segments. Empty for a duplicate, where the
	// keeper was one of the copies and is staying where it is.
	Keeper string `json:"keeper,omitempty"`
}

// UnmergeGroup undoes a merge: the copies come back out of the trash, and a
// joined recording goes into it.
//
// It leaves the group dismissed rather than pending, and that is the whole
// design. Segment groups are merged by a worker without anybody being asked, so
// a group put back to pending would be re-joined within the minute and the undo
// would appear not to have worked. Dismissed says what actually happened —
// somebody saw this merge and did not want it — and it is the state that stops
// it being proposed again.
func (s *Store) UnmergeGroup(ctx context.Context, id string) (Unmerged, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Unmerged{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind string
	var keeper, batch *string
	err = tx.QueryRow(ctx, `
		select kind, keeper_asset_id, delete_batch from merge_groups
		where id = $1::uuid and state = 'merged' for update`, id).Scan(&kind, &keeper, &batch)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Either it does not exist or it was never merged. The caller cannot
		// act differently on the two and neither can anybody reading this.
		return Unmerged{}, ErrNotFound
	case err != nil:
		return Unmerged{}, fmt.Errorf("read merge group %s: %w", id, err)
	}

	var out Unmerged
	var restored []string
	if batch != nil {
		if err := tx.QueryRow(ctx, `
			with restored as (
				update assets a set deleted_at = null, purge_after = null, delete_batch = null
				where a.delete_batch = $1::uuid and a.deleted_at is not null
				returning a.id, (`+notComponent+`) as item
			)
			select (select count(*)::int from restored where item),
			       (select coalesce(array_agg(id::text), '{}') from restored)`,
			*batch).Scan(&out.Restored, &restored); err != nil {
			return Unmerged{}, fmt.Errorf("restore the merged copies: %w", err)
		}
	}

	// The joined recording is a file this archive built rather than one it was
	// given, so an undo has to take it away again — otherwise the library ends
	// up with the minute of video *and* the six pieces it was made from. To the
	// trash rather than gone, because that is what everything else here does
	// and because somebody may well change their mind twice.
	if kind == merge.KindSegments && keeper != nil {
		unbatch, err := newBatch()
		if err != nil {
			return Unmerged{}, err
		}
		if _, err := tx.Exec(ctx, `
			update assets set deleted_at = now(),
			                  purge_after = now() + make_interval(days => $2),
			                  delete_batch = $3::uuid
			where id = $1::uuid and deleted_at is null`,
			*keeper, int32(TrashRetentionDays), unbatch); err != nil {
			return Unmerged{}, fmt.Errorf("trash the joined recording: %w", err)
		}
		out.Keeper = *keeper
	}

	if _, err := tx.Exec(ctx, `
		update merge_groups set state = $2, keeper_asset_id = null, delete_batch = null, resolved_at = now()
		where id = $1::uuid`, id, MergeDismissed); err != nil {
		return Unmerged{}, fmt.Errorf("record the undo: %w", err)
	}
	// Dismissed, so nothing will propose this again — and nothing should be
	// left in the queue claiming to owe it a join either.
	if err := discardMergeJob(ctx, tx, id); err != nil {
		return Unmerged{}, err
	}

	// The copies are out of the trash, so they want their search rows back —
	// see db.Store.Restore for why coming out of the trash is the one thing a
	// tsvector does not survive on its own.
	if err := refresh(ctx, tx, restored); err != nil {
		return Unmerged{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Unmerged{}, fmt.Errorf("commit transaction: %w", err)
	}
	return out, nil
}

// PendingGroupWithMember finds the unanswered group of one kind that an asset
// belongs to.
//
// It is how the merge job finds its group. The jobs table names an asset and
// nothing else — it is a queue of work owed to *assets*, and it has been since
// before any of this existed — so a merge is queued against the first piece of
// the recording and looks the rest up from here.
//
// ErrNotFound when there is none, which is an ordinary outcome rather than a
// problem: somebody may have dismissed the group between the queueing and the
// claim.
func (s *Store) PendingGroupWithMember(ctx context.Context, kind, assetID string) (MergeGroup, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		select g.id from merge_groups g
		join merge_members m on m.group_id = g.id
		where m.asset_id = $1::uuid and g.kind = $2 and g.state = 'pending'
		limit 1`, assetID, kind).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return MergeGroup{}, ErrNotFound
	case err != nil:
		return MergeGroup{}, fmt.Errorf("find the pending group holding %s: %w", assetID, err)
	}
	return s.Group(ctx, id)
}

// PendingGroupHeads lists the first member of every unanswered group of one
// kind — which is the asset its work is queued against.
func (s *Store) PendingGroupHeads(ctx context.Context, kind string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		select distinct on (g.id) m.asset_id
		from merge_groups g
		join merge_members m on m.group_id = g.id
		where g.kind = $1 and g.state = 'pending'
		order by g.id, m.position`, kind)
	if err != nil {
		return nil, fmt.Errorf("read pending group heads: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read pending group heads: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
