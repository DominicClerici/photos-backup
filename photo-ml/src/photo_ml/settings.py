"""Runtime settings, from the environment.

Deliberately the same shape as the Go side's internal/config: every setting has
a default that works on a developer's machine, and anything unparseable falls
back to that default rather than refusing to start. A typo in an env file should
cost you the default, not the service.
"""

from __future__ import annotations

import os
from dataclasses import dataclass


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
    # is the difference between seven hours and two and a half over the whole
    # library, and it is bounded by VRAM rather than by throughput — 8 images
    # peaks at about 10.1GB, 16 at 11.2GB, and the card also has the encoder,
    # the query parser and a desktop session on it.
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
        describe_batch=_positive_int(os.environ.get("PHOTO_ML_DESCRIBE_BATCH"), 8),
        batch_window=_positive_float(os.environ.get("PHOTO_ML_BATCH_WINDOW_MS"), 30.0) / 1000,
        models=_models(os.environ.get("PHOTO_ML_MODELS")),
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
