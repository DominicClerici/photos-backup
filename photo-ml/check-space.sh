#!/usr/bin/env bash
# Does a photograph land near the words for what is in it?
#
# The one property the whole search feature rests on, and the one that fails
# silently: a text tower padded to the wrong length, or an image fed through the
# wrong preprocessing, produces vectors that are unit length, well-formed, and
# quietly meaningless. Nothing errors. The only way to find out is to ask.
#
#   ./check-space.sh photo.webp "a dog" "a spreadsheet"
#
# The first phrase should score highest. If the ranking is arbitrary, suspect
# TEXT_LENGTH in encoder.py before suspecting the model.
set -euo pipefail

if [ $# -lt 2 ]; then
    echo "usage: $0 IMAGE PHRASE [PHRASE...]" >&2
    exit 64
fi

image=$1
shift
PHOTO_ML_URL=${PHOTO_ML_URL:-http://127.0.0.1:8789} \
IMAGE="$image" python3 - "$@" <<'PY'
import base64, json, os, sys, urllib.request

url = os.environ["PHOTO_ML_URL"].rstrip("/") + "/embed"


def embed(body):
    req = urllib.request.Request(
        url, data=json.dumps(body).encode(), headers={"content-type": "application/json"}
    )
    return json.load(urllib.request.urlopen(req))


with open(os.environ["IMAGE"], "rb") as f:
    picture = embed({"images": [base64.b64encode(f.read()).decode()]})

phrases = sys.argv[1:]
words = embed({"texts": phrases})

# Cosine similarity is a dot product, because photo-ml answers with unit
# vectors — the same reason the SQL uses `<=>` without normalising anything.
scored = sorted(
    (sum(a * b for a, b in zip(picture["vectors"][0], v)), p)
    for v, p in zip(words["vectors"], phrases)
)

print(f'{picture["model"]}, {picture["dim"]}-d, {picture["took_ms"]}ms\n')
for score, phrase in reversed(scored):
    print(f"  {score:+.4f}  {phrase}")
PY
