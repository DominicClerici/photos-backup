package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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
	groups, err := s.Groups(context.Background(), kind, MergePending, 100)
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

	groups, err := s.Groups(context.Background(), merge.KindDuplicate, MergePending, 100)
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

	groups, err := s.Groups(ctx, merge.KindDuplicate, MergePending, 10)
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

	groups, err := s.Groups(ctx, merge.KindDuplicate, MergePending, 3)
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
