package db

import (
	"context"
	"strings"
	"testing"
)

func TestPutEmbeddingsStoresOneRowPerFrame(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := recordTestAsset(t, store, "clip.mov", MediaVideo)

	if err := store.PutEmbeddings(ctx, id, VisionModel, []Embedding{
		{Frame: 0, Vector: unit(0)},
		{Frame: 1, Vector: unit(1)},
		{Frame: 2, Vector: unit(2)},
	}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}

	// Six frames of a clip are six chances to match, which is the whole reason
	// the table is keyed by frame: a video that goes from the beach to a
	// restaurant is findable as both.
	if got := countRows(t, store, id, VisionModel); got != 3 {
		t.Fatalf("stored %d rows, want one per frame", got)
	}
}

// A clip re-sampled shorter must not keep describing frames it no longer has.
// An upsert would leave the tail behind — findable as a restaurant it was never
// in — which is exactly the drift clipRenditions removes on disk.
func TestPutEmbeddingsReplacesRatherThanMerges(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := recordTestAsset(t, store, "clip.mov", MediaVideo)

	six := make([]Embedding, 6)
	for i := range six {
		six[i] = Embedding{Frame: i, Vector: unit(i)}
	}
	if err := store.PutEmbeddings(ctx, id, VisionModel, six); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if err := store.PutEmbeddings(ctx, id, VisionModel, six[:2]); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	if got := countRows(t, store, id, VisionModel); got != 2 {
		t.Fatalf("after re-embedding two frames there are %d rows, want 2", got)
	}
}

// The model swap the schema was designed for: two encoders' vectors sit in the
// table together while somebody measures one against the other, and neither
// pass disturbs the other's rows.
func TestPutEmbeddingsIsScopedToOneModel(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := recordTestAsset(t, store, "IMG_0001.HEIC", MediaImage)

	if err := store.PutEmbeddings(ctx, id, VisionModel, []Embedding{{Vector: unit(0)}}); err != nil {
		t.Fatalf("current model: %v", err)
	}
	if err := store.PutEmbeddings(ctx, id, "some-other-encoder", []Embedding{{Vector: unit(1)}}); err != nil {
		t.Fatalf("other model: %v", err)
	}

	if got := countRows(t, store, id, VisionModel); got != 1 {
		t.Fatalf("the first model has %d rows, want 1 — the second pass should not have touched them", got)
	}
	if got := countRows(t, store, id, "some-other-encoder"); got != 1 {
		t.Fatalf("the second model has %d rows, want 1", got)
	}
}

// An embedding is a searchable description of what a photograph looks like,
// which is precisely what the vault exists to stop this server holding. The
// worker checks before it calls the GPU; this checks again after, because a
// photograph can be hidden while a call is in flight.
func TestPutEmbeddingsRefusesAVaultedAsset(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := recordTestAsset(t, store, "secret.HEIC", MediaImage)

	if _, err := store.pool.Exec(ctx, "update assets set vault = 'hidden' where id = $1::uuid", id); err != nil {
		t.Fatalf("hide the asset: %v", err)
	}
	// Not an error. The row simply does not appear, the same way ApplyPlaces
	// declines to write a place name onto something hidden mid-backfill.
	if err := store.PutEmbeddings(ctx, id, VisionModel, []Embedding{{Vector: unit(0)}}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}
	if got := countRows(t, store, id, VisionModel); got != 0 {
		t.Fatalf("stored %d rows for a hidden photograph, want 0", got)
	}
}

// The column is halfvec(1152) and Postgres would reject a mismatch anyway. The
// check is here so the message names the frame and both widths, which is the
// difference between "photo-ml is misconfigured" and a constraint violation.
func TestPutEmbeddingsRejectsTheWrongWidth(t *testing.T) {
	store := testStore(t)
	id := recordTestAsset(t, store, "IMG_0001.HEIC", MediaImage)

	err := store.PutEmbeddings(context.Background(), id, VisionModel, []Embedding{{Frame: 2, Vector: make([]float32, 768)}})
	if err == nil {
		t.Fatal("a 768-dimension vector should not be storable in a 1152-dimension column")
	}
	if !strings.Contains(err.Error(), "frame 2") || !strings.Contains(err.Error(), "768") {
		t.Fatalf("err = %q, want it to name the frame and the width", err)
	}
}

func TestEmbeddingCoverageCountsAssetsAndFrames(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	still := recordTestAsset(t, store, "IMG_0001.HEIC", MediaImage)
	clip := recordTestAsset(t, store, "clip.mov", MediaVideo)
	for _, id := range []string{still, clip} {
		if _, err := store.pool.Exec(ctx,
			`insert into jobs (kind, asset_id, state) values ('mlprep', $1::uuid, 'done')
			 on conflict (asset_id, kind) do update set state = 'done'`, id); err != nil {
			t.Fatalf("mark mlprep done: %v", err)
		}
	}

	if err := store.PutEmbeddings(ctx, clip, VisionModel, []Embedding{
		{Frame: 0, Vector: unit(0)}, {Frame: 1, Vector: unit(1)},
	}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}

	got, err := store.EmbeddingCoverage(ctx, VisionModel)
	if err != nil {
		t.Fatalf("EmbeddingCoverage: %v", err)
	}
	// Two assets have renditions, one of them has been described, and it took
	// two rows to do it. That the third number is larger than the second is the
	// point — "rows written" is not "photographs covered".
	if got.Assets != 2 || got.Embedded != 1 || got.Frames != 2 {
		t.Fatalf("coverage = %+v, want assets=2 embedded=1 frames=2", got)
	}
}

func TestVectorLiteralIsWhatPgvectorReads(t *testing.T) {
	if got := VectorLiteral([]float32{1, -0.5, 0}); got != "[1,-0.5,0]" {
		t.Fatalf("VectorLiteral = %q, want [1,-0.5,0]", got)
	}
}

// unit is a distinct vector per seed, of the width the column holds.
func unit(seed int) []float32 {
	v := make([]float32, VisionDim)
	v[seed%VisionDim] = 1
	return v
}

func countRows(t *testing.T, store *Store, assetID, model string) int {
	t.Helper()
	var n int
	err := store.pool.QueryRow(context.Background(),
		"select count(*) from asset_embeddings where asset_id = $1::uuid and model = $2",
		assetID, model).Scan(&n)
	if err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	return n
}

func recordTestAsset(t *testing.T, store *Store, name, kind string) string {
	t.Helper()
	sha := strings.Repeat("a", 63) + string(rune('a'+len(name)%26))
	id, _, err := store.RecordAsset(context.Background(), Asset{
		SHA256:           sha,
		MD5:              sha[:32],
		ByteSize:         1024,
		OriginalFilename: name,
		Ext:              name[strings.LastIndex(name, "."):],
		ContentType:      "application/octet-stream",
		MediaKind:        kind,
		DeviceID:         "test-device",
		LocalID:          name,
	})
	if err != nil {
		t.Fatalf("record asset %s: %v", name, err)
	}
	return id
}
