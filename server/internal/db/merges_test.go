package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

var mergeEpoch = time.Date(2019, 3, 4, 12, 0, 0, 0, time.UTC)

// seedCopies records n assets standing in for copies of one photograph. They
// have nothing in common as far as the database is concerned — grouping them is
// something the scan decided and this package only records.
func seedCopies(t *testing.T, s *Store, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range n {
		ids[i] = seedAsset(t, s, i, mergeEpoch.Add(time.Duration(i)*time.Minute))
	}
	return ids
}

func recordGroup(t *testing.T, s *Store, kind string, ids ...string) string {
	t.Helper()
	if _, err := s.RecordGroups(context.Background(), []merge.Group{{Kind: kind, IDs: ids}}); err != nil {
		t.Fatalf("RecordGroups: %v", err)
	}
	groups, err := s.Groups(context.Background(), MergeQuery{Kind: kind, State: MergePending, Limit: 100})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	for _, g := range groups {
		if len(g.Members) != len(ids) {
			continue
		}
		match := true
		for i, m := range g.Members {
			if m.AssetID != ids[i] {
				match = false
				break
			}
		}
		if match {
			return g.ID
		}
	}
	t.Fatalf("the group just recorded is not among the %d pending ones", len(groups))
	return ""
}

func TestRecordGroupsKeepsTheOrderItWasGiven(t *testing.T) {
	s := testStore(t)
	ids := seedCopies(t, s, 4)

	// Deliberately not the order they were created in: for a set of video
	// segments the position is the order they get concatenated in, and a store
	// that sorted them would run a minute of video backwards.
	want := []string{ids[2], ids[0], ids[3], ids[1]}
	id := recordGroup(t, s, merge.KindSegments, want...)

	group, err := s.Group(context.Background(), id)
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	for i, m := range group.Members {
		if m.AssetID != want[i] || m.Position != i {
			t.Fatalf("member %d is %s at position %d, want %s", i, m.AssetID, m.Position, want[i])
		}
	}
}

// A scan runs over the whole library every time and proposes the same groups it
// proposed last week. All but the first have to be a no-op, or the review page
// fills with the same question repeated.
func TestRecordGroupsIsIdempotent(t *testing.T) {
	s := testStore(t)
	ids := seedCopies(t, s, 3)
	groups := []merge.Group{{Kind: merge.KindDuplicate, IDs: ids}}

	created, err := s.RecordGroups(context.Background(), groups)
	if err != nil {
		t.Fatalf("RecordGroups: %v", err)
	}
	if created != 1 {
		t.Fatalf("first run created %d groups, want 1", created)
	}

	for range 3 {
		created, err := s.RecordGroups(context.Background(), groups)
		if err != nil {
			t.Fatalf("RecordGroups: %v", err)
		}
		if created != 0 {
			t.Fatalf("a repeat run created %d groups, want 0", created)
		}
	}

	// And the same members in a different order are the same question.
	shuffled := []merge.Group{{Kind: merge.KindDuplicate, IDs: []string{ids[2], ids[0], ids[1]}}}
	if created, err := s.RecordGroups(context.Background(), shuffled); err != nil {
		t.Fatalf("RecordGroups: %v", err)
	} else if created != 0 {
		t.Errorf("the same members in another order created %d groups, want 0", created)
	}
}

// A third copy turns up. The old question is about a set that no longer
// describes the situation, and leaving both would offer two overlapping choices
// whose answers contradict each other.
func TestRecordGroupsSupersedesAnOverlappingPendingGroup(t *testing.T) {
	s := testStore(t)
	ids := seedCopies(t, s, 3)

	recordGroup(t, s, merge.KindDuplicate, ids[0], ids[1])
	recordGroup(t, s, merge.KindDuplicate, ids[0], ids[1], ids[2])

	groups, err := s.Groups(context.Background(), MergeQuery{Kind: merge.KindDuplicate, State: MergePending, Limit: 100})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("%d pending groups, want 1", len(groups))
	}
	if len(groups[0].Members) != 3 {
		t.Errorf("the surviving group has %d members, want 3", len(groups[0].Members))
	}
}

