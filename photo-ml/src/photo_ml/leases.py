"""Who gets the card, and for how long.

residency.py answers "what is loaded". This answers the question in front of it:
whether anything should be loaded at all right now, and for whose benefit. They
were one thing until the archive had been running for a while and it became
obvious that they are not, because the two models search needs were declared
RESIDENT — loaded at startup, never given back — and a machine with nobody
looking at it was holding three gigabytes of weights plus a CUDA context, all
day, for a search box nobody had open.

The fix is not a shorter idle timer. An idle timer is a guess at "nothing will
want this again soon" made by a process that cannot see a browser; guessing
better is still guessing. What actually knows is photod, which is serving the
gallery, and what photod can say is a lease.

Two of them, and they are mutually exclusive:

  search  the encoder and the query parser, about 3GB, pinned while somebody has
          the gallery open. Taken on a page load or an app coming to the
          foreground and renewed by ordinary gallery traffic, so it lapses five
          minutes after the last person stopped looking. Refused while an
          ingestion pass holds the card.
  ingest  the right to run the expensive passes — captions, text recognition,
          the embedding backfill. Held by photod's vision pool while it has
          work, released the moment its queue goes dry. Refused while search is
          held.

Mutual exclusion rather than a priority, and it is worth being explicit that
this is a choice. The captioner peaks at 12.98GB with the resident models in
place, which left 0.71GB of a 15.51GB card — the margin NVENC needs, and nothing
more. Adding a browser to that is where the 503s came from. So the two do not
overlap at all: a backfill runs on an idle machine and yields the whole card to
the first person who opens the gallery on the next pass of photod's gate, and a
search that arrives mid-backfill is answered by Postgres alone. That last part
is a real cost and it is the right one — a text-ranked search returns
photographs, and a search that hangs for twenty seconds waiting on a checkpoint
does not.

Each lease also has a budget, checked against vram.Card at the moment it is
taken: 8GB of somebody else's VRAM is enough to refuse search, 4GB is enough to
refuse an ingestion pass. Asymmetric because the consequences are: search is
three gigabytes that somebody is waiting for, and a backfill is ten that nobody
is. The budgets are about processes outside this project entirely — a desktop
session, a game, something else with a model open. photod's own NVENC
transcoding is counted as ours; see vram.py.

Nothing here decides *when* to ask. photod does, because photod is the only
process that can see either a gallery request or a queue.
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass, field
from typing import Any

from .residency import Residency
from .vram import Card

log = logging.getLogger("photo_ml.leases")

SEARCH = "search"
INGEST = "ingest"

GIB = 1 << 30


@dataclass(frozen=True)
class Group:
    """One lease's policy: what it pins, what it costs, how long it lasts."""

    name: str
    # Residency keys loaded when this lease is taken and unloaded when it
    # lapses. Empty for `ingest`, which does not pin anything: the captioner and
    # the recogniser are still loaded on demand by the routes that need them and
    # still reaped when they go idle, and what the lease buys is the guarantee
    # that nothing will pin three gigabytes of search underneath them.
    pins: tuple[str, ...]
    # How much VRAM outside this project is enough to refuse. See the module
    # docstring for why the two numbers are not the same.
    budget: int
    # How long a grant lasts when the caller does not name a term, and the cap
    # on what a caller may name. A lease nobody renews has to expire, because
    # the process holding it is allowed to be restarted, killed, or on the other
    # side of a network cable that has just been unplugged.
    default_ttl: float
    max_ttl: float


@dataclass
class State:
    expires_at: float | None = None
    taken_at: float | None = None
    # The last refusal, kept so /health can say why the card is empty rather
    # than only that it is.
    last_refusal: str = ""
    warming: threading.Thread | None = field(default=None, repr=False)


@dataclass(frozen=True)
class Grant:
    """The answer to one acquire, and everything the caller needs from it.

    `held` and `ready` are deliberately two fields. A grant is immediate and
    loading 3GB of weights is not, so the twenty seconds between them is a real
    state that photod has to be able to see: a search issued in that window
    should fall back to the text ranking rather than block a person behind a
    checkpoint. See internal/api/search.go.
    """

    group: str
    held: bool
    ready: bool
    reason: str
    expires_in: float | None
    foreign_bytes: int | None
    budget_bytes: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "group": self.group,
            "held": self.held,
            "ready": self.ready,
            "reason": self.reason,
            "expires_in": round(self.expires_in, 1) if self.expires_in is not None else None,
            "foreign_bytes": self.foreign_bytes,
            "budget_bytes": self.budget_bytes,
        }


