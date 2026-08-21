"""The HTTP surface: /health, /embed, /describe, /ocr, /parse and /triage.

All five of ML_IMAGES.md §6, and the shape the file was written to take. Every
route borrows its model through the same Residency, so what separates the
encoder that is always loaded from the captioner that is not is one argument at
registration — and nothing in a handler knows or cares which it got.

Three of the four models have very different costs, and the residency table is
where that is expressed rather than in any of the code below:

  encoder    1.8GB, resident. A query becomes a vector on every search.
  parser     1.2GB, resident. A query waits on it; a 20-second load is not a
             search box.
  captioner  ~9GB, on demand. The expensive pass in the system, and the reason
             `empty_cache()` in residency.py matters — NVENC wants that memory
             back when the backfill goes quiet.
  recogniser ~0.5GB, on demand, and on the CPU. It does not touch the card at
             all, which is why the OCR pass can be running while the captioner
             is not loaded and vice versa.

Images arrive as base64 in JSON rather than as multipart. It costs a third more
bytes over a loopback socket that is not the bottleneck, and it buys a service
whose every endpoint can be exercised with curl and a here-document — which,
for the one part of this system that will be debugged at 1am while a backfill
is running, is worth more than the bandwidth.
"""

from __future__ import annotations

import base64
import binascii
import io
import logging
import time

import numpy as np
from fastapi import FastAPI, HTTPException
from PIL import Image
from pydantic import BaseModel, Field

from .batching import MicroBatcher
from . import captioner as cap
from . import encoder as enc
from . import ocr as textrec
from . import parser as qp
from .residency import Residency, Role
from .settings import Settings, from_env

log = logging.getLogger("photo_ml")

# The keys the models are registered under. One name each for "the thing that
# turns pictures into vectors", "the thing that writes captions" and so on, so
# the day a bench replaces a checkpoint the routes below do not mention the old
# one.
VISION = "vision"
CAPTION = "caption"
OCR = "ocr"
PARSE = "parse"


class EmbedRequest(BaseModel):
    # Exactly one of the two, and the service says which when given both. They
    # land in the same space — that is the whole point of the model — but a
    # request carrying both is a caller that has confused two questions, and
    # answering it would hide the confusion behind a correct-looking response.
    images: list[str] | None = Field(default=None, description="base64-encoded image bytes")
    texts: list[str] | None = Field(default=None, description="query phrases")


class ImagesRequest(BaseModel):
    """Frames, base64, for the two routes that read pictures rather than
    compare them."""

    images: list[str] = Field(description="base64-encoded image bytes")


class TriageRequest(BaseModel):
    """Words from the tag vocabulary, to be judged useful or not.

    Strings rather than ids: this service knows nothing about the archive, and a
    tag id would be a fact about a database it has never connected to. photod
    matches the answers back by position — see db.PutTriage.
    """

    words: list[str] = Field(description="tag names to judge")


class ParseRequest(BaseModel):
    query: str
    # The clock, because the model has none and "last summer" is unanswerable
    # without one. Passed rather than read here so that the answer to a query
    # is a function of its inputs, which is what makes it testable at all.
    today: str = ""
    # The archive's own people, as a hint. Only a hint: the Go side matches
    # whatever comes back against the same list again, so a model that ignores
    # this costs nothing. See searchquery.Merge.
    people: list[str] = Field(default_factory=list)


class EmbedResponse(BaseModel):
    model: str
    dim: int
    # Unit length, always. See encoder._normalize for why that is the service's
    # job rather than the database's.
    normalized: bool
    vectors: list[list[float]]
    took_ms: float


