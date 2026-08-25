"""Runtime settings, from the environment.

Deliberately the same shape as the Go side's internal/config: every setting has
a default that works on a developer's machine, and anything unparseable falls
back to that default rather than refusing to start. A typo in an env file should
cost you the default, not the service.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

from . import ocr


@dataclass(frozen=True)
class Settings:
    # Loopback, and not configurable to anything else by accident. photod
    # reaches this over 127.0.0.1 and nothing else has any business reaching it:
    # there is no authentication here, because the only thing on the other end
    # of the socket is a process running on the same machine.
    addr: str
    port: int

    # Which device the tensors go on. "auto" takes CUDA when it is there and
    # falls back to the CPU when it is not — slow, roughly fifty times so, but
    # working. A machine whose driver is mid-upgrade should degrade rather than
    # take search down, which is the same rule photod applies to this whole
    # service.
    device: str

    # How long an on-demand model may sit unused before it is unloaded and its
    # VRAM handed back. See residency.py: this is the number that stops an
    # overnight backfill from starving the video encoder.
    idle_seconds: float

    # How often the reaper looks. Small relative to idle_seconds; it costs a
    # wakeup and a comparison.
    sweep_seconds: float

    # Largest batch /embed, /describe and /ocr will accept in one request. The
    # worker sends one image for a photograph and three or six for a video, so
    # this is a bound on mistakes rather than a tuning knob.
    max_batch: int

    # How many images the captioner may put through one forward pass, across
    # however many requests happened to arrive together. See batching.py: this
    # is worth hours over the whole library, and it is bounded by VRAM rather
    # than by throughput — the card also has the encoder, the query parser and a
    # desktop session on it.
    #
    # 4 rather than the 8 this used to be, and captioner.MAX_PIXELS is why: at
    # 1024*1024 a batch of 8 does not fit. Measured with the resident models in
    # place, 8 images peak past the card and 4 peak at 12.98GB, leaving 0.71GB —
    # which is the margin NVENC needs and the whole reason residency.py exists.
    # The throughput this gives up is small, because a batch amortises reading
    # the weights during decode and 907 vision tokens per image make the pass
    # prefill-bound instead: 1.21s an image here against 1.12s at a batch of 8,
    # measured with the parser evicted to make room for one.
    describe_batch: int

    # How long the collector waits for company before running a batch. Small,
    # because it is latency every request pays and only a backfill benefits
    # from; with photod's vision pool at its default concurrency the requests
    # are already in flight and this only matters at the tail of a queue.
    batch_window: float

    # Which models this instance loads. Every one of them by default.
    #
    # Naming a subset is how two instances share one card: a captioning-only
    # process during a backfill beside a search-only one answering queries, or a
    # second instance holding a candidate encoder while the first goes on
    # serving the archive. Nine gigabytes of captioner and two of encoder do not
    # both fit twice, and "run it twice and compare" is the shape of every bench
    # ML_IMAGES.md §9 describes.
    #
    # A route whose model is not loaded answers 404, which is the honest thing:
    # photod's vision pool asks /health before it claims, and a service that
    # does not offer /describe is one whose describe jobs should stay queued.
    models: frozenset[str]

    # Which captioner, by the name its rows carry. See captioner.CAPTIONERS for
    # what the names are; an unknown one falls back to the default, loudly.
    #
    # The one setting here that has a counterpart on the other side of the
    # socket: photod stores this string on every row it writes and reads
    # captions back by it, so PHOTOD_CAPTION_MODEL has to be set to the same
    # value in the same breath. They are deliberately two variables rather than
    # one — the two processes are separately deployable and a bench runs a
    # second photo-ml against the same photod — and photod compares them on
    # every result it reads.
    caption_model: str

    # Which PP-OCR weights the text recogniser loads, and where it runs them.
    #
    # "medium" because it is what measured best: PP-OCRv5 server read *less*
    # text than v6 small did and took three times as long, so this is not a
    # bigger-is-better dial and the top of it is not the answer. See ocr.py.
    #
    # The device follows `device` when it is left alone, which is the setting
    # anybody changing this actually means. It is separable because the two
    # models on the card have different appetites — a machine that wants the
    # captioner on the GPU and the recogniser off it, while something else is
    # transcoding, can say so without moving both.
    ocr_model_type: str
    ocr_device: str

    # Where transformers caches weights. Set explicitly so the systemd unit can
    # give the service one writable directory and no home directory at all.
    cache_dir: str | None


def from_env() -> Settings:
    return Settings(
        addr=os.environ.get("PHOTO_ML_ADDR", "127.0.0.1"),
        port=_positive_int(os.environ.get("PHOTO_ML_PORT"), 8789),
        device=os.environ.get("PHOTO_ML_DEVICE", "auto"),
        idle_seconds=_positive_float(os.environ.get("PHOTO_ML_IDLE_SECONDS"), 300.0),
        sweep_seconds=_positive_float(os.environ.get("PHOTO_ML_SWEEP_SECONDS"), 15.0),
        max_batch=_positive_int(os.environ.get("PHOTO_ML_MAX_BATCH"), 32),
        describe_batch=_positive_int(os.environ.get("PHOTO_ML_DESCRIBE_BATCH"), 4),
        batch_window=_positive_float(os.environ.get("PHOTO_ML_BATCH_WINDOW_MS"), 30.0) / 1000,
        models=_models(os.environ.get("PHOTO_ML_MODELS")),
        caption_model=os.environ.get("PHOTO_ML_CAPTION_MODEL", ""),
        ocr_model_type=os.environ.get("PHOTO_ML_OCR_MODEL_TYPE", "").strip().lower()
        or ocr.DEFAULT_MODEL_TYPE,
        ocr_device=os.environ.get("PHOTO_ML_OCR_DEVICE", "").strip().lower() or "",
        cache_dir=os.environ.get("PHOTO_ML_CACHE_DIR") or None,
    )


# The keys, which are also the residency keys and the names in /health.
ALL_MODELS = frozenset({"vision", "caption", "ocr", "parse"})


def _models(value: str | None) -> frozenset[str]:
    """Every model unless a subset is named, and unknown names are ignored.

    Ignored rather than refused, for the reason every other setting here falls
    back to its default: a typo in an env file should cost you a model, not the
    service. What it costs is visible — /health lists what is registered — which
    is where a typo is noticed.
    """
    if not value:
        return ALL_MODELS
    named = {name.strip().lower() for name in value.split(",") if name.strip()}
    chosen = named & ALL_MODELS
    return frozenset(chosen) if chosen else ALL_MODELS


def _positive_int(value: str | None, fallback: int) -> int:
    try:
        parsed = int(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return fallback
    return parsed if parsed > 0 else fallback


def _positive_float(value: str | None, fallback: float) -> float:
    try:
        parsed = float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return fallback
    return parsed if parsed > 0 else fallback
