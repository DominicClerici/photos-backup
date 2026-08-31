"""How much of the card belongs to somebody else.

Every VRAM number in this project used to be a measured constant in a comment —
1.8GB of encoder, 9.6GB of captioner, 0.71GB of margin for NVENC — and the
arithmetic was done once, by hand, against a card with a desktop session on it.
That was fine while the answer was "load everything and never give it back". It
stops being fine the moment the question becomes "is there room right now",
because the desktop session is not a constant: a browser with a video playing,
a second CUDA process somebody started to try something, a game. The only thing
that knows is the driver.

So this asks it. NVML reports, per process, how much of the card that process is
holding, and the sum over the processes that are not ours is the number both
budgets in leases.py are compared against.

Two things about "not ours" are worth stating, because they are policy rather
than mechanics.

The first is that photod counts as ours. It transcodes with h264_nvenc, NVENC is
a separate silicon block that does not fight the models for SMs but very much
does want VRAM, and an overnight backfill that postponed itself because the
archive was busy encoding video would be this project declining to run because
this project is running. Named by process name rather than by pid because photod
forks an ffmpeg per clip and this service has no way to be told about a child
that will exist in four seconds.

The second is that a card we cannot measure raises no objection. photo-ml is
optional forever — PROJECT.md §4 — and a machine with no NVIDIA driver, a
driver mid-upgrade, or an nvidia-ml-py that will not import must degrade towards
working. `foreign_bytes()` answers None there, and every caller reads None as
"no reason not to", which is exactly the behaviour this whole file replaced.
"""

from __future__ import annotations

import logging
import os
import threading
import time

log = logging.getLogger("photo_ml.vram")

# How long one reading is reused. The leases are asked about on every gallery
# request that comes through photod's rate limiter and on every pass of the
# vision pool's gate, which is often enough that an uncached NVML round trip per
# question would be a measurable share of what this service does when it is
# doing nothing. Two seconds is far shorter than anything either budget is
# protecting against — a process that allocates four gigabytes takes longer than
# that to do it — and it collapses a burst of questions into one answer.
CACHE_SECONDS = 2.0

# Processes whose VRAM is this project's own. See the module docstring: photod
# encodes video on the card, and a backfill that stood aside for it would be
# standing aside for itself.
#
# photobackup is the admin CLI, which runs the same pipeline from a terminal.
# ffprobe does not touch the card today and is here because it is the one of the
# three that might start to.
#
# photo-ml is in the list as well as being excluded by pid, and the two are not
# the same exclusion. The pid covers this process; the name covers a *second*
# instance, which settings.models exists to make possible — a captioning-only
# process during a backfill beside a search-only one answering queries, or a
# candidate encoder held for a bench. Those are this project, and the budgets
# are about processes outside it.
DEFAULT_IGNORE = ("photo-ml", "photod", "photobackup", "ffmpeg", "ffprobe")


