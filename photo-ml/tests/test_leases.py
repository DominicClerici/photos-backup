"""Who gets the card. The rules that decide whether an overnight pass runs."""

import threading
import time

import pytest

from photo_ml.leases import INGEST, SEARCH, Arbiter, Group
from photo_ml.residency import Residency, Role

GIB = 1 << 30


class Fake:
    def __init__(self, name: str) -> None:
        self.name = name
        self.unloaded = False

    def unload(self) -> None:
        self.unloaded = True


class StubCard:
    """A card whose occupancy the test sets. None is "cannot be measured"."""

    def __init__(self, foreign: int | None = 0) -> None:
        self.foreign = foreign

    def foreign_bytes(self) -> int | None:
        return self.foreign

    def report(self) -> dict:
        return {"measurable": self.foreign is not None, "foreign_bytes": self.foreign}


def build(foreign: int | None = 0, search_ttl: float = 300.0, ingest_ttl: float = 90.0):
    residency = Residency(idle_seconds=3600.0, sweep_seconds=0.01)
    models = {}
    for key in ("vision", "parse"):
        models[key] = Fake(key)
        residency.register(key, name=key, role=Role.LEASED, load=lambda k=key: models[k])
    card = StubCard(foreign)
    arbiter = Arbiter(
        residency,
        card,
        {
            SEARCH: Group(SEARCH, ("vision", "parse"), 8 * GIB, search_ttl, search_ttl * 2),
            INGEST: Group(INGEST, (), 4 * GIB, ingest_ttl, ingest_ttl * 4),
        },
    )
    residency.pin_check(arbiter.pins)
    return residency, arbiter, card, models


