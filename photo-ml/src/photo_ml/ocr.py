"""What the photograph says.

A dedicated text recogniser rather than the captioner reading the words out,
which is ML_IMAGES.md §6's split and worth keeping for two reasons. A VLM asked
what a blurry sign says will tell you something plausible; a recogniser returns
a confidence and can be thresholded. And this one runs on ONNX Runtime on the
CPU, so it is not competing with the captioner for the card at all — the whole
OCR pass over the library costs no VRAM and finishes while the captioner is
still on its first thousand photographs.

It is pointed at the same frames the captioner gets, videos included, and that
is deliberate rather than incidental: a Snapchat memory carries its caption
burned into the picture, and mlprep burns the overlay in on purpose. Reading
somebody's handwriting back out of a five-year-old memory is the single most
satisfying thing in this file.
"""

from __future__ import annotations

import logging

import numpy as np
from PIL import Image

log = logging.getLogger("photo_ml.ocr")

# The name that ends up in asset_ocr.model, and db.OCRModel on the Go side.
# Deliberately the family rather than the exact PP-OCR weights: rapidocr picks
# its own detection and recognition checkpoints per release, and pinning the
# name to the thing this archive can actually reason about — "the text
# recogniser" — is more useful than a version string that changes on `uv sync`.
MODEL_NAME = "rapidocr"

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
    """The ONNX pipeline, and the one thing it is asked."""

    name = MODEL_NAME

    def __init__(self) -> None:
        self._engine = _open()

    def unload(self) -> None:
        self._engine = None

    def read(self, images: list[Image.Image]) -> list[dict]:
        """One block of text per image, in the order given."""
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


def _open():
    """Build the engine, across two incompatible releases of the package.

    rapidocr 3.x is `rapidocr.RapidOCR`; 1.x was `rapidocr_onnxruntime.RapidOCR`.
    Accepting both rather than pinning one, for the reason encoder._features
    accepts two shapes of the same tensor: the alternative is a service that
    stops starting the next time somebody runs `uv sync`, and the thing being
    papered over is an import line.
    """
    try:
        from rapidocr import RapidOCR  # type: ignore
    except ImportError:  # pragma: no cover — the older package
        from rapidocr_onnxruntime import RapidOCR  # type: ignore
    return RapidOCR()


def _rows(result) -> list[tuple[str, float, list[float]]]:
    """Flatten whichever shape the engine answered in.

    3.x returns an object with parallel `txts`, `scores` and `boxes`; 1.x
    returned `(rows, elapsed)` where each row is `[box, text, score]`. Both mean
    the same thing and neither is worth a version pin.
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
