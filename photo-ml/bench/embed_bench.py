"""Measure one encoder against another, on this archive, with this archive's questions.

ML_IMAGES.md §9 step 1 was a bench that never ran, and §4 records why it was
skipped rather than run: the width was already pinned at 1152, which pins the
family, and `model` in the embeddings' primary key means a second encoder is a
delete and a requeue rather than a project. Both halves of that are still true.
What it left behind is an archive whose search quality is an assumption, and a
set of plausible improvements — a bigger input, a native aspect ratio, a prompt
template — with no way to tell which of them are real.

This is that missing instrument. Five steps, each of which can be run on its
own, because the expensive two are worth never running twice:

  manifest   what is on disk, from the database that knows. Cheap.
  encode     the whole library through one encoder. ~20-40 minutes of GPU.
  retrieve   the queries through the same encoder's text tower, then an exact
             search. Seconds, and exact on purpose — see below.
  judge      a VLM says whether each retrieved photograph answers its query.
             The expensive one, and the one that is cached hardest.
  score      nDCG, precision and recall over what the judge said. Instant.

Two decisions shape everything else.

**Search here is exact, and production's is not.** A numpy matmul over 31,583
vectors takes forty milliseconds and has no recall loss at all, which is
precisely what makes it the right tool: the question "is patch16-512 better than
patch14-384" must not be answered by an HNSW graph that drops results from one
of them. Index recall is a separate property, measured separately, by the
`indexrecall` step against the production table. Confusing the two is how a
model gets blamed for a graph.

**Relevance is judged by pooling, not by a fixed answer key.** There is no
ground truth for "a birthday cake" over somebody's photographs, and building one
by hand for 45 queries is a weekend. So every system under test contributes its
top results to a common pool, a vision-language model judges each pooled
photograph once, and the judgements are cached by (query, asset, frame) so that
adding a fourth encoder costs only the pairs the first three never retrieved.
This is how TREC has done it since 1992 and it has the same known bias: a
photograph no system retrieved is treated as irrelevant, so recall is recall
*within the pool*. It is fair between systems that all fed the pool, and it is
not comparable to a recall number from anywhere else. Every table this prints
says so.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

import numpy as np

BENCH = Path(__file__).resolve().parent
RESULTS = BENCH / "results"
DERIVATIVES = Path(os.environ.get("DERIVATIVES_ROOT", "/var/lib/photod/derivatives"))
CONTAINER = os.environ.get("PG_CONTAINER", "photos-backup-postgres-1")
PGUSER = os.environ.get("PGUSER", "photobackup")
PGDATABASE = os.environ.get("PGDATABASE", "photobackup")

# Where the incumbent's vectors already live, and the name its rows carry. Only
# `indexrecall` reads them; every other step re-encodes from the renditions, so
# that each variant is measured through one preprocessing path rather than one
# variant being measured through the service's and the rest through this file's.
PRODUCTION_MODEL = "siglip2-so400m-patch14-384"

# What separates one cached judgement from another. A printable delimiter that
# cannot occur in a UUID or in a query id from queries.json, so the key is
# reversible by split() and legible in the cache file when something looks wrong.
SEP = "|"


# ---------------------------------------------------------------- the encoders

@dataclass(frozen=True)
class Variant:
    """One encoder under test.

    `family` is the only field here that is not a string handed straight to
    transformers, and it exists because SigLIP-2's two families take their
    images differently. The fixed-resolution checkpoints are `model_type:
    siglip` and hard-resize to a square, distortion and all. NaFlex is
    `model_type: siglip2`, keeps the aspect ratio, and wants a patch budget
    instead of an edge length — which is a different processor call, not a
    different argument to the same one.
    """

    name: str
    hf_id: str
    family: str = "siglip"
    # NaFlex only: how many 16px patches one image may become. 1024 is the
    # budget the checkpoint's card quotes, and it is also exactly the token
    # count of patch16-512 — which is what makes those two comparable as
    # something other than "the one that looked at more pixels won".
    max_num_patches: int = 1024


VARIANTS: dict[str, Variant] = {
    v.name: v
    for v in (
        # The incumbent, and the baseline everything else is measured against.
        Variant("patch14-384", "google/siglip2-so400m-patch14-384"),
        # Same width, same family, same load path — 1152 dimensions, so this one
        # is a drop-in that needs no migration and no schema thought at all.
        # 1024 patches against 729: the hypothesis is that a phone photograph
        # has detail at 512 that does not survive the trip down to 384.
        Variant("patch16-512", "google/siglip2-so400m-patch16-512"),
        # The interesting one for this library specifically. 78% of these
        # photographs are portrait, and every one of them is currently stretched
        # a third of its width sideways to reach a square. NaFlex is the variant
        # that does not do that.
        Variant("naflex", "google/siglip2-so400m-patch16-naflex", family="siglip2"),
    )
}


class Encoder:
    """A variant's weights, and the two things they are asked.

    Deliberately not photo_ml.encoder.VisionEncoder, which would be the obvious
    reuse and is the wrong one. That class is the service's contract with the
    archive and hardcodes one checkpoint, one width and one processor call
    because being unambiguous about those is its whole job. A bench needs the
    opposite — several checkpoints, whatever width each has — and making the
    service generic in order to serve the bench would put the bench's
    flexibility in the path of every search the archive answers.

    What is copied across verbatim is the text tower's padding. SigLIP's text
    encoder was trained on sequences padded to a fixed 64 tokens and pads to the
    batch's longest if not told otherwise, and the result is vectors that are
    subtly, unimprovably wrong with no error and no warning. It is the single
    most expensive line to get wrong in either file.
    """

    TEXT_LENGTH = 64

    def __init__(self, variant: Variant, device, dtype, cache_dir: str | None) -> None:
        import torch
        from transformers import AutoModel, AutoProcessor

        self.variant = variant
        self.device = device
        self.dtype = dtype
        self._torch = torch
        self._model = (
            AutoModel.from_pretrained(variant.hf_id, dtype=dtype, cache_dir=cache_dir)
            .to(device)
            .eval()
        )
        self._processor = AutoProcessor.from_pretrained(variant.hf_id, cache_dir=cache_dir)
        self.dim = int(self._model.config.text_config.hidden_size)

    def _image_inputs(self, images):
        if self.variant.family == "siglip2":
            return self._processor(
                images=images,
                max_num_patches=self.variant.max_num_patches,
                return_tensors="pt",
            )
        return self._processor(images=images, return_tensors="pt")

    def embed_images(self, images) -> np.ndarray:
        with self._torch.inference_mode():
            inputs = self._image_inputs(images).to(self.device)
            return _unit(self._features(self._model.get_image_features(**inputs)))

    def embed_texts(self, texts: list[str]) -> np.ndarray:
        with self._torch.inference_mode():
            inputs = self._processor(
                text=texts,
                padding="max_length",
                max_length=self.TEXT_LENGTH,
                truncation=True,
                return_tensors="pt",
            ).to(self.device)
            return _unit(self._features(self._model.get_text_features(**inputs)))

    def _features(self, out):
        """Unwrap whatever get_*_features handed back.

        The same two shapes photo_ml.encoder tolerates, for the same reason:
        transformers changed the getters from returning a tensor to returning a
        pooling wrapper around one, and accepting both is one attribute against
        a bench that stops running the next time somebody syncs.
        """
        if isinstance(out, self._torch.Tensor):
            return out
        pooled = getattr(out, "pooler_output", None)
        if pooled is None:
            raise TypeError(f"no pooled features in a {type(out).__name__}")
        return pooled


def _unit(features) -> np.ndarray:
    """Unit length, float32, on the CPU.

    Normalising here is what makes every dot product downstream a cosine
    similarity, which is what lets scores from two different frames — or two
    different variants — be compared without either having been scaled by how
    bright the photograph was.
    """
    features = features / features.norm(p=2, dim=-1, keepdim=True)
    return features.float().cpu().numpy()


# ------------------------------------------------------------ the query tower

# What a search phrase is wrapped in before the text tower sees it.
#
# Not cosmetic. Measured on the incumbent, "dog" and "a photo of a dog" agree on
# 31 of their top 60 photographs — the phrasing decides half the page. A
# contrastive model has no notion of a search box; it learned alt-text, and
# which alt-text a phrase happens to resemble is a property of the phrase rather
# than of the thing being looked for. Averaging over several wrappings is the
# standard answer to that in the CLIP literature, and it costs one forward pass
# over a batch of eight short strings — five milliseconds on this card.
TEMPLATES = (
    "{q}",
    "a photo of {q}.",
    "a photo of {q}",
    "a picture of {q}.",
    "a snapshot of {q}.",
    "an image of {q}.",
    "a photograph of {q}.",
    "a phone photo of {q}.",
)

STRATEGIES: dict[str, tuple[str, ...]] = {
    # What production does today: the phrase, verbatim, as the parser left it.
    "bare": ("{q}",),
    # The commonest wrapping on its own, so that an ensemble's gain can be told
    # apart from "any template at all beats none".
    "template": ("a photo of {q}.",),
    # All of them, averaged and renormalised.
    "ensemble": TEMPLATES,
}


def query_vectors(encoder: Encoder, texts: list[str], strategy: str) -> np.ndarray:
    """One vector per query, whatever the strategy cost to get there."""
    templates = STRATEGIES[strategy]
    filled = [t.format(q=text) for text in texts for t in templates]
    vectors = encoder.embed_texts(filled).reshape(len(texts), len(templates), -1)
    # The mean of directions rather than of scores. What is wanted is the
    # direction the wordings agree on; a wording that lands somewhere
    # idiosyncratic should be outvoted rather than allowed to drag a magnitude.
    pooled = vectors.mean(axis=1)
    return pooled / np.linalg.norm(pooled, axis=-1, keepdims=True)


# ------------------------------------------------------------------- manifest

def _psql(sql: str) -> list[list[str]]:
    """One statement, unaligned and untitled, through the container's own psql.

    Shelling out rather than adding psycopg to pyproject.toml: this reads a
    fixed list of assets, once, and a dependency in the service's own manifest
    would be a bench's convenience carried by every deployment of photo-ml
    forever.
    """
    out = subprocess.run(
        ["docker", "exec", "-i", CONTAINER, "psql", "-U", PGUSER, "-d", PGDATABASE,
         "-X", "-q", "-t", "-A", "-F", "\t", "-c", sql],
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        raise SystemExit(f"psql failed:\n{out.stderr.strip()}")
    return [line.split("\t") for line in out.stdout.splitlines() if line.strip()]


def cmd_manifest(args) -> None:
    """Every asset the vision pass would look at, and the renditions it has.

    The predicate is worker.runMLPrep's, restated: no vault, no trash, no
    overlays, no Live Photo halves. Restated rather than inferred from which
    files happen to exist, for the reason that job restates it rather than
    inferring it — the two must not be able to drift into disagreeing about what
    an item is.
    """
    rows = _psql("""
        select a.id::text, a.sha256, a.media_kind
        from assets a
        where a.vault = '' and a.deleted_at is null and not a.is_overlay
          and a.live_parent_local_id = '' and a.live_parent_asset_id is null
        order by a.id
    """)

    items, missing, frames = [], 0, 0
    for asset_id, sha, kind in rows:
        suffixes = [f".ml.{i}.webp" for i in range(6)] if kind == "video" else [".ml.webp"]
        present = []
        for i, suffix in enumerate(suffixes):
            path = DERIVATIVES / sha[0:2] / sha[2:4] / (sha + suffix)
            try:
                if path.stat().st_size > 0:
                    present.append([i, str(path)])
            except OSError:
                pass
        if not present:
            missing += 1
            continue
        frames += len(present)
        items.append({"asset": asset_id, "kind": kind, "frames": present})

    RESULTS.mkdir(parents=True, exist_ok=True)
    out = RESULTS / "manifest.json"
    out.write_text(json.dumps({"assets": items}))
    print(f"{len(items)} assets, {frames} renditions, {missing} assets with none -> {out}")


def load_manifest() -> list[dict]:
    path = RESULTS / "manifest.json"
    if not path.exists():
        raise SystemExit("no manifest; run `embed_bench.py manifest` first")
    return json.loads(path.read_text())["assets"]


# --------------------------------------------------------------------- encode

def cmd_encode(args) -> None:
    """One encoder over every rendition, to a file rather than to the archive.

    Deliberately not written into asset_embeddings, though the schema was built
    to hold exactly this and `model` in its primary key is an open invitation.
    Two reasons. A candidate encoder has no HNSW index — the one in migration
    0017 names the incumbent in its predicate — so every search against it would
    be a sequential scan, and the bench would be slower for having used the
    production table. And a bench that writes to the archive is a bench somebody
    has to remember to clean up; this one is deleted with `rm -r results/`.
    """
    import torch
    from PIL import Image

    variant = VARIANTS[args.variant]
    device, dtype = _device()
    print(f"loading {variant.hf_id} on {device}/{dtype}", flush=True)
    t0 = time.perf_counter()
    encoder = Encoder(variant, device, dtype, args.cache_dir)
    print(f"loaded in {time.perf_counter() - t0:.1f}s, dim={encoder.dim}", flush=True)

    work: list[tuple[str, int, str]] = []
    for item in load_manifest():
        for frame, path in item["frames"]:
            work.append((item["asset"], frame, path))
    if args.limit:
        work = work[: args.limit]

    vectors = np.zeros((len(work), encoder.dim), dtype=np.float16)
    # Decoding a 1536px WebP is about 10ms of CPU against 16ms of GPU, so the
    # two are worth overlapping — but only just, and only because there are
    # 31,000 of them. A thread pool rather than a process pool: PIL releases the
    # GIL inside the decoder, which is where the time goes.
    pool = ThreadPoolExecutor(max_workers=8)

    def decode(path: str):
        return Image.open(path).convert("RGB")

    started = time.perf_counter()
    for start in range(0, len(work), args.batch):
        chunk = work[start : start + args.batch]
        images = list(pool.map(decode, [p for _, _, p in chunk]))
        vectors[start : start + len(chunk)] = encoder.embed_images(images).astype(np.float16)
        for image in images:
            image.close()
        done = start + len(chunk)
        if start % (args.batch * 40) == 0 or done == len(work):
            rate = done / (time.perf_counter() - started)
            print(f"  {done}/{len(work)}  {rate:.1f} img/s  "
                  f"eta {(len(work) - done) / max(rate, 1e-9) / 60:.1f} min", flush=True)
    pool.shutdown()

    elapsed = time.perf_counter() - started
    out = RESULTS / "vectors"
    out.mkdir(parents=True, exist_ok=True)
    meta = {
        "variant": variant.name, "hf_id": variant.hf_id, "dim": encoder.dim,
        "seconds": round(elapsed, 1),
        "per_image_ms": round(1000 * elapsed / len(work), 2),
        "peak_gb": round(torch.cuda.max_memory_allocated() / 2**30, 2)
        if device.type == "cuda" else 0.0,
    }
    np.savez(
        out / f"{variant.name}.npz",
        vectors=vectors,
        assets=np.array([a for a, _, _ in work]),
        frames=np.array([f for _, f, _ in work], dtype=np.int16),
        meta=np.array([json.dumps(meta)]),
    )
    print(f"{len(work)} vectors in {elapsed / 60:.1f} min "
          f"({meta['per_image_ms']} ms/img, peak {meta['peak_gb']} GB) "
          f"-> {out / (variant.name + '.npz')}")


def _device():
    import torch

    if torch.cuda.is_available():
        return torch.device("cuda"), torch.float16
    print("warning: no CUDA; this will take hours", file=sys.stderr)
    return torch.device("cpu"), torch.float32


def load_vectors(name: str):
    path = RESULTS / "vectors" / f"{name}.npz"
    if not path.exists():
        raise SystemExit(f"no vectors for {name}; run `embed_bench.py encode --variant {name}`")
    data = np.load(path, allow_pickle=False)
    return (data["vectors"].astype(np.float32), data["assets"], data["frames"],
            json.loads(str(data["meta"][0])))


# ------------------------------------------------------------------- retrieve

def load_queries() -> list[dict]:
    return json.loads((BENCH / "queries.json").read_text())["queries"]


def cmd_retrieve(args) -> None:
    """The queries, through one encoder's text tower, against its own vectors.

    Exact. A matmul over 31,583 rows is forty milliseconds and has no recall
    loss, which is the entire reason it is here rather than a call to the
    production index: the question this step answers is what an encoder knows,
    and an approximate graph would answer it partly with what a graph found.

    Frames collapse to assets by best frame — the same `min(distance)`
    db.searchSQL applies, for the reason ML_IMAGES.md §4 gives. A clip that goes
    from a beach to a restaurant is as relevant as its best moment, and
    averaging six frames of it would make it neither.
    """
    variant = VARIANTS[args.variant]
    matrix, assets, frames, meta = load_vectors(variant.name)
    queries = load_queries()

    device, dtype = _device()
    encoder = Encoder(variant, device, dtype, args.cache_dir)
    started = time.perf_counter()
    qv = query_vectors(encoder, [q["text"] for q in queries], args.strategy)
    text_ms = 1000 * (time.perf_counter() - started) / len(queries)

    # One matmul for every query at once. Unit vectors on both sides, so this is
    # cosine similarity and larger is better.
    scores = qv.astype(np.float32) @ matrix.T

    # Enough frames that the collapse to assets cannot run short of depth. A
    # video contributes six rows and 15% of this library is video, so the top
    # 400 frames are routinely only about 310 distinct assets.
    pool_frames = min(400, matrix.shape[0])

    runs = []
    for i, query in enumerate(queries):
        row = scores[i]
        top = np.argpartition(-row, pool_frames - 1)[:pool_frames]
        top = top[np.argsort(-row[top])]
        best: dict[str, tuple[float, int]] = {}
        for j in top:
            asset = str(assets[j])
            if asset not in best:
                best[asset] = (float(row[j]), int(frames[j]))
        ranked = sorted(best.items(), key=lambda kv: -kv[1][0])[: args.depth]
        runs.append({
            "query": query["id"],
            "results": [{"asset": a, "frame": f, "score": round(s, 5)} for a, (s, f) in ranked],
        })

    out = RESULTS / "runs"
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{variant.name}.{args.strategy}.json"
    path.write_text(json.dumps({
        "variant": variant.name, "strategy": args.strategy,
        "text_ms_per_query": round(text_ms, 2), "encode": meta, "runs": runs,
    }))
    print(f"{len(queries)} queries x top-{args.depth} "
          f"({text_ms:.1f} ms/query in the text tower) -> {path}")


# ---------------------------------------------------------------------- judge

# What the judge is told it is doing.
#
# "Would this satisfy the person who typed it" rather than "does this contain
# X", because the two differ exactly where a photo library is interesting. A
# picture of a chairlift satisfies "skiing" and contains no skiing; a stock
# photograph of a cake on a phone screen contains a cake and satisfies nobody. A
# judge asked the narrow question marks the first wrong and the second right,
# and both of those are errors a search feature gets judged on.
JUDGE_SYSTEM = """You are judging a personal photo library's search results.

