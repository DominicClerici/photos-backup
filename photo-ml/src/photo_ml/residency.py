"""Which models are in VRAM, and when they stop being.

The card is 16.3GB and the desktop session is already holding about 1.4 of it.
Everything else that wants it — the captioner, the text recogniser, and NVENC,
which is a separate silicon block that will not fight the models for SMs but
very much does want VRAM — has to fit in what is left.

So a model declares a role rather than being loaded wherever somebody first
needs it:

  LEASED     loaded when somebody takes a lease on it and unloaded when that
             lease lapses. The vision encoder and the query parser, because
             interactive search embeds a query on every keystroke-ish and a
             20-second load in front of that is not a search box — but only
             while somebody actually has the gallery open. See leases.py.
  ON_DEMAND  loaded when something asks, unloaded once nothing has asked for
             idle_seconds. The heavy ones. A backfill gets the whole card and
             hands it back a few minutes after the queue goes dry.

LEASED used to be RESIDENT: loaded at startup and never given back. That was
right about the latency and wrong about everything else, because it meant a
machine with nobody looking at it held three gigabytes of weights and a CUDA
context all day for a search box nobody had open. The role kept the latency and
gave up the "forever": a lease is photod saying somebody is looking, which is a
fact this process cannot observe and does not have to guess at.

A LEASED model is still reaped on idle when nothing pins it, which is what makes
a stray request that arrives outside any lease — an admin curl, a search that
raced a lapse — cost twenty seconds rather than three gigabytes until restart.

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

A model may also declare what it cannot share the card with. The captioner and
the text recogniser are both on it now, and at their peaks they do not both
fit — so the captioner evicts the recogniser on the way in. In an ordinary
backfill this never fires, because jobs.ClaimInOrder has already drained every
ocr job before the first describe job is claimed; it is there for the stray new
upload that interleaves the two, where the alternative is an allocation failure
five minutes into a four-hour pass.
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
    LEASED = "leased"
    ON_DEMAND = "on_demand"


@dataclass
class Entry:
    """One model, and what is currently true of it."""

    # The name that ends up in asset_embeddings.model. It identifies what
    # produced a vector, so it must not change without the vectors changing.
    name: str
    role: Role
    load: Callable[[], Any]
    # Keys this model may not be on the card beside. Applied on the way in, and
    # deliberately one-directional: see _evict_for.
    evicts: tuple[str, ...] = ()

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
        # Whether a lease is currently holding this key on the card. Installed
        # by leases.Arbiter rather than known here, because "somebody has the
        # gallery open" is not a fact about weights and this module is only
        # about weights. Absent — in a test, or in an instance nothing has
        # leased from — nothing is pinned and the reaper's clock is the only
        # rule, which is exactly how ON_DEMAND has always behaved.
        self._pinned: Callable[[str], bool] | None = None
        # Run at the end of every sweep, so the lease deadlines are checked on
        # the same thread and the same interval the idle timers are. One reaper
        # rather than two: they are the same job asked by two different clocks.
        self._on_sweep: list[Callable[[], None]] = []

    def pin_check(self, pinned: Callable[[str], bool]) -> None:
        """Tell the reaper how to ask whether a key is spoken for."""
        self._pinned = pinned

    def on_sweep(self, hook: Callable[[], None]) -> None:
        self._on_sweep.append(hook)

    def register(
        self,
        key: str,
        name: str,
        role: Role,
        load: Callable[[], Any],
        evicts: tuple[str, ...] = (),
    ) -> None:
        with self._mu:
            self._entries[key] = Entry(name=name, role=role, load=load, evicts=evicts)

    def start(self) -> None:
        """Begin reaping.

        Nothing is loaded here, and that is the change this file exists to
        record. Startup used to warm the two search models on a background
        thread so that the socket was listening immediately while 3GB of
        checkpoints arrived; now nothing loads until somebody asks, either by
        taking a lease or by sending a request. The service is up in about a
        second and holds no VRAM at all until there is a reason to.

        What that costs is the first search after a lapse, and leases.py is
        where that cost is managed: photod takes the lease on a page load, so
        the weights are arriving while the first screen of thumbnails is.
        """
        self._reaper = threading.Thread(target=self._reap_loop, name="reap", daemon=True)
        self._reaper.start()

    def stop(self) -> None:
        self._stop.set()

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
            self._evict_for(entry)
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

    def _evict_for(self, entry: Entry) -> None:
        """Unload what this model may not share the card with.

        One-directional by construction — the captioner evicts the recogniser
        and never the other way round — and that is what makes reaching for a
        second entry's lock in here safe: with no cycle there is nothing to
        deadlock on. It is also the right direction on its own terms. Evicting
        ten gigabytes of captioner to read the text in one photograph, and then
        loading it again, costs far more than the read is worth.

        A model somebody is mid-forward-pass on is left alone, for the reason
        the reaper leaves it alone: unloading weights another thread is using is
        a segfault rather than an error. Loading beside it may then run the card
        out of memory, and that is the failure to prefer — photo-ml answers 503,
        mlclient puts the job down without spending an attempt on it, and the
        next claim finds a card with room on it.
        """
        for key in entry.evicts:
            other = self._entries.get(key)
            if other is None:
                continue
            with other.lock:
                if other.model is None:
                    continue
                if other.in_use > 0:
                    log.warning(
                        "%s wants the card and %s is on it, but it is in use; loading beside it",
                        entry.name, other.name,
                    )
                    continue
                held = other.model
                other.model = None
                other.loaded_at = None
            _drop(held)
            log.info("unloaded %s to make room for %s", other.name, entry.name)

    def release(self, key: str) -> bool:
        """Hand one on-demand model's card back now, if nobody is using it.

        The reaper with the clock taken out, and it exists because the clock was
        asking the wrong question. `_idle` is a guess at "nothing will want this
        again soon" made by a service that has never seen a queue; photod knows
        the answer exactly, and for the captioner the difference is five minutes
        of a nearly full card every time a caption pass drains.

        The other half of what `_evict_for` will not do. That is one-directional
        on purpose — evicting ten gigabytes of captioner to read the text in one
        photograph costs far more than the read is worth — and the case it
        therefore cannot serve is the one where the captioner has no work left at
        all, where the reload it is protecting is never going to happen. So the
        direction is not reversed; the decision is moved to the only process that
        can make it. See mlclient.Recognize.

        Nothing here can thrash against `_evict_for`. A caption that is due keeps
        photod from saying the queue is empty, and photod holds the captioner off
        the card entirely until the cheap passes have been quiet for two minutes
        — see worker.visionHold. By the time either of those lets the captioner
        load, this has long since stopped being called.

        In use means left alone, for the reason the reaper leaves it alone:
        unloading weights another thread is mid-forward-pass on is a segfault
        rather than an error. Returns whether anything was actually unloaded.

        The lock is taken without blocking, and a model whose lock is held is
        left alone for the same reason a model in use is. Callers of this are on
        latency budgets that a checkpoint load does not fit inside — the vision
        pool's gate gives photo-ml five seconds to answer, and a lease that had
        to wait twenty for a warm thread to finish would time it out. Nothing is
        lost by declining: the reaper's next sweep asks the same question, and
        the model that was mid-load is one somebody has just asked for anyway.
        """
        entry = self._entries.get(key)
        if entry is None:
            return False
        if not entry.lock.acquire(blocking=False):
            return False
        try:
            if entry.model is None or entry.in_use > 0:
                return False
            held = entry.model
            entry.model = None
            entry.loaded_at = None
        finally:
            entry.lock.release()
        _drop(held)
        log.info("unloaded %s; nothing is queued that needs it", entry.name)
        return True

    def _reap_loop(self) -> None:
        while not self._stop.wait(self._sweep):
            # The leases first, so that a term which has just run out releases
            # what it pins before the pass below asks whether anything is idle.
            # The other order costs one whole sweep interval on every lapse.
            for hook in list(self._on_sweep):
                try:
                    hook()
                except Exception:
                    log.exception("a residency sweep hook failed")
            for key, entry in list(self._entries.items()):
                self._reap(key, entry)

    def _reap(self, key: str, entry: Entry) -> None:
        with entry.lock:
            if entry.model is None or entry.in_use > 0:
                return
            if entry.last_used is None or time.monotonic() - entry.last_used < self._idle:
                return
            # A leased model is not idle in the sense this clock means. Somebody
            # has the gallery open and has not typed for six minutes, which is
            # the exact case the lease exists to keep weights loaded through —
            # the alternative is a search box that is instant, then twenty
            # seconds, then instant again, for no reason the person can see.
            if self._pinned is not None and self._pinned(key):
                return
            held = entry.model
            entry.model = None
            entry.loaded_at = None
        _drop(held)
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

    def loaded(self, key: str) -> bool:
        """Whether this model's weights are on the card right now."""
        entry = self._entries.get(key)
        return entry is not None and entry.model is not None

    def ready(self) -> bool:
        """True when this service can be handed work.

        It used to mean "every resident model is actually resident", which was
        the right question while two checkpoints were pulled off a mirror at
        startup and the pool had to be kept off a service that was listening but
        empty. Nothing is loaded at startup any more, so that question has no
        content: the service is ready the moment it is up, and a first request
        that waits twenty seconds for weights is what mlclient.DefaultTimeout
        has always been sized for.

        What is left worth asking is whether anything here can load at all. A
        checkpoint that will not download stays broken, and a vision pool that
        spent an overnight backfill discovering that one asset at a time would
        park the library as permanently failed. So this goes false when *every*
        registered model has a recorded failure, and /health names each one.

        Every rather than any, deliberately. A recogniser that will not download
        must not stop embeddings: the three passes are independent, they fail
        independently, and a per-job failure already reaches the right place —
        mlclient turns photo-ml's 503 into a deferral and the queue keeps the
        other two kinds moving. This gate is for the case where nothing is going
        to work, which is the only case where claiming work is pointless.
        """
        entries = list(self._entries.values())
        return not entries or any(entry.error is None for entry in entries)


def _drop(held: Any) -> None:
    """Let go of one model's weights and hand the card back."""
    unload = getattr(held, "unload", None)
    if callable(unload):
        unload()
    del held
    _release_vram()


def _release_vram() -> None:
    try:
        import torch

        if torch.cuda.is_available():
            torch.cuda.empty_cache()
    except Exception:  # pragma: no cover — a CPU-only host has nothing to free
        pass
