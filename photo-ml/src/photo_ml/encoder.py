"""The vision encoder: a photograph, or a phrase, as 1152 numbers.

One model with two towers, which is the property the whole search feature rests
on. An image and the sentence "at the beach" go into the same space, so finding
a beach nobody wrote the word "beach" about is a dot product rather than a
keyword match.

SigLIP-2 so400m at patch14-384 because ML_IMAGES.md §4 already committed to
halfvec(1152), and 1152 is this model's width. The bench in §9 step 1 is still
worth running, and the schema is built so that running it later is a
`delete where model = ...` plus a requeue rather than a migration — `model` is
part of the embeddings' primary key for exactly that reason.
"""

from __future__ import annotations

import logging

import numpy as np
import torch
from PIL import Image
from transformers import AutoModel, AutoProcessor

log = logging.getLogger("photo_ml.encoder")

# The checkpoint, and the name that ends up in asset_embeddings.model.
#
# The two are deliberately not the same string. The row records what produced
# the vector, and it is going to outlive whatever the weights were called on
# whichever mirror they came from — so the identity is ours, and the Go side has
# it as a constant beside the HNSW index that names it in a predicate.
HF_ID = "google/siglip2-so400m-patch14-384"
MODEL_NAME = "siglip2-so400m-patch14-384"
DIM = 1152

# SigLIP's text tower was trained on sequences padded to a fixed 64 tokens, and
# it is sensitive to it in a way most encoders are not: pad to the batch's
# longest instead and the vectors come back subtly, unimprovably wrong — no
# error, no warning, just a search that returns almost-plausible rubbish. This
# is the single most expensive line in the file to get wrong.
TEXT_LENGTH = 64


class VisionEncoder:
    """Loaded weights plus the two things they can be asked."""

    name = MODEL_NAME
    dim = DIM

    def __init__(self, device: torch.device, dtype: torch.dtype, cache_dir: str | None) -> None:
        self.device = device
        self.dtype = dtype
        self._model = AutoModel.from_pretrained(
            HF_ID, dtype=dtype, cache_dir=cache_dir
        ).to(device).eval()
        self._processor = AutoProcessor.from_pretrained(HF_ID, cache_dir=cache_dir)

    def unload(self) -> None:
        """Drop the weights. Residency calls empty_cache() after this."""
        self._model = None
        self._processor = None

    @torch.inference_mode()
    def embed_images(self, images: list[Image.Image]) -> np.ndarray:
        inputs = self._processor(images=images, return_tensors="pt").to(self.device)
        return _normalize(_features(self._model.get_image_features(**inputs)))

    @torch.inference_mode()
    def embed_texts(self, texts: list[str]) -> np.ndarray:
        inputs = self._processor(
            text=texts,
            padding="max_length",
            max_length=TEXT_LENGTH,
            truncation=True,
            return_tensors="pt",
        ).to(self.device)
        return _normalize(_features(self._model.get_text_features(**inputs)))


def _features(out) -> torch.Tensor:
    """Unwrap whatever get_*_features handed back.

    transformers changed this under us between 4.x and 5.x: the getters used to
    return the tensor and now return a BaseModelOutputWithPooling around it. Both
    shapes are accepted rather than the version being pinned, because the
    alternative is a service that stops loading its own weights the next time
    somebody runs `uv sync` — and the thing being unwrapped is one attribute.
    """
    if isinstance(out, torch.Tensor):
        return out
    pooled = getattr(out, "pooler_output", None)
    if pooled is None:
        raise TypeError(f"no pooled features in a {type(out).__name__}")
    return pooled


def _normalize(features: torch.Tensor) -> np.ndarray:
    """Unit length, always, and float32 on the way out.

    Normalising here rather than in SQL is what makes `<=>` in Postgres mean
    what it says. Cosine distance between unit vectors is 1 - dot, so pgvector's
    HNSW index over halfvec_cosine_ops ranks correctly and cheaply, and a query
    can compare two similarities from two different frames without either of
    them having been scaled by how bright the photograph was.

    float32 rather than the fp16 the forward pass ran in, because JSON is going
    to carry these and a float16 rounded through a decimal string is worse than
    either. Postgres narrows them back to halfvec on insert, which is the one
    place the precision is actually wanted.
    """
    features = features / features.norm(p=2, dim=-1, keepdim=True)
    return features.to(torch.float32).cpu().numpy()


def resolve_device(requested: str) -> tuple[torch.device, torch.dtype]:
    """Pick the device, and say so out loud when it is not the one wanted.

    A CPU fallback is roughly fifty times slower and completely correct, which
    is the right way round: a driver being upgraded should make search slow, not
    make the archive refuse to run. The log line is there because "slow" is
    otherwise indistinguishable from "broken".
    """
    if requested == "cpu":
        return torch.device("cpu"), torch.float32
    if torch.cuda.is_available():
        return torch.device(requested if requested != "auto" else "cuda"), torch.float16
    if requested != "auto":
        log.warning("PHOTO_ML_DEVICE=%s but CUDA is not available; falling back to the CPU", requested)
    else:
        log.warning("no CUDA device; embedding on the CPU, which is roughly fifty times slower")
    return torch.device("cpu"), torch.float32
