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
from dataclasses import dataclass

import torch
from PIL import Image
from transformers import AutoModelForImageTextToText, AutoProcessor, FineGrainedFP8Config

log = logging.getLogger("photo_ml.captioner")

@dataclass(frozen=True)
class Spec:
    """One captioner: which weights, how to load them, and what its rows are
    called.

    Two strings on purpose, as with the encoder: the row records what produced a
    caption and will outlive whatever the weights were called on whichever
    mirror they came from, so the identity is ours. `name` is what lands in
    asset_descriptions.model, and PHOTOD_CAPTION_MODEL on the Go side has to
    agree with it — rebuild_asset_search reads captions by that string, so a
    service quietly running something else writes rows that are correct, stored,
    and never reach the tsvector. photod's worker compares the two on every
    result and says so; see worker.checkModel.
    """

    name: str
    hf_id: str
    # Quantise the weights on load, in FP8 with a 128-block scale.
    #
    # This is a deliberate reversal of what the module docstring above says
    # about 4-bit, and the reasoning survives the reversal intact. The objection
    # was never to quantisation as such: it was that the AWQ and GPTQ *kernels*
    # are community code and sm_120 is the architecture most likely to be
    # missing from them, so the failure mode is a service that installs, starts
    # and then dies on the first forward pass. FP8 is not community code. It is
    # Blackwell's own datatype, transformers dispatches it to DeepGEMM on
    # SM100+, and it falls back to a Triton kernel rather than to nothing.
    #
    # What it buys is the only thing that lets a 9B model onto this card at all:
    # 19.3GB of bf16 becomes about 10, which is the envelope the 4B already
    # occupies. What it costs is a load that reads the full bf16 checkpoint into
    # host memory first — minutes, and once per residency load rather than once
    # per batch.
    fp8: bool = False
    # Modules the FP8 quantiser must leave in bf16, and this list is a bug
    # worked around rather than a tuning choice.
    #
    # transformers' on-the-fly quantiser writes a `weight_scale_inv` beside
    # every block it converts, and for Qwen3.5 it silently fails to write them
    # for the Gated DeltaNet projections and the vision MLPs — the load report
    # says MISSING and initialises them fresh, which means the dequantisation is
    # multiplying by noise. There is no error. What comes out is a model that
    # loads, runs at full speed, reports no NaN in its weights, and answers
    # every one of two hundred photographs with a caption that is two hundred
    # exclamation marks.
    #
    # Excluding those two families leaves the language MLPs — which is where the
    # parameters actually are — quantised, and costs about half a gigabyte
    # against a whole-model conversion that does not work. Worth re-testing
    # empty on a later transformers; the symptom is loud once you know it.
    fp8_skip: tuple[str, ...] = ()


# Every captioner this service knows how to be, keyed by the name its rows carry.
#
# A registry rather than a constant because ML_IMAGES.md §9 step 1 — "bench
# first, on this library" — is the one item of that plan still unstruck, and the
# schema was built for it: `model` is in the primary key of asset_descriptions,
# so two captioners coexist in the tables and a bench is a delete and a requeue
# rather than a migration. Changing what one of them *does* under an unchanged
# name — the MAX_PIXELS below, the prompt — is the case the primary key cannot
# see, and `photobackup ml backfill --force` is that half: it requeues without
# deleting, because the write path already replaces a photograph's words in
# place. This is the other half of that, and it is what makes
# choosing one an env var instead of an edit.
CAPTIONERS: dict[str, Spec] = {
    spec.name: spec
    for spec in (
        # The incumbent. Qwen3-VL's 4B at bf16, about 9GB on the card.
        Spec("qwen3-vl-4b-instruct", "Qwen/Qwen3-VL-4B-Instruct"),
        # The same size, one generation on. Qwen3.5 is natively multimodal —
        # there is no separate -VL line, because text and image tokens were
        # interleaved from scratch rather than a vision tower being adapted onto
        # a language model — and its own documentation claims it beats Qwen3-VL
        # on visual understanding. Same VRAM as the incumbent, so the swap is
        # free if the claim holds here.
        Spec("qwen3.5-4b", "Qwen/Qwen3.5-4B"),
        # Twice the parameters, in the same VRAM, at the cost of a slower load
        # and a Triton fallback if DeepGEMM has no sm_120 kernel. See Spec.fp8.
        Spec("qwen3.5-9b-fp8", "Qwen/Qwen3.5-9B", fp8=True,
             fp8_skip=("linear_attn", "visual")),
    )
}

