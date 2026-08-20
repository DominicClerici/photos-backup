package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// VisionModel is the encoder whose vectors this archive currently searches by,
// and it is a constant in three places at once: here, in
// migrations/0017_vision.sql where it is the HNSW index's predicate, and in
// photo-ml's encoder.MODEL_NAME where the weights are actually loaded.
//
// It has to be repeated in the SQL rather than parameterised, because a partial
// index is only reachable from a query that repeats its predicate literally.
// Every search says `where model = 'siglip2-so400m-patch14-384'` even while it
// is the only model in the table — leave it out and Postgres answers the same
// rows by sequential scan over sixty thousand vectors, which is correct and
// slow enough to look like a bug in the model.
//
// It is a name we chose rather than the checkpoint's own
// (google/siglip2-so400m-patch14-384), because the row records what produced a
// vector and is going to outlive whatever the weights were called on whichever
// mirror they came from.
const VisionModel = "siglip2-so400m-patch14-384"

// VisionDim is the width of those vectors, and the width asset_embeddings was
// declared at in migration 0016 — before the model was chosen, which is why §4
// picked a family before it picked a member.
const VisionDim = 1152

// Embedding is one frame of one asset, as unit-length numbers.
//
// A still is frame 0 and nothing else. A video is one row per sampled frame,
// which is what lets a clip that goes from the beach to a restaurant be found
// as both: ranking takes max(similarity) across an asset's frames, where
// averaging them into a single vector per video would make it neither.
type Embedding struct {
	Frame  int
	Vector []float32
}

// PutEmbeddings replaces everything this model has said about an asset.
//
// Replace rather than upsert, in one transaction, because the set of frames is
// itself part of the answer. A clip re-sampled after its renditions were
// rebuilt can yield four frames where it once yielded six, and an upsert would
// leave the last two describing a video that no longer has them — findable as a
// restaurant it was never in. Deleting the model's rows first makes "frame N
// exists" and "frame N is of this asset, now" the same statement, which is the
// property clipRenditions maintains on disk and this maintains in the table.
//
// Scoped to one model. The other models' rows are untouched, so two encoders'
// vectors coexist while somebody measures them against each other, and losing
// the loser is a delete.
//
// The vault check is repeated here even though the worker already made it,
// because the two are separated by however long a GPU call took and a
// photograph can be hidden in between. An embedding is a description of what a
// photograph looks like — a considerably more searchable one than a thumbnail —
// so writing one for something in the vault would be this server recording the
// thing the vault exists to stop it knowing. Same guard, same reason, as
// ApplyPlaces.
func (s *Store) PutEmbeddings(ctx context.Context, assetID, model string, embeddings []Embedding) error {
	frames := make([]int32, len(embeddings))
	vectors := make([]string, len(embeddings))
	for i, e := range embeddings {
		if len(e.Vector) != VisionDim {
			return fmt.Errorf("embedding for frame %d has %d dimensions, want %d", e.Frame, len(e.Vector), VisionDim)
		}
		frames[i] = int32(e.Frame)
		vectors[i] = VectorLiteral(e.Vector)
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const clear = `delete from asset_embeddings where asset_id = $1::uuid and model = $2`
		if _, err := tx.Exec(ctx, clear, assetID, model); err != nil {
			return fmt.Errorf("clear embeddings for %s: %w", assetID, err)
		}
		if len(embeddings) == 0 {
			return nil
		}

		// The join against assets is the vault guard, and it is also what makes
		// this a no-op rather than a foreign-key error for an asset purged
		// between the GPU call and the write.
		const insert = `
			insert into asset_embeddings (asset_id, frame, model, embedding)
			select a.id, v.frame, $2, v.vec::halfvec
			from assets a, unnest($3::int[], $4::text[]) as v (frame, vec)
			where a.id = $1::uuid and a.vault = '' and a.deleted_at is null`
		if _, err := tx.Exec(ctx, insert, assetID, model, frames, vectors); err != nil {
			return fmt.Errorf("store embeddings for %s: %w", assetID, err)
		}
		return nil
	})
}

// VectorLiteral renders a vector the way pgvector reads one: `[1,2,3]`.
//
// A string cast to halfvec rather than a bound parameter of the type, which is
// a deliberate choice not to take a dependency. pgx does not know pgvector's
// types, and teaching it means pulling in pgvector-go and registering a codec
// on every connection in the pool — for a value this package builds in one
// function and Postgres parses with the input function it would have used
// anyway. The cast is explicit in the SQL, so a malformed literal is a type
// error at insert rather than a silently wrong vector.
//
// 'f' with -1 precision gives the shortest decimal that round-trips through
// float32. The column narrows these to fp16 on the way in, which is where the
// precision is actually wanted: at sixty thousand rows of 1152 dimensions the
// recall difference against full vector is not measurable, and the storage is
// half.
func VectorLiteral(vector []float32) string {
	var b strings.Builder
	b.Grow(len(vector)*10 + 2)
	b.WriteByte('[')
	for i, v := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// EmbeddingCoverage is how far the vision pass has got, for a model.
//
// Two numbers rather than one because the interesting question during a
// backfill is not "how many rows are there" but "how many of the things that
// could have one do". Assets is what the timeline shows and mlprep has written
// renditions for; Embedded is how many of those this model has described.
type EmbeddingCoverage struct {
	Model    string
	Assets   int64
	Embedded int64
	Frames   int64
}

func (s *Store) EmbeddingCoverage(ctx context.Context, model string) (EmbeddingCoverage, error) {
	const query = `
		select
		    (select count(*) from assets a
		       join jobs prep on prep.asset_id = a.id
		            and prep.kind = 'mlprep' and prep.state = 'done'
		      where a.vault = '' and a.deleted_at is null),
		    (select count(distinct asset_id) from asset_embeddings where model = $1),
		    (select count(*) from asset_embeddings where model = $1)`

	c := EmbeddingCoverage{Model: model}
	if err := s.pool.QueryRow(ctx, query, model).Scan(&c.Assets, &c.Embedded, &c.Frames); err != nil {
		return c, fmt.Errorf("count embedding coverage: %w", err)
	}
	return c, nil
}