def warmed(residency: Residency, keys=("vision", "parse"), timeout: float = 5.0) -> bool:
    """The grant is immediate and the checkpoint is not; this is the gap."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if all(residency.loaded(key) for key in keys):
            return True
        time.sleep(0.005)
    return False


# ---------------------------------------------------------------------------
# The search lease


def test_a_page_load_loads_the_search_models():
    residency, arbiter, _, _ = build()
    grant = arbiter.acquire(SEARCH)

    assert grant.held
    assert grant.reason == "taken"
    assert warmed(residency), "the encoder and the parser should both arrive"


def test_the_grant_is_immediate_and_the_weights_are_not():
    """photod has to be able to tell the two apart: a search issued while the
    checkpoints are still arriving falls back to the text ranking rather than
    hanging a person behind twenty seconds of load."""
    residency, arbiter, _, _ = build()
    slow = Residency(idle_seconds=3600.0, sweep_seconds=0.01)
    holding = []
    slow.register("vision", name="v", role=Role.LEASED, load=lambda: (time.sleep(0.2), holding.append(1), Fake("v"))[-1])
    card = StubCard(0)
    arbiter = Arbiter(slow, card, {SEARCH: Group(SEARCH, ("vision",), 8 * GIB, 300.0, 600.0)})
    slow.pin_check(arbiter.pins)

    grant = arbiter.acquire(SEARCH)
    assert grant.held and not grant.ready
    assert warmed(slow, ("vision",))
    assert arbiter.status(SEARCH).ready


def test_search_is_refused_when_the_card_is_busy_elsewhere():
    """Nine gigabytes of somebody else's model is not a card to put three more
    on. The search box still works; it ranks by words. See api/search.go."""
    _, arbiter, _, _ = build(foreign=9 * GIB)
    grant = arbiter.acquire(SEARCH)

    assert not grant.held
    assert "outside this archive" in grant.reason
    assert grant.foreign_bytes == 9 * GIB


def test_a_card_that_cannot_be_measured_raises_no_objection():
    """A host with no driver, or a driver mid-upgrade, must degrade towards
    working: this is how the whole feature behaved before vram.py existed."""
    residency, arbiter, _, _ = build(foreign=None)
    assert arbiter.acquire(SEARCH).held
    assert warmed(residency)


def test_the_lease_lapses_and_takes_the_weights_with_it():
    residency, arbiter, _, models = build(search_ttl=0.05)
    arbiter.acquire(SEARCH)
    assert warmed(residency)

    time.sleep(0.1)
    arbiter.sweep()

    assert not arbiter.held(SEARCH)
    assert models["vision"].unloaded and models["parse"].unloaded, (
        "unload() is what hands the VRAM back; dropping the reference does not"
    )


def test_gallery_traffic_pushes_the_deadline_forward():
    """Five minutes since the last request, not five minutes since the first."""
    _, arbiter, _, _ = build(search_ttl=0.3)
    arbiter.acquire(SEARCH)
    for _ in range(4):
        time.sleep(0.1)
        assert arbiter.acquire(SEARCH).reason == "renewed"
    assert arbiter.held(SEARCH)


def test_a_renewal_does_not_re_ask_the_budget():
    """Unloading the encoder halfway through somebody's browsing session because
    a game started would cost the search and free nothing the game is not
    already using."""
    _, arbiter, card, _ = build()
    assert arbiter.acquire(SEARCH).held
    card.foreign = 15 * GIB
    assert arbiter.acquire(SEARCH).held


def test_a_term_longer_than_the_cap_is_trimmed():
    _, arbiter, _, _ = build(search_ttl=300.0)
    grant = arbiter.acquire(SEARCH, ttl=86400)
    assert grant.expires_in is not None and grant.expires_in <= 600.0


# ---------------------------------------------------------------------------
# The ingest lease, and the exclusion between them


def test_an_ingestion_pass_is_refused_while_the_gallery_is_open():
    """The captioner peaks at 12.98GB. Three more of search is where the 503s
    came from, so the two do not overlap at all."""
    _, arbiter, _, _ = build()
    assert arbiter.acquire(SEARCH).held

    grant = arbiter.acquire(INGEST)
    assert not grant.held
    assert grant.reason == "the search lease holds the card"


def test_the_gallery_is_refused_while_an_ingestion_pass_is_running():
    """The other direction, and the one that costs something visible: a search
    typed mid-backfill is answered by Postgres alone."""
    residency, arbiter, _, _ = build()
    assert arbiter.acquire(INGEST).held

    grant = arbiter.acquire(SEARCH)
    assert not grant.held
    assert grant.reason == "the ingest lease holds the card"
    assert not residency.loaded("vision"), "a refused lease must not warm anything"


def test_ingest_wants_more_of_the_card_free_than_search_does():
    """Search is three gigabytes somebody is waiting for; a backfill is ten that
    nobody is, held for hours."""
    _, arbiter, _, _ = build(foreign=6 * GIB)
    assert arbiter.acquire(SEARCH).held
    arbiter.release(SEARCH)
    assert not arbiter.acquire(INGEST).held


def test_releasing_the_pass_hands_the_gallery_the_card_back():
    """photod calls this the moment its queue goes dry. A clock could not know."""
    residency, arbiter, _, _ = build()
    arbiter.acquire(INGEST)
    arbiter.release(INGEST)

    assert arbiter.acquire(SEARCH).held
    assert warmed(residency)


def test_an_ingestion_pass_whose_holder_died_lets_go_on_its_own():
    """photod killed mid-backfill must not leave the card unusable until it is
    restarted. The term is what makes that true."""
    _, arbiter, _, _ = build(ingest_ttl=0.05)
    assert arbiter.acquire(INGEST).held

    time.sleep(0.1)
    assert not arbiter.held(INGEST)
    assert arbiter.acquire(SEARCH).held


def test_ingest_pins_nothing():
    """What the lease holds is permission. The captioner is still loaded by the
    route that needs it and still reaped when it goes idle, rather than sitting
    on ten gigabytes through every gap in a queue."""
    residency, arbiter, _, _ = build()
    arbiter.acquire(INGEST)
    time.sleep(0.05)
    assert not residency.loaded("vision")
    assert not arbiter.pins("vision")


def test_an_unknown_group_is_a_caller_error():
    _, arbiter, _, _ = build()
    with pytest.raises(KeyError):
        arbiter.acquire("everything")


# ---------------------------------------------------------------------------
# The lock order, which is the part that fails at 3am or not at all


def test_flapping_leases_do_not_deadlock_the_reaper():
    """The reaper asks whether a key is pinned while holding that key's lock,
    and releasing a lease takes those same locks while holding the arbiter's.
    That is a lock-order inversion unless `pins()` is lock-free, and the failure
    it produces is a service that answers /health and nothing else.
    """
    residency, arbiter, _, _ = build(search_ttl=0.02, ingest_ttl=0.02)
    residency.pin_check(arbiter.pins)
    residency.on_sweep(arbiter.sweep)
    residency.start()

    stop = threading.Event()
    errors: list[BaseException] = []

    def churn(group: str) -> None:
        try:
            while not stop.is_set():
                arbiter.acquire(group)
                arbiter.report()
                arbiter.release(group)
        except BaseException as exc:  # pragma: no cover - the failure being tested
            errors.append(exc)

    threads = [threading.Thread(target=churn, args=(name,)) for name in (SEARCH, INGEST, SEARCH)]
    for t in threads:
        t.start()
    time.sleep(1.0)
    stop.set()
    for t in threads:
        t.join(5)
        assert not t.is_alive(), "a thread is still holding a lock somebody else wants"
    residency.stop()
    assert not errors, errors
