package worker

import (
	"context"
	"sort"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// describeFrames is how many of an asset's ML renditions go to the heavy models.
//
// One for a photograph, and at most three for a video — the first, the middle
// and the last of whatever mlprep managed to sample. Three rather than six
// because the captioner is the expensive pass in the whole system and a clip's
// fourth frame rarely says anything its second did not; three rather than one
// because a video that opens on a beach and ends in a restaurant is two
// photographs as far as anybody searching is concerned, and captioning only the
// middle would make it neither.
//
// It is the same three for the recogniser, which matters more than it sounds:
// a Snapchat memory carries its caption burned into the frame, and mlprep burns
// the overlay in on purpose so that the text recognition pass is exactly what
// reads somebody's handwriting back out of it.
const describeFrames = 3

// runDescribe asks what a photograph is of, and writes the answer down in
// words.
//
// The same shape as runVision and for the same reasons: it reads files, posts
// bytes over loopback, and stores rows. Go decodes, Python does tensors, and
// photo-ml never opens a file under /mnt/photos.
func (r *Runner) runDescribe(ctx context.Context, assetID string) error {
	asset, ok, err := r.describable(ctx, assetID)
	if err != nil || !ok {
		return err
	}

	images, err := r.sampledRenditions(asset)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		r.log().Debug("no ML renditions to describe; the asset will not be searchable by what it shows",
			"asset", asset.ID, "kind", asset.MediaKind)
		return nil
	}

	result, err := r.ML.Describe(ctx, images)
	if err != nil {
		return err
	}
	r.checkModel(&r.wrongCaptioner, "captioner", result.Model, db.CaptionModel)

	caption, tags := foldDescriptions(result.Results)
	return r.Store.PutDescription(ctx, asset.ID, result.Model, caption, tags)
}

// runOCR reads whatever text is in the photograph.
//
// Cheap next to the captioner and worth having first: it is the whole of what
// makes a screenshot, a receipt or a road sign findable by what it says, and it
// finishes in the time the captioner spends on the first thousand photographs.
func (r *Runner) runOCR(ctx context.Context, assetID string) error {
	asset, ok, err := r.describable(ctx, assetID)
	if err != nil || !ok {
		return err
	}

	images, err := r.sampledRenditions(asset)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return nil
	}

	result, err := r.ML.Recognize(ctx, images)
	if err != nil {
		return err
	}
	r.checkModel(&r.wrongRecogniser, "text recogniser", result.Model, db.OCRModel)

	// An empty answer is stored rather than skipped, and it is the difference
	// between "there is no text in this photograph" and "nobody has looked".
	// Without the row, the backfill would offer every wordless photograph in
	// the archive again on every run — which is 90% of it.
	return r.Store.PutOCR(ctx, asset.ID, result.Model, foldText(result.Results))
}

// describable applies the four guards every ML job applies, and reports whether
// there is anything to do.
//
// Repeated rather than inferred from the absence of renditions, so that the
// jobs cannot drift into disagreeing with mlprep about what an item is. The
// vault case is the one with the least room for argument here: a caption is the
// most legible description of a photograph this server ever writes, and writing
// one for something in the vault would be recording in plain English the thing
// the vault exists to stop it knowing.
func (r *Runner) describable(ctx context.Context, assetID string) (db.Asset, bool, error) {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return db.Asset{}, false, err
	}
	switch {
	case vaulted(asset), asset.DeletedAt != nil, asset.IsOverlay, asset.IsLivePair():
		return asset, false, nil
	}
	return asset, true, nil
}

// sampledRenditions reads what mlprep left on disk and thins a video down to
// describeFrames of it.
//
// First, middle, last of what actually exists rather than of what was asked for:
// a clip shorter than the sampling interval yields fewer frames, and reaching
// for the sixth would be reaching for a file that is not there.
func (r *Runner) sampledRenditions(asset db.Asset) ([][]byte, error) {
	_, images, err := r.renditions(asset)
	if err != nil || len(images) <= describeFrames {
		return images, err
	}
	last := len(images) - 1
	return [][]byte{images[0], images[last/2], images[last]}, nil
}

// foldDescriptions turns several frames' worth of answers into one asset's.
//
// The captions are joined rather than picked between, because they are
// describing different moments of the same clip and there is no basis for
// preferring one — a video that goes from a beach to a restaurant is both, and
// the whole point of sampling more than one frame is to say so. Duplicates go,
// because a static clip produces the same sentence three times and three copies
// of it in the tsvector would weight that video as though somebody had described
// it very carefully.
//
// The tags are unioned at their highest confidence. A word seen in one frame of
// three is still a word about the video.
func foldDescriptions(results []mlclient.Description) (string, []db.Tag) {
	var captions []string
	seen := map[string]bool{}
	best := map[string]float32{}

	for _, result := range results {
		caption := strings.TrimSpace(result.Caption)
		if caption != "" && !seen[strings.ToLower(caption)] {
			seen[strings.ToLower(caption)] = true
			captions = append(captions, caption)
		}
		for _, tag := range result.Tags {
			name := strings.ToLower(strings.TrimSpace(tag.Name))
			if name != "" && tag.Confidence >= best[name] {
				best[name] = tag.Confidence
			}
		}
	}

	tags := make([]db.Tag, 0, len(best))
	for name, confidence := range best {
		tags = append(tags, db.Tag{Name: name, Confidence: confidence})
	}
	// Sorted by confidence so that db.normalizeTags, which keeps the first
	// twelve, keeps the twelve the model was surest of rather than the twelve
	// that happened to hash first.
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Confidence != tags[j].Confidence {
			return tags[i].Confidence > tags[j].Confidence
		}
		return tags[i].Name < tags[j].Name
	})

	// A separator that reads as a boundary to a person and tokenizes as nothing
	// to Postgres, so two captions do not become one sentence that neither
	// frame contained.
	return strings.Join(captions, " · "), tags
}

// foldText joins several frames' recognised text, dropping the lines that
// repeat.
//
// A burned-in Snapchat caption is on every frame of the clip by construction,
// and storing it three times would make that video rank as though the words
// were three times as present as they are.
func foldText(results []mlclient.Recognition) string {
	var lines []string
	seen := map[string]bool{}
	for _, result := range results {
		for _, line := range strings.Split(result.Text, "\n") {
			line = strings.TrimSpace(line)
			key := strings.ToLower(line)
			if line == "" || seen[key] {
				continue
			}
			seen[key] = true
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
