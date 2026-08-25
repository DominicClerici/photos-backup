"""The card's occupancy rules, which are the ones that fail at 3am or not at all."""

import threading
import time

import pytest

from photo_ml.residency import Residency, Role


class Fake:
    """Something loadable that says whether it was let go of."""

    def __init__(self, name: str) -> None:
        self.name = name
        self.unloaded = False

    def unload(self) -> None:
        self.unloaded = True


def build(**kw) -> Residency:
    return Residency(idle_seconds=kw.pop("idle", 3600.0), sweep_seconds=kw.pop("sweep", 0.01))


def test_the_captioner_evicts_the_recogniser_on_the_way_in():
    """Ten gigabytes and a detection do not both fit, so one of them leaves."""
    r = build()
    ocr, caption = Fake("ocr"), Fake("caption")
    r.register("ocr", name="rapidocr-v6-medium", role=Role.ON_DEMAND, load=lambda: ocr)
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: caption, evicts=("ocr",))

    with r.use("ocr"):
        pass
    assert [e["resident"] for e in r.report() if e["key"] == "ocr"] == [True]

    with r.use("caption"):
        resident = {e["key"]: e["resident"] for e in r.report()}
    assert resident["ocr"] is False, "the recogniser should have been unloaded"
    assert resident["caption"] is True
    assert ocr.unloaded, "unload() is what hands the VRAM back; dropping the reference does not"


def test_eviction_does_not_run_the_other_way():
    """The recogniser never evicts the captioner.

    Not only to keep the lock order acyclic — reloading nine gigabytes to read
    the text in one photograph costs far more than the read is worth.
    """
    r = build()
    ocr, caption = Fake("ocr"), Fake("caption")
    r.register("ocr", name="rapidocr-v6-medium", role=Role.ON_DEMAND, load=lambda: ocr)
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: caption, evicts=("ocr",))

    with r.use("caption"):
        pass
    with r.use("ocr"):
        pass

    assert not caption.unloaded
    assert {e["key"]: e["resident"] for e in r.report()} == {"ocr": True, "caption": True}


def test_a_model_in_use_is_never_evicted():
    """Unloading weights another thread is mid-forward-pass on is a segfault.

    So the load goes ahead beside it. If that runs the card out, photo-ml
    answers 503 and mlclient puts the job down without spending an attempt —
    which is the failure to prefer over a crashed service.
    """
    r = build()
    ocr, caption = Fake("ocr"), Fake("caption")
    r.register("ocr", name="rapidocr-v6-medium", role=Role.ON_DEMAND, load=lambda: ocr)
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: caption, evicts=("ocr",))

    entered, release = threading.Event(), threading.Event()

    def hold():
        with r.use("ocr"):
            entered.set()
            release.wait(5)

    t = threading.Thread(target=hold)
    t.start()
    assert entered.wait(5)
    try:
        with r.use("caption"):
            pass
        assert not ocr.unloaded, "a model in use must survive an eviction"
    finally:
        release.set()
        t.join(5)


def test_a_resident_model_is_never_reaped():
    r = build(idle=0.0)
    encoder = Fake("encoder")
    r.register("vision", name="siglip", role=Role.RESIDENT, load=lambda: encoder)
    r.start()
    for _ in range(100):
        if r.ready():
            break
        time.sleep(0.01)
    assert r.ready()
    time.sleep(0.1)
    r.stop()
    assert not encoder.unloaded


def test_release_hands_back_an_idle_captioner():
    """The case _evict_for is right to refuse and photod is right to ask for.

    Evicting the captioner to read one photograph is a bad trade while there are
    captions still owed. When there are none, the reload it is protecting never
    happens, and five minutes of idle_seconds is five minutes of a nearly full
    card.
    """
    r = build()
    caption = Fake("caption")
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: caption)

    with r.use("caption"):
        pass
    assert [e["resident"] for e in r.report() if e["key"] == "caption"] == [True]

    assert r.release("caption") is True
    assert caption.unloaded, "unload() is what hands the VRAM back"
    assert [e["resident"] for e in r.report() if e["key"] == "caption"] == [False]


def test_release_leaves_a_model_that_is_in_use():
    """Unloading weights another thread is mid-forward-pass on is a segfault."""
    r = build()
    caption = Fake("caption")
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: caption)

    with r.use("caption"):
        assert r.release("caption") is False
    assert not caption.unloaded


def test_release_will_not_touch_a_resident_model():
    """The encoder and the parser answer a search box; nothing may send them away."""
    r = build()
    vision = Fake("vision")
    r.register("vision", name="siglip", role=Role.RESIDENT, load=lambda: vision)
    r.start()
    try:
        assert r.release("vision") is False
        assert not vision.unloaded
    finally:
        r.stop()


def test_release_of_something_absent_is_not_an_error():
    """photod says what it knows on every ocr request; most say nothing to do."""
    r = build()
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: Fake("caption"))
    assert r.release("caption") is False
    assert r.release("nothing-registered") is False