# What runs when nobody has said otherwise, and it is the incumbent rather than
# the newest: the archive already holds rows under this name, and a default that
# moves with the registry would silently orphan them.
DEFAULT_CAPTIONER = "qwen3-vl-4b-instruct"


def spec_for(name: str | None) -> Spec:
    """The captioner a name asks for, or the default.

    Falls back rather than raising, which is settings.py's rule for every value
    that comes out of an env file: a typo should cost you the default, not the
    service. What it costs is visible — /health reports the name that is
    actually loaded, and photod logs a mismatch on the first result it reads.
    """
    if name and name in CAPTIONERS:
        return CAPTIONERS[name]
    if name:
        log.warning("unknown captioner %r; falling back to %s", name, DEFAULT_CAPTIONER)
    return CAPTIONERS[DEFAULT_CAPTIONER]

# What bounds the vision tokens: the processor scales any photograph down to
# this many pixels, so this number and not derivstore.MLEdge is what the
# captioner actually looks at. Roughly max_pixels/1024 tokens — 235 at the old
# 512*512, 907 here, both measured over this archive's own renditions.
#
# This was 512*512 and the sentence that used to be here said the larger
# renditions cost this pass nothing, which was true and was the wrong thing to
# be measuring. 96% of renditions carry more pixels than that budget, so what it
# bought was a downscale on nearly every photograph in the library. Benched
# across 512-1536 on 33 renditions, what the extra pixels are worth:
#
#   - Vague nouns per caption fall 0.55 -> 0.45, and only from here up: at 640
#     and 768 the number is flat or slightly worse, so the settings between the
#     two are churn without a gain. "holding an object" becomes "holding a
#     phone"; a bowl of onions at the edge of the frame gets seen at all.
#   - Two confabulations went away. A browser home screen read as "a digital
#     clock" at 512, and a screenshot of ASCII art as "a photo library indexing
#     interface" — a caption that is both searchable and wrong, which is worse
#     than a vague one and is the argument that decided this.
#
# It is not free and the cost is not only time. 907 tokens against 235 is the
# whole of it: the describe pass roughly doubles, and the forward pass no longer
# fits at eight images, which is why settings.describe_batch is 4. Junk tags
# also rise from 10% to 14% by the triage pass's own reckoning, because a model
# that can suddenly read a credit card starts writing "valid thru" — the tags
# are rotated rather than strictly improved, and it is the caption that got
# better.
#
# 1024 rather than more because the next step costs a second batch halving for
# no measured gain: 1280 is 10.2 hours over the library against 7.9, and past it
# the curve flattens anyway — a portrait rendition is 1152x1536, so it reaches
# its own native size before the budget binds.
MAX_PIXELS = 1024 * 1024
MIN_PIXELS = 224 * 224

# Long enough for a sentence and a dozen words, short enough that a model that
# has decided to write an essay is cut off rather than allowed to set the pace
# of the whole backfill. A truncated answer loses its closing brace and falls
# back to being read as prose; see _read.
MAX_NEW_TOKENS = 192

# The four prohibitions in the rules below were not in the first version of this
# prompt, and they are not guesses. They are TRIAGE_SYSTEM's own categories,
# moved forward from the second thing these weights are asked to the first,
# after 2,572 photographs showed what asking loosely produces.
#
# The mood clause is the clearest of them. This prompt used to ask for "the
# occasion or mood when it is obvious", and TRIAGE_SYSTEM ninety lines below
# names "casual" and "friendly" as junk by way of example. The model obeyed both
# — 94 casual, 46 peaceful, 26 calm, 25 relaxed — writing words it would later
# strike out itself. The occasion half of that clause was the good half (concert
# 50, party 50, graduation 43), so it is split rather than dropped.
#
# The phrase rule is the other measured one. 1,376 of the first 3,505 words were
# two-word phrases; they carried a fifth of the claims, two thirds of them were
# used exactly once, and 71% had every one of their words already in that
# photograph's own caption, at the same weight A, from the same model, about the
# same picture. They grow a tail the merge review has to read and buy the
# tsvector almost nothing. db.normalizeTags enforces what this asks for rather
# than trusting it — see maxPhrases there, and see ML_IMAGES.md §9 for why the
# vocabulary is still open.
PROMPT = """You are indexing a personal photo library so that its owner can search it later.

Look at the image and reply with JSON only, in exactly this shape:
{"caption": "one sentence", "tags": ["word", "word", "word"]}

caption: one plain sentence, at most 25 words, describing what is visible.
tags: 4 to 10 lowercase tags. Cover the subject, the setting, notable objects,
the activity, and the occasion when there is one — a birthday, a concert, a
graduation, a holiday.

Prefer single words. Use a two-word phrase only when the two words name one
thing that has no single-word name: "ski resort", "power lines", "christmas
tree". Not "white tank top", not "high vantage point" — say "shirt", say
"overlook".

Rules:
- Describe only what you can actually see.
- Never guess anyone's name, and never guess a city, country or landmark name.
  Say "a man", "a dog", "a beach", "a ski resort".
- If the image is a screenshot or a photograph of text, say so and describe what
  kind of thing it shows — but do not copy the words off the screen.
  "screenshot", "chat", "receipt", "map"; never "login", "submit", "result".
- No mood, no opinion, no judgement. Not "casual", "peaceful", "professional",
  "modern". A tag is a thing that is there.
- No words about photography itself. Not "photo", "image", "screen",
  "high quality", "well composed".
- No preamble, no explanation, no markdown fence. JSON only."""


