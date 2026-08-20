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
GET  /health          which models are resident, and on what device
POST /embed           images[] or texts[] → unit vectors[], 1152-d
```

`/describe`, `/ocr` and `/parse` are ML_IMAGES.md step 6 and 7. They are not
here yet, and the shape of `app.py` assumes they will be: each borrows a model
through the same `Residency`, so adding the captioner is a `register()` call and
a route rather than a second way of managing the card.

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

## Settings

All optional; every one falls back to its default rather than refusing to start.

| | |
|---|---|
| `PHOTO_ML_ADDR` | `127.0.0.1` — loopback, and it should stay there. There is no authentication here because the only thing on the other end is a process on this machine. |
| `PHOTO_ML_PORT` | `8789`, beside photod's 8787 and 8788 |
| `PHOTO_ML_DEVICE` | `auto` — CUDA when it is there, the CPU when it is not. The fallback is about fifty times slower and completely correct. |
| `PHOTO_ML_IDLE_SECONDS` | `300` — how long an on-demand model may sit unused before its VRAM goes back |
| `PHOTO_ML_MAX_BATCH` | `32`. The worker sends 1 for a photograph and 6 for a video, so this bounds mistakes rather than tuning anything. |
| `PHOTO_ML_CACHE_DIR` | where transformers keeps weights. Set explicitly by the unit, which gives the service one writable directory and no home. |

## The model

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

## Model residency

The card is 16.3GB and the desktop session already holds about 1.4 of it. A
model declares a role rather than being loaded wherever somebody first needs it:

- **resident** — loaded at startup, never given back. The vision encoder, at
  1.8GB, because interactive search embeds a phrase per query and a query that
  waits twenty seconds for a checkpoint is not a search box.
- **on demand** — loaded when asked, unloaded once nothing has asked for
  `PHOTO_ML_IDLE_SECONDS`. The 6GB captioner and the text recogniser that arrive
  in step 6.

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