// The merge is the point: the copies that go are not merely deleted, everything
// they knew moves onto the one that stays.
func TestMergeDuplicateCarriesAlbumsPeopleAndTheHeartOntoTheKeeper(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 3)
	keeper, loserA, loserB := ids[0], ids[1], ids[2]

	// The losers are the ones that were organised. This is the case the whole
	// carry-over exists for: keeping the bigger file would otherwise silently
	// drop the photograph out of two albums.
	if err := s.ApplyImportMetadata(ctx, loserA, ImportMetadata{
		Source:   SourceGoogleTakeout,
		Raw:      []byte(`{}`),
		Favorite: true,
		Albums:   []AlbumRef{{Title: "Iceland"}},
		People:   []string{"Ada"},
	}); err != nil {
		t.Fatalf("describe loser A: %v", err)
	}
	if err := s.ApplyImportMetadata(ctx, loserB, ImportMetadata{
		Source:      SourceGoogleTakeout,
		Raw:         []byte(`{}`),
		Description: "the good one",
		Albums:      []AlbumRef{{Title: "Best of 2019"}},
		People:      []string{"Grace"},
	}); err != nil {
		t.Fatalf("describe loser B: %v", err)
	}

	group := recordGroup(t, s, merge.KindDuplicate, keeper, loserA, loserB)
	result, err := s.MergeDuplicate(ctx, group, keeper)
	if err != nil {
		t.Fatalf("MergeDuplicate: %v", err)
	}
	if result.Keeper != keeper {
		t.Errorf("Keeper = %s, want %s", result.Keeper, keeper)
	}
	if result.Trashed != 2 {
		t.Errorf("Trashed = %d, want 2", result.Trashed)
	}

	kept, err := s.Asset(ctx, keeper)
	if err != nil {
		t.Fatalf("read the keeper: %v", err)
	}
	if !kept.Favorite {
		t.Error("the heart from a discarded copy did not move onto the keeper")
	}
	if kept.Description != "the good one" {
		t.Errorf("description = %q, want the caption from the discarded copy", kept.Description)
	}
	if kept.DeletedAt != nil {
		t.Error("the keeper was trashed")
	}

	albums, err := s.AlbumIDsOf(ctx, keeper)
	if err != nil {
		t.Fatalf("AlbumIDsOf: %v", err)
	}
	if len(albums) != 2 {
		t.Errorf("the keeper is in %d albums, want the 2 its copies were in", len(albums))
	}

	var people []string
	rows, err := s.pool.Query(ctx, `select name from asset_people where asset_id = $1::uuid order by name`, keeper)
	if err != nil {
		t.Fatalf("read people: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		people = append(people, name)
	}
	if !slices.Equal(people, []string{"Ada", "Grace"}) {
		t.Errorf("people on the keeper = %v, want [Ada Grace]", people)
	}

	for _, id := range []string{loserA, loserB} {
		gone, err := s.Asset(ctx, id)
		if err != nil {
			t.Fatalf("read a discarded copy: %v", err)
		}
		if gone.DeletedAt == nil {
			t.Errorf("copy %s was not moved to the trash", id)
		}
		if gone.PurgeAfter == nil {
			t.Errorf("copy %s went to the trash with no expiry", id)
		}
	}
}

// A caption already on the keeper is never replaced. The rule throughout the
// carry-over is additive: what is being merged is several records of one
// photograph, and the union is the only reading that cannot lose something.
func TestMergeDuplicateDoesNotOverwriteTheKeepersOwnCaption(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 2)

	for id, caption := range map[string]string{ids[0]: "mine", ids[1]: "theirs"} {
		if err := s.ApplyImportMetadata(ctx, id, ImportMetadata{
			Source: SourceGoogleTakeout, Raw: []byte(`{}`), Description: caption,
		}); err != nil {
			t.Fatalf("describe %s: %v", id, err)
		}
	}

	group := recordGroup(t, s, merge.KindDuplicate, ids[0], ids[1])
	if _, err := s.MergeDuplicate(ctx, group, ids[0]); err != nil {
		t.Fatalf("MergeDuplicate: %v", err)
	}

	kept, err := s.Asset(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if kept.Description != "mine" {
		t.Errorf("description = %q, want the keeper's own caption kept", kept.Description)
	}
}

// Two clicks on the same button must not trash the same photographs twice, and
// must not trash the copy that was kept the first time.
func TestMergeDuplicateRefusesToRunTwice(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 3)

	group := recordGroup(t, s, merge.KindDuplicate, ids...)
	if _, err := s.MergeDuplicate(ctx, group, ids[0]); err != nil {
		t.Fatalf("MergeDuplicate: %v", err)
	}

	_, err := s.MergeDuplicate(ctx, group, ids[1])
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("second merge error = %v, want ErrNotPending", err)
	}

	kept, err := s.Asset(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if kept.DeletedAt != nil {
		t.Error("the second merge trashed the copy the first one kept")
	}
}

