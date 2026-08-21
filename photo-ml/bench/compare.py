"""Compare what two or more captioners said about the same photographs.

The axes are this archive's, not a leaderboard's. MMMU and OCRBench say nothing
about whether a vocabulary is cheap to clean, and the cleanup is what the last
two migrations were about — so what is counted here is what migration 0019 and
ML_IMAGES.md §9 spend a person's evening on:

  vocabulary     distinct words per hundred photographs. The Heaps curve
                 measured over the first generation extrapolated to 8-12k words
                 at full coverage; a model that says the same thing twice
                 instead of two things once is worth hours of review.
  hapax          words used exactly once in the sample. 55% of generation one,
                 and the whole of the tail the merge UI has to be paged through.
  phrases        two-word tags, against the cap db.normalizeTags now enforces.
                 Counted before the cap is applied, because what is being
                 measured is the model's inclination, not the bound.
  junk shapes    the three categories TRIAGE_SYSTEM names, as literal word
                 lists. Crude on purpose — a substring match over a few dozen
                 known offenders, which is enough to rank models and not enough
                 to replace the triage.
  caption words  the sentence is weight A and does more of the searching than
                 the tags do; a model that writes three words is not cheaper,
                 it is worse.

None of this is quality. Quality is read by a person off the two files this
prints paths to, and this only says where to look.
"""

from __future__ import annotations

import argparse
import json
from collections import Counter
from pathlib import Path

# The three shapes ML_IMAGES.md §9 measured on the real vocabulary. Not
# exhaustive — a sample of the offenders, used as a ratio between models rather
# than as an absolute rate.
INTERFACE = {
    "login", "submit", "result", "results", "true", "false", "details", "post",
    "user name", "username", "password", "settings", "menu", "button", "click",
    "search", "home", "back", "next", "cancel", "ok", "error", "loading", "page",
}
MOOD = {
    "casual", "friendly", "collaborative", "peaceful", "calm", "relaxed",
    "serene", "nostalgic", "joyful", "happy", "fun", "moody", "vibrant",
    "professional", "modern", "interesting", "nice", "beautiful", "cozy",
    "candid", "celebratory", "energetic", "cheerful", "warm", "inviting",
}
PHOTOGRAPHY = {
    "photo", "photograph", "photography", "image", "picture", "screen",
    "high quality", "well composed", "close-up", "closeup", "shot", "snapshot",
    "portrait mode", "camera", "lens", "composition", "framing",
}


def load(path: Path) -> tuple[dict, list[dict]]:
    lines = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
    return lines[0], lines[1:]


def report(path: Path) -> dict:
    summary, rows = load(path)
    tags = [t for row in rows for t in row["tags"]]
    counts = Counter(tags)
    captions = [row["caption"] for row in rows]

    return {
        "model": summary["model"],
        "images": len(rows),
        "s/image": summary["per_image_s"],
        "hours over library": round(summary["per_image_s"] * 23400 / 3600, 1),
        "peak GB": summary["peak_gb"],
        "load s": summary["load_s"],
        "tags/photo": round(len(tags) / max(1, len(rows)), 2),
        "distinct": len(counts),
        "distinct/100": round(100 * len(counts) / max(1, len(rows)), 1),
        "hapax %": round(100 * sum(1 for c in counts.values() if c == 1) / max(1, len(counts))),
        "phrases %": round(100 * sum(1 for t in tags if " " in t) / max(1, len(tags))),
        "interface %": round(100 * sum(1 for t in tags if t in INTERFACE) / max(1, len(tags)), 1),
        "mood %": round(100 * sum(1 for t in tags if t in MOOD) / max(1, len(tags)), 1),
        "photography %": round(100 * sum(1 for t in tags if t in PHOTOGRAPHY) / max(1, len(tags)), 1),
        "caption words": round(sum(len(c.split()) for c in captions) / max(1, len(captions)), 1),
        "empty captions": sum(1 for c in captions if not c.strip()),
        # A caption that came back as prose rather than JSON lost its tags with
        # it — see _read's fallback. It is the failure mode that does not look
        # like one.
        "no tags": sum(1 for row in rows if not row["tags"]),
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("results", nargs="+", type=Path)
    ap.add_argument("--year-split", type=int, default=2024,
                    help="the era boundary; 68%% of this archive predates 2024 and "
                         "no captioner has ever been asked about it")
    args = ap.parse_args()

    reports = [report(p) for p in args.results]
    keys = list(reports[0])
    width = max(len(k) for k in keys)
    print(f"{'':{width}}  " + "  ".join(f"{r['model']:>22}" for r in reports))
    for key in keys[1:]:
        print(f"{key:{width}}  " + "  ".join(f"{str(r[key]):>22}" for r in reports))

    print("\ndistinct words by era (the half of the archive nothing has captioned):")
    for path in args.results:
        summary, rows = load(path)
        old = {t for r in rows if r["year"] < args.year_split for t in r["tags"]}
        new = {t for r in rows if r["year"] >= args.year_split for t in r["tags"]}
        n_old = sum(1 for r in rows if r["year"] < args.year_split)
        print(f"  {summary['model']:>22}  pre-{args.year_split}: {len(old):4d} words "
              f"over {n_old} photos   {args.year_split}+: {len(new):4d}")

    print("\nwords one model wrote and another never did (top 20 by use):")
    named = [(report(p)["model"], load(p)[1]) for p in args.results]
    for name, rows in named:
        mine = Counter(t for r in rows for t in r["tags"])
        theirs = {t for other, other_rows in named if other != name
                  for r in other_rows for t in r["tags"]}
        only = [(w, c) for w, c in mine.most_common() if w not in theirs]
        print(f"  {name}: " + ", ".join(f"{w}({c})" for w, c in only[:20]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
