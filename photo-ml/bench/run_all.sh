#!/usr/bin/env bash
# Bench every candidate captioner over the same photographs, one at a time.
#
# Sequential and not negotiable about it: each of these wants most of a 16GB
# card, so two at once is an OOM rather than a faster bench. The wait at the top
# is for photo-ml to let go of the encoder and the query parser — 3.5GB that the
# 9B needs and that nothing here can free itself.
set -u

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT="$(dirname "$BENCH_DIR")"
OUT="${OUT:?set OUT to the results directory}"
IMAGES="${IMAGES:?set IMAGES to the sample list}"
BATCH="${BATCH:-8}"

export HF_HOME="$PROJECT/.cache"
export HF_HUB_CACHE="$HF_HOME"

# Enough for the largest candidate's weights plus its activations. Polled rather
# than assumed, because the thing being waited for is a person typing systemctl.
NEED_MIB=13500
DEADLINE=$((SECONDS + 1800))

free_mib() { nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | head -1; }

# The renditions are 0600 photod:photod — the archive protecting itself, and not
# something a bench should change. stage_renditions.sh copies the sampled ones
# out under the bench user; this waits for that to have happened, because
# os.path.exists() is true for a file this process cannot open and the failure
# lands 200 images later.
first_image="$(head -1 "$IMAGES" | cut -d'|' -f1)"
echo "waiting for readable renditions and for $NEED_MIB MiB (have $(free_mib))"
while { [ ! -r "$first_image" ] || [ "$(free_mib)" -lt "$NEED_MIB" ]; } \
      && [ "$SECONDS" -lt "$DEADLINE" ]; do
    sleep 10
done
if [ ! -r "$first_image" ]; then
    echo "ABORT: $first_image is still unreadable — run stage_renditions.sh"
    exit 1
fi

AVAILABLE=$(free_mib)
if [ "$AVAILABLE" -lt "$NEED_MIB" ]; then
    # Not a failure. The two 4B candidates are the like-for-like comparison and
    # they fit in what is free; the 9B is the one that needed the room.
    echo "TIMED OUT with ${AVAILABLE}MiB free — running the 4B candidates only"
    MODELS=(qwen3-vl-4b-instruct qwen3.5-4b)
else
    echo "proceeding with ${AVAILABLE}MiB free"
    MODELS=(qwen3-vl-4b-instruct qwen3.5-4b qwen3.5-9b-fp8)
fi

mkdir -p "$OUT"
for model in "${MODELS[@]}"; do
    echo "=== $model ==="
    # A model that will not load is a result too: the next one still runs, and
    # the log says which one was missing when the comparison is read.
    ( cd "$PROJECT" && uv run python bench/caption_bench.py \
        --model "$model" --images "$IMAGES" --batch "$BATCH" \
        --cache-dir "$HF_HOME" --out "$OUT/$model.jsonl" ) \
        > "$OUT/$model.log" 2>&1 \
        && echo "  ok — $(head -1 "$OUT/$model.jsonl")" \
        || echo "  FAILED — see $OUT/$model.log: $(tail -3 "$OUT/$model.log" | tr '\n' ' ')"
done

echo "BENCH_COMPLETE"
