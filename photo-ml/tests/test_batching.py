"""The one piece of concurrency in this service, tested on its own.

Everything else here is a model call with a request wrapped round it. This is a
thread, a queue and a set of waits, and a bug in it does not look like a bug —
it looks like a backfill that stopped moving at three in the morning.
"""

from __future__ import annotations

import threading
import time

import pytest

from photo_ml.batching import MicroBatcher


def batcher(run, max_items=8, window=0.05, wait_timeout=5.0):
    b = MicroBatcher("test", run, max_items=max_items, window=window, wait_timeout=wait_timeout)
    b.start()
    return b


def test_one_caller_gets_its_own_results_back():
    b = batcher(lambda items: [x * 2 for x in items])
    try:
        assert b.submit([1, 2, 3]) == [2, 4, 6]
    finally:
        b.stop()


def test_concurrent_callers_share_a_batch_and_get_their_own_slice():
    """The whole point: several requests, one call, and nobody gets anybody
    else's answer."""
    batches: list[list[int]] = []

    def run(items):
        batches.append(list(items))
        return [x * 10 for x in items]

    b = batcher(run, window=0.2)
    results: dict[int, list[int]] = {}

    def caller(n):
        results[n] = b.submit([n, n])

    try:
        threads = [threading.Thread(target=caller, args=(i,)) for i in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=5)
    finally:
        b.stop()

    for i in range(4):
        assert results[i] == [i * 10, i * 10], f"caller {i} got {results[i]}"
    assert len(batches) == 1, f"expected one batched call, got {batches}"
    assert len(batches[0]) == 8


def test_a_batch_stops_growing_at_the_cap():
    """VRAM is what the cap is protecting, so it has to actually cap."""
    sizes: list[int] = []

    def run(items):
        sizes.append(len(items))
        return list(items)

    b = batcher(run, max_items=4, window=0.3)
    try:
        threads = [threading.Thread(target=lambda: b.submit([1, 1, 1])) for _ in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=5)
    finally:
        b.stop()

    # Overshoot by at most one request's worth: the collector cannot see how big
    # the next item is until it has taken it, and putting it back would reorder
    # the queue.
    assert sizes, "nothing ran"
    assert max(sizes) <= 4 + 3, f"batch sizes {sizes} went past the cap"


def test_a_failure_reaches_every_caller_in_the_batch():
    """What fails a batched generate is a card that went away, and it was about
    to fail the next request too. Everyone hears about it, and nobody is left
    waiting."""

    def run(items):
        raise RuntimeError("the card went away")

    b = batcher(run, window=0.2)
    errors: list[BaseException] = []

    def caller():
        try:
            b.submit([1])
        except BaseException as exc:  # noqa: BLE001
            errors.append(exc)

    try:
        threads = [threading.Thread(target=caller) for _ in range(3)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=5)
    finally:
        b.stop()

    assert len(errors) == 3
    assert all("card went away" in str(e) for e in errors)


def test_the_collector_survives_a_failure_and_serves_the_next_batch():
    """A loop body that could raise past itself would take the service down for
    every photograph after the one that broke."""
    calls = {"n": 0}

    def run(items):
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("first one is bad")
        return [x + 1 for x in items]

    b = batcher(run)
    try:
        with pytest.raises(RuntimeError):
            b.submit([1])
        assert b.submit([1]) == [2]
    finally:
        b.stop()


def test_a_wrong_length_answer_is_refused_rather_than_misattributed():
    """Silently handing caller A the caption of caller B's photograph is the
    worst thing this module could do."""
    b = batcher(lambda items: [1])
    try:
        with pytest.raises(RuntimeError, match="results for"):
            b.submit([1, 2, 3])
    finally:
        b.stop()


def test_an_empty_request_never_reaches_the_model():
    ran = {"n": 0}

    def run(items):
        ran["n"] += 1
        return list(items)

    b = batcher(run)
    try:
        assert b.submit([]) == []
        assert ran["n"] == 0
    finally:
        b.stop()


def test_a_caller_is_not_left_waiting_forever_on_a_stalled_model():
    def run(items):
        time.sleep(2)
        return list(items)

    b = batcher(run, wait_timeout=0.3)
    try:
        with pytest.raises(TimeoutError):
            b.submit([1])
    finally:
        b.stop()