class Arbiter:
    """The two leases, and the one lock they are decided under.

    One lock for both, and that is the point of the class. Deciding them
    independently would let a gallery request and a vision worker each read a
    card with four gigabytes free and each conclude they may have it.
    """

    def __init__(self, residency: Residency, card: Card, groups: dict[str, Group]) -> None:
        self._residency = residency
        self._card = card
        self._groups = groups
        self._state = {name: State() for name in groups}
        self._mu = threading.RLock()
        # The pinned keys, as a snapshot the reaper can read without taking the
        # lock above. It has to be able to: the reaper asks this question while
        # holding a residency Entry's lock, and releasing a lease takes those
        # same locks while holding this one — so a `pins()` that locked would be
        # a lock-order inversion between the sweep thread and whichever thread
        # is opening a gallery. Rebound rather than mutated, so a reader either
        # sees the whole old set or the whole new one.
        self._pinned: frozenset[str] = frozenset()

    # -- asking ------------------------------------------------------------

    def acquire(self, name: str, ttl: float | None = None) -> Grant:
        """Take or renew a lease.

        Renewing is the same call as taking, on purpose: photod pushes the
        deadline forward on ordinary gallery traffic and on every pass of the
        vision pool's gate, and neither of those knows or should have to know
        whether it is the first one. A renewal skips the budget check — the
        weights are already on the card, and unloading them halfway through
        somebody's search because a game started would cost the search and free
        nothing that the game is not already using.
        """
        group = self._groups.get(name)
        if group is None:
            raise KeyError(f"no lease group named {name!r}")

        with self._mu:
            self._expire_locked()
            state = self._state[name]
            term = _term(ttl, group)

            if state.expires_at is not None:
                state.expires_at = time.monotonic() + term
                return self._grant_locked(name, "renewed")

            other = self._blocking_locked(name)
            if other is not None:
                state.last_refusal = f"the {other} lease holds the card"
                return self._refusal_locked(name, state.last_refusal)

            foreign = self._card.foreign_bytes()
            if foreign is not None and foreign >= group.budget:
                state.last_refusal = (
                    f"{foreign / GIB:.1f}GB of the card is held outside this archive; "
                    f"the {name} lease wants it under {group.budget / GIB:.0f}GB"
                )
                return self._refusal_locked(name, state.last_refusal)

            now = time.monotonic()
            state.expires_at = now + term
            state.taken_at = now
            state.last_refusal = ""
            self._repin_locked()
            log.info("took the %s lease for %.0fs", name, term)
            self._warm_locked(name, group)
            return self._grant_locked(name, "taken")

    def release(self, name: str) -> Grant:
        """Hand a lease back now, rather than waiting for it to lapse.

        The vision pool calls this the moment its queue goes dry, which is the
        whole difference between "the captioner is unloaded a few minutes after
        the backfill ends" and "the gallery is answerable again on the next
        request". photod knows exactly when there is no more work; a clock never
        will.
        """
        group = self._groups.get(name)
        if group is None:
            raise KeyError(f"no lease group named {name!r}")
        with self._mu:
            self._release_locked(name, group, "released")
            return self._grant_locked(name, "released")

    def held(self, name: str) -> bool:
        with self._mu:
            self._expire_locked()
            return self._state[name].expires_at is not None

    def pins(self, key: str) -> bool:
        """Whether a live lease is holding this residency key on the card.

        Installed on the Residency as its pin check, which is the whole of what
        that module needs to know about any of this: not who is looking at the
        gallery, only that the reaper's clock is not the last word on one
        particular set of weights.

        Deliberately lock-free — see `_pinned`. It reads a snapshot rather than
        recomputing, which means a term that has just run out can still read as
        pinned for the rest of one sweep. It cannot: the reaper runs the expiry
        hook before it reaps, so by the time this is asked the snapshot is of
        the same instant the clock was read at.
        """
        return key in self._pinned

    def status(self, name: str) -> Grant:
        with self._mu:
            self._expire_locked()
            state = self._state[name]
            if state.expires_at is None:
                return self._refusal_locked(name, state.last_refusal or "not held")
            return self._grant_locked(name, "held")

    def sweep(self) -> None:
        """Let go of anything whose term is up. Called by residency's reaper."""
        with self._mu:
            self._expire_locked()

    def report(self) -> list[dict[str, Any]]:
        with self._mu:
            self._expire_locked()
            now = time.monotonic()
            out = []
            for name, group in self._groups.items():
                state = self._state[name]
                out.append(
                    {
                        "group": name,
                        "held": state.expires_at is not None,
                        "ready": self._ready_locked(name),
                        "pins": list(group.pins),
                        "budget_bytes": group.budget,
                        "expires_in": round(state.expires_at - now, 1) if state.expires_at else None,
                        "held_for_seconds": round(now - state.taken_at, 1) if state.taken_at else None,
                        "last_refusal": state.last_refusal,
                    }
                )
            return out

    # -- deciding ----------------------------------------------------------

    def _repin_locked(self) -> None:
        """Rebuild the lock-free snapshot `pins()` reads."""
        keys: set[str] = set()
        for name, state in self._state.items():
            if state.expires_at is not None:
                keys.update(self._groups[name].pins)
        self._pinned = frozenset(keys)

    def _blocking_locked(self, name: str) -> str | None:
        """Which other live lease refuses this one. See the module docstring."""
        for other, state in self._state.items():
            if other != name and state.expires_at is not None:
                return other
        return None

    def _expire_locked(self) -> None:
        now = time.monotonic()
        for name, state in self._state.items():
            if state.expires_at is not None and state.expires_at <= now:
                self._release_locked(name, self._groups[name], "lapsed")

    def _release_locked(self, name: str, group: Group, why: str) -> None:
        state = self._state[name]
        if state.expires_at is None:
            return
        state.expires_at = None
        state.taken_at = None
        self._repin_locked()
        log.info("the %s lease %s", name, why)
        # Outside the residency entry's own lock by construction: `release`
        # there declines to touch a model another thread is mid-forward-pass on,
        # which is the same rule the reaper follows and for the same reason. A
        # search that is in flight keeps its weights; the next sweep gets them.
        for key in group.pins:
            self._residency.release(key)

    def _warm_locked(self, name: str, group: Group) -> None:
        """Load what this lease pins, on a thread.

        On a thread because the caller is a page load. photod asks for this
        while a browser is fetching its first screen of thumbnails, and the
        answer it needs — yes, you have the card — is known immediately; the
        twenty seconds of checkpoint that follow are what `ready` is for.
        """
        state = self._state[name]
        if not group.pins:
            return
        if state.warming is not None and state.warming.is_alive():
            return

        def warm() -> None:
            for key in group.pins:
                try:
                    # Borrowed and given straight back, rather than loaded
                    # directly, because `use` is the only path that takes the
                    # entry's lock around the load — a second thread asking for
                    # the same model mid-checkpoint waits for it rather than
                    # starting its own.
                    with self._residency.use(key):
                        pass
                except KeyError:
                    # This instance was started without that model — see
                    # settings.models. Not an error: the route answers 404 and
                    # the lease is doing no harm.
                    continue
                except Exception:
                    log.exception("could not load %s for the %s lease", key, group.name)

        state.warming = threading.Thread(target=warm, name=f"warm-{name}", daemon=True)
        state.warming.start()

    def _ready_locked(self, name: str) -> bool:
        """Whether everything this lease pins is actually on the card.

        Vacuously true for a lease that pins nothing, which is the honest answer
        for `ingest`: what it holds is permission, and permission is never
        warming up.
        """
        group = self._groups[name]
        if not group.pins:
            return self._state[name].expires_at is not None
        return all(self._residency.loaded(key) for key in group.pins)

    def _grant_locked(self, name: str, reason: str) -> Grant:
        state = self._state[name]
        group = self._groups[name]
        return Grant(
            group=name,
            held=state.expires_at is not None,
            ready=self._ready_locked(name),
            reason=reason,
            expires_in=state.expires_at - time.monotonic() if state.expires_at else None,
            foreign_bytes=self._card.foreign_bytes(),
            budget_bytes=group.budget,
        )

    def _refusal_locked(self, name: str, reason: str) -> Grant:
        group = self._groups[name]
        log.debug("refused the %s lease: %s", name, reason)
        return Grant(
            group=name,
            held=False,
            ready=False,
            reason=reason,
            expires_in=None,
            foreign_bytes=self._card.foreign_bytes(),
            budget_bytes=group.budget,
        )



def _term(ttl: float | None, group: Group) -> float:
    """How long this grant lasts.

    Capped rather than trusted. The term is how long the card stays committed
    after the holder stops talking, and a caller that asks for a day has either
    made a mistake or is about to be killed — either way the card should not be
    unusable until tomorrow because of it.
    """
    if ttl is None or ttl <= 0:
        return group.default_ttl
    return min(float(ttl), group.max_ttl)
