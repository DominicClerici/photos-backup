package db

import (
	"context"
	"testing"
	"time"
)

// The panel's whole job, in one asset: everything three models said, read back
// together, with the merge resolved on the way out.
func TestAssetAnalysisReadsBackWhatTheModelsSaid(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, store, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	if err := store.PutDescription(ctx, id, CaptionModel,
		"A golden retriever running along a beach at sunset",
		[]Tag{{Name: "puppy", Confidence: 0.4}, {Name: "beach", Confidence: 0.9}},
	); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	if err := store.PutOCR(ctx, id, OCRModel, "SUNSET BEACH PARKING $4"); err != nil {
		t.Fatalf("PutOCR: %v", err)
	}
	if err := store.PutEmbeddings(ctx, id, VisionModel, []Embedding{{Frame: 0, Vector: unit(1)}}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}
	mergeTag(t, store, "puppy", "dog")

	a, err := store.AssetAnalysis(ctx, id)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}

	if a.Caption == "" || a.CaptionModel != CaptionModel || a.CaptionedAt == nil {
		t.Errorf("caption = %q from %q at %v, want all three", a.Caption, a.CaptionModel, a.CaptionedAt)
	}
	if a.Text != "SUNSET BEACH PARKING $4" || a.TextModel != OCRModel {
		t.Errorf("recognised text = %q from %q", a.Text, a.TextModel)
	}
	if a.Frames != 1 || a.VisionModel != VisionModel {
		t.Errorf("frames = %d from %q, want 1 from the encoder", a.Frames, a.VisionModel)
	}

	// Confidence order, not alphabetical: the order the captioner was surest in
	// is the only order anything here chose.
	if len(a.Tags) != 2 || a.Tags[0].Name != "beach" {
		t.Fatalf("tags = %+v, want beach first", a.Tags)
	}
	// The merge, visible from both sides. A search resolves this photograph's
	// "puppy" to "dog", and the panel can still say which word was written —
	// which is the whole of what makes ML_IMAGES.md §9's cleanup reviewable.
	if a.Tags[1].Name != "dog" || a.Tags[1].Raw != "puppy" {
		t.Errorf("merged tag = %+v, want dog written as puppy", a.Tags[1])
	}
	// And an unmerged word does not claim to have been written as itself.
	if a.Tags[0].Raw != "" {
		t.Errorf("unmerged tag carries raw = %q, want empty", a.Tags[0].Raw)
	}
}

// The distinction the panel exists to draw: a photograph nothing has looked at
// is not a photograph with nothing in it.
func TestAssetAnalysisReportsWhatHasNotRunYet(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, store, 2, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	a, err := store.AssetAnalysis(ctx, id)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if a.Caption != "" || len(a.Tags) != 0 || a.Frames != 0 {
		t.Errorf("a fresh asset already has an analysis: %+v", a)
	}
	// RecordAsset queues the metadata job, and mlprep comes off the back of it.
	// Whatever is or is not queued, the map is the evidence — a caller with no
	// job states cannot tell "queued" from "nothing will ever run".
	if _, ok := a.Jobs["describe"]; ok {
		t.Errorf("nothing should have queued the captioner: %v", a.Jobs)
	}

	if _, err := store.pool.Exec(ctx,
		`insert into jobs (kind, asset_id, state) values ('describe', $1::uuid, 'failed')`, id); err != nil {
		t.Fatalf("queue a failed describe: %v", err)
	}
	a, err = store.AssetAnalysis(ctx, id)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if a.Jobs["describe"] != "failed" {
		t.Errorf("job states = %v, want the captioner reported as failed", a.Jobs)
	}
}

// The vault's objection is to a legible description of what it is holding, and
// a caption is the most legible one this server produces. The write paths all
// refuse a sealed asset; this is the read path agreeing.
func TestAssetAnalysisSaysNothingAboutASealedPhotograph(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := seedAsset(t, store, 3, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))

	if err := store.PutDescription(ctx, id, CaptionModel, "a dog", []Tag{{Name: "dog"}}); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	if _, err := store.pool.Exec(ctx,
		`update assets set vault = 'hidden' where id = $1::uuid`, id); err != nil {
		t.Fatalf("seal the asset: %v", err)
	}

	a, err := store.AssetAnalysis(ctx, id)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if a.Caption != "" || a.Frames != 0 {
		t.Errorf("a sealed photograph described itself: %+v", a)
	}
}
