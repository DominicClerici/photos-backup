# photo-ml

What a photograph looks like, as 1152 numbers.

A stateless HTTP service on loopback. It holds no state, opens no files under
the archive, and talks to no database — everything it knows arrives as bytes on
a socket and leaves as numbers. That is the enforcement of ML_IMAGES.md §3
rather than a description of it: **Go decodes, Python does tensors**, and the
systemd unit puts `/mnt/photos` out of reach so no future change to this service
can quietly start reading the vault.

It is optional forever. photod runs without it, backups complete without it, and
the gallery draws without it. What is lost when it is down is the ability to
find a beach nobody wrote the word "beach" about.

## Running it

```sh
uv run photo-ml
```

First start pulls ~1.8GB of weights into the transformers cache and takes a
minute or two. The socket is listening the whole time — `/health` answers
immediately and says `ready: false` until the encoder is up, because a service
that answers nothing while it warms is indistinguishable from a service that is
gone, and telling those apart is the one thing photod needs from this process.

## Endpoints

```
GET  /health          which models are loaded, and on what device
POST /embed           images[] or texts[] → unit vectors[], 1152-d
POST /describe        images[] → [{caption, tags[]}]
POST /ocr             images[] → [{text, lines[]}]
POST /parse           query → filter JSON, as a suggestion
POST /triage          words[] → [{word, junk, score}]
```

The five of ML_IMAGES.md §6, and one more the tag cleanup needed. Every one of them borrows its model through the
same `Residency`, so what separates the encoder that is always loaded from the
captioner that is not is one argument at registration — and no handler knows
which it got.

Each route answers **per image, in order, with no merging**. This service holds
no state and knows nothing about assets: it is handed some pictures and says
what is in each of them, so deciding that three of them are one video is a
decision for the process that knows what a video is. See
`worker.foldDescriptions`.

### Proving it works, with curl alone

Up, and warm:

```sh
curl -s localhost:8789/health | python3 -m json.tool
```

```json
{
  "ok": true,
  "ready": true,
  "device": "cuda",
  "dtype": "float16",
  "models": [
    {
      "key": "vision",
      "model": "siglip2-so400m-patch14-384",
      "role": "resident",
      "resident": true,
      "in_use": 0,
      "loaded_for_seconds": 41.2,
      "idle_seconds": 3.1,
      "error": null
    }
  ]
}
```

A phrase, as a vector:

```sh
curl -s localhost:8789/embed \
  -H 'content-type: application/json' \
  -d '{"texts":["a dog on a beach"]}' | head -c 200
```

An image, as a vector — any ML rendition off the derivatives disk will do:

```sh
img=$(base64 -w0 /var/lib/photod/derivatives/ab/cd/abcd….ml.webp)
curl -s localhost:8789/embed \
  -H 'content-type: application/json' \
  -d "{\"images\":[\"$img\"]}" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r["model"], r["dim"], r["took_ms"])'
```

And the check that the two towers share a space, which is the property the whole
search feature rests on — embed a photograph and two phrases, and the right
phrase should score higher:

```sh
photo-ml/check-space.sh /path/to/some.ml.webp "a dog" "a spreadsheet"
```

A caption and some words for it:

```sh
curl -s localhost:8789/describe \
  -H 'content-type: application/json' \
  -d "{\"images\":[\"$img\"]}" | python3 -m json.tool
```

```json
{
  "model": "qwen3-vl-4b-instruct",
  "results": [
    {
      "caption": "Group of people sitting on a grassy hillside overlooking a forested mountain range under a clear blue sky.",
      "tags": [
        {"name": "mountain", "confidence": 1.0},
        {"name": "group", "confidence": 0.95},
        {"name": "hillside", "confidence": 0.9}
      ]
    }
  ]
}
```

The confidences are the model's own ordering turned into a number, not a
probability — it reports none. They exist so that the twelve-tag cap in
`db.normalizeTags` keeps the words the model led with rather than whichever
twelve hashed first.

Whatever is written on it:

```sh
curl -s localhost:8789/ocr -H 'content-type: application/json' \
  -d "{\"images\":[\"$img\"]}" | python3 -m json.tool
```

And what it thinks a search query was asking for — remembering that the Go
grammar has already answered by the time photod asks this, and that everything
below will be checked against the archive before any of it is believed:

```sh
curl -s localhost:8789/parse -H 'content-type: application/json' \
  -d '{"query":"addy and me in mexico","today":"2026-08-20","people":["Dominic","Addy"]}'
```

```json
{"people":["addy"],"place":"mexico","after":"","before":"","visual":""}
```

And whether the words it wrote were worth writing:

```sh
curl -s localhost:8789/triage -H 'content-type: application/json' \
  -d '{"words":["login","dog","high quality","snow"]}' | python3 -m json.tool
```

```json
{
  "model": "qwen3-vl-4b-instruct",
  "results": [
    {"word": "login", "junk": true, "score": 1.0},
    {"word": "dog", "junk": false, "score": 0.0},
    {"word": "high quality", "junk": true, "score": 1.0},
    {"word": "snow", "junk": false, "score": 0.0}
  ]
}
```

The scores saturate because this is a classifier rather than a generation — see
below — and they are answered in the order the words were sent, which `photod`
relies on: it matches the verdicts back by position, having never sent this
service a tag id it could match them by.

## Settings

All optional; every one falls back to its default rather than refusing to start.

| | |
|---|---|
| `PHOTO_ML_ADDR` | `127.0.0.1` — loopback, and it should stay there. There is no authentication here because the only thing on the other end is a process on this machine. |
| `PHOTO_ML_PORT` | `8789`, beside photod's 8787 |
| `PHOTO_ML_DEVICE` | `auto` — CUDA when it is there, the CPU when it is not. The fallback is about fifty times slower and completely correct. |
| `PHOTO_ML_IDLE_SECONDS` | `300` — how long an on-demand model may sit unused before its VRAM goes back |
| `PHOTO_ML_MAX_BATCH` | `32`. The worker sends 1 image for a photograph and 3 or 6 for a video, so this bounds mistakes rather than tuning anything. |
| `PHOTO_ML_MODELS` | which models this instance loads; every one by default. `PHOTO_ML_MODELS=caption` is how a second instance shares the card with the first — see below. |
| `PHOTO_ML_DESCRIBE_BATCH` | `4` — how many images the captioner may put through one forward pass. Bounded by VRAM, not by throughput: 8 does not fit at `MAX_PIXELS = 1024*1024`. See [Batching](#batching-the-captioner). |
| `PHOTO_ML_BATCH_WINDOW_MS` | `30` — how long the collector waits for company before running a batch |
| `PHOTO_ML_PARSER_MODEL` | `Qwen/Qwen3-0.6B`. Swappable; see [The query parser](#the-query-parser). |
| `PHOTO_ML_CACHE_DIR` | where transformers keeps weights. Set explicitly by the unit, which gives the service one writable directory and no home. |

## The models

| key | checkpoint | stored as | VRAM | residency |
|---|---|---|---|---|
| `vision` | `google/siglip2-so400m-patch14-384` | `siglip2-so400m-patch14-384` | 2.3GB | resident |
| `parse` | `Qwen/Qwen3-0.6B` | — | 1.2GB | resident |
| `caption` | `Qwen/Qwen3-VL-4B-Instruct` | `qwen3-vl-4b-instruct` | 9.1–13.0GB | on demand |
| `ocr` | rapidocr (PP-OCRv6 medium, torch) | `rapidocr-v6-medium` | 127MB, 1.2GB peak | on demand |

Checkpoint and stored name are deliberately two strings. The row records what
produced a caption or a vector and will outlive whatever the weights were called
on whichever mirror they came from, so the identity is ours: `db.VisionModel`,
`db.CaptionModel` and `db.OCRModel` hold the same constants on the Go side, and
migration `0017_vision.sql` names the encoder in the HNSW index's predicate.

That table is also a VRAM budget, and it is nearly full. 3.25GB resident for
the encoder and the parser together, 12.98GB at the peak of a describe batch,
about 1.1GB for the desktop session — which leaves 0.71GB of a 15.51GB card.
That is why the captioner is on demand, why the parser is 0.6B, and why
`PHOTO_ML_DESCRIBE_BATCH` is 4: at `captioner.MAX_PIXELS = 1024*1024` a batch of
8 does not fit at all, measured, with the resident models in place. The 0.71GB
is not slack — it is what NVENC allocates against while the archive transcodes,
and the failure this budget exists to prevent.

### The encoder

`google/siglip2-so400m-patch14-384`, recorded in the database as
`siglip2-so400m-patch14-384`.

Those are deliberately two strings. The row records what produced a vector and
will outlive whatever the weights were called on whichever mirror they came
from, so the identity is ours — `db.VisionModel` on the Go side holds the same
constant, and migration `0017_vision.sql` names it in the HNSW index's
predicate.

1152 dimensions because ML_IMAGES.md §4 committed the schema to `halfvec(1152)`,
and 1152 is this model's width. The bench in §9 step 1 is still worth running;
the schema is built so that running it later is a `delete where model = …` plus
a requeue rather than a migration, which is why `model` is part of the
embeddings' primary key.

### Two things that are expensive to get wrong

**The text tower wants exactly 64 tokens.** SigLIP was trained on sequences
padded to a fixed length and is sensitive to it in a way most encoders are not.
Pad to the batch's longest instead and the vectors come back subtly,
unimprovably wrong — no error, no warning, just a search that returns
almost-plausible rubbish. See `TEXT_LENGTH` in `encoder.py`.

**Blackwell needs cu128.** The RTX 5060 Ti is compute capability 12.0, and the
torch wheels on PyPI are built for nothing newer than Hopper. Without the
`pytorch-cu128` index in `pyproject.toml` the service installs cleanly, starts
cleanly, and then reports *no kernel image is available for execution on the
device* on the first forward pass.

**torchvision has to come from that index too**, and that one is easy to lose an
hour to: PyPI's build is against CUDA 13, so mixing it with a cu128 torch gives
*PyTorch and torchvision were compiled with different CUDA major versions* on a
package nothing here calls directly. It is present only because transformers'
Qwen3-VL processor imports a video processor that requires it, whether or not a
video is ever sent.

### The captioner

`Qwen/Qwen3-VL-4B-Instruct` in bf16, which is a deliberate step away from §6's
sketch of a 7–8B model at 4-bit. The AWQ and GPTQ kernels are the part of that
ecosystem most likely to be missing an architecture on a card this new, and the
failure mode is a service that installs, starts, and then reports *no kernel
image* on the first forward pass — exactly the 1am debugging session this whole
design is arranged to avoid. 4B in bf16 costs about the same VRAM as 8B in
4-bit, needs no extra dependency, and runs the code path everything else here
already runs on.

Its prompt is deliberately narrow: no names, no places, no guessing. Names come
from `asset_people`, where a person confirmed them, and places from the offline
geocoder. ML_IMAGES.md §11's seam between *a name somebody approved* and *a word
a model produced* runs right through `captioner.py`, and a captioner inventing
"Phoenix" or "Chicago" would be the first thing to cross it.

It returns JSON, and a malformed answer becomes a caption with no tags rather
than a failed job — the sentence is the more useful half, and a 4xx here would
burn an attempt against a photograph that is not the problem.

#### And the same weights, asked about their own words

`POST /triage` is ML_IMAGES.md §9's cleanup, one stage before the merge: given
tag names, which of them are worth keeping. It borrows `CAPTION` rather than
registering a model of its own, and that is the point — no second checkpoint, no
second nine gigabytes competing for a card that has to leave room for NVENC, and
the honest framing besides, since these are the captioner's own words coming
back to it.

**Nothing is generated.** The obvious implementation asks for the junk words as a
JSON list and it does not work: measured against the archive's real vocabulary, a
0.6B invented words that had never been in the list and repeated one of them four
hundred times, and even a well-behaved model has to be matched back to its input
by string comparison — for a task whose entire output is one bit per word. So
`judge()` runs one forward pass per word and reads the answer off the logits of
`KEEP` and `JUNK`. It cannot hallucinate a word, cannot skip one, cannot reorder
them, and it is prefill only: about 50ms a word against 1.5s for a generated
answer, which is the difference between a two-minute pass over a vocabulary and a
forty-minute one.

`logits_to_keep=1` is load-bearing rather than tidy. Without it the head runs over
every position of every prompt — half a gigabyte of logits per batch of eight, on
a card already holding nine gigabytes of these weights, all of it thrown away
except one row. It OOMs.

What comes back is P(junk) rather than a bit, and the extra is what the review
screen sorts by: a confident wrong verdict is the one worth catching. On this
archive the pass is right about "casual", "peaceful", "photograph", "login",
"result" and "text", and calls "screenshot" junk — which is arguable, and is
exactly why `photod` writes these only where nobody has answered and a person
signs them off. See server/README.md § Cleaning up the vocabulary.

### The text recogniser

rapidocr's PP-OCRv6 **medium** weights, on the card through its torch backend.

Both halves of that were measured, and both came out against what the first
version of this assumed.

**Bigger is not the axis.** PP-OCRv5 *server* read less text than v6 small did
and took three times as long. medium is the one that wins; the top of the range
is not the answer, which is why `PHOTO_ML_OCR_MODEL_TYPE` exists and why it is
not set to `server`.

**The rendition was the real limit.** At the old `derivstore.MLEdge` of 512px a
line of receipt text is about five pixels tall, and rapidocr upsamples that back
to a 736px short side before it looks — interpolation, adding no information.
Scored against known text on a 4032x3024 photograph:

| text height | 512px | 1536px | a line that size is |
|---|---|---|---|
| 2.1% of frame | 1.00 | 1.00 | a road sign, a shopfront, a headline |
| 1.2% of frame | 0.42 | 1.00 | a receipt held at arm's length |
| 0.9% of frame | 0.00 | 1.00 | body text in a screenshot, a menu |

MLEdge is 1536 now. The rows reading 0.00 are the photographs where recognised
text is the only thing that could ever have made them findable.

**The card is free during this pass, so it uses it.** `jobs.ClaimInOrder` drains
every `ocr` job before the first `describe` job is claimed — deliberately, so the
captioner and the recogniser do not thrash the card past each other — which
leaves the recogniser a GPU holding nothing but the encoder and the parser. On it
the same weights read the same text four times faster: 0.12s an image against
0.49s, a 0.8 hour pass against 3.2. Set `PHOTO_ML_OCR_DEVICE=cpu` to give it
back; the text is identical either way.

torch rather than onnxruntime-gpu because this card is Blackwell, and §6's lesson
about quantisation kernels applies just as well to inference ones: torch cu128 is
already installed and already proven here by the captioner, so putting the
recogniser on the GPU installs nothing new. If the build fails anyway it falls
back to the CPU with a log line rather than refusing to start.

One forward pass runs at a time whatever `VISION_CONCURRENCY` is. Four concurrent
calls serialise on the same kernels and buy nothing — the finding §10 records for
the captioner — while costing four copies of the 1.2GB a detection allocates on a
1536px frame, on a card that has to keep leaving room for NVENC.

A dedicated recogniser rather than asking the VLM to read the words: a model
asked what a blurry sign says will tell you something plausible, and this one
returns a confidence that can be thresholded. Lines under 0.5 confidence or two
characters are dropped in `ocr.py`, because everything that gets through lands
in a tsvector where a wrong word is a wrong search result.

It is pointed at the same frames the captioner gets, videos included, and that
is deliberate: a Snapchat memory carries its caption burned into the picture,
and `mlprep` burns the overlay in on purpose.

### The query parser

`Qwen/Qwen3-0.6B`, resident, and worth being honest about. Measured against this
archive's own queries, the gates in `searchquery.Merge` reject nearly everything
it produces: it answers "today" to questions with no date in them, offers
"beach" as a place, and handed the archive's people as a spelling hint returns
the whole list on every query. What survives the gates is correct and
occasionally useful — *addy and me in mexico* comes back with both — and what
does not survive costs nothing.

That is the arrangement §11 asks for, not a disappointment: **the Go grammar is
the parser** and this is a suggestion box with a bouncer. It stays small because
it stays resident, and the card has to fit a 9.6GB captioner beside it.
`PHOTO_ML_PARSER_MODEL` swaps it — Qwen3-1.7B is meaningfully better at this and
costs 3.4GB, which fits fine on a machine that is not captioning and does not fit
on one that is.

## Batching the captioner

The captioner is memory-bandwidth bound *while it decodes*: 8.8GB of weights are
read for every token it generates, whether that token is for one image or
twelve. Measured on the RTX 5060 Ti over this archive's own renditions, back
when `captioner.MAX_PIXELS` was `512*512` and an image cost 235 vision tokens:

| batch | per image | over the library | peak VRAM |
|---|---|---|---|
| 1 | 1.47 s | 7.3 h | 9.1GB |
| 4 | 0.78 s | 3.9 h | 9.6GB |
| 8 | 0.48 s | 2.4 h | 10.1GB |
| 16 | 0.38 s | 1.9 h | 11.2GB |

That table is why this mechanism exists and it is no longer the operating point.
`MAX_PIXELS` is `1024*1024`, an image costs 907 vision tokens, and both halves of
the trade moved. A batch of 8 no longer fits beside the resident models, and the
batch of 4 that does buys much less than the table suggests — measured at the new
size, with the resident models in place:

| batch | per image | over the library | peak VRAM | headroom |
|---|---|---|---|---|
| 4 | 1.21 s | 7.9 h | 12.98GB | 0.71GB |
| 8 | 1.12 s | 7.3 h | 13.24GB | 0.48GB — and only with the parser evicted |

Prefill is compute-bound where decode is bandwidth-bound, and at 907 tokens an
image the pass is mostly prefill. Batching still pays handsomely against a batch
of 1; it no longer pays much per doubling, which is what makes 4 an easy default
rather than a sacrifice.

Threads do not help — four concurrent single-image calls take exactly four times
as long as one, because they serialise on the same kernels. Only a real batch
does.

So the requests have to meet somewhere, and the only place they can is inside
this service. photod's vision pool is several workers each holding one asset's
job, and it must stay that way: a job is a claim on one photograph, and batching
at the queue level would mean a lease, a heartbeat and a failure shared between
eight unrelated files. `batching.py` is one thread and a queue — a caller
enqueues and waits, the collector takes the first thing it sees, waits 30ms for
company, and runs whatever turned up as one call.

Which means `VISION_CONCURRENCY` on the photod side is now a throughput knob
rather than a no-op. It defaults to 4, which now exactly fills a
describe batch; raising it further no longer helps this pass, for the reason the
second table gives.

## Running two instances

`PHOTO_ML_MODELS` names which models an instance loads. Nine gigabytes of
captioner and two of encoder do not both fit twice, so a bench — the shape of
every comparison §9 describes — means splitting them:

```sh
PHOTO_ML_PORT=8789 PHOTO_ML_MODELS=vision,parse,ocr photo-ml   # search
PHOTO_ML_PORT=8799 PHOTO_ML_MODELS=caption          photo-ml   # captioning
```

A route whose model is not loaded answers **404**, not 503, and the difference
matters to the caller: 503 means come back later and costs a job no attempt,
while this is a permanent property of how the service was started. photod turns
a 4xx into an ordinary failure, which is right — the fix is an env file, not
time.

## Model residency

The card is 16.3GB and the desktop session already holds about 1.4 of it. A
model declares a role rather than being loaded wherever somebody first needs it:

- **resident** — loaded at startup, never given back. The vision encoder, at
  2.3GB, and the query parser at 1.2GB, because interactive search embeds a
  phrase and parses a sentence per query, and a query that waits twenty seconds
  for a checkpoint is not a search box.
- **on demand** — loaded when asked, unloaded once nothing has asked for
  `PHOTO_ML_IDLE_SECONDS`. The 9.6GB captioner and the text recogniser.

The mechanism is `residency.py`, and two details in it are the reason it is a
module rather than a dict.

`use()` refcounts, because the reaper runs on its own thread and unloading a
model another thread is mid-forward-pass on is a segfault rather than an error.

Unloading calls `torch.cuda.empty_cache()` rather than just dropping the last
reference. Dropping the reference returns the blocks to torch's caching
allocator, which keeps them reserved against the driver: `nvidia-smi` goes on
showing 6GB held, and NVENC goes on failing to allocate. Giving the memory back
is the entire point of the role, and this is the line that actually does it —
which matters here because the archive machine transcodes with `h264_nvenc`, and
ML_IMAGES.md §11 names *transcodes failing to allocate during a backfill* as the
number to watch.

## Deployment

`deploy/photo-ml.service` and `deploy/README.md § photo-ml`. The unit runs as
`User=photo-ml` with `InaccessiblePaths=/mnt/photos`, which is §3 enforced by
the kernel rather than by a comment.
