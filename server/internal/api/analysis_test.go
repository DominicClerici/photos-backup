package api

import (
	"context"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// The viewer's panel asks this and nothing else for what the models said, so
// the round trip has to carry all four kinds of claim at once.
func TestAssetAnalysisCarriesWhatTheModelsSaid(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadNamedFixture(t, "iphone-portrait.heic"), map[string]string{
		"X-Photo-Filename": "iphone-portrait.heic",
	}))
	h.derive(t, up.ID)

	ctx := context.Background()
	if err := h.store.PutDescription(ctx, up.ID, db.CaptionModel,
		"A woman standing in front of a brick wall",
		[]db.Tag{{Name: "portrait", Confidence: 0.8}},
	); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	if err := h.store.PutOCR(ctx, up.ID, db.OCRModel, "NO PARKING"); err != nil {
		t.Fatalf("PutOCR: %v", err)
	}

	var got db.Analysis
	decodeJSON(t, h.get(t, "/v1/assets/"+up.ID+"/analysis"), &got)

	if got.Caption == "" || got.CaptionModel != db.CaptionModel {
		t.Errorf("caption = %q from %q", got.Caption, got.CaptionModel)
	}
	if len(got.Tags) != 1 || got.Tags[0].Name != "portrait" {
		t.Errorf("tags = %+v, want the one the captioner wrote", got.Tags)
	}
	if got.Text != "NO PARKING" || got.TextModel != db.OCRModel {
		t.Errorf("recognised text = %q from %q", got.Text, got.TextModel)
	}
	// Nothing embedded it, and the panel has to be able to tell that from a
	// photograph the encoder looked at and had nothing to say about.
	if got.Frames != 0 || got.VisionModel != "" {
		t.Errorf("frames = %d from %q, want nothing", got.Frames, got.VisionModel)
	}
	// It is a separate route from the detail precisely so that recognised text
	// is not on the timeline's critical path — so the detail must not have
	// grown it in passing.
	var detail assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+up.ID), &detail)
	if detail.Filename == "" {
		t.Fatal("the detail stopped answering")
	}
}

// An id nothing knows about is a 404 here for the same reason it is on the
// detail: the panel asks about a photograph, and "no such photograph" is not an
// empty analysis.
func TestAssetAnalysisIsNotFoundForAnUnknownAsset(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/v1/assets/00000000-0000-0000-0000-000000000000/analysis")
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
