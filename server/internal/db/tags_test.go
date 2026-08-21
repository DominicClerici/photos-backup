package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// tagID returns the id of a word, creating it if the vocabulary has not seen it.
func tagID(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	var id int64
	err := store.pool.QueryRow(context.Background(), `
		insert into tags (name) values ($1)
		on conflict (name) do update set name = excluded.name
		returning id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("intern %q: %v", name, err)
	}
	return id
}

// describeWith is the shortest route to a photograph carrying some words, and
// it goes through the real write path so the search rows are real too.
func describeWith(t *testing.T, store *Store, n int, caption string, words ...string) string {
	t.Helper()
	id := seedAsset(t, store, n, time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))
	tags := make([]Tag, len(words))
	for i, word := range words {
		tags[i] = Tag{Name: word, Confidence: 0.9}
	}
	if err := store.PutDescription(context.Background(), id, CaptionModel, caption, tags); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	return id
}

// The rule that makes the triage re-runnable over a vocabulary that has grown:
// a model may fill in a blank and may never overrule an answer somebody gave.
func TestPutTriageWillNotOverruleAPerson(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	describeWith(t, store, 1, "a screenshot of a login form", "screenshot", "login")

	screenshot, login := tagID(t, store, "screenshot"), tagID(t, store, "login")

	// The model junks both; nobody has said anything yet, so both stand.
	if _, err := store.PutTriage(ctx, []TagVerdict{
		{ID: screenshot, Junk: true, Score: 0.94},
		{ID: login, Junk: true, Score: 0.99},
	}); err != nil {
		t.Fatalf("PutTriage: %v", err)
	}

	// A person disagrees about one of them.
	if _, err := store.JudgeTags(ctx, []int64{screenshot}, false); err != nil {
		t.Fatalf("JudgeTags: %v", err)
	}

	// A second pass — a bigger vocabulary, a swapped model — says junk again.
	if _, err := store.PutTriage(ctx, []TagVerdict{
		{ID: screenshot, Junk: true, Score: 0.98},
		{ID: login, Junk: true, Score: 0.99},
	}); err != nil {
		t.Fatalf("second PutTriage: %v", err)
	}

	kept, _, err := store.TagWords(ctx, TagWordQuery{})
	if err != nil {
		t.Fatalf("TagWords: %v", err)
	}
	if len(kept) != 1 || kept[0].Name != "screenshot" {
		t.Fatalf("kept words = %v, want screenshot alone: a person's answer was overruled", names(kept))
	}
	if kept[0].JudgedAt == nil {
		t.Error("a word a person kept does not carry judged_at")
	}
}

// Junk is not a label, it is a removal: the word stops being something the
// archive can be searched by, everywhere at once.
func TestJunkTagsLeaveTheSearchIndexAndTheVocabulary(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	// The caption deliberately says none of these words: what is under test is the
	// tag half of the tsvector, and a caption mentioning "login" would match
	// through weight A whatever the vocabulary said.
	id := describeWith(t, store, 2, "a phone held up in front of a window", "screenshot", "login")

	if !matches(t, store, id, "login") {
		t.Fatal("a freshly written tag is not in the search index")
	}

	if _, err := store.JudgeTags(ctx, []int64{tagID(t, store, "login")}, true); err != nil {
		t.Fatalf("JudgeTags: %v", err)
	}

	// The photograph is still findable by the word that survived, and no longer
	// by the one that did not. No job ran and nothing was re-captioned: the
	// tsvector was rebuilt inside the same transaction, which is the obligation
	// ML_IMAGES.md §11 said somebody would have to remember.
	if matches(t, store, id, "login") {
		t.Error("a junked word is still in the search index")
	}
	if !matches(t, store, id, "screenshot") {
		t.Error("junking one word took another out of the index with it")
	}

	// And the query parser stops offering it, for the same reason.
	vocab, err := store.SearchVocabulary(ctx)
	if err != nil {
		t.Fatalf("SearchVocabulary: %v", err)
	}
	if _, ok := vocab.Tags["login"]; ok {
		t.Error("a junked word is still in the parser's vocabulary")
	}
	if _, ok := vocab.Tags["screenshot"]; !ok {
		t.Error("a kept word is missing from the parser's vocabulary")
	}

	// The claim itself is untouched, which is what makes this undoable.
	if _, err := store.JudgeTags(ctx, []int64{tagID(t, store, "login")}, false); err != nil {
		t.Fatalf("JudgeTags back: %v", err)
	}
	if !matches(t, store, id, "login") {
		t.Error("un-junking a word did not put it back in the index")
	}
}

// The merge, and the thing about it that is easy to get wrong: canonical_id is
// resolved exactly one hop everywhere it is read, so a merge onto a word that
// was itself a head has to bring that head's children with it.
func TestMergeTagsRepointsAnEarlierMergesChildren(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	doggo := describeWith(t, store, 3, "a small dog", "doggo")
	describeWith(t, store, 4, "a small dog", "puppy")

	// doggo → puppy, then puppy → dog. Without the repointing, doggo would
	// resolve to puppy — a word that is no longer canonical — and the
	// photograph would be findable as neither.
	if _, err := store.MergeTags(ctx, tagID(t, store, "puppy"), []int64{tagID(t, store, "doggo")}, nil); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if _, err := store.MergeTags(ctx, tagID(t, store, "dog"), []int64{tagID(t, store, "puppy")}, nil); err != nil {
		t.Fatalf("second merge: %v", err)
	}

	if !matches(t, store, doggo, "dog") {
		t.Error("a photograph called doggo is not findable as dog after two merges")
	}

	a, err := store.AssetAnalysis(ctx, doggo)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if len(a.Tags) != 1 || a.Tags[0].Name != "dog" || a.Tags[0].Raw != "doggo" {
		t.Errorf("tags = %+v, want dog written as doggo", a.Tags)
	}
}

// A merge into a word that is itself folded is refused rather than followed,
// because following it would build the chain the test above exists to prevent.
func TestMergeTagsRefusesAFoldedHead(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	describeWith(t, store, 5, "a small dog", "puppy", "doggo")

	if _, err := store.MergeTags(ctx, tagID(t, store, "dog"), []int64{tagID(t, store, "puppy")}, nil); err != nil {
		t.Fatalf("merge: %v", err)
	}
	_, err := store.MergeTags(ctx, tagID(t, store, "puppy"), []int64{tagID(t, store, "doggo")}, nil)
	if !errors.Is(err, ErrTagFolded) {
		t.Errorf("merging into a folded word: %v, want ErrTagFolded", err)
	}
	_, err = store.MergeTags(ctx, 99999, []int64{tagID(t, store, "doggo")}, nil)
	if !errors.Is(err, ErrNoSuchTag) {
		t.Errorf("merging into a word that is not there: %v, want ErrNoSuchTag", err)
	}
}

// Undoing a merge is clearing one column, and the search index has to follow it
// back the same way it followed it out.
func TestUnmergeTagsPutsAWordBack(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := describeWith(t, store, 6, "a small dog", "puppy")

	if _, err := store.MergeTags(ctx, tagID(t, store, "dog"), []int64{tagID(t, store, "puppy")}, nil); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if matches(t, store, id, "puppy") {
		t.Error("a folded word is still searchable under its own name")
	}

	restored, err := store.UnmergeTags(ctx, []int64{tagID(t, store, "puppy")})
	if err != nil {
		t.Fatalf("UnmergeTags: %v", err)
	}
	if restored != 1 {
		t.Errorf("restored = %d, want 1", restored)
	}
	if !matches(t, store, id, "puppy") {
		t.Error("undoing a merge did not put the word back in the index")
	}
}

// The clustering: groups form around the most-used word, a rejected pair never
// comes back, and a word that has been answered is not asked about again.
func TestTagProposalsClusterAroundTheMostUsedWord(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Three photographs of a dog and one of a puppy, so "dog" leads on use.
	for i, word := range []string{"dog", "dog", "dog", "puppy", "beach"} {
		describeWith(t, store, 10+i, "a dog", word)
	}

	// Vectors chosen rather than measured: two words a hair apart and a third
	// on its own. What is under test is the grouping rule, not the encoder.
	near := unit(1)
	alike := append([]float32(nil), near...)
	alike[1] = 0.05
	if err := store.PutTagEmbeddings(ctx, VisionModel,
		[]int64{tagID(t, store, "dog"), tagID(t, store, "puppy"), tagID(t, store, "beach")},
		[][]float32{near, alike, unit(2)}); err != nil {
		t.Fatalf("PutTagEmbeddings: %v", err)
	}

	groups, err := store.TagProposals(ctx, TagProposalQuery{Similarity: 0.9})
	if err != nil {
		t.Fatalf("TagProposals: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want one: %v", len(groups), groups)
	}
	// The head is the word the archive actually speaks, not the first one seen.
	if groups[0].Canonical.Name != "dog" {
		t.Errorf("head = %q, want dog: three photographs say dog and one says puppy", groups[0].Canonical.Name)
	}
	if len(groups[0].Members) != 1 || groups[0].Members[0].Name != "puppy" {
		t.Errorf("members = %v, want puppy alone", names(groups[0].Members))
	}
	if groups[0].Uses != 4 {
		t.Errorf("group uses = %d, want 4", groups[0].Uses)
	}

	// Rejected, and it stays rejected: a clustering run over unchanged vectors
	// would otherwise propose exactly the group somebody just said no to.
	if _, err := store.DismissTagProposal(ctx, []int64{tagID(t, store, "dog"), tagID(t, store, "puppy")}); err != nil {
		t.Fatalf("DismissTagProposal: %v", err)
	}
	groups, err = store.TagProposals(ctx, TagProposalQuery{Similarity: 0.9})
	if err != nil {
		t.Fatalf("TagProposals after dismiss: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("a dismissed pair was proposed again: %v", groups)
	}
}

// A word that has been answered — junked, or folded into another — stops being a
// candidate, and its vector goes with it so it cannot use up a neighbour's slot.
func TestAnsweredWordsLeaveTheClustering(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	describeWith(t, store, 20, "a screenshot", "screenshot", "puppy", "dog")

	ids := []int64{tagID(t, store, "screenshot"), tagID(t, store, "puppy"), tagID(t, store, "dog")}
	if err := store.PutTagEmbeddings(ctx, VisionModel, ids,
		[][]float32{unit(1), unit(2), unit(3)}); err != nil {
		t.Fatalf("PutTagEmbeddings: %v", err)
	}

	counts, err := store.TagCleanupCounts(ctx)
	if err != nil {
		t.Fatalf("TagCleanupCounts: %v", err)
	}
	if counts.Unembedded != 0 || counts.Kept != 3 {
		t.Fatalf("counts = %+v, want three kept words all embedded", counts)
	}

	if _, err := store.JudgeTags(ctx, ids[:1], true); err != nil {
		t.Fatalf("JudgeTags: %v", err)
	}
	if _, err := store.MergeTags(ctx, ids[2], ids[1:2], nil); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	words, err := store.candidateWords(ctx)
	if err != nil {
		t.Fatalf("candidateWords: %v", err)
	}
	if len(words) != 1 || words[0].Name != "dog" {
		t.Errorf("candidates = %v, want dog alone", names(words))
	}

	// And un-junking it makes it a candidate owed a vector again, rather than
	// one that quietly kept the old one.
	if _, err := store.JudgeTags(ctx, ids[:1], false); err != nil {
		t.Fatalf("JudgeTags back: %v", err)
	}
	counts, err = store.TagCleanupCounts(ctx)
	if err != nil {
		t.Fatalf("TagCleanupCounts: %v", err)
	}
	if counts.Unembedded != 1 {
		t.Errorf("unembedded = %d, want the restored word to be owed a vector", counts.Unembedded)
	}
}

// Approving changes no verdict. What it changes is whose verdict each one is,
// and it is the moment the whole index is made true.
func TestApproveTriageStampsTheModelsVerdicts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	describeWith(t, store, 30, "a screenshot of a login form", "screenshot", "login")

	if _, err := store.PutTriage(ctx, []TagVerdict{
		{ID: tagID(t, store, "screenshot"), Junk: false, Score: 0.2},
		{ID: tagID(t, store, "login"), Junk: true, Score: 0.99},
	}); err != nil {
		t.Fatalf("PutTriage: %v", err)
	}

	before, err := store.TagCleanupCounts(ctx)
	if err != nil {
		t.Fatalf("TagCleanupCounts: %v", err)
	}
	if before.Unreviewed != 2 || before.Untriaged != 0 {
		t.Fatalf("counts = %+v, want two verdicts waiting to be read", before)
	}

	approved, reindexed, err := store.ApproveTriage(ctx)
	if err != nil {
		t.Fatalf("ApproveTriage: %v", err)
	}
	if approved != 2 {
		t.Errorf("approved = %d, want 2", approved)
	}
	if reindexed == 0 {
		t.Error("approving rebuilt nothing; the bulk pass never rebuilds as it goes, so this is the only rebuild")
	}

	after, err := store.TagCleanupCounts(ctx)
	if err != nil {
		t.Fatalf("TagCleanupCounts: %v", err)
	}
	if after.Unreviewed != 0 || after.Junk != 1 || after.Kept != 1 {
		t.Errorf("counts = %+v, want one junk and one kept, none waiting", after)
	}
}

func names(words []TagWord) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w.Name
	}
	return out
}
