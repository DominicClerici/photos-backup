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


def test_a_pinned_model_is_not_reaped():
    """A lease is what keeps the search models loaded through a quiet minute.

    Somebody with the gallery open who has not typed for six minutes is exactly
    the case the pin exists for: the reaper's clock says idle and it is wrong,
    because the next thing that happens is a search.
    """
    r = build(idle=0.0)
    encoder = Fake("encoder")
    r.register("vision", name="siglip", role=Role.LEASED, load=lambda: encoder)
    r.pin_check(lambda key: key == "vision")
    r.start()
    try:
        with r.use("vision"):
            pass
        time.sleep(0.1)
        assert not encoder.unloaded
    finally:
        r.stop()


def test_a_leased_model_nothing_pins_is_reaped_like_any_other():
    """A request that arrives outside a lease costs twenty seconds, not 1.8GB.

    Nothing takes a lease for an admin curl or for a search that raced a lapse,
    and without this the weights it loaded would sit on the card until restart.
    """
    r = build(idle=0.0)
    encoder = Fake("encoder")
    r.register("vision", name="siglip", role=Role.LEASED, load=lambda: encoder)
    r.start()
    try:
        with r.use("vision"):
            pass
        for _ in range(200):
            if encoder.unloaded:
                break
            time.sleep(0.01)
        assert encoder.unloaded
    finally:
        r.stop()


def test_nothing_is_loaded_at_startup():
    """The whole point: an archive nobody is looking at holds no VRAM.

    This used to be the opposite assertion — the encoder and the parser were
    warmed on the way up and never given back — and an idle machine held three
    gigabytes all day for a search box nobody had open.
    """
    r = build()
    loaded = []
    r.register("vision", name="siglip", role=Role.LEASED, load=lambda: loaded.append("vision") or Fake("v"))
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: Fake("c"))
    r.start()
    try:
        time.sleep(0.1)
        assert loaded == []
        assert all(not e["resident"] for e in r.report())
        assert r.ready(), "a service holding nothing is still a service that can be given work"
    finally:
        r.stop()


def test_ready_is_false_only_when_nothing_can_load():
    """One broken checkpoint must not idle the passes that have nothing to do
    with it: the three kinds fail independently and mlclient defers per job."""
    r = build()
    r.register("vision", name="siglip", role=Role.LEASED, load=lambda: Fake("v"))
    r.register("ocr", name="rapidocr", role=Role.ON_DEMAND, load=_explode)

    with pytest.raises(RuntimeError):
        with r.use("ocr"):
            pass
    assert r.ready(), "the encoder is still perfectly able to embed"

    broken = build()
    broken.register("ocr", name="rapidocr", role=Role.ON_DEMAND, load=_explode)
    with pytest.raises(RuntimeError):
        with broken.use("ocr"):
            pass
    assert not broken.ready(), "nothing here can load; claiming work would only queue failures"


def _explode():
    raise RuntimeError("no such checkpoint")


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


def test_release_hands_back_a_leased_model_too():
    """Which is how a lapsed lease gives the card back. See leases.Arbiter."""
    r = build()
    vision = Fake("vision")
    r.register("vision", name="siglip", role=Role.LEASED, load=lambda: vision)
    with r.use("vision"):
        pass
    assert r.release("vision") is True
    assert vision.unloaded


def test_release_of_something_absent_is_not_an_error():
    """photod says what it knows on every ocr request; most say nothing to do."""
    r = build()
    r.register("caption", name="qwen", role=Role.ON_DEMAND, load=lambda: Fake("caption"))
    assert r.release("caption") is False
    assert r.release("nothing-registered") is False
