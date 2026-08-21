"""Run one captioner over a fixed set of photographs and write down what it said.

ML_IMAGES.md §9 step 1 is the only item of that plan still unstruck — "bench
first, on this library ... half a day, and it turns 'best quality' into a
measurement" — and this is the half of it that concerns the captioner.

Deliberately not an HTTP client. photo-ml holds four models and unloads them on
a timer, which is exactly right for a service answering a backfill and exactly
wrong for a measurement: a bench that goes through the socket measures the
residency, the micro-batcher and the collector window as well as the weights,
and reports the sum as though it were the model. So this imports Captioner and
calls describe() directly, which is also what lets it report peak VRAM for the
one model under test rather than for whatever else the service was holding.

What it does share with the service is everything that decides output: the same
Captioner class, the same PROMPT, the same _read, the same batching arithmetic.
A bench that measured a different prompt would be measuring the prompt.

    uv run python bench/caption_bench.py \\
        --model qwen3.5-4b --images bench_images.txt --out gen2-qwen35-4b.jsonl

The images file is one `path|year|sha` per line, and the year is carried through
untouched so the comparison can be read by era — which matters here, because 68%
of this archive predates anything the incumbent has ever been asked about.
"""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path

import torch
from PIL import Image

from photo_ml import captioner as cap
from photo_ml.encoder import resolve_device


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--model", required=True, choices=sorted(cap.CAPTIONERS))
    ap.add_argument("--images", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    # The service's own default, so the numbers below are the numbers a real
    # pass would see. Worth lowering for a model whose weights leave less room.
    ap.add_argument("--batch", type=int, default=8)
    ap.add_argument("--cache-dir", default=None)
    args = ap.parse_args()

    rows = [line.strip().split("|") for line in args.images.read_text().splitlines() if line.strip()]
    spec = cap.spec_for(args.model)
    device, dtype = resolve_device("auto")

    torch.cuda.reset_peak_memory_stats() if device.type == "cuda" else None
    load_started = time.perf_counter()
    model = cap.Captioner(device, dtype, args.cache_dir, spec)
    load_s = time.perf_counter() - load_started
    weights_gb = torch.cuda.max_memory_allocated() / 1e9 if device.type == "cuda" else 0.0
    print(f"{spec.name}: loaded in {load_s:.0f}s, {weights_gb:.1f}GB of weights", flush=True)

    results, total_s = [], 0.0
    for start in range(0, len(rows), args.batch):
        chunk = rows[start : start + args.batch]
        images = [Image.open(path).convert("RGB") for path, _, _ in chunk]

        # Synchronised on both sides, because CUDA calls are queued and a timer
        # around an unsynchronised launch measures the queueing.
        if device.type == "cuda":
            torch.cuda.synchronize()
        began = time.perf_counter()
        answers = model.describe(images)
        if device.type == "cuda":
            torch.cuda.synchronize()
        took = time.perf_counter() - began
        total_s += took

        for (path, year, sha), answer in zip(chunk, answers):
            results.append(
                {
                    "sha": sha,
                    "year": int(year),
                    "path": path,
                    "caption": answer["caption"],
                    "tags": [t["name"] for t in answer["tags"]],
                    "ms": round(1000 * took / len(chunk), 1),
                }
            )
        done = start + len(chunk)
        print(f"  {done}/{len(rows)}  {1000 * took / len(chunk):.0f}ms/image", flush=True)

    peak_gb = torch.cuda.max_memory_allocated() / 1e9 if device.type == "cuda" else 0.0
    summary = {
        "model": spec.name,
        "hf_id": spec.hf_id,
        "fp8": spec.fp8,
        "batch": args.batch,
        "images": len(results),
        "load_s": round(load_s, 1),
        "weights_gb": round(weights_gb, 2),
        "peak_gb": round(peak_gb, 2),
        "total_s": round(total_s, 1),
        "per_image_s": round(total_s / max(1, len(results)), 3),
    }
    # Summary first, so `head -1` on the file is the measurement and the rest is
    # the evidence for it.
    with args.out.open("w") as f:
        f.write(json.dumps(summary) + "\n")
        for row in results:
            f.write(json.dumps(row) + "\n")

    print(json.dumps(summary, indent=2), flush=True)
    # The number ML_IMAGES.md §10's table is in: this pass, extrapolated over
    # every asset the captioner has to see.
    print(f"→ {summary['per_image_s'] * 23400 / 3600:.1f}h over the library", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
