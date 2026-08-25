"""What the photograph says.

A dedicated text recogniser rather than the captioner reading the words out,
which is ML_IMAGES.md §6's split and worth keeping for two reasons. A VLM asked
what a blurry sign says will tell you something plausible; a recogniser returns
a confidence and can be thresholded. And the whole pass is one model that loads
in a second, against nine gigabytes of captioner that does not.

It is pointed at the same frames the captioner gets, videos included, and that
is deliberate rather than incidental: a Snapchat memory carries its caption
burned into the picture, and mlprep burns the overlay in on purpose. Reading
somebody's handwriting back out of a five-year-old memory is the single most
satisfying thing in this file.

Two things about this pass were measured and both came out against what the
first version of it assumed.

**The rendition was the limit, not the model.** At derivstore.MLEdge's old 512px
the detector was being handed a picture in which a line of receipt text is five
pixels tall, and rapidocr's own preprocessing upsamples that back to a 736px
short side before it looks — interpolation, adding no information. Scored
against known text on a 4032x3024 frame, at rendition edges of 512 and 1536:

    text height   512    1536      what that size is
    2.1% frame   1.00    1.00      a road sign, a shopfront, a headline
    1.2% frame   0.42    1.00      a receipt held at arm's length
    0.9% frame   0.00    1.00      body text in a screenshot, a menu

The 512px column is what the archive was read at, and the rows that read 0.00
are the photographs where recognised text is the only thing that could ever
have made them findable — no caption will ever contain a confirmation number.
MLEdge is 1536 now, and derivstore says why it costs the other two passes
nothing.

**Bigger is not better past a point, and the card is free.** PP-OCRv5 server
scored *worse* than v6 small and ran three times as slow. v6 medium is the one
that wins, and on the GPU through rapidocr's torch backend it reads the same
text four times faster than v6 small manages on the CPU: 0.12s an image against
0.49s, which is the difference between a 0.8 hour pass and a 3.2 hour one.

That the card is free during this pass is not luck, it is jobs.ClaimInOrder:
every ocr job is drained before the first describe job is claimed, precisely so
the captioner and the recogniser do not thrash the card past each other. So the
recogniser gets a card holding nothing but the encoder and the parser, and its
127MB of weights and 1.2GB of transient activation fit with room to spare. For
the stray upload that interleaves the two passes anyway, residency's eviction is
the backstop.

The torch backend rather than onnxruntime-gpu, and that is the Blackwell lesson
from §6 applied a second time: this card is sm_120, quantisation and inference
kernels are the part of that ecosystem most likely not to have been built for an
architecture this new, and torch cu128 is already here and already proven by the
captioner. Nothing new is installed to put this on the GPU.
"""

from __future__ import annotations

import logging
import threading

import numpy as np
from PIL import Image

log = logging.getLogger("photo_ml.ocr")

# Which PP-OCR weights, by the name its rows carry, and the counterpart of
# photod's PHOTOD_OCR_MODEL. Unlike the old name — "rapidocr", deliberately the
# family — this names the generation, because the generation is now a thing
# somebody chooses and the two choices do not read the same text. ML_IMAGES.md
# §4's rule then does the rest: `model` is on every row, so a swap is a delete
# and a requeue while the old rows sit beside the new ones being compared.
#
# The device is *not* in it. The same weights on the card and on the CPU read
# the same text — measured, identical to the character on every case in the
# table above — so where it ran is an operational fact rather than a property of
# what was written down.
MODEL_TYPES = ("small", "medium", "server")
DEFAULT_MODEL_TYPE = "medium"


def model_name(model_type: str) -> str:
    return f"rapidocr-v6-{model_type}"


# Below this, a line is noise: a compression artefact read as a comma, one
# letter of a logo, the edge of a face read as a bracket. Set here rather than
# in Go because the numbers belong with the model that produced them, and
# because everything that gets through lands in a tsvector at weight B where a
# wrong word is a wrong search result.
MIN_CONFIDENCE = 0.5

# Two characters of nothing is not text. One-character detections are almost
# always artefacts, and they are the ones that fill a vocabulary with "l" and
# "i".
MIN_LENGTH = 2