func TestMergeDuplicateRefusesAKeeperFromOutsideTheGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 3)

	group := recordGroup(t, s, merge.KindDuplicate, ids[0], ids[1])
	if _, err := s.MergeDuplicate(ctx, group, ids[2]); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("error = %v, want ErrNotAMember", err)
	}

	// And nothing was trashed on the way to refusing.
	for _, id := range ids {
		a, err := s.Asset(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt != nil {
			t.Fatalf("%s was trashed by a refused merge", id)
		}
	}
}

// The segment half. The keeper is a file that did not exist when the group was
// found, and every member is discarded.
func TestMergeSegmentsTrashesEveryPiece(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 4)
	joined := seedAsset(t, s, 99, mergeEpoch)

	group := recordGroup(t, s, merge.KindSegments, pieces...)
	result, err := s.MergeSegments(ctx, group, joined)
	if err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}
	if result.Trashed != len(pieces) {
		t.Errorf("Trashed = %d, want %d", result.Trashed, len(pieces))
	}

	for _, id := range pieces {
		a, err := s.Asset(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt == nil {
			t.Errorf("piece %s is still in the library", id)
		}
	}
	kept, err := s.Asset(ctx, joined)
	if err != nil {
		t.Fatal(err)
	}
	if kept.DeletedAt != nil {
		t.Error("the joined recording was trashed by its own merge")
	}
}

func TestMergeSegmentsRefusesAJoinThatIsOneOfItsOwnParts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)

	group := recordGroup(t, s, merge.KindSegments, pieces...)
	if _, err := s.MergeSegments(ctx, group, pieces[0]); err == nil {
		t.Fatal("MergeSegments accepted one of the pieces as the joined recording")
	}
}

// The undo for the half of this feature that happens without being asked.
func TestUnmergeSegmentsPutsThePiecesBackAndTakesTheJoinAway(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	joined := seedAsset(t, s, 99, mergeEpoch)

	group := recordGroup(t, s, merge.KindSegments, pieces...)
	if _, err := s.MergeSegments(ctx, group, joined); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}

	out, err := s.UnmergeGroup(ctx, group)
	if err != nil {
		t.Fatalf("UnmergeGroup: %v", err)
	}
	if out.Restored != len(pieces) {
		t.Errorf("Restored = %d, want %d", out.Restored, len(pieces))
	}
	if out.Keeper != joined {
		t.Errorf("Keeper = %q, want the joined recording %s", out.Keeper, joined)
	}

	for _, id := range pieces {
		a, err := s.Asset(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt != nil {
			t.Errorf("piece %s did not come back out of the trash", id)
		}
	}
	gone, err := s.Asset(ctx, joined)
	if err != nil {
		t.Fatal(err)
	}
	if gone.DeletedAt == nil {
		t.Error("the joined recording is still in the library beside the pieces it was made from")
	}
}

// The whole reason an undo lands in `dismissed` rather than back in `pending`:
// segment groups are merged by a worker without anybody being asked, so a
// pending group would be re-joined within the minute.
func TestUnmergeLeavesTheGroupDismissed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	joined := seedAsset(t, s, 99, mergeEpoch)

	group := recordGroup(t, s, merge.KindSegments, pieces...)
	if _, err := s.MergeSegments(ctx, group, joined); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}
	if _, err := s.UnmergeGroup(ctx, group); err != nil {
		t.Fatalf("UnmergeGroup: %v", err)
	}

	got, err := s.Group(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != MergeDismissed {
		t.Errorf("state after an undo = %q, want %q", got.State, MergeDismissed)
	}

	// And the scan will not simply find it again.
	blocked, err := s.BlockedPairs(ctx)
	if err != nil {
		t.Fatalf("BlockedPairs: %v", err)
	}
	if !blocked.Blocked(pieces[0], pieces[1]) {
		t.Error("the pieces of an undone join are not blocked from being regrouped")
	}
}

func TestUnmergeRefusesAGroupThatWasNeverMerged(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 2)
	group := recordGroup(t, s, merge.KindDuplicate, ids...)

	if _, err := s.UnmergeGroup(ctx, group); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// A dismissal is not a delete. The row stays so that every pair inside it
