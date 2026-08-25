# The encoder bench — what it measured, 2026-08-21

ML_IMAGES.md §9 step 1 was skipped rather than run, and §4 says why: the width
was pinned at 1152 before a model was chosen, and `model` in the embeddings'
primary key makes a second encoder a delete and a requeue. Both are still true.
This is what running it anyway found.

`embed_bench.py` has the method. In one paragraph: 45 search phrases grounded in
what this archive holds, every system's top 50 pooled together, each pooled
photograph judged once by Qwen3-VL-4B reading YES/NO off its own logits, and
nDCG over what it said. Retrieval is **exact** — a numpy matmul over all 31,583
vectors — so that no approximate index can be blamed for a model. The index is
measured separately, below.

Everything here is one library, one judge and 45 queries. It answers "which of
these is better *here*", and it does not answer "which is better".

---

## 1. The index was the whole problem

Frame-level recall of the production HNSW scan against an exhaustive scan of the
same vectors, meaned over the 45 queries:

| build | ef_search | top-10 | top-20 | top-60 |
|---|---|---|---|---|
| `m=16, ef_construction=64` — what 0017 built | 40 | 0.86 | 0.85 | 0.89 |
| `m=16, ef_construction=64`, 1GB build memory | 40 | 0.82 | 0.81 | 0.86 |
| `m=32, ef_construction=200` | 40 | 0.94 | 0.92 | 0.93 |
| `m=32, ef_construction=200` | 200 | 0.97 | 0.96 | 0.96 |
| **`m=48, ef_construction=400`** | **200** | **0.99** | **0.99** | **0.98** |

0.86 is not a rounding error. For "flowers" the old graph returned **none** of
the true top ten; for "a birthday cake", three. Those photographs were in the
table, correctly embedded, at the right distance, and unreachable — which looks
from the outside exactly like a model that does not know what a flower is.

Two things this cost a wrong guess to learn:

**It was not memory.** The old index is 81MB against a 64MB
`maintenance_work_mem`, so the build could not hold its graph. Rebuilding at the
same parameters with 1GB measured 0.82 — the same number wearing a different
hat. HNSW construction is randomised; 0.82 against 0.86 is two draws from one
distribution.

**`ef_search` is free.** 200 is both more accurate than 40 *and faster* — 6-12ms
against 12-16ms. `candidateDepth` asks for 400 rows and `iterative_scan` keeps
resuming until it has them, so a narrow first pass does not save work, it buys a
walk that has to be restarted from the top repeatedly.

Shipped as migration `0020_hnsw_quality.sql` and one `set local` in `db.Search`.

## 2. Wrapping the query in a template makes search worse

nDCG@10 over all 45 queries, and the paired bootstrap against the incumbent:

| system | nDCG@10 | vs baseline | 95% CI | p |
|---|---|---|---|---|
| **patch14-384 / bare** — production today | **0.9213** | — | — | — |
| patch14-384 / 8-template ensemble | 0.9024 | −0.0188 | [−0.034, −0.006] | **0.001** |
| patch14-384 / `a photo of {q}.` | 0.8903 | −0.0310 | [−0.061, −0.009] | **0.002** |
| patch16-512 / ensemble | 0.8957 | −0.0255 | [−0.048, −0.006] | **0.010** |
| naflex / ensemble | 0.9005 | −0.0207 | [−0.051, +0.004] | 0.109 |

Same sign under all three encoders, significant under three of four. It is not a
tie being read the wrong way round.

The reason is that every image in the corpus *is* a photograph, so "a photo of"
points in a direction that carries no discriminative signal and dilutes the
subject. "fog" scores 1.000 and "a photo of fog." scores 0.469. Prompt
ensembling is standard practice for zero-shot *classification* over bare class
names; a search box already receives natural phrasing, and `searchquery` hands
this tower a noun phrase rather than a label.

There is no short-vs-long split: over 15 short queries the mean delta is −0.018
and over 30 long ones −0.015.

## 3. Neither newer encoder is distinguishable from the incumbent

| encoder | dim | nDCG@10 (45) | nDCG@10 (9 hard) | vs baseline | p | encode |
|---|---|---|---|---|---|---|
| patch14-384 — incumbent | 1152 | 0.9213 | 0.6575 | — | — | 12.2 min |
| **naflex** @1024 patches | 1152 | 0.9214 | 0.6633 | +0.0001 | 0.97 | 19.2 min |
| patch16-512 | 1152 | 0.9138 | 0.6099 | −0.0075 | 0.47 | 15.8 min |

Both hypotheses failed to show up. patch16-512 sees 1024 patches against 729 and
did not get better; naflex keeps the native aspect ratio of a library that is
**78% portrait** — every one of those currently stretched a third of its width
sideways — and landed on the baseline to four decimal places.

Rankings are stable at judge thresholds of 0.5, 0.9 and 0.99.

**One caveat that matters.** This ran against the renditions that exist, and
they are 512px on the long edge: `derivstore.MLEdge = 1536` is an *uncommitted
working-tree change* and `photobackup ml renditions` has not been run. So
patch16-512 was fed exactly its native resolution with no headroom above it. If
the 1536 renditions land, this comparison is worth re-running — `encode` plus
`retrieve` plus an incremental `judge` is under half an hour, and the judgement
cache means only genuinely new pairs are paid for.

## 4. Where the ceiling is

33 of 45 queries score 1.0 for every system: this archive holds several hundred
beaches, and a top ten of beaches is not a discriminating test. The differences
live in nine queries — birthday cake, beer, basketball, menu, christmas tree,
baby, selfie, cooking, dancing — where the best system manages 0.66.

That is the honest read on all three encoders: they have been at the ceiling on
ordinary queries for some time, and the remaining headroom is in specific
objects and small scenes, which is not where a bigger input or a different
aspect ratio helps.

## Known limitations

- **Pooled recall.** A photograph no system retrieved counts as irrelevant, so
  R@20 is against the pool and comparable between these rows and nothing else.
- **The judge is strict about literal subject match.** Spot-checked: it
  correctly accepted a decorated cake and correctly rejected a night driveway
  for "basketball", but rejected a *cupcake with birthday candles* for "a
  birthday cake", which a person would probably accept. This depresses absolute
  numbers; it does not favour any system, since one gold set scores them all.
- **One judge, no human agreement estimate.** Every number is a comparison
  between systems under a fixed judge, not an absolute quality claim.