def build(settings: Settings) -> FastAPI:
    residency = Residency(settings.idle_seconds, settings.sweep_seconds)
    device, dtype = enc.resolve_device(settings.device)

    def register(key: str, **kwargs) -> None:
        """Register a model unless this instance was told not to hold it."""
        if key in settings.models:
            residency.register(key, **kwargs)

    def require(key: str) -> None:
        """404 for a route whose model this instance was not given.

        404 rather than 503, and the difference matters to the caller: 503 means
        come back later and costs a job no attempt, while this is a permanent
        property of how the service was started. photod's mlclient turns a 4xx
        into an ordinary failure, which is right — the fix is an env file, not
        time.
        """
        if key not in settings.models:
            raise HTTPException(
                404, f"this photo-ml holds no {key} model (PHOTO_ML_MODELS={','.join(sorted(settings.models))})"
            )

    register(
        CAPTION,
        name=cap.MODEL_NAME,
        # On demand, and the reason the whole residency mechanism exists. Nine
        # gigabytes is more than half this card, the archive machine transcodes
        # with h264_nvenc, and NVENC is a separate silicon block that will not
        # fight the models for SMs but very much does want VRAM. Transcodes
        # getting slower during a backfill is acceptable; transcodes failing to
        # allocate is not.
        role=Role.ON_DEMAND,
        load=lambda: cap.Captioner(device, dtype, settings.cache_dir),
    )
    register(
        OCR,
        name=textrec.MODEL_NAME,
        # On demand as well, though it costs the card nothing — it runs on the
        # CPU. Half a gigabyte of host memory and an ONNX session are still
        # worth handing back on a machine whose main job is holding
        # photographs.
        role=Role.ON_DEMAND,
        load=textrec.Recognizer,
    )
    register(
        PARSE,
        name=qp.MODEL_NAME,
        # Resident, like the encoder, and for the same reason: somebody is
        # waiting on it. 1.2GB is what makes that affordable beside a 9GB
        # captioner on a 16GB card.
        role=Role.RESIDENT,
        load=lambda: qp.QueryParser(device, dtype, settings.cache_dir),
    )
    register(
        VISION,
        name=enc.MODEL_NAME,
        # Resident, and the only one that will be. Interactive search embeds a
        # phrase per query, and a query that waits twenty seconds for a
        # checkpoint is not a search box. It is also the cheap one: 1.8GB
        # against the 6GB the captioner in step 6 will want, which is what makes
        # keeping it affordable at all.
        role=Role.RESIDENT,
        load=lambda: enc.VisionEncoder(device, dtype, settings.cache_dir),
    )

    # The captioner is the one model where a batch is worth waiting for: it is
    # memory-bandwidth bound, so eight images cost barely more than one, and
    # photod's vision pool has several requests in flight at any moment during a
    # backfill. See batching.py for the measurements.
    #
    # The recogniser gets none of this — it runs on the CPU one image at a time
    # whatever it is handed — and neither does /parse, where one person is
    # waiting for one answer.
    def _describe_batch(images: list[Image.Image]) -> list[dict]:
        # The borrow happens here, on the collector thread, and is given back
        # when the batch is done — which is what lets the reaper unload nine
        # gigabytes a few minutes after the queue goes dry. Holding it open
        # across batches would be a captioner that is resident in everything but
        # name.
        with residency.use(CAPTION) as model:
            return model.describe(images)

    describer = MicroBatcher(
        name="describe",
        run=_describe_batch,
        max_items=settings.describe_batch,
        window=settings.batch_window,
    )

    app = FastAPI(title="photo-ml", version="0.1.0")
    app.state.residency = residency
    app.state.settings = settings
    app.state.device = device
    app.state.dtype = dtype

    @app.on_event("startup")
    def _start() -> None:
        residency.start()
        describer.start()

    @app.on_event("shutdown")
    def _stop() -> None:
        describer.stop()
        residency.stop()

    @app.get("/health")
    def health() -> dict:
        """Up, and which models are resident.

        Always 200, including while the encoder is still loading and including
        when it failed to load. The distinction photod needs is not "is this
        endpoint answering" — it is "can this service embed something right
        now", and that is `ready`. A 503 here would make a service that is
        warming up look like a service that is gone, and the vision pool would
        back off from work it is about to be able to do.
        """
        return {
            "ok": True,
            "ready": residency.ready(),
            "device": str(device),
            "dtype": str(dtype).removeprefix("torch."),
            "models": residency.report(),
        }

    @app.post("/embed", response_model=EmbedResponse)
    def embed(req: EmbedRequest) -> EmbedResponse:
        require(VISION)
        if (req.images is None) == (req.texts is None):
            raise HTTPException(400, "send exactly one of images or texts")

        started = time.perf_counter()
        items = req.images if req.images is not None else req.texts
        assert items is not None
        if not items:
            raise HTTPException(400, "nothing to embed")
        if len(items) > settings.max_batch:
            raise HTTPException(
                413, f"{len(items)} items in one request; the limit is {settings.max_batch}"
            )

        try:
            with residency.use(VISION) as model:
                if req.images is not None:
                    vectors = model.embed_images([_decode(b) for b in req.images])
                else:
                    vectors = model.embed_texts(list(req.texts or []))
        except HTTPException:
            raise
        except Exception as exc:
            # 503 rather than 500, and deliberately: everything that reaches
            # here is the service failing rather than the request being wrong —
            # a checkpoint that would not load, a card that went away. The Go
            # client treats 5xx as "come back later" and puts the job down
            # without spending an attempt on it, which is the right answer for
            # all of them.
            log.exception("embed failed")
            raise HTTPException(503, f"{type(exc).__name__}: {exc}") from exc

        return EmbedResponse(
            model=enc.MODEL_NAME,
            dim=enc.DIM,
            normalized=True,
            vectors=_as_lists(vectors),
            took_ms=round((time.perf_counter() - started) * 1000, 1),
        )

    @app.post("/describe")
    def describe(req: ImagesRequest) -> dict:
        """What each photograph is of: a sentence and a handful of words.

        One result per image, in order, and no merging. This service holds no
        state and knows nothing about assets — it is handed some pictures and
        says what is in each of them — so deciding that three of them are one
        video is a decision for the process that knows what a video is. See
        worker.foldDescriptions.
        """
        require(CAPTION)
        started = time.perf_counter()
        images = _images(req, settings)
        results = _guard("describe", lambda: describer.submit(images))
        return {
            "model": cap.MODEL_NAME,
            "results": results,
            "took_ms": round((time.perf_counter() - started) * 1000, 1),
        }

    @app.post("/ocr")
    def ocr(req: ImagesRequest) -> dict:
        """What each photograph says.

        An empty answer is a result, not a failure: most photographs contain no
        text, and the row recording that is what stops the backfill offering
        ninety percent of the library again on every run.
        """
        require(OCR)
        started = time.perf_counter()
        images = _images(req, settings)
        with residency.use(OCR) as model:
            results = _guard("ocr", lambda: model.read(images))
        return {
            "model": textrec.MODEL_NAME,
            "results": results,
            "took_ms": round((time.perf_counter() - started) * 1000, 1),
        }

    @app.post("/triage")
    def triage(req: TriageRequest) -> dict:
        """Which of these words are worth keeping — ML_IMAGES.md §9, stage one.

        The captioner marking its own homework, and the same weights the
        /describe route borrows: no second checkpoint, no second entry in the
        residency table, and no second nine gigabytes competing for the card.
        See captioner.judge() for why the answer is read off logits instead of
        generated.

        A claim rather than an answer, in exactly the way /parse is. photod
        writes these verdicts only where nobody has given one, a person reviews
        them in two lists, and approving is what turns them into the archive
        owner's own opinion. See internal/db/tags.go.
        """
        require(CAPTION)
        if not req.words:
            raise HTTPException(400, "nothing to judge")
        if len(req.words) > settings.max_batch:
            raise HTTPException(
                413, f"{len(req.words)} words in one request; the limit is {settings.max_batch}"
            )

        started = time.perf_counter()
        with residency.use(CAPTION) as model:
            results = _guard("triage", lambda: model.judge(list(req.words)))
        return {
            "model": cap.MODEL_NAME,
            "results": results,
            "took_ms": round((time.perf_counter() - started) * 1000, 1),
        }

    @app.post("/parse")
    def parse(req: ParseRequest) -> dict:
        """What a typed sentence was asking for — as a suggestion.

        The Go grammar has already parsed this query by the time the request
        arrives, and everything answered here is checked against the archive's
        own people and places before any of it is believed. See parser.py and
        searchquery.Merge for why that asymmetry is the design.
        """
        require(PARSE)
        if not req.query.strip():
            raise HTTPException(400, "nothing to parse")
        with residency.use(PARSE) as model:
            return _guard("parse", lambda: model.parse(req.query, req.today, req.people))

    return app


