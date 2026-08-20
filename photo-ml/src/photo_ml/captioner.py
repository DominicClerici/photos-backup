"""What a photograph is of, in a sentence and a handful of words.

A 4B vision-language model, loaded on demand and given back a few minutes after
the queue goes dry. It is the expensive half of this service by a wide margin —
four hours over a library the encoder gets through in sixteen minutes — which is
why ML_IMAGES.md §5 gives it its own job kind, and why residency.py exists at
all.

bf16 rather than a 4-bit quantisation, which is a deliberate step away from
§6's sketch. The card is Blackwell (sm_120) and the AWQ and GPTQ kernels are the
part of that ecosystem most likely to be missing an architecture — the failure
mode being a service that installs, starts, and then reports "no kernel image is
available for execution on the device" on the first forward pass, which is
exactly the 1am debugging session this whole design is trying to avoid. 4B in
bf16 costs about the same VRAM as 8B in 4-bit, needs no extra dependency, and
uses the code path everything else here already runs on.

The prompt is the other half of the design and it is deliberately narrow: no
names, no places, no guessing. Names come from asset_people, where a person
confirmed them, and places from the offline geocoder — ML_IMAGES.md §11's seam
between "a name somebody approved" and "a word a model produced" runs right
through this file, and a captioner inventing "Phoenix" or "Chicago" would be the
first thing to cross it.
"""

from __future__ import annotations

import json
import logging
import re

import torch
from PIL import Image
from transformers import AutoModelForImageTextToText, AutoProcessor

log = logging.getLogger("photo_ml.captioner")

# The checkpoint, and the name that ends up in asset_descriptions.model.
#
# Two strings on purpose, as with the encoder: the row records what produced a
# caption and will outlive whatever the weights were called on whichever mirror
# they came from, so the identity is ours. db.CaptionModel holds the same
# constant on the Go side, and rebuild_asset_search reads captions by it — a
# service quietly running something else writes rows that are correct, stored,
# and never reach the tsvector.
HF_ID = "Qwen/Qwen3-VL-4B-Instruct"
MODEL_NAME = "qwen3-vl-4b-instruct"

# The ML renditions are 512px on their longest edge (derivstore.MLEdge), so
# capping the processor here costs nothing and bounds the vision tokens: a
# photograph that somehow arrives larger is scaled down rather than turned into
# a thousand-token forward pass that takes four times as long for a caption
# nobody can tell apart.
MAX_PIXELS = 512 * 512
MIN_PIXELS = 224 * 224

# Long enough for a sentence and a dozen words, short enough that a model that
# has decided to write an essay is cut off rather than allowed to set the pace
# of the whole backfill. A truncated answer loses its closing brace and falls
# back to being read as prose; see _read.
MAX_NEW_TOKENS = 192

PROMPT = """You are indexing a personal photo library so that its owner can search it later.

Look at the image and reply with JSON only, in exactly this shape:
{"caption": "one sentence", "tags": ["word", "word", "word"]}

caption: one plain sentence, at most 25 words, describing what is visible.
tags: 4 to 10 lowercase words or two-word phrases. Cover the subject, the
setting, notable objects, the activity, and the occasion or mood when it is
obvious.

Rules:
- Describe only what you can actually see.
- Never guess anyone's name, and never guess a city, country or landmark name.
  Say "a man", "a dog", "a beach", "a ski resort".
- If the image is a screenshot or a photograph of text, say so and describe what
  kind of thing it shows.
- No preamble, no explanation, no markdown fence. JSON only."""


class Captioner:
    """Loaded weights, and the one thing they are asked."""

    name = MODEL_NAME

    def __init__(self, device: torch.device, dtype: torch.dtype, cache_dir: str | None) -> None:
        self.device = device
        self.dtype = dtype
        self._model = (
            AutoModelForImageTextToText.from_pretrained(HF_ID, dtype=dtype, cache_dir=cache_dir)
            .to(device)
            .eval()
        )
        self._processor = AutoProcessor.from_pretrained(
            HF_ID, cache_dir=cache_dir, min_pixels=MIN_PIXELS, max_pixels=MAX_PIXELS
        )

    def unload(self) -> None:
        """Drop the weights. Residency calls empty_cache() after this, which is
        the line that actually gives the VRAM back to NVENC."""
        self._model = None
        self._processor = None

    @torch.inference_mode()
    def describe(self, images: list[Image.Image]) -> list[dict]:
        """One caption and one tag list per image, in the order given.

        Batched, because the worker sends three frames of a video as one request
        and a batch of three costs barely more than one on this card. Left
        padding, because generation continues from the end of the sequence and
        right padding would have the model continuing from padding.
        """
        messages = [
            [{"role": "user", "content": [{"type": "image"}, {"type": "text", "text": PROMPT}]}]
            for _ in images
        ]
        prompts = [
            self._processor.apply_chat_template(m, tokenize=False, add_generation_prompt=True)
            for m in messages
        ]

        inputs = self._processor(
            text=prompts,
            images=[[img] for img in images],
            padding=True,
            padding_side="left",
            return_tensors="pt",
        ).to(self.device)

        generated = self._model.generate(
            **inputs,
            max_new_tokens=MAX_NEW_TOKENS,
            do_sample=False,
        )
        # Only the continuation. The prompt is a few hundred vision tokens and
        # decoding it back would be several kilobytes of placeholder per image.
        trimmed = [out[len(prompt):] for prompt, out in zip(inputs.input_ids, generated)]
        answers = self._processor.batch_decode(
            trimmed, skip_special_tokens=True, clean_up_tokenization_spaces=False
        )
        return [_read(answer) for answer in answers]


# A JSON object anywhere in the answer. A model told "JSON only" complies about
# nineteen times in twenty and wraps it in a markdown fence the twentieth, and
# refusing that answer would throw away a perfectly good caption over three
# backticks.
_OBJECT = re.compile(r"\{.*\}", re.DOTALL)


def _read(answer: str) -> dict:
    """Turn whatever the model said into a caption and some tags.

    Never raises. A malformed answer becomes a caption with no tags rather than
    a failed job: the sentence is the more useful half, a 4xx here would burn an
    attempt against a photograph that is not the problem, and a photograph with
    a caption and no tags is still findable.
    """
    answer = answer.strip()
    caption, tags = "", []

    match = _OBJECT.search(answer)
    if match:
        try:
            parsed = json.loads(match.group(0))
            caption = str(parsed.get("caption") or "").strip()
            raw = parsed.get("tags") or []
            if isinstance(raw, str):
                raw = [part.strip() for part in raw.split(",")]
            tags = [str(tag).strip() for tag in raw if str(tag).strip()]
        except (json.JSONDecodeError, TypeError, AttributeError):
            log.debug("unreadable caption JSON: %s", answer[:200])

    if not caption:
        # The fallback that keeps a truncated or fenced answer useful: strip
        # what looks like markup and treat the whole thing as prose.
        caption = re.sub(r"^```(?:json)?|```$", "", answer).strip()[:500]

    return {
        "caption": caption,
        # The model reports no probability, so this is its own ordering turned
        # into a number: the words it led with come out first. That matters
        # downstream because db.normalizeTags keeps the twelve highest — without
        # a value here it would keep whichever twelve hashed first, which is not
        # a decision anybody made.
        "tags": [
            {"name": tag, "confidence": round(max(0.05, 1.0 - 0.05 * i), 3)}
            for i, tag in enumerate(tags)
        ],
    }