class Recognizer:
    """The pipeline, and the one thing it is asked."""

    def __init__(self, device: str = "cpu", model_type: str = DEFAULT_MODEL_TYPE) -> None:
        if model_type not in MODEL_TYPES:
            log.warning("unknown OCR model type %r; using %s", model_type, DEFAULT_MODEL_TYPE)
            model_type = DEFAULT_MODEL_TYPE
        self.name = model_name(model_type)
        self._engine, self.device = _open(device, model_type)
        # One forward pass at a time, whatever VISION_CONCURRENCY is.
        #
        # The same finding ML_IMAGES.md §10 records for the captioner, and for
        # the same reason: four concurrent calls serialise on the same kernels
        # and take four times as long as one, so nothing is bought by letting
        # them overlap. What is *spent* by letting them overlap is four copies
        # of the 1.2GB a detection allocates on a 1536px frame, on a card that
        # has to keep leaving room for NVENC. On the CPU the argument is the
        # same one in different units — onnxruntime already has all 32 cores.
        self._lock = threading.Lock()

    def unload(self) -> None:
        self._engine = None

    def read(self, images: list[Image.Image]) -> list[dict]:
        """One block of text per image, in the order given."""
        with self._lock:
            return [self._one(image) for image in images]

    def _one(self, image: Image.Image) -> dict:
        # rapidocr wants an array and does its own colour handling; PIL has
        # already given us RGB.
        try:
            result = self._engine(np.asarray(image))
        except Exception as exc:
            # A recogniser that cannot read one photograph has not failed as a
            # service. Returning nothing stores "there is no text here", which
            # is wrong for this one image and costs a great deal less than
            # parking the job — the caption still describes it and the file is
            # archived either way.
            log.warning("could not read text from an image: %s", exc)
            return {"text": "", "lines": []}

        lines = []
        for text, score, box in _rows(result):
            text = " ".join(text.split())
            if len(text) < MIN_LENGTH or score < MIN_CONFIDENCE:
                continue
            lines.append({"text": text, "confidence": round(float(score), 3), "box": box})

        return {"text": "\n".join(line["text"] for line in lines), "lines": lines}


def _open(device: str, model_type: str):
    """Build the engine, preferring the card and falling back loudly.

    Falling back rather than refusing, which is the rule settings.py applies to
    every value that comes out of an env file and the rule §6 arrived at the
    hard way: a service that installs, starts, and then reports "no kernel image
    is available" on the first forward pass is the 1am debugging session this
    whole design spends paragraphs avoiding. A recogniser that quietly runs four
    times slower on the CPU costs an overnight pass two hours and costs the
    archive nothing.

    The two-package dance the previous version did — rapidocr 3.x against the
    1.x `rapidocr_onnxruntime` — is gone, because choosing weights at all needs
    3.x's params API. pyproject already pins `rapidocr>=3`; what is kept is the
    shape of the tolerance, one rung further down: an engine built with no
    options at all still reads text.
    """
    try:
        from rapidocr import EngineType, ModelType, RapidOCR
    except ImportError as exc:  # pragma: no cover — an install that predates the pin
        log.error("rapidocr 3.x is required to choose OCR weights (%s); using whatever is installed", exc)
        from rapidocr import RapidOCR  # type: ignore

        return RapidOCR(), "cpu"

    weights = {
        "Det.model_type": ModelType(model_type),
        "Rec.model_type": ModelType(model_type),
    }

    if device.startswith("cuda"):
        try:
            engine = RapidOCR(params=weights | {
                "Det.engine_type": EngineType.TORCH,
                "Rec.engine_type": EngineType.TORCH,
                # The orientation classifier has no medium weights and needs
                # none — it answers one binary question about a cropped line —
                # but it has to be on the same engine as the rest so that one
                # process is not holding both runtimes.
                "Cls.engine_type": EngineType.TORCH,
                "EngineConfig.torch.use_cuda": True,
                "EngineConfig.torch.gpu_id": 0,
            })
            log.info("text recogniser on the GPU: PP-OCRv6 %s, torch", model_type)
            return engine, "cuda"
        except Exception as exc:
            log.warning("could not put the text recogniser on the card (%s); falling back to the CPU", exc)

    engine = RapidOCR(params=weights)
    log.info("text recogniser on the CPU: PP-OCRv6 %s, onnxruntime", model_type)
    return engine, "cpu"


def _rows(result) -> list[tuple[str, float, list[float]]]:
    """Flatten whichever shape the engine answered in.

    3.x returns an object with parallel `txts`, `scores` and `boxes`; 1.x
    returned `(rows, elapsed)` where each row is `[box, text, score]`. Both mean
    the same thing, and the second is still accepted for the same reason _open
    still tolerates an engine it could not configure.
    """
    if result is None:
        return []

    txts = getattr(result, "txts", None)
    if txts is not None:
        scores = getattr(result, "scores", None) or [1.0] * len(txts)
        boxes = getattr(result, "boxes", None)
        out = []
        for i, text in enumerate(txts):
            box = _bounds(boxes[i]) if boxes is not None and i < len(boxes) else [0.0, 0.0, 0.0, 0.0]
            out.append((str(text), float(scores[i]), box))
        return out

    rows = result[0] if isinstance(result, tuple) else result
    if not rows:
        return []
    return [(str(row[1]), float(row[2]), _bounds(row[0])) for row in rows]


def _bounds(polygon) -> list[float]:
    """The four corners rapidocr detects, as one rectangle.

    A quadrilateral is what the detector actually produces — text on a photograph
    is rarely axis-aligned — but nothing downstream wants to draw a rotated box,
    and asset_ocr stores no geometry at all. The rectangle is here so that
    somebody debugging a bad read with curl can see roughly where on the
    photograph it came from.
    """
    try:
        points = np.asarray(polygon, dtype=float).reshape(-1, 2)
        return [
            float(points[:, 0].min()), float(points[:, 1].min()),
            float(points[:, 0].max()), float(points[:, 1].max()),
        ]
    except Exception:
        return [0.0, 0.0, 0.0, 0.0]
