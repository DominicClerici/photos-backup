"""Which models are in VRAM, and when they stop being.

The card is 16.3GB and the desktop session is already holding about 1.4 of it.
The vision encoder is small enough to keep loaded forever; the captioner and the
text recogniser that arrive in ML_IMAGES.md step 6 are not, and neither is
anything else that wants the card — the archive machine transcodes with
h264_nvenc, and NVENC is a separate silicon block that will not fight the models
for SMs but very much does want VRAM.

So a model declares a role rather than being loaded wherever somebody first
needs it:

  RESIDENT   loaded once, at startup, and never given back. The vision encoder,
             because interactive search embeds a query on every keystroke-ish
             and a 20-second load in front of that is not a search box.
  ON_DEMAND  loaded when something asks, unloaded once nothing has asked for
             idle_seconds. The heavy ones. A backfill gets the whole card and
             hands it back a few minutes after the queue goes dry.

Two details that are the entire reason this is a module rather than a dict.

`use()` refcounts. The reaper runs on its own thread, and unloading a model
another thread is in the middle of a forward pass on is a segfault rather than
an error — so a model in use is never a candidate, however long it has been
since the count last hit zero.

Unloading calls torch's allocator, not just `del`. Dropping the last Python
reference returns the blocks to torch's caching allocator, which keeps them
reserved against the driver: `nvidia-smi` goes on showing 6GB held and NVENC
goes on failing to allocate. `empty_cache()` is what actually gives it back, and
giving it back is the whole point of the role.
"""

from __future__ import annotations

import enum
import logging
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator
from contextlib import contextmanager

log = logging.getLogger("photo_ml.residency")


class Role(enum.Enum):
    RESIDENT = "resident"
    ON_DEMAND = "on_demand"


@dataclass
class Entry:
    """One model, and what is currently true of it."""

    # The name that ends up in asset_embeddings.model. It identifies what
    # produced a vector, so it must not change without the vectors changing.
    name: str
    role: Role
    load: Callable[[], Any]

    model: Any | None = None
    loaded_at: float | None = None
    last_used: float | None = None
    # Failures are remembered rather than retried in a tight loop: a checkpoint
    # that will not download is going to go on not downloading, and /health
    # saying so is more use than a log line every fifteen seconds.
    error: str | None = None

    in_use: int = 0
    lock: threading.RLock = field(default_factory=threading.RLock)


class Residency:
    def __init__(self, idle_seconds: float, sweep_seconds: float) -> None:
        self._idle = idle_seconds
        self._sweep = sweep_seconds
        self._entries: dict[str, Entry] = {}
        self._mu = threading.Lock()
        self._stop = threading.Event()
        self._reaper: threading.Thread | None = None

    def register(self, key: str, name: str, role: Role, load: Callable[[], Any]) -> None:
        with self._mu:
            self._entries[key] = Entry(name=name, role=role, load=load)

    def start(self) -> None:
        """Warm the resident models and begin reaping the on-demand ones.

        Warming happens on a background thread so the socket is listening
        immediately. A service that answers nothing for twenty seconds while a
        checkpoint loads is indistinguishable from a service that is down, and
        the one thing photod needs to be able to tell about this process is
        exactly that — so /health answers from the first moment and says
        `ready: false` until the encoder is up.
        """
        threading.Thread(target=self._warm, name="warm", daemon=True).start()
        self._reaper = threading.Thread(target=self._reap_loop, name="reap", daemon=True)
        self._reaper.start()

    def stop(self) -> None:
        self._stop.set()

    def _warm(self) -> None:
        for key, entry in list(self._entries.items()):
            if entry.role is Role.RESIDENT:
                try:
                    self._ensure(entry)
                except Exception:
                    # Already recorded on the entry and surfaced by /health.
                    # Raising here would kill a daemon thread and change
                    # nothing else.
                    log.exception("could not load the resident model %s", key)

    @contextmanager
    def use(self, key: str) -> Iterator[Any]:
        """Borrow a model, loading it if it is not there.

        The refcount is taken under the same lock that guards loading, so the
        reaper cannot decide a model is idle in the window between it being
        loaded and it being used.
        """
        entry = self._entries.get(key)
        if entry is None:
            raise KeyError(f"no model registered as {key!r}")

        with entry.lock:
            model = self._ensure(entry)
            entry.in_use += 1
            entry.last_used = time.monotonic()
        try:
            yield model
        finally:
            with entry.lock:
                entry.in_use -= 1
                entry.last_used = time.monotonic()

    def _ensure(self, entry: Entry) -> Any:
        with entry.lock:
            if entry.model is not None:
                return entry.model
            started = time.monotonic()
            try:
                entry.model = entry.load()
            except Exception as exc:
                entry.error = f"{type(exc).__name__}: {exc}"
                raise
            entry.error = None
            entry.loaded_at = time.monotonic()
            entry.last_used = entry.loaded_at
            log.info("loaded %s in %.1fs", entry.name, entry.loaded_at - started)
            return entry.model

    def _reap_loop(self) -> None:
        while not self._stop.wait(self._sweep):
            for entry in list(self._entries.values()):
                self._reap(entry)

    def _reap(self, entry: Entry) -> None:
        if entry.role is Role.RESIDENT:
            return
        with entry.lock:
            if entry.model is None or entry.in_use > 0:
                return
            if entry.last_used is None or time.monotonic() - entry.last_used < self._idle:
                return
            held = entry.model
            entry.model = None
            entry.loaded_at = None
        unload = getattr(held, "unload", None)
        if callable(unload):
            unload()
        del held
        _release_vram()
        log.info("unloaded %s after %.0fs idle", entry.name, self._idle)

    def report(self) -> list[dict[str, Any]]:
        """What /health says: which models are resident, and since when."""
        now = time.monotonic()
        out = []
        for key, entry in self._entries.items():
            out.append(
                {
                    "key": key,
                    "model": entry.name,
                    "role": entry.role.value,
                    "resident": entry.model is not None,
                    "in_use": entry.in_use,
                    "loaded_for_seconds": round(now - entry.loaded_at, 1) if entry.loaded_at else None,
                    "idle_seconds": round(now - entry.last_used, 1) if entry.last_used and entry.in_use == 0 else None,
                    "error": entry.error,
                }
            )
        return out

    def ready(self) -> bool:
        """True once every resident model is actually resident.

        The one question photod's vision pool asks before it claims a job. A
        service that is listening but still pulling 1.8GB of weights off a
        mirror is not a service the pool should be handing work to.
        """
        return all(
            entry.model is not None
            for entry in self._entries.values()
            if entry.role is Role.RESIDENT
        )


def _release_vram() -> None:
    try:
        import torch

        if torch.cuda.is_available():
            torch.cuda.empty_cache()
    except Exception:  # pragma: no cover — a CPU-only host has nothing to free
        pass