// becomes a pair the scan will never link again.
func TestDismissBlocksEveryPairInTheGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 3)

	group := recordGroup(t, s, merge.KindDuplicate, ids...)
	if err := s.DismissGroup(ctx, group); err != nil {
		t.Fatalf("DismissGroup: %v", err)
	}

	blocked, err := s.BlockedPairs(ctx)
	if err != nil {
		t.Fatalf("BlockedPairs: %v", err)
	}
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if !blocked.Blocked(ids[i], ids[j]) {
				t.Errorf("pair (%d, %d) is not blocked", i, j)
			}
			// And the answer must not depend on which way round it is asked.
			if !blocked.Blocked(ids[j], ids[i]) {
				t.Errorf("pair (%d, %d) is not blocked in reverse", j, i)
			}
		}
	}

	if err := s.DismissGroup(ctx, group); !errors.Is(err, ErrNotPending) {
		t.Errorf("dismissing twice = %v, want ErrNotPending", err)
	}
}

func TestMergeCountsSeparatesTheTwoKinds(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 7)

	recordGroup(t, s, merge.KindDuplicate, ids[0], ids[1], ids[2])
	segments := recordGroup(t, s, merge.KindSegments, ids[3], ids[4])
	joined := seedAsset(t, s, 99, mergeEpoch)
	if _, err := s.MergeSegments(ctx, segments, joined); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}
	recordGroup(t, s, merge.KindSegments, ids[5], ids[6])

	counts, err := s.MergeCounts(ctx)
	if err != nil {
		t.Fatalf("MergeCounts: %v", err)
	}
	if counts.PendingDuplicates != 1 {
		t.Errorf("PendingDuplicates = %d, want 1", counts.PendingDuplicates)
	}
	if counts.DuplicateItems != 3 {
		t.Errorf("DuplicateItems = %d, want 3", counts.DuplicateItems)
	}
	if counts.MergedSegments != 1 {
		t.Errorf("MergedSegments = %d, want 1", counts.MergedSegments)
	}
	if counts.PendingSegments != 1 {
		t.Errorf("PendingSegments = %d, want 1", counts.PendingSegments)
	}
}

// Signatures round-trip through a column type that cannot represent half of
// them. Postgres has no unsigned integers, so every hash with its top bit set
// is stored as a negative number and has to come back identical.
func TestSignaturesSurviveTheSignedColumn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := seedAsset(t, s, 0, mergeEpoch)
	if err := s.ApplyMetadata(ctx, id, Metadata{Width: ptr(4032), Height: ptr(3024)}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	want := Signature{
		Version:         merge.SignatureVersion,
		Difference:      0xffff_ffff_ffff_ffff,
		Perceptual:      0x8000_0000_0000_0001,
		Aspect:          4.0 / 3.0,
		FrameDifference: []uint64{0, 1, 0xffff_ffff_ffff_ffff},
		FramePerceptual: []uint64{0xdead_beef_dead_beef, 2, 3},
	}
	if err := s.PutSignature(ctx, id, want); err != nil {
		t.Fatalf("PutSignature: %v", err)
	}

	got, err := s.SignaturesForScan(ctx)
	if err != nil {
		t.Fatalf("SignaturesForScan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d signatures, want 1", len(got))
	}
	if got[0].Difference != want.Difference || got[0].Perceptual != want.Perceptual {
		t.Errorf("hashes came back as %#016x/%#016x, want %#016x/%#016x",
			got[0].Difference, got[0].Perceptual, want.Difference, want.Perceptual)
	}
	if !slices.Equal(got[0].FrameDifference, want.FrameDifference) {
		t.Errorf("frame hashes = %#v, want %#v", got[0].FrameDifference, want.FrameDifference)
	}
	if !slices.Equal(got[0].FramePerceptual, want.FramePerceptual) {
		t.Errorf("frame hashes = %#v, want %#v", got[0].FramePerceptual, want.FramePerceptual)
	}
}

