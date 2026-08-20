"""The HTTP surface: /health and /embed.

Two routes in this step, and ML_IMAGES.md §6 has three more coming — /describe,
/ocr and /parse. They are not here yet and the shape of the file assumes they
will be: every one of them borrows a model through the same Residency, so
adding the captioner is a register() call and a route rather than a second way
of managing the card.

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

from . import encoder as enc
from .residency import Residency, Role
from .settings import Settings, from_env

log = logging.getLogger("photo_ml")

# The key the encoder is registered under. One name for "the thing that turns
# pictures and phrases into vectors", so the day a bench replaces the checkpoint
# the routes below do not mention the old one.
VISION = "vision"


class EmbedRequest(BaseModel):
    # Exactly one of the two, and the service says which when given both. They
    # land in the same space — that is the whole point of the model — but a
    # request carrying both is a caller that has confused two questions, and
    # answering it would hide the confusion behind a correct-looking response.
    images: list[str] | None = Field(default=None, description="base64-encoded image bytes")
    texts: list[str] | None = Field(default=None, description="query phrases")


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

    residency.register(
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

    app = FastAPI(title="photo-ml", version="0.1.0")
    app.state.residency = residency
    app.state.settings = settings
    app.state.device = device
    app.state.dtype = dtype

    @app.on_event("startup")
    def _start() -> None:
        residency.start()

    @app.on_event("shutdown")
    def _stop() -> None:
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

    return app


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