class Card:
    """The one GPU this service runs on, asked what else is on it.

    Constructed once and shared. Everything below is guarded by a single lock,
    because the two leases are decided under their own lock and a reading that
    changed between them would let both be granted against the same free space.
    """

    def __init__(
        self,
        index: int = 0,
        ignore: tuple[str, ...] = DEFAULT_IGNORE,
        cache_seconds: float = CACHE_SECONDS,
    ) -> None:
        self._index = index
        self._ignore = frozenset(name.strip().lower() for name in ignore if name.strip())
        self._cache = cache_seconds
        self._mu = threading.Lock()
        self._handle = None
        self._nvml = None
        # Remembered rather than retried, the way residency.Entry remembers a
        # checkpoint that would not download: a driver that is not there is
        # going to go on not being there, and /health saying so once is more use
        # than an exception every two seconds.
        self._unavailable: str | None = None
        self._reading: int | None = None
        self._read_at: float | None = None

    # -----------------------------------------------------------------------

    def foreign_bytes(self) -> int | None:
        """VRAM held by processes that are not this project, or None.

        None means the question could not be asked — no driver, no NVML, a card
        that does not report per-process usage. Callers read that as consent;
        see the module docstring for why that is the only safe direction for a
        service the archive is allowed not to have.
        """
        with self._mu:
            now = time.monotonic()
            if self._read_at is not None and now - self._read_at < self._cache:
                return self._reading
            self._reading = self._measure()
            self._read_at = now
            return self._reading

    def available(self) -> bool:
        """Whether the driver answered the last time it was asked."""
        return self.foreign_bytes() is not None

    def report(self) -> dict:
        """What /health says about the card itself."""
        foreign = self.foreign_bytes()
        return {
            "measurable": foreign is not None,
            "foreign_bytes": foreign,
            "unavailable": self._unavailable,
            "ignoring": sorted(self._ignore),
        }

    # -----------------------------------------------------------------------

    def _measure(self) -> int | None:
        handle = self._open()
        if handle is None:
            return None
        nvml = self._nvml
        assert nvml is not None

        try:
            procs = list(_running(nvml, handle))
        except Exception as exc:
            self._unavailable = f"{type(exc).__name__}: {exc}"
            return None

        mine = os.getpid()
        total = 0
        for proc in procs:
            used = getattr(proc, "usedGpuMemory", None)
            if not used:
                # NVML answers None for a process whose usage it cannot
                # attribute — MIG, vGPU, a container with a restricted view.
                # Skipping it under-counts, which is the direction that lets
                # work happen; refusing to answer at all because one process was
                # opaque would take the card out of service over a detail.
                continue
            pid = int(getattr(proc, "pid", 0))
            if pid == mine or self._ours(pid):
                continue
            total += int(used)
        return total

    def _ours(self, pid: int) -> bool:
        """Whether this pid is one of the archive's own processes.

        By name, out of /proc, because photod forks an ffmpeg per clip and this
        service cannot be told about a child that will exist in four seconds. A
        pid that has already gone is not ours and not anybody's; it contributes
        nothing either way, because NVML would not have listed it if it were
        still holding memory.

        `/proc/<pid>/comm` is world-readable on an ordinary kernel, which is what
        lets a service running as `photo-ml` recognise a process running as
        `photod`. A host mounting /proc with `hidepid=2` takes that away, and the
        consequence is legible rather than mysterious: photod's NVENC transcoding
        starts counting against both budgets, so an overnight backfill postpones
        itself while the archive is encoding video. `PHOTO_ML_VRAM_IGNORE=` — set
        to nothing — is not the fix; raising the budgets is.
        """
        try:
            with open(f"/proc/{pid}/comm", "r", encoding="utf-8") as fh:
                return fh.read().strip().lower() in self._ignore
        except OSError:
            return False

    def _open(self):
        if self._handle is not None:
            return self._handle
        if self._unavailable is not None:
            return None
        try:
            import pynvml  # nvidia-ml-py

            pynvml.nvmlInit()
            self._nvml = pynvml
            self._handle = pynvml.nvmlDeviceGetHandleByIndex(self._index)
            log.info("reading VRAM from the driver; %s are counted as this project's own",
                     ", ".join(sorted(self._ignore)))
            return self._handle
        except Exception as exc:
            self._unavailable = f"{type(exc).__name__}: {exc}"
            # Info rather than warning. On a CPU-only host this is the expected
            # answer and not a fault, and the consequence — both budgets always
            # consent — is what a host with no card should do.
            log.info("cannot read VRAM from the driver; the residency budgets will not object (%s)",
                     self._unavailable)
            return None


def _running(nvml, handle):
    """Every process holding memory on this card, compute and graphics both.

    Both lists, because which one a process lands in is not a property this
    service should have an opinion about: a CUDA process is compute, a desktop
    compositor is graphics, and an ffmpeg encoding with NVENC has been seen in
    either depending on the driver. The union is the question actually being
    asked — what is on the card — and a pid in both is counted once because NVML
    reports the same usage in both and the dict below keys on it.
    """
    seen: dict[int, object] = {}
    for name in ("nvmlDeviceGetComputeRunningProcesses", "nvmlDeviceGetGraphicsRunningProcesses"):
        fn = getattr(nvml, name, None)
        if fn is None:
            continue
        try:
            for proc in fn(handle):
                seen[int(getattr(proc, "pid", 0))] = proc
        except Exception:
            # One of the two lists being unsupported is not the card being
            # unreadable. The other one is still an answer.
            continue
    return seen.values()