# The second thing these weights are asked, and it is asked of the words they
# themselves wrote — ML_IMAGES.md §9's cleanup, one stage before the merge.
#
# A free-form vocabulary is about a third rubbish, and the rubbish has a shape:
# interface text lifted off screenshots ("login", "result", "true", "details"),
# vague judgements about mood ("casual", "friendly", "collaborative"), and words
# about photography rather than about the photograph ("photograph", "image",
# "high quality"). None of them merges into anything, none of them will ever be
# typed into a search box, and all of them sit in the weight-A half of every
# tsvector they are attached to.
#
# The same model rather than a new one, and that is the whole reason this lives
# in captioner.py. It is already registered, already on demand, already the nine
# gigabytes the residency table budgets for; a second 4B checkpoint for a
# judgement this size would be a second download, a second entry, and a second
# way for the card to run out of room. Asking the captioner to mark its own
# homework is also the honest framing: these are its words.
TRIAGE_SYSTEM = """You are cleaning up the tag vocabulary of a personal photo archive. A vision model looked at each photograph and wrote whatever words it liked, so the list holds both useful words and rubbish.

KEEP a word that names something a person could see in a photograph and would search for: an object, an animal, a place, a scene, an activity, clothing, weather, time of day, a colour, or a kind of picture such as a selfie or a screenshot.

JUNK a word that would never help anyone find a photograph: interface labels read off a screen (login, result, submit, true, user name), vague judgements (nice, casual, interesting, friendly), words about photography itself (photograph, image, high quality, well composed), and anything meaningless on its own.

Answer with one word: KEEP or JUNK."""

# How many words go through one forward pass.
#
# Small, and not for throughput. This is prefill only — see judge() — so the
# batch buys little, while the activation memory it costs comes out of a budget
# that already has 9GB of these weights in it and has to leave room for NVENC.
# Eight words is about 40ms.
TRIAGE_BATCH = 8