You will see one photograph from someone's own camera roll and the phrase they
typed into the search box. Answer whether this photograph is one they were
looking for.

Answer YES if the photograph would satisfy them: the thing they asked for is
visibly there, or the photograph is plainly of that scene, activity or moment.
Answer NO if it would not: the subject is absent, is a tiny incidental detail,
or the match is only by association.

Reply with exactly one word, YES or NO."""

# What the judge is allowed to look at, and deliberately not the captioner's own
# budget even though this borrows that model's weights.
#
# captioner.MAX_PIXELS is 1024*1024 — 907 vision tokens — because writing a
# caption and a tag list means reading a scene in detail, and settings.py cut
# describe_batch to 4 to pay for it. This is a different question. "Is this
# photograph one the person who typed 'a beach' was looking for" is answerable
# from a thumbnail, and 234 tokens against 907 is four times the pool judged per
# minute and a batch of 8 that fits beside a resident encoder rather than
# fighting it for the card.
JUDGE_MAX_PIXELS = 512 * 512
JUDGE_MIN_PIXELS = 224 * 224


def cmd_judge(args) -> None:
    """A vision-language model, once per (query, photograph, frame), cached.

    Not generated. The answer is read off the logits of the first position, the
    way captioner.judge() reads its triage verdicts and for the same reasons: a
    classifier cannot hallucinate a verdict, cannot skip one, cannot reorder
    them against its inputs, and costs prefill only. Over a pool of a couple of
    thousand photographs that is twenty minutes rather than two hours.

    What is stored is P(yes) rather than the bit. The bit is what the metrics
    use, at a threshold of 0.5, but the probability is what makes a disagreement
    between two encoders legible afterwards — a pool full of 0.51s is a query
    the judge found genuinely ambiguous, and reading a three-point nDCG
    difference off those would be reading noise.
    """
    import torch
    from PIL import Image
    from transformers import AutoModelForImageTextToText, AutoProcessor

    sys.path.insert(0, str(BENCH.parent / "src"))
    from photo_ml.captioner import _template_kwargs

    queries = {q["id"]: q["text"] for q in load_queries()}
    paths: dict[tuple[str, int], str] = {}
    for item in load_manifest():
        for frame, path in item["frames"]:
            paths[(item["asset"], frame)] = path

    cache_path = RESULTS / "judgments.json"
    cache: dict[str, float] = json.loads(cache_path.read_text()) if cache_path.exists() else {}

    # The pool: every (query, asset, frame) that any run under test put in its
    # top-N. Every system feeds it, which is what makes the comparison fair; a
    # photograph none of them retrieved is treated as irrelevant, which is the
    # bias this method is known for and the reason every table says "pooled".
    run_paths = _run_paths(args.runs)
    pool: list[tuple[str, str, int]] = []
    seen: set[tuple[str, str, int]] = set()
    for run_path in run_paths:
        run = json.loads(run_path.read_text())
        for entry in run["runs"]:
            for hit in entry["results"][: args.pool_depth]:
                key = (entry["query"], hit["asset"], hit["frame"])
                if key not in seen:
                    seen.add(key)
                    pool.append(key)

    todo = [k for k in pool if _key(k) not in cache]
    print(f"pool: {len(pool)} (query, photograph) pairs from {len(run_paths)} runs; "
          f"{len(pool) - len(todo)} already judged, {len(todo)} to do", flush=True)
    if not todo:
        return

    device, _ = _device()
    hf_id = "Qwen/Qwen3-VL-4B-Instruct"
    print(f"loading {hf_id}", flush=True)
    model = AutoModelForImageTextToText.from_pretrained(
        hf_id, dtype=torch.bfloat16, cache_dir=args.cache_dir).to(device).eval()
    processor = AutoProcessor.from_pretrained(
        hf_id, cache_dir=args.cache_dir,
        min_pixels=JUDGE_MIN_PIXELS, max_pixels=JUDGE_MAX_PIXELS)
    template = _template_kwargs(processor)

    tokenizer = processor.tokenizer
    yes = tokenizer.encode("YES", add_special_tokens=False)[0]
    no = tokenizer.encode("NO", add_special_tokens=False)[0]
    if yes == no:
        raise SystemExit("YES and NO share a first token; every verdict would be a coin toss")

    started = time.perf_counter()
    with torch.inference_mode():
        for start in range(0, len(todo), args.batch):
            chunk = todo[start : start + args.batch]
            images, prompts = [], []
            for query_id, asset, frame in chunk:
                images.append(Image.open(paths[(asset, frame)]).convert("RGB"))
                prompts.append(processor.apply_chat_template(
                    [
                        {"role": "system", "content": [{"type": "text", "text": JUDGE_SYSTEM}]},
                        {"role": "user", "content": [
                            {"type": "image"},
                            {"type": "text", "text": f'Search: "{queries[query_id]}"'},
                        ]},
                    ],
                    tokenize=False, add_generation_prompt=True, **template,
                ))
            # Left padding, because the verdict is read at the last position and
            # right padding would read it off a pad token.
            inputs = processor(
                text=prompts, images=[[i] for i in images], padding=True,
                padding_side="left", return_tensors="pt",
            ).to(device)
            # logits_to_keep=1 for captioner.judge()'s reason: without it the
            # head runs over every position of every prompt, which for a 150k
            # vocabulary is half a gigabyte of logits per batch, allocated on a
            # card that is holding nine gigabytes of these weights and thrown
            # away undecoded except for one row.
            logits = model(**inputs, logits_to_keep=1).logits[:, -1, :]
            p = torch.softmax(logits[:, [no, yes]].float(), dim=-1)[:, 1]
            for key, score in zip(chunk, p.tolist()):
                cache[_key(key)] = round(score, 4)
            for image in images:
                image.close()

            done = start + len(chunk)
            if start % (args.batch * 10) == 0 or done == len(todo):
                rate = done / (time.perf_counter() - started)
                cache_path.write_text(json.dumps(cache))
                print(f"  {done}/{len(todo)}  {rate:.1f} judgments/s  "
                      f"eta {(len(todo) - done) / max(rate, 1e-9) / 60:.1f} min", flush=True)

    cache_path.write_text(json.dumps(cache))
    positive = sum(1 for k in pool if cache.get(_key(k), 0) >= 0.5)
    print(f"judged {len(todo)} in {(time.perf_counter() - started) / 60:.1f} min; "
          f"{positive}/{len(pool)} of the pool is relevant -> {cache_path}")


def _key(key: tuple[str, str, int]) -> str:
    return f"{key[0]}{SEP}{key[1]}{SEP}{key[2]}"


def _run_paths(names: list[str]) -> list[Path]:
    runs = RESULTS / "runs"
    if not names:
        return sorted(runs.glob("*.json"))
    return [runs / (n if n.endswith(".json") else n + ".json") for n in names]


# ---------------------------------------------------------------------- score

def cmd_score(args) -> None:
    """What the judge said, per system, as numbers that can sit side by side.

    Every system is scored against the same judgement cache and the same pooled
    relevant set, which is the only way the columns mean anything relative to
    each other. Recall is against that pool and is named accordingly; nDCG and
    precision are ordinary.

    An asset is relevant if any judged frame of it is. A video retrieved by its
    beach frame and judged against "a beach" is relevant whether or not its
    restaurant frame would have been, which is the same rule the ranking applies
    one step earlier.
    """
    cache_path = RESULTS / "judgments.json"
    if not cache_path.exists():
        raise SystemExit("nothing judged yet; run `embed_bench.py judge`")
    cache = json.loads(cache_path.read_text())

    relevant: dict[str, set[str]] = {}
    for key, score in cache.items():
        query_id, asset, _ = key.split(SEP)
        if score >= 0.5:
            relevant.setdefault(query_id, set()).add(asset)

    queries = load_queries()
    kinds = {q["id"]: q["kind"] for q in queries}
    rows = []
    for run_path in _run_paths(args.runs):
        run = json.loads(run_path.read_text())
        per_query = {}
        for entry in run["runs"]:
            gold = relevant.get(entry["query"], set())
            if not gold:
                continue
            ranked = [hit["asset"] for hit in entry["results"]]
            per_query[entry["query"]] = {
                "ndcg@10": _ndcg(ranked, gold, 10),
                "ndcg@20": _ndcg(ranked, gold, 20),
                "p@10": sum(1 for a in ranked[:10] if a in gold) / 10,
                "recall@20": len([a for a in ranked[:20] if a in gold]) / len(gold),
                "gold": len(gold),
            }
        rows.append((run["variant"], run["strategy"], run.get("text_ms_per_query", 0.0), per_query))

    if not rows:
        raise SystemExit("no runs to score")
    scored = sorted({q for _, _, _, pq in rows for q in pq})
    print(f"\n{len(scored)} of {len(queries)} queries have at least one relevant photograph "
          f"in the pool; the rest are excluded from every column below.")
    print("Recall@20 is against the pooled relevant set, not the library: it is comparable "
          "between these rows and to nothing else.\n")

    _table("every query", scored, rows)

    # The queries where anything is actually being measured.
    #
    # This archive holds several hundred beaches, so "a beach" fills its top ten
    # with beaches under every system here and scores 1.0 for all of them. That
    # is a true result and a useless one: averaged in, a third of the query set
    # contributes an identical number to every row and drags each of them the
    # same distance towards the ceiling, which makes a real difference on the
    # hard queries look like a rounding error on the mean.
    #
    # So the second table drops the queries no system got wrong. The cut is made
    # on the *best* score across all systems rather than on any one of them,
    # which is what keeps it from being a way of choosing the winner: a query is
    # dropped only when nobody could have been separated by it.
    ceiling = {}
    for query_id in scored:
        ceiling[query_id] = max(
            (pq[query_id]["ndcg@10"] for _, _, _, pq in rows if query_id in pq), default=0.0)
    hard = [q for q in scored if ceiling[q] < args.hard_below]
    if hard and len(hard) < len(scored):
        print(f"\nThe {len(hard)} of {len(scored)} queries no system answered perfectly "
              f"(best nDCG@10 < {args.hard_below}); the other {len(scored) - len(hard)} "
              f"score 1.0 for every row above and separate nothing:\n")
        _table("hard queries", hard, rows)

    if args.by_kind:
        print("\nnDCG@10 by what the query is asking for:\n")
        all_kinds = sorted({kinds[q] for q in scored})
        print(f"{'system':30s} " + " ".join(f"{k:>10s}" for k in all_kinds))
        for variant, strategy, _, per_query in rows:
            cells = []
            for kind in all_kinds:
                vals = [per_query[q] for q in scored if q in per_query and kinds[q] == kind]
                cells.append(f"{_mean(vals, 'ndcg@10'):10.4f}" if vals else f"{'-':>10s}")
            print(f"{variant + ' / ' + strategy:30s} " + " ".join(cells))

    if args.per_query:
        print("\nnDCG@10 per query; the number beside the name is how many relevant "
              "photographs the pool holds for it:\n")
        labels = [f"{v}/{s}" for v, s, _, _ in rows]
        print(f"{'query':30s} " + " ".join(f"{lab:>20s}" for lab in labels))
        for query_id in scored:
            gold = next((pq[query_id]["gold"] for _, _, _, pq in rows if query_id in pq), 0)
            cells = " ".join(
                f"{pq[query_id]['ndcg@10']:20.4f}" if query_id in pq else f"{'-':>20s}"
                for _, _, _, pq in rows
            )
            print(f"{query_id + ' (' + str(gold) + ')':30s} " + cells)


def _table(label: str, query_ids: list[str], rows) -> None:
    header = (f"{'system (' + label + ')':34s} {'nDCG@10':>8s} {'nDCG@20':>8s} "
              f"{'P@10':>7s} {'R@20':>7s} {'ms/q':>6s}")
    print(header)
    print("-" * len(header))
    for variant, strategy, ms, per_query in rows:
        vals = [per_query[q] for q in query_ids if q in per_query]
        if not vals:
            continue
        print(f"{variant + ' / ' + strategy:34s} "
              f"{_mean(vals, 'ndcg@10'):8.4f} {_mean(vals, 'ndcg@20'):8.4f} "
              f"{_mean(vals, 'p@10'):7.3f} {_mean(vals, 'recall@20'):7.3f} {ms:6.1f}")


def _mean(vals: list[dict], key: str) -> float:
    return sum(v[key] for v in vals) / len(vals)


def _ndcg(ranked: list[str], gold: set[str], k: int) -> float:
    """Binary gain, log2 discount, the ideal being every relevant one at the top.

    The ideal is capped at k as well as the actual, so a query with three
    relevant photographs can still score 1.0 at k=10 rather than being punished
    for the library not containing seven more.
    """
    dcg = sum(1 / math.log2(i + 2) for i, a in enumerate(ranked[:k]) if a in gold)
    ideal = sum(1 / math.log2(i + 2) for i in range(min(len(gold), k)))
    return dcg / ideal if ideal else 0.0


# ---------------------------------------------------------------- index recall

def cmd_indexrecall(args) -> None:
    """What the production HNSW graph finds, against what is actually there.

    A different question from everything above, and deliberately kept apart from
    it. The rest of this file measures encoders with exact search precisely so
    that no graph can be blamed for a model; this measures the graph, on the
    model the archive is actually running, by asking postgres the same question
    twice — once through the index and once with index scans switched off.

    The comparison is at frame level rather than asset level because frames are
    where the index operates: db.searchSQL takes 400 frames and collapses them
    afterwards, so a frame the graph never reached is a frame the collapse never
    sees.
    """
    import urllib.request

    queries = load_queries()
    settings = (f"set local hnsw.ef_search = {args.ef_search}; "
                "set local hnsw.iterative_scan = relaxed_order; "
                "set local hnsw.max_scan_tuples = 100000; ")

    print(f"model={args.model}  ef_search={args.ef_search}  depth={args.depth}\n")
    header = f"{'query':34s} {'top-10':>8s} {'top-20':>8s} {'top-' + str(args.depth):>8s}"
    print(header)
    print("-" * len(header))

    totals = [0.0, 0.0, 0.0]
    worst = []
    for query in queries:
        body = json.dumps({"texts": [query["text"]]}).encode()
        request = urllib.request.Request(
            args.ml_url + "/embed", data=body, headers={"content-type": "application/json"})
        vector = json.load(urllib.request.urlopen(request, timeout=120))["vectors"][0]
        literal = "[" + ",".join(f"{x:.6f}" for x in vector) + "]"
        select = (f"select e.asset_id||':'||e.frame from asset_embeddings e "
                  f"where e.model='{args.model}' "
                  f"order by e.embedding <=> '{literal}'::halfvec limit {args.depth}")
        exact = [r[0] for r in _psql(
            "set enable_indexscan=off; set enable_bitmapscan=off; " + select)]
        approx = [r[0] for r in _psql(settings + select)]
        cells = []
        for i, k in enumerate((10, 20, args.depth)):
            hit = len(set(exact[:k]) & set(approx[:k])) / k
            totals[i] += hit
            cells.append(f"{hit:8.2f}")
        worst.append((len(set(exact[:10]) & set(approx[:10])) / 10, query["text"]))
        print(f"{query['text'][:33]:34s} " + " ".join(cells), flush=True)

    print("-" * len(header))
    n = len(queries)
    print(f"{'mean':34s} " + " ".join(f"{t / n:8.2f}" for t in totals))
    bad = sorted(worst)[:5]
    print("\nworst five at top-10: " + ", ".join(f"{t} ({r:.0%})" for r, t in bad))


# ----------------------------------------------------------------------- main

def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--cache-dir", default=os.environ.get("PHOTO_ML_CACHE_DIR"))
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("manifest").set_defaults(func=cmd_manifest)

    encode = sub.add_parser("encode")
    encode.add_argument("--variant", required=True, choices=sorted(VARIANTS))
    encode.add_argument("--batch", type=int, default=16)
    encode.add_argument("--limit", type=int, default=0)
    encode.set_defaults(func=cmd_encode)

    retrieve = sub.add_parser("retrieve")
    retrieve.add_argument("--variant", required=True, choices=sorted(VARIANTS))
    retrieve.add_argument("--strategy", default="bare", choices=sorted(STRATEGIES))
    retrieve.add_argument("--depth", type=int, default=50)
    retrieve.set_defaults(func=cmd_retrieve)

    judge = sub.add_parser("judge")
    judge.add_argument("--runs", nargs="*", default=[])
    judge.add_argument("--pool-depth", type=int, default=20)
    judge.add_argument("--batch", type=int, default=8)
    judge.set_defaults(func=cmd_judge)

    score = sub.add_parser("score")
    score.add_argument("--runs", nargs="*", default=[])
    score.add_argument("--by-kind", action="store_true")
    score.add_argument("--per-query", action="store_true")
    score.add_argument("--hard-below", type=float, default=0.999)
    score.set_defaults(func=cmd_score)

    index = sub.add_parser("indexrecall")
    index.add_argument("--model", default=PRODUCTION_MODEL)
    index.add_argument("--ml-url", default=os.environ.get("ML_URL", "http://127.0.0.1:8789"))
    index.add_argument("--ef-search", type=int, default=40)
    index.add_argument("--depth", type=int, default=60)
    index.set_defaults(func=cmd_indexrecall)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