// An upsert, because the version is the point: a changed algorithm requeues
// every asset in the archive and each of those jobs finds a row already there.
func TestPutSignatureReplaces(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id := seedAsset(t, s, 0, mergeEpoch)

	if err := s.PutSignature(ctx, id, Signature{Version: merge.SignatureVersion - 1, Difference: 1}); err != nil {
		t.Fatalf("PutSignature: %v", err)
	}
	// Stale, so the scan cannot see it.
	if got, err := s.SignaturesForScan(ctx); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("a stale signature was returned to the scan")
	}

	if err := s.PutSignature(ctx, id, Signature{Version: merge.SignatureVersion, Difference: 2}); err != nil {
		t.Fatalf("PutSignature: %v", err)
	}
	got, err := s.SignaturesForScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Difference != 2 {
		t.Fatalf("got %d signatures; want the one that was written second", len(got))
	}
}

// The one place the two halves of this feature have to know about each other: a
// recording exported in six pieces is six rows that look alike, and offering
// them as duplicates would be asking somebody to approve throwing away five
// sixths of a minute of video.
func TestSignaturesForScanExcludesPendingSegments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 4)
	for i, id := range ids {
		if err := s.PutSignature(ctx, id, Signature{
			Version: merge.SignatureVersion, Difference: uint64(i), Aspect: 0.5,
		}); err != nil {
			t.Fatalf("PutSignature: %v", err)
		}
	}

	group := recordGroup(t, s, merge.KindSegments, ids[0], ids[1])

	got, err := s.SignaturesForScan(ctx)
	if err != nil {
		t.Fatalf("SignaturesForScan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d signatures, want the 2 that are not pending segments", len(got))
	}

	// Once the group is dismissed they are ordinary assets again: the scan was
	// told those six are not one recording, which says nothing about whether
	// any of them is a copy of something else.
	if err := s.DismissGroup(ctx, group); err != nil {
		t.Fatalf("DismissGroup: %v", err)
	}
	if got, err := s.SignaturesForScan(ctx); err != nil {
		t.Fatal(err)
	} else if len(got) != 4 {
		t.Errorf("%d signatures after the group was dismissed, want 4", len(got))
	}
}

