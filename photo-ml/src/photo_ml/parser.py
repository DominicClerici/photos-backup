"""What a typed sentence was asking for — as a suggestion, not an answer.

This is the model ML_IMAGES.md §11 warns about: "a query parser fails
*confidently*. It silently filters out the right answer because it decided 'last
summer' meant 2024, and the user sees an empty grid with no evidence of why."

So it is not the parser. `internal/searchquery` is the parser — a deterministic
Go grammar that reads dates, matches names against the archive's own list of
people and places, and hands the rest to the encoder. This model runs *after*
it and is only allowed to speak where the grammar was silent, and everything it
says is checked against that same list before any of it is believed. A person
who is not in asset_people, a date that does not read, a phrase nobody typed:
dropped. See searchquery.Merge.

Which is why it is 0.6B. Its whole job is to notice that "the snowy one from the
ski trip with Chris" mentions somebody, when the grammar only ever finds a name
spelled the way the archive spells it. That is a small job, it is verified
downstream, and the alternative — a 4B model resident on a card that has to fit
a captioner beside it — buys accuracy in a place where accuracy is not what is
load-bearing.

Leased rather than on demand, unlike the captioner. A query waits on this, and a
search box that pauses twenty seconds to load a checkpoint is not a search box —
but only while there is a search box open, which is why it is a lease rather
than the permanent residency it used to be. See leases.py.
"""

from __future__ import annotations

import json
import logging
import os
import re

import torch
from transformers import AutoModelForCausalLM, AutoTokenizer

log = logging.getLogger("photo_ml.parser")

# 0.6B, and it is worth being honest about what that buys. Measured against this
# archive's own queries, the gates in searchquery.Merge reject nearly everything
# this model produces: it answers "today" to questions with no date in them, it
# offers "beach" as a place, and asked to spell a loose name it usually declines.
# What survives the gates is correct and occasionally useful — "addy and me in
# mexico" comes back with both — and what does not survive costs nothing, which
# is exactly the arrangement §11 asks for.
#
# It stays small because it is *pinned for the whole of a browsing session*, and
# because the lease that pins it is the thing standing between a backfill and the
# card. PHOTO_ML_PARSER_MODEL swaps it: Qwen3-1.7B is meaningfully better at this
# and costs 3.4GB, which is affordable now in a way it was not when this was
# resident — the two never share the card, so what it competes with is the
# encoder and a desktop session rather than a 9.6GB captioner.
HF_ID = os.environ.get("PHOTO_ML_PARSER_MODEL", "Qwen/Qwen3-0.6B")
MODEL_NAME = HF_ID.split("/")[-1].lower()

# Enough for the JSON object below and nothing else. The failure this bounds is
# a small model that starts explaining itself.
MAX_NEW_TOKENS = 160

# Five fields, and the ones that are missing were removed deliberately. Media
# kind, category and favourites are a closed list of English that the Go grammar
# already reads perfectly — asking about them here buys nothing and invites
# disagreement. Tags are absent because filtering by a word another model
# invented, chosen by this one, is two guesses stacked. See searchquery.Merge.
SYSTEM = """You extract search filters from a photo-library query. Reply with JSON only, no explanation.

Shape, and nothing else:
{"people": [], "place": "", "after": "", "before": "", "visual": ""}

people:  people the query actually names, spelled as in the known-people list.
         [] unless a person is mentioned by name in the query itself.
place:   a city, state or country the query actually names. "" otherwise.
after,
before:  an inclusive date range as YYYY-MM-DD, worked out from today's date.
         Both "" unless the query says something about when.
visual:  the words describing what the photograph looks like, copied from the
         query with names, dates and places taken out. "" if there are none.

Rules:
- Every value must come from words in the query. Never invent anything.
- The known-people list is only there for spelling. Never copy a name out of it
  that the query does not mention.
- Leave a field empty when you are unsure. Empty is a good answer."""


class QueryParser:
    name = MODEL_NAME

    def __init__(self, device: torch.device, dtype: torch.dtype, cache_dir: str | None) -> None:
        self.device = device
        self.dtype = dtype
        self._model = (
            AutoModelForCausalLM.from_pretrained(HF_ID, dtype=dtype, cache_dir=cache_dir)
            .to(device)
            .eval()
        )
        self._tokenizer = AutoTokenizer.from_pretrained(HF_ID, cache_dir=cache_dir)

    def unload(self) -> None:
        self._model = None
        self._tokenizer = None

    @torch.inference_mode()
    def parse(self, query: str, today: str, people: list[str]) -> dict:
        """Read one query. Never raises; an unreadable answer is an empty parse.

        today is passed in because the model has no clock and "last summer" is
        unanswerable without one. The known people are passed for their
        *spelling* — this model's one real job is turning "chris" into "Chris
        Morrison" — and for no other purpose: the Go side checks every name that
        comes back against the query itself, so a model that copies the whole
        list out (which a 0.6B reliably does) contributes exactly nothing rather
        than six ANDed filters that match no photograph. See
        searchquery.mentions.
        """
        known = ", ".join(people) if people else "(none)"
        prompt = self._tokenizer.apply_chat_template(
            [
                {"role": "system", "content": SYSTEM},
                {
                    "role": "user",
                    "content": f"Today is {today}.\nKnown people: {known}\n\nQuery: {query}",
                },
            ],
            tokenize=False,
            add_generation_prompt=True,
            # Qwen3 is a hybrid-thinking model and will otherwise spend its
            # entire budget in a <think> block before writing any JSON. The
            # thinking is not worth eight seconds of somebody's search.
            enable_thinking=False,
        )

        inputs = self._tokenizer([prompt], return_tensors="pt").to(self.device)
        generated = self._model.generate(
            **inputs, max_new_tokens=MAX_NEW_TOKENS, do_sample=False
        )
        answer = self._tokenizer.decode(
            generated[0][len(inputs.input_ids[0]):], skip_special_tokens=True
        )
        return _read(answer)


_OBJECT = re.compile(r"\{.*\}", re.DOTALL)


def _read(answer: str) -> dict:
    """Whatever came back, as the fields the Go side knows how to check.

    An empty parse is a perfectly good answer here, and by far the most common
    correct one: most queries have nothing for this model to add, and the
    grammar has already answered. Returning nothing costs a refinement; raising
    would cost the search.
    """
    empty = {"people": [], "place": "", "after": "", "before": "", "visual": ""}

    match = _OBJECT.search(answer.strip())
    if not match:
        log.debug("no JSON in the parse answer: %s", answer[:200])
        return empty
    try:
        parsed = json.loads(match.group(0))
    except json.JSONDecodeError:
        log.debug("unreadable parse JSON: %s", answer[:200])
        return empty
    if not isinstance(parsed, dict):
        return empty

    people = parsed.get("people") or []
    if isinstance(people, str):
        people = [people]

    return {
        "people": [str(p).strip() for p in people if str(p).strip()],
        "place": str(parsed.get("place") or "").strip(),
        "after": str(parsed.get("after") or "").strip(),
        "before": str(parsed.get("before") or "").strip(),
        "visual": str(parsed.get("visual") or "").strip(),
    }