class Captioner:
    """Loaded weights, and the two things they are asked."""

    def __init__(
        self,
        device: torch.device,
        dtype: torch.dtype,
        cache_dir: str | None,
        spec: Spec | None = None,
    ) -> None:
        self.spec = spec or CAPTIONERS[DEFAULT_CAPTIONER]
        # The instance carries the name rather than the class, because there is
        # more than one of these now and a class attribute would report whichever
        # checkpoint was written down first. photod reads this off every result.
        self.name = self.spec.name
        self.device = device
        self.dtype = dtype

        load: dict = {"dtype": dtype, "cache_dir": cache_dir}
        if self.spec.fp8:
            # device_map, and it is not optional here: quantising on load places
            # the weights as it converts them, and a .to(device) afterwards would
            # move already-quantised blocks a second time. The block size is the
            # 128 the Blackwell path expects.
            load |= {
                "quantization_config": FineGrainedFP8Config(
                    weight_block_size=(128, 128),
                    modules_to_not_convert=list(self.spec.fp8_skip) or None,
                ),
                "device_map": device,
            }

        model = AutoModelForImageTextToText.from_pretrained(self.spec.hf_id, **load)
        self._model = model.eval() if self.spec.fp8 else model.to(device).eval()
        self._processor = AutoProcessor.from_pretrained(
            self.spec.hf_id, cache_dir=cache_dir, min_pixels=MIN_PIXELS, max_pixels=MAX_PIXELS
        )
        self._template = _template_kwargs(self._processor)

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
            self._processor.apply_chat_template(
                m, tokenize=False, add_generation_prompt=True, **self._template
            )
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

    @torch.inference_mode()
    def judge(self, words: list[str]) -> list[dict]:
        """Junk or not, for each word, as a probability rather than a sentence.

        Not generated. The obvious implementation asks for a JSON list of the
        junk words and it does not work: measured against this archive's own
        vocabulary, a 0.6B invents words that were never in the list and repeats
        one of them four hundred times, and even a well-behaved model has to be
        matched back to its input by string comparison — for a task whose whole
        output is one bit per word.

        So there is no generation at all. One forward pass over "is this word
        useful", and the answer is read off the logits of the two tokens the
        model was told to choose between. That is a classifier: it cannot
        hallucinate a word, cannot skip one, cannot reorder them, and costs
        prefill only — a couple of minutes over three thousand words rather than
        the twenty a generated answer would take.

        What comes back is P(junk), which is worth more than the bit it
        replaces. The review screen reads the list in that order, because a
        confident wrong verdict is the one worth catching and an uncertain one
        is usually a word that genuinely could go either way.
        """
        keep, junk = self._verdict_tokens()
        out: list[dict] = []
        for start in range(0, len(words), TRIAGE_BATCH):
            chunk = words[start:start + TRIAGE_BATCH]
            prompts = [
                self._processor.apply_chat_template(
                    [
                        {"role": "system", "content": [{"type": "text", "text": TRIAGE_SYSTEM}]},
                        {"role": "user", "content": [{"type": "text", "text": f'Word: "{word}"'}]},
                    ],
                    tokenize=False,
                    add_generation_prompt=True,
                    # Doubly required here. judge() reads its answer off the
                    # logits of the *first* generated position, and a reasoning
                    # template makes that position the opening of a thought
                    # rather than KEEP or JUNK — so the verdict would be a
                    # coin toss between two tokens the model was never about to
                    # write.
                    **self._template,
                )
                for word in chunk
            ]
            # Left padding, for the reason describe() uses it: the answer is
            # read at the last position, and right padding would read it off a
            # pad token.
            inputs = self._processor(
                text=prompts, padding=True, padding_side="left", return_tensors="pt"
            ).to(self.device)

            # logits_to_keep=1 is not a micro-optimisation. Without it the
            # head is run over every position of every prompt, which for a
            # 150k vocabulary at bf16 is half a gigabyte of logits per batch —
            # allocated on a card that is already holding nine gigabytes of
            # these weights, and thrown away undecoded except for one row.
            logits = self._model(**inputs, logits_to_keep=1).logits[:, -1, :]
            scores = torch.softmax(logits[:, [keep, junk]].float(), dim=-1)[:, 1]
            for word, score in zip(chunk, scores.tolist()):
                out.append({"word": word, "junk": score >= 0.5, "score": round(score, 4)})
        return out

    def _verdict_tokens(self) -> tuple[int, int]:
        """The first token of each answer, which is all that is looked at.

        Asserted rather than assumed: a tokenizer that gave both words the same
        first token would make every score exactly 0.5, which reads as a model
        with no opinion instead of as a bug in this line.
        """
        tokenizer = self._processor.tokenizer
        keep = tokenizer.encode("KEEP", add_special_tokens=False)[0]
        junk = tokenizer.encode("JUNK", add_special_tokens=False)[0]
        if keep == junk:
            raise RuntimeError("KEEP and JUNK share a first token in this tokenizer")
        return keep, junk


# A JSON object anywhere in the answer. A model told "JSON only" complies about
# nineteen times in twenty and wraps it in a markdown fence the twentieth, and
# refusing that answer would throw away a perfectly good caption over three
# backticks.
_OBJECT = re.compile(r"\{.*\}", re.DOTALL)

def _template_kwargs(processor) -> dict:
    """Turn a reasoning model's thinking off, where it has any to turn off.

    Found the expensive way, and it is the reason a bench measures output rather
    than trusting a model card. Qwen3.5 is a reasoning model whose chat template
    defaults `enable_thinking` to true, so the answer begins "The user wants me
    to index a photo..." and works towards the JSON — and MAX_NEW_TOKENS cuts it
    off two hundred words before it arrives. Measured over 200 photographs: 200
    captions of unusable prose and not one tag, which _read cannot distinguish
    from a model that is simply bad at this. The incumbent has no such notion in
    its template, and comparing the two without this would have been comparing
    a default rather than a model.

    Detected rather than configured per spec, because the thing being asked is a
    property of the checkpoint's own template and a Spec field would be a second
    place for it to be wrong. Absent the toggle the dict is empty, which is
    exactly what every non-reasoning checkpoint wants.
    """
    template = getattr(processor, "chat_template", None) or getattr(
        getattr(processor, "tokenizer", None), "chat_template", ""
    )
    return {"enable_thinking": False} if "enable_thinking" in (template or "") else {}


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
