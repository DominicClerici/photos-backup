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

    # Largest batch /embed will accept in one request. The worker sends one
    # image for a photograph and six for a video, so this is a bound on
    # mistakes rather than a tuning knob.
    max_batch: int

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
        cache_dir=os.environ.get("PHOTO_ML_CACHE_DIR") or None,
    )


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