def _images(req: ImagesRequest, settings: Settings) -> list[Image.Image]:
    """The shared front half of /describe and /ocr: decode, and refuse a batch
    that is a mistake rather than a request."""
    if not req.images:
        raise HTTPException(400, "nothing to look at")
    if len(req.images) > settings.max_batch:
        raise HTTPException(
            413, f"{len(req.images)} items in one request; the limit is {settings.max_batch}"
        )
    return [_decode(b) for b in req.images]


def _guard(what: str, run):
    """Run a model, and turn anything that goes wrong into a 503.

    503 rather than 500, and deliberately, for the reason /embed does it:
    everything that reaches here is the service failing rather than the request
    being wrong — a checkpoint that would not load, a card that went away. The
    Go client treats 5xx as "come back later" and puts the job down without
    spending an attempt on it, which is the right answer for all of them. A
    request that is actually wrong has already been refused with a 4xx above.
    """
    try:
        return run()
    except HTTPException:
        raise
    except Exception as exc:
        log.exception("%s failed", what)
        raise HTTPException(503, f"{type(exc).__name__}: {exc}") from exc


def _decode(payload: str) -> Image.Image:
    """base64 → a picture, or a 400 that says which of the two steps failed."""
    try:
        raw = base64.b64decode(payload, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise HTTPException(400, f"image is not valid base64: {exc}") from exc
    try:
        # RGB unconditionally. The renditions are WebP and some of them carry an
        # alpha channel — a screenshot, a Snapchat overlay composited over
        # nothing — and the processor wants three channels either way.
        return Image.open(io.BytesIO(raw)).convert("RGB")
    except Exception as exc:
        raise HTTPException(400, f"could not decode the image: {exc}") from exc


def _as_lists(vectors: np.ndarray) -> list[list[float]]:
    return [[float(x) for x in row] for row in vectors]


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    import uvicorn

    settings = from_env()
    log.info("photo-ml listening on %s:%d", settings.addr, settings.port)
    uvicorn.run(
        build(settings),
        host=settings.addr,
        port=settings.port,
        # One worker, always. The models are the expensive thing here and a
        # second worker process is a second copy of them in VRAM answering half
        # the requests — which is how a 1.8GB encoder becomes 3.6GB and the
        # captioner in step 6 stops fitting on the card at all. Concurrency
        # belongs on the other side of the socket, where photod's vision pool is
        # already a queue in front of one GPU.
        workers=1,
        log_level="info",
        access_log=False,
    )


if __name__ == "__main__":
    main()