// A deleted photograph is not a duplicate of anything: it is already gone, and
// the copies of it that remain are the library.
func TestSignaturesForScanIgnoresTheTrash(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 2)
	for i, id := range ids {
		if err := s.PutSignature(ctx, id, Signature{Version: merge.SignatureVersion, Difference: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Trash(ctx, Selection{IDs: []string{ids[0]}}); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	got, err := s.SignaturesForScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ids[1] {
		t.Errorf("got %d signatures; want only the one still in the library", len(got))
	}
}

func TestSignatureCoverageCountsWhatTheScanCanSee(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 5)
	for _, id := range ids[:2] {
		if err := s.PutSignature(ctx, id, Signature{Version: merge.SignatureVersion}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.SignatureCoverage(ctx)
	if err != nil {
		t.Fatalf("SignatureCoverage: %v", err)
	}
	if got.Assets != 5 || got.Signed != 2 {
		t.Errorf("coverage = %d of %d, want 2 of 5", got.Signed, got.Assets)
	}
}

func TestPendingGroupHeadsNamesTheFirstPiece(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 3)
	want := []string{ids[2], ids[0], ids[1]}
	recordGroup(t, s, merge.KindSegments, want...)

	heads, err := s.PendingGroupHeads(ctx, merge.KindSegments)
	if err != nil {
		t.Fatalf("PendingGroupHeads: %v", err)
	}
	if len(heads) != 1 || heads[0] != want[0] {
		t.Fatalf("heads = %v, want [%s]", heads, want[0])
	}

	group, err := s.PendingGroupWithMember(ctx, merge.KindSegments, want[0])
	if err != nil {
		t.Fatalf("PendingGroupWithMember: %v", err)
	}
	if len(group.Members) != 3 {
		t.Errorf("the group found from its head has %d members, want 3", len(group.Members))
	}
}

func TestGroupReportsAnUnknownID(t *testing.T) {
	s := testStore(t)
	_, err := s.Group(context.Background(), "9f8e7d6c-0000-4000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// The review page draws a comparison, so each member has to arrive carrying
// what the comparison is made on.
func TestGroupsDescribeEachMember(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 2)
	if err := s.ApplyMetadata(ctx, ids[0], Metadata{Width: ptr(4032), Height: ptr(3024)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyImportMetadata(ctx, ids[0], ImportMetadata{
		Source: SourceGoogleTakeout, Raw: []byte(`{}`),
		Albums: []AlbumRef{{Title: "Iceland"}}, People: []string{"Ada"},
	}); err != nil {
		t.Fatal(err)
	}
	recordGroup(t, s, merge.KindDuplicate, ids...)

	groups, err := s.Groups(ctx, MergeQuery{Kind: merge.KindDuplicate, State: MergePending, Limit: 10})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1", len(groups))
	}
	first := groups[0].Members[0]
	if first.Width == nil || *first.Width != 4032 {
		t.Errorf("width = %v, want 4032", first.Width)
	}
	if first.ByteSize == 0 {
		t.Error("byte size is missing, and it is half of what 'higher quality' means")
	}
	if !slices.Equal(first.Albums, []string{"Iceland"}) {
		t.Errorf("albums = %v, want [Iceland]", first.Albums)
	}
	if !slices.Equal(first.People, []string{"Ada"}) {
		t.Errorf("people = %v, want [Ada]", first.People)
	}
	if first.ImportSource != SourceGoogleTakeout {
		t.Errorf("import source = %q, want %q", first.ImportSource, SourceGoogleTakeout)
	}
}

func TestGroupsHonoursTheLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ids := seedCopies(t, s, 10)
	for i := 0; i < 10; i += 2 {
		recordGroup(t, s, merge.KindDuplicate, ids[i], ids[i+1])
	}

	groups, err := s.Groups(ctx, MergeQuery{Kind: merge.KindDuplicate, State: MergePending, Limit: 3})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("%d groups, want 3", len(groups))
	}
	for _, g := range groups {
		if len(g.Members) != 2 {
			t.Errorf("group %s came back with %d members, want 2", g.ID, len(g.Members))
		}
	}
}

func ptr[T any](v T) *T { return &v }

// failJoin puts the group's queued join into the state a worker that gave up
// leaves it in. The job is queued against the group's first member, which is
// the only thing the jobs table knows about a group.
func failJoin(t *testing.T, s *Store, group, head, message string) {
	t.Helper()
	ctx := context.Background()
	if err := jobs.Enqueue(ctx, s.pool, jobs.KindMerge, head); err != nil {
		t.Fatalf("enqueue the join: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		update jobs set state = 'failed', attempts = 5, last_error = $3, updated_at = now()
		where kind = $1 and asset_id = $2::uuid`, jobs.KindMerge, head, message); err != nil {
		t.Fatalf("fail the join: %v", err)
	}
}

// A join that gave up leaves the group pending — nothing about the photographs
// has changed — so it has to be findable by something other than the state
// column, and it has to bring the error with it.
func TestFailedFindsGroupsWhoseJoinGaveUp(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	group := recordGroup(t, s, merge.KindSegments, pieces...)

	const why = "join: 3 parts totalling 28.643s came out as 28.797s; refusing to archive that"
	failJoin(t, s, group, pieces[0], why)

	failed, err := s.Groups(ctx, MergeQuery{Kind: merge.KindSegments, State: MergeFailedState, Limit: 10})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("%d failed groups, want 1", len(failed))
	}
	if failed[0].ID != group {
		t.Errorf("found group %s, want %s", failed[0].ID, group)
	}
	if failed[0].State != MergePending {
		t.Errorf("state = %q, want it to still be pending", failed[0].State)
	}
	if failed[0].Failure == nil {
		t.Fatal("the failure did not come with it, so the page has nothing to explain the row with")
	}
	if failed[0].Failure.Error != why || failed[0].Failure.Attempts != 5 {
		t.Errorf("failure = %+v, want the error verbatim and 5 attempts", failed[0].Failure)
	}

	// And a group nobody has failed to join is not one of them.
	other := recordGroup(t, s, merge.KindSegments, seedTwo(t, s, 60)...)
	failed, err = s.Groups(ctx, MergeQuery{Kind: merge.KindSegments, State: MergeFailedState, Limit: 10})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(failed) != 1 || failed[0].ID == other {
		t.Errorf("%d failed groups, want only the one whose job failed", len(failed))
	}
}

// Approving is the only way the joined recordings log ever gets shorter. It
// changes nothing about the photographs, which is the whole reason it is safe.
func TestApproveTakesAJoinOffTheList(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	joined := seedAsset(t, s, 99, mergeEpoch)
	group := recordGroup(t, s, merge.KindSegments, pieces...)
	if _, err := s.MergeSegments(ctx, group, joined); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}

	merged := MergeQuery{Kind: merge.KindSegments, State: MergeMerged, Limit: 10}
	if groups, err := s.Groups(ctx, merged); err != nil || len(groups) != 1 {
		t.Fatalf("before approving: %d groups, %v; want 1", len(groups), err)
	}

	if err := s.ApproveGroup(ctx, group, true); err != nil {
		t.Fatalf("ApproveGroup: %v", err)
	}
	groups, err := s.Groups(ctx, merged)
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("%d groups after approving, want none on the default list", len(groups))
	}

	merged.Approved = true
	groups, err = s.Groups(ctx, merged)
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 1 || groups[0].ApprovedAt == nil {
		t.Fatalf("show-approved returned %d groups, want the approved one with its date", len(groups))
	}
	// Still merged, still undoable: approving is a note, not a resolution.
	if groups[0].State != MergeMerged || groups[0].KeeperAssetID == nil {
		t.Errorf("approving changed the group to %+v", groups[0])
	}

	if err := s.ApproveGroup(ctx, group, false); err != nil {
		t.Fatalf("ApproveGroup(false): %v", err)
	}
	if groups, err := s.Groups(ctx, MergeQuery{Kind: merge.KindSegments, State: MergeMerged, Limit: 10}); err != nil || len(groups) != 1 {
		t.Fatalf("after unapproving: %d groups, %v; want it back", len(groups), err)
	}
}

// A pending group has not happened yet, so there is nothing to have read.
func TestApproveRefusesAGroupThatWasNeverMerged(t *testing.T) {
	s := testStore(t)
	group := recordGroup(t, s, merge.KindSegments, seedCopies(t, s, 2)...)

	if err := s.ApproveGroup(context.Background(), group, true); !errors.Is(err, ErrNotMerged) {
		t.Errorf("ApproveGroup on a pending group = %v, want ErrNotMerged", err)
	}
}

// The counts behind the status card. An approved join stops being counted and a
// failed one is counted apart from the ones that are merely slow.
func TestMergeCountsSeparatesFailedAndApprovedSegments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	slow := recordGroup(t, s, merge.KindSegments, seedCopies(t, s, 2)...)
	_ = slow
	stuck := seedTwo(t, s, 10)
	stuckGroup := recordGroup(t, s, merge.KindSegments, stuck...)
	failJoin(t, s, stuckGroup, stuck[0], "no")

	done := seedTwo(t, s, 20)
	doneGroup := recordGroup(t, s, merge.KindSegments, done...)
	if _, err := s.MergeSegments(ctx, doneGroup, seedAsset(t, s, 90, mergeEpoch)); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}

	counts, err := s.MergeCounts(ctx)
	if err != nil {
		t.Fatalf("MergeCounts: %v", err)
	}
	if counts.PendingSegments != 1 || counts.FailedSegments != 1 || counts.MergedSegments != 1 || counts.ApprovedSegments != 0 {
		t.Fatalf("counts = %+v, want one still being joined, one failed, one merged", counts)
	}

	if err := s.ApproveGroup(ctx, doneGroup, true); err != nil {
		t.Fatalf("ApproveGroup: %v", err)
	}
	counts, err = s.MergeCounts(ctx)
	if err != nil {
		t.Fatalf("MergeCounts: %v", err)
	}
	if counts.MergedSegments != 0 || counts.ApprovedSegments != 1 {
		t.Errorf("after approving: merged = %d, approved = %d; want 0 and 1",
			counts.MergedSegments, counts.ApprovedSegments)
	}
}

// seedTwo is two more assets with digests that will not collide with the ones
// seedCopies already handed out.
func seedTwo(t *testing.T, s *Store, from int) []string {
	t.Helper()
	return []string{
		seedAsset(t, s, from, mergeEpoch),
		seedAsset(t, s, from+1, mergeEpoch.Add(10*time.Second)),
	}
}

// Overruling the duration check puts the work back on the queue, and says so on
// the group rather than on the job — every retry has to make the same choice.
func TestForceJoinFlagsTheGroupAndRequeuesTheWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	group := recordGroup(t, s, merge.KindSegments, pieces...)
	failJoin(t, s, group, pieces[0], "the parts do not add up")

	if err := s.ForceJoin(ctx, group); err != nil {
		t.Fatalf("ForceJoin: %v", err)
	}

	g, err := s.Group(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Forced {
		t.Error("the override was not recorded on the group, so the next attempt would refuse again")
	}

	var state string
	var attempts int
	if err := s.pool.QueryRow(ctx, `
		select state, attempts from jobs where kind = $1 and asset_id = $2::uuid`,
		jobs.KindMerge, pieces[0]).Scan(&state, &attempts); err != nil {
		t.Fatalf("read the job: %v", err)
	}
	if state != string(jobs.StatePending) || attempts != 0 {
		t.Errorf("job is %s after %d attempts, want pending from scratch", state, attempts)
	}

	// And a group that has already been resolved is not something to force.
	other := recordGroup(t, s, merge.KindSegments, seedTwo(t, s, 40)...)
	if _, err := s.MergeSegments(ctx, other, seedAsset(t, s, 91, mergeEpoch)); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}
	if err := s.ForceJoin(ctx, other); !errors.Is(err, ErrNotPending) {
		t.Errorf("ForceJoin on a merged group = %v, want ErrNotPending", err)
	}
}

// Refusing a join takes the work off the queue with it. Without that the status
// page goes on reporting a job that gave up on something nobody wants done.
func TestDismissTakesTheFailedJoinOffTheQueue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	group := recordGroup(t, s, merge.KindSegments, pieces...)
	failJoin(t, s, group, pieces[0], "the parts do not add up")

	if err := s.DismissGroup(ctx, group); err != nil {
		t.Fatalf("DismissGroup: %v", err)
	}

	var left int
	if err := s.pool.QueryRow(ctx,
		`select count(*)::int from jobs where kind = $1`, jobs.KindMerge).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d merge jobs left after the group was refused, want none", left)
	}
}

// The list a sweep reconciles the derivative tree against. A group qualifies
// while it is still a question with parts to answer it, and not afterwards.
func TestSegmentPreviewsListsOnlyGroupsStillWaitingToBeJoined(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	pieces := seedCopies(t, s, 3)
	waiting := recordGroup(t, s, merge.KindSegments, pieces...)

	resolved := recordGroup(t, s, merge.KindSegments, seedTwo(t, s, 30)...)
	if _, err := s.MergeSegments(ctx, resolved, seedAsset(t, s, 92, mergeEpoch)); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}
	refused := recordGroup(t, s, merge.KindSegments, seedTwo(t, s, 50)...)
	if err := s.DismissGroup(ctx, refused); err != nil {
		t.Fatalf("DismissGroup: %v", err)
	}

	want, err := s.Group(ctx, waiting)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SegmentPreviews(ctx)
	if err != nil {
		t.Fatalf("SegmentPreviews: %v", err)
	}
	if len(got) != 1 || got[0] != want.Fingerprint {
		t.Fatalf("SegmentPreviews = %v, want just %s", got, want.Fingerprint)
	}

	// A group whose parts have been destroyed is not owed a preview either,
	// even though nothing has answered its question.
	if _, err := s.pool.Exec(ctx, `delete from assets where id = any($1::uuid[])`, pieces); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SegmentPreviews(ctx); err != nil {
		t.Fatalf("SegmentPreviews: %v", err)
	} else if len(got) != 0 {
		t.Errorf("SegmentPreviews = %v after its parts were destroyed, want none", got)
	}
}

// An undo is a restore, and restores rebuild search rows. A merge can sit
// merged for as long as anybody leaves it there, so the rebuild that took the
// copies' tsvectors away has almost certainly happened by the time somebody
// changes their mind — see db.Store.Restore.
func TestUnmergeGivesThePiecesBackTheirSearchRows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pieces := seedCopies(t, s, 3)
	joined := seedAsset(t, s, 99, mergeEpoch)

	for _, id := range pieces {
		if err := s.PutDescription(ctx, id, CaptionModel,
			"A golden retriever running along a beach at sunset", nil); err != nil {
			t.Fatalf("PutDescription: %v", err)
		}
	}

	group := recordGroup(t, s, merge.KindSegments, pieces...)
	if _, err := s.MergeSegments(ctx, group, joined); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}
	if _, err := s.RefreshSearch(ctx); err != nil {
		t.Fatalf("RefreshSearch: %v", err)
	}

	if _, err := s.UnmergeGroup(ctx, group); err != nil {
		t.Fatalf("UnmergeGroup: %v", err)
	}
	for _, id := range pieces {
		if !matches(t, s, id, "golden retriever") {
			t.Errorf("piece %s came back out of the trash without its tsvector", id)
		}
	}
}
