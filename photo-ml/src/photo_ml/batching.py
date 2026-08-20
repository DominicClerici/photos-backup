"""Turning several one-photograph requests into one forward pass.

The captioner is memory-bandwidth bound: 8.8GB of weights have to be read for
every token it generates, whether that token is for one image or for twelve.
Measured on the RTX 5060 Ti, over the archive's own renditions:

    batch  1    1.47 s/image     7.3 h for the library
    batch  4    0.78 s/image     3.9 h
    batch  8    0.48 s/image     2.4 h
    batch 16    0.38 s/image     1.9 h   (11.2GB, too close to the edge)

Threads do not help — four concurrent single-image calls take exactly four times
as long as one, because they serialise on the same kernels. Only a real batch
does. So the requests have to meet somewhere, and the only place they can is
here: photod's vision pool is several workers each holding one asset's job, and
it must stay that way — a job is a claim on one photograph, and batching at the
queue level would mean a lease, a heartbeat and a failure shared between eight
unrelated files.

This is what worker.go's comment meant by "the batching that would help lives on
the other side of the socket".

The mechanism is one thread and a queue, deliberately, rather than leader
election among the request threads. A caller enqueues and waits; the collector
takes the first thing it sees, waits a few milliseconds for company, and runs
whatever turned up as one call. Nothing here can deadlock: there is exactly one
thread that ever calls the model, every wait has a timeout, and the loop body
cannot raise past itself — a failure fails the batch that caused it and the
collector goes back to waiting.
"""

from __future__ import annotations

import logging
import queue
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable

log = logging.getLogger("photo_ml.batching")


@dataclass
class _Slot:
    """One caller's request, and the place its answer will be put."""

    items: list[Any]
    done: threading.Event = field(default_factory=threading.Event)
    result: list[Any] | None = None
    error: BaseException | None = None


class MicroBatcher:
    """Collects concurrent calls into batched ones.

    `run` is handed a flat list of items and must return one result per item, in
    order. It is called from the collector thread and from nowhere else, which
    is what makes borrowing a model through Residency inside it safe: the
    refcount is taken and dropped by one thread per batch rather than by every
    request.
    """

    def __init__(
        self,
        name: str,
        run: Callable[[list[Any]], list[Any]],
        max_items: int,
        window: float,
        wait_timeout: float = 600.0,
    ) -> None:
        self._name = name
        self._run = run
        self._max = max(1, max_items)
        self._window = max(0.0, window)
        self._wait = wait_timeout
        self._queue: queue.Queue[_Slot] = queue.Queue()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        if self._thread is not None:
            return
        self._thread = threading.Thread(
            target=self._loop, name=f"batch-{self._name}", daemon=True
        )
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()

    def submit(self, items: list[Any]) -> list[Any]:
        """Hand over some items and wait for their results.

        Raises whatever the model raised, so the caller's error handling is the
        same as it would be without batching in the way. A batch that fails
        fails for everyone in it, which is the one behaviour this introduces
        that direct calls do not have — and it is the right one: what fails a
        batched generate is a card that went away or weights that would not
        load, and both of those were about to fail the next request too.
        """
        if not items:
            return []
        slot = _Slot(items=list(items))
        self._queue.put(slot)

        if not slot.done.wait(self._wait):
            # The collector has not answered in ten minutes. The HTTP client
            # gave up long ago; this exists so the thread does not leak.
            raise TimeoutError(f"{self._name} did not answer within {self._wait:.0f}s")
        if slot.error is not None:
            raise slot.error
        return slot.result or []

    def _loop(self) -> None:
        while not self._stop.is_set():
            try:
                # A poll rather than a block, so stop() is noticed by a service
                # that is shutting down with an empty queue.
                first = self._queue.get(timeout=0.25)
            except queue.Empty:
                continue
            self._serve(self._gather(first))

    def _gather(self, first: _Slot) -> list[_Slot]:
        """Wait a moment for company, then take everything that turned up.

        The window is small — tens of milliseconds — because it is latency that
        every request pays and only a backfill benefits from. With photod's
        vision pool running four workers, four requests are already in flight
        before the first one is picked up, so in practice this returns
        immediately with a full batch and the window only matters at the very
        end of a queue.
        """
        batch = [first]
        total = len(first.items)
        deadline = time.monotonic() + self._window

        while total < self._max:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            try:
                nxt = self._queue.get(timeout=remaining)
            except queue.Empty:
                break
            batch.append(nxt)
            total += len(nxt.items)
        return batch

    def _serve(self, batch: list[_Slot]) -> None:
        items = [item for slot in batch for item in slot.items]
        try:
            results = self._run(items)
            if len(results) != len(items):
                raise RuntimeError(
                    f"{self._name} returned {len(results)} results for {len(items)} items"
                )
        except BaseException as exc:  # noqa: BLE001 — re-raised in every caller
            log.exception("batched %s failed for %d items", self._name, len(items))
            for slot in batch:
                slot.error = exc
                slot.done.set()
            return

        at = 0
        for slot in batch:
            slot.result = results[at : at + len(slot.items)]
            at += len(slot.items)
            slot.done.set()
