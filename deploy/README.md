# Deploying to the archive machine

Written during Phase 4 and revised in Phase 5. The systemd units and the SELinux
notes have been run on this Fedora box. The TLS steps and every CLI invocation
below have since been corrected against the deployed service — the first draft
was written against a photod on scratch ports and paths, and inherited an
environment from the shell that the real one does not get. The phone-side half of
pairing still has not been run end to end. Treat the install order as reliable
and the exact paths as worth reading before pasting.

## What runs where

| | where | why |
|---|---|---|
| `blobs/`, `manifest.jsonl`, `incoming/` | `/mnt/photos`, the archive partition | irreplaceable, and large |
| `vault/` | `/mnt/photos/vault`, beside the blobs | irreplaceable too, and encrypted — Archive and Hidden live here |
| derivatives | `/var/lib/photod/derivatives`, the SSD | the gallery reads them constantly, and all of them can be rebuilt |
| `derivatives/vault/` | the SSD, beside them | the encrypted ones, which **cannot** be rebuilt without the password |
| Postgres | the SSD, in the compose container | ordinary database reasons |
| `geonames/` | `/var/lib/photod/geonames`, the SSD | the offline geocoder's reference tables — re-downloadable, not part of the library |
| `ca.key`, `server.key` | `/var/lib/photod/tls`, the SSD | machine identity, not library — and losing `ca.key` means re-pairing every device |

`incoming/` has to be on the same filesystem as `blobs/`, because a completed
upload becomes a blob by rename. That is why it is under `PHOTOS_ROOT` and not
configurable separately.

The two `vault/` trees are the one place the "derivatives are always rebuildable"
rule does not hold. A hidden photograph's thumbnails are rebuilt from its
original, and its original is encrypted — so a backup that skips
`derivatives/vault/` is one that will need the vault password before the gallery
can draw a grid again. Back both trees up, and back up the `vault_secret` row in
Postgres with them: it holds the wrapped private key, and no amount of the
password recovers a vault without it.

### The archive partition

The 6TB drive is split, and photod only gets part of it:

```
sda           5.5T
├─sda1        500G  ext4   /mnt/photos                  the archive
└─sda2          5T  ntfs   /run/media/dominic/storage   not photod's
```

500GB against a ~100GB library is about 5x headroom, so this is sized for v1 and
then some. Two consequences worth knowing before they surprise you:

- Free space on the drive is not free space for the archive. `df -h /mnt/photos`
  is the number that matters; the 5TB next to it is a different filesystem.
- The NTFS partition is mounted by the desktop session under `/run/media`, which
  is outside `ReadWritePaths=` and blocked by `ProtectHome=yes` besides. photod
  cannot read it even if asked. Importing anything from there means copying it
  somewhere photod can see first.

Growing the archive means resizing across a partition boundary, not extending
into adjacent free space. Cheapest while `/mnt/photos` is still empty.

## Install

```sh
sudo useradd --system --home-dir /var/lib/photod --shell /usr/sbin/nologin photod
sudo install -d -o photod -g photod /var/lib/photod /var/lib/photod/derivatives
# photod tightens this to 0700 on startup; creating it now just settles ownership.
sudo install -d -o photod -g photod -m 0700 /var/lib/photod/tls
sudo chown -R photod:photod /mnt/photos

# -C server because the module root is server/, not the repository root.
go build -C server -o /tmp/photod ./cmd/photod
go build -C server -o /tmp/photobackup ./cmd/photobackup
sudo install -m 0755 /tmp/photod /tmp/photobackup /usr/local/bin/
sudo install -m 0755 deploy/photobackup-admin /usr/local/bin/   # see "Running the CLI"

sudo install -d -m 0755 /etc/photod
sudo install -m 0640 -g photod deploy/photod.env.example /etc/photod/photod.env
sudoedit /etc/photod/photod.env          # at minimum, the database password

# The offline geocoder's reference tables. Optional: without them photographs
# keep their coordinates and simply have no place name. See server/README.md.
sudo install -d -o photod -g photod /var/lib/photod/geonames
cd /var/lib/photod/geonames
sudo -u photod curl -O https://download.geonames.org/export/dump/cities500.zip
sudo -u photod curl -O https://download.geonames.org/export/dump/admin1CodesASCII.txt
sudo -u photod curl -O https://download.geonames.org/export/dump/countryInfo.txt
cd -
photobackup-admin geocode          # name the places for everything already archived

sudo cp deploy/photod.service deploy/photobackup-verify.service \
        deploy/photobackup-verify.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now photod photobackup-verify.timer
```

## photo-ml

Optional, and optional forever. Everything above works without it; what it adds
is being able to find a photograph by what is in it. Skip this section and the
archive is exactly what it was.

It is a `uv` project rather than a Go binary, so it installs differently from
everything else here: the source goes to `/opt/photo-ml` with its virtualenv
beside it, the model weights go to `/var/lib/photo-ml`, and the service runs as
its own user that cannot see the archive.

```sh
sudo useradd --system --home-dir /var/lib/photo-ml --shell /usr/sbin/nologin photo-ml
sudo install -d -o photo-ml -g photo-ml /var/lib/photo-ml \
        /var/lib/photo-ml/cache /var/lib/photo-ml/triton

# The source, and a venv built beside it. ~6GB: torch and CUDA are most of it.
sudo rsync -a --delete --exclude .venv --exclude .cache photo-ml/ /opt/photo-ml/
sudo chown -R photo-ml:photo-ml /opt/photo-ml
sudo -u photo-ml env HOME=/var/lib/photo-ml \
        sh -c 'cd /opt/photo-ml && uv sync --frozen'

# Pull the weights now, as the service user, so the first start is a start
# rather than a 14GB download with systemd watching. Four models: the encoder,
# the captioner, the query parser, and the OCR pipeline — which fetches its own
# ONNX files on first use rather than through the hub.
sudo -u photo-ml env HOME=/var/lib/photo-ml HF_HOME=/var/lib/photo-ml/cache \
        PHOTO_ML_CACHE_DIR=/var/lib/photo-ml/cache \
        /opt/photo-ml/.venv/bin/python -c "
from huggingface_hub import snapshot_download
from photo_ml import encoder, captioner, parser
for hf_id in (encoder.HF_ID, captioner.HF_ID, parser.HF_ID):
    print(hf_id, snapshot_download(hf_id, cache_dir='/var/lib/photo-ml/cache'))
from photo_ml.ocr import Recognizer
Recognizer()
"

sudo cp deploy/photo-ml.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now photo-ml
```

Then tell photod about it — `ML_URL` in `/etc/photod/photod.env`, which
`photod.env.example` already carries — and restart:

```sh
sudoedit /etc/photod/photod.env       # ML_URL=http://127.0.0.1:8789
sudo systemctl restart photod
```

That restart is the *embedding* backfill. photod's `ReconcileVision` finds every
asset whose ML renditions exist and that this model has said nothing about,
queues one `vision` job each, and the pool drains them in about fifteen minutes
for a library this size.

The captions and the recognised text are not queued by a restart, deliberately:
hours of GPU work begun by `systemctl restart photod` is a restart with a
surprise in it. They are a command, and because it is a command it can be
bounded:

```sh
photobackup ml status                                # how far anything has got
photobackup ml backfill --stills 1000 --videos 20    # a sample first, newest
photobackup ml backfill                              # then the whole library
```

The pool drains text recognition before captions — twenty minutes before four
hours — so the screenshots become searchable while the captioner is still
starting. Watch either with `photobackup ml status`.

Raising `VISION_CONCURRENCY` to 8 for an overnight run is worth it and is the
only knob that matters: photo-ml batches, and the batch can only form if several
requests are in flight. See photo-ml/README.md § Batching the captioner.

### Checking it

```sh
curl -s localhost:8789/health | python3 -m json.tool
```

`ready: true` and the encoder listed as resident. Then, from the source tree:

```sh
photo-ml/check-space.sh some-photo.webp "a dog" "a spreadsheet"
```

The right phrase should score higher. That check exists because this is the
failure that does not announce itself: a text tower padded to the wrong length
produces vectors that are unit length, well-formed, and quietly meaningless.

And the sandbox, which is the reason this service has a user of its own:

```sh
sudo systemd-run --uid=photo-ml -p InaccessiblePaths=/mnt/photos \
     --pty --wait ls /mnt/photos          # ls: cannot access: No such file or directory
```

The unit puts both `/mnt/photos` and `/var/lib/photod` out of reach by mount
namespace rather than by file mode — `/mnt/photos` is 0755 here, so anyone who
can name a blob can read it, and the namespace is what actually stops this
process. That is ML_IMAGES.md §3 enforced by the kernel: photo-ml is handed the
bytes of the photographs it is meant to see, over a socket, by a process that
has already decided which those are. It cannot go looking for the others, and no
future change to it can start.

### Updating it

`photobackup-admin redeploy` syncs `photo-ml.service` if it is installed, and
deliberately does nothing else here: rebuilding a 5GB venv is not something a
two-second photod redeploy should do on every run. When the Python changes:

```sh
sudo rsync -a --delete --exclude .venv --exclude .cache photo-ml/ /opt/photo-ml/
sudo chown -R photo-ml:photo-ml /opt/photo-ml
sudo -u photo-ml env HOME=/var/lib/photo-ml sh -c 'cd /opt/photo-ml && uv sync --frozen'
sudo systemctl restart photo-ml
```

An update that adds a model adds a download, and systemd will not wait patiently
for one — `uv sync` installs the code, and the weights come down on the first
request that needs them. Pre-fetch as the service user, the same way the install
above does, before restarting:

```sh
sudo -u photo-ml env HOME=/var/lib/photo-ml HF_HOME=/var/lib/photo-ml/cache \
        /opt/photo-ml/.venv/bin/python -c "
from huggingface_hub import snapshot_download
from photo_ml import captioner, parser
for hf_id in (captioner.HF_ID, parser.HF_ID):
    print(hf_id, snapshot_download(hf_id, cache_dir='/var/lib/photo-ml/cache'))
from photo_ml.ocr import Recognizer
Recognizer()
"
```

`curl -s localhost:8789/health` afterwards lists what is registered and, for
anything that failed to load, why.

Restarting it under a running backfill costs nothing. The vision pool asks
whether the service is there before it claims anything, and the one job in
flight is put back with its attempt returned rather than spent — see
`jobs.Defer`. The queue pauses and resumes; it does not fail.

### VRAM, and what to watch

Four models, and the budget is tight enough to be worth writing down:

| | |
|---|---|
| encoder, resident | 2.3GB |
| query parser, resident | 1.2GB |
| captioner, on demand | 9.1–10.1GB |
| text recogniser | none — it runs on the CPU |
| desktop session | ~1.4GB |

About 15 of 16.3GB at the peak of a captioning pass. That is why the captioner
is on demand and why unloading calls `torch.cuda.empty_cache()` rather than just
dropping a reference — dropping it returns the blocks to torch's allocator,
which keeps them reserved against the driver, and `nvidia-smi` goes on showing
10GB held.

ML_IMAGES.md §11 names the number to watch: **whether NVENC transcodes ever fail
to allocate during a backfill.** If they do, the answer is a lower
`PHOTO_ML_DESCRIBE_BATCH` (4 → 2 gives back about a third of a gigabyte) or
pausing the vision pool, not a bigger card. The batch is already 4 rather than
8, because a batch of 8 does not fit at `captioner.MAX_PIXELS = 1024*1024` — so
this lever has less left in it than it used to, and the pause is the likelier
answer.

### Changing the model

The whole point of `model` being part of `asset_embeddings`' primary key:

```sql
delete from asset_embeddings where model = 'siglip2-so400m-patch14-384';
```

Restart photod and the reconcile queues the library again. The captioner and the
recogniser are the same operation against their own tables, followed by
`photobackup ml backfill` rather than a restart:

```sql
delete from asset_descriptions where model = 'qwen3-vl-4b-instruct';
delete from asset_ocr          where model = 'rapidocr';
```

Those two deletes are for retiring a model *name*, which is the case where the
old rows are genuinely rubbish and should stop being searchable at once. If the
name is staying and only the recipe changed — a raised `captioner.MAX_PIXELS`, a
new prompt — use `photobackup ml backfill --force` instead and delete nothing:
it queues every asset again and each one's words are replaced in place as the
pass reaches it, so search is stale for a few hours rather than empty for
them.

Each is independent of the other two, which is the whole reason they are three
job kinds: an encoder bench is fifteen minutes and must not drag four hours of
captioning behind it. Two encoders can also
sit in the table together while they are compared — nothing above requires the
old rows to go first. What does need writing by hand is the HNSW index for the
new name, since its predicate names one model literally; migration
`0017_vision.sql` is the template.

### If it will not start

- `no kernel image is available for execution on the device` — the wheels are
  built for an older architecture than this card. `pyproject.toml` pins the
  `pytorch-cu128` index for exactly this; check `uv sync` actually used it.
- `/health` says `"device": "cpu"` on a machine with a GPU — the sandbox is in
  the way, and the service will not say so beyond one warning at startup:
  `cudaGetDeviceCount() ... Error 304: OS call failed`. Three ways to cause it,
  in the order they have actually happened here:
  - **`AF_UNIX` missing from `RestrictAddressFamilies`.** The CUDA driver opens
    a unix socket while initialising. This is the one that caught the first
    install, and the error message names neither sockets nor the setting.
  - `PrivateDevices=yes`, which hides `/dev/nvidia*`. It must stay unset.
  - the device nodes tightened from 0666, which needs
    `SupplementaryGroups=video render`.

  The bisect is quick and needs no theory — run the probe under the full set of
  options and then under the set minus one, as your own user:

  ```sh
  systemd-run --user --quiet --wait --pipe -p RestrictAddressFamilies="AF_INET AF_INET6" \
      /opt/photo-ml/.venv/bin/python -c 'import torch; print(torch.cuda.is_available())'
  ```
- `ready` stays false — it is still pulling weights, or it failed to. The
  `error` field in `/health`'s model list says which.

## The database

Postgres runs from the repository's `docker-compose.yml` on this machine — the
same container the tests use, on 5432, which is what `DATABASE_URL` in
`photod.env` points at. There is no system `postgresql` unit.

The image is `pgvector/pgvector:pg18`, not the stock `postgres:18-alpine`, and
the difference matters twice. Migration 0016 opens with `create extension
vector`, which the alpine image cannot satisfy — and the two images are built on
different C libraries, which is not a detail.

**Swapping from the alpine image is a data operation.** musl's `strcoll` is
`strcmp`, so under alpine a `en_US.utf8` database was collating in byte order;
glibc collates by ISO 14651. Every text B-tree written under the old image is
therefore in the wrong order for the new one, and a wrong-ordered unique index
does not raise an error — it fails to find rows, which for `assets.sha256` means
`on conflict (sha256) do nothing` stops catching a duplicate and the next backup
quietly archives a second copy of a photograph. So photod stops first, and every
database is rebuilt before anything connects:

```sh
sudo systemctl stop photod
docker compose pull postgres && docker compose up -d postgres
for db in $(docker exec photos-backup-postgres-1 psql -U photobackup -tAc \
      "select datname from pg_database where datallowconn"); do
  docker exec photos-backup-postgres-1 psql -U photobackup -d "$db" -c "reindex database \"$db\";"
done
sudo systemctl start photod
```

One more statement is worth running once, and it is not cosmetic:

```sh
docker exec photos-backup-postgres-1 psql -U photobackup -c \
  "update pg_database set datcollversion = pg_database_collation_actual_version(oid)
   where datcollversion is null and datlocprovider = 'c';"
```

A database created under musl records no collation version at all, and
`ALTER DATABASE … REFRESH COLLATION VERSION` refuses to fill one in — it will
not accept a change from "unknown". Left null, Postgres has nothing to compare
against and will *not* warn the next time glibc changes its collation, which is
the same silent index corruption arriving again with no announcement. Writing it
by hand is what restores the warning.

This was run on this machine on 2026-08-20, against 23,080 assets, and took
about two minutes end to end.

## Firewall

Fedora runs firewalld, and none of these ports are open by default. Without this
step the phone's mDNS scan finds the server and every connection to it times out,
which looks exactly like photod not running.

```sh
sudo firewall-cmd --permanent --add-port=8787/tcp     # photod, HTTPS: pairing and uploads
sudo firewall-cmd --permanent --add-port=5353/udp     # mDNS, for Avahi to answer
sudo firewall-cmd --reload
```

Port 8788 stays closed. That is the read-only plaintext listener, bound to
127.0.0.1 for the Next app and the CLI — it is the one way into the archive that
asks for nothing, so opening it would put the whole archive on the LAN
unauthenticated, which is a decision, not a step (see "What is still open"
below).

`photobackup ca --serve` needs one port open for as long as the transfer takes:

```sh
sudo firewall-cmd --add-port=8789/tcp                 # no --permanent: gone on reload
```

## Running the CLI

`photobackup` reads its configuration from the environment, exactly as photod
does, and `EnvironmentFile=` in the unit is the only thing that loads
`/etc/photod/photod.env`. `sudo -u photod` starts from a clean environment, so a
bare `sudo -u photod photobackup ...` gets every fallback in `config.FromEnv`:
`./data/photos` relative to whatever directory you happen to be in, and the
development database URL.

`photobackup-admin` hands it the env file the way systemd does — installed
alongside the binaries above, so it is on `PATH` in any shell:

```sh
sudo install -m 0755 deploy/photobackup-admin /usr/local/bin/
```

It wraps `systemd-run`, which reads the env file as root before dropping to the
`photod` user, so the database password never lands on a command line or in `ps`.
Every `photobackup` subcommand works through it.

Not a nicety. Two of those fallbacks fail in ways that do not look like failure:

- `photobackup ca` *creates* a CA when it finds none at `TLS_DIR`. Run without
  the env somewhere writable and it mints a second, unrelated CA and serves that
  to the phone, which then trusts a CA photod is not using — indistinguishable
  from never having trusted it, and it costs an afternoon.
- `photobackup verify` against an empty `./data/photos` finds nothing missing and
  exits 0. An all-clear from an archive that is not yours.

`ca` reads no database, so it can also be run without the password going
anywhere:

```sh
sudo -u photod env TLS_DIR=/var/lib/photod/tls photobackup ca --serve --addr :8789
```

## Redeploying after a change

The Install section above is the first deployment. Every one after it is:

```sh
photobackup-admin redeploy
```

It rebuilds `photod` and `photobackup` from the checkout, updates any of the
installed unit files that have drifted from `deploy/`, cycles the Postgres
container, applies any pending database migrations, restarts photod, and waits
for `/health` to answer before reporting success. If it does not answer within
fifteen seconds it prints the unit status and the last thirty journal lines and
exits non-zero.

```sh
photobackup-admin redeploy --dry-run          # build and diff, change nothing
photobackup-admin redeploy --no-db            # leave the container alone
photobackup-admin redeploy --src ~/src/photos-backup
```

Five things worth knowing:

- **The build happens before the stop.** A tree that does not compile costs a
  minute and leaves the running service untouched. Downtime is the install and
  the restart, a second or two.
- **It runs as you, not as photod.** The Go build cache belongs to the invoking
  user, and photod has no shell and no read access to the repository. This is
  why `redeploy` is handled inside the wrapper rather than being a `photobackup`
  subcommand — it is the one thing here that is not about the archive.
- **It finds the source tree by walking up from the current directory**, looking
  for `server/go.mod` and `deploy/` together. Run it from anywhere inside the
  checkout, or set `PHOTOBACKUP_SRC`, or pass `--src`.
- **The database is cycled too**, from the checkout's `docker-compose.yml`, in
  the window where photod is already down: `stop` then `up -d --wait`, so a
  changed compose file is actually applied and the healthcheck — not a guess —
  decides when migrations may run. Nothing restarts that container at boot, so
  a machine that has rebooted since the last deploy comes back with the
  database gone and finds out at the migration step, with photod already
  stopped; this is what closes that. Data lives in the `pgdata` volume and
  neither command touches it. `--no-db` skips the whole step, and so does a
  machine with no docker or no compose file — with a note saying so.
- **Migrations run while photod is stopped**, from the binary that was just
  installed, so the schema change and the code that needs it land together.
  photod migrates on start as well, but a failure there is a service that will
  not come back; a failure here is a deploy that stops and says so, leaving the
  new binaries in place and photod down. Fix the schema and finish by hand:
  `photobackup-admin migrate && sudo systemctl start photod`.

Unit files are only refreshed if they are already installed; `redeploy` will not
perform a first install, and it refuses to run at all if `photod.service` is
missing. `systemctl daemon-reload` runs only when something actually changed.

Binaries are installed by writing beside the target and renaming, so nothing
ever reads a half-written file — including `photobackup-admin`, which replaces
itself from the same tree as the binaries it installs. That does mean the very
first `redeploy` has to come from the checkout, since the installed copy predates
the subcommand:

```sh
./deploy/photobackup-admin redeploy
```

## Pairing the phone

Three steps, in this order. The first two are once per phone; the last is once
per phone forever.

**1. Get the CA onto the phone.** photod generated one on first start. It has to
be installed *and* trusted before the app can reach the upload path at all,
because iOS will not let an app decide for itself which certificates to accept.

```sh
photobackup-admin ca --serve --addr :8789
```

That prints the SHA-256 fingerprint and a URL. Check the fingerprint against the
one photod logged at startup (`TLS ready ... ca_sha256=`) before going near the
phone — if they differ, the CLI found a different `TLS_DIR` than the daemon and
is serving the wrong CA.

Open the URL in **Safari** on the phone, then:

- Settings › Profile Downloaded › Install
- Settings › General › About › Certificate Trust Settings › switch photobackup on

Both steps are required. Installing without trusting changes nothing, and it is
the single most likely thing to go wrong here. Check the fingerprint iOS shows
against the one printed — the transfer is over plain HTTP, and it has to be,
since the phone cannot validate this server's TLS until it holds the file being
fetched.

The server stops serving after one download.

**2. Pair.**

```sh
photobackup-admin pair
```

Type the eight-character code into the app's Pairing section. It is good for ten
minutes, works once, and hands the phone a token that lives in the keychain and
never expires.

**3. Check.**

```sh
photobackup-admin devices
```

### When the app cannot connect

In order of likelihood:

| symptom | cause |
|---|---|
| pairing fails as though the server were unreachable | the CA is installed but not *trusted*, or not installed |
| everything times out | firewalld — port 8787 is not open |
| pairing works, uploads 426 | the app is on an `http://` address; it reached the read-only listener |
| worked at home, fails away | the Tailscale address is not in the certificate — `photobackup ca` lists what is |
| the CA is installed *and* trusted, and pairing still fails as unreachable | the CA on the phone is not the one photod serves with; compare the fingerprint against `ca_sha256` in photod's startup log |

iOS reports a rejected certificate and a dead host identically, which is why the
first two look the same from the phone. Rule the firewall out from the server
first: `curl -sk https://<lan-ip>:8787/health`.

## Starting over

`photobackup reset` erases the archive and leaves pairing intact:

```sh
sudo systemctl stop photod
photobackup-admin reset --dry-run   # what would go
photobackup-admin reset             # asks for confirmation
sudo systemctl start photod
```

It empties `assets`, `device_assets` and `jobs`, then removes `blobs/`,
`manifest.jsonl`, `incoming/` and everything under `DERIVATIVES_ROOT`. The
`devices` and `pairing_codes` tables and the CA under `TLS_DIR` are untouched, so
every paired phone keeps its token and does not have to be paired again.

All four stores go together because any one of them left behind would undo the
others: `reindex` rebuilds the database from `manifest.jsonl`, so emptying
Postgres alone means the next reindex puts the entire library straight back.

Two guards. It refuses to run while something is listening on `LISTEN_ADDR` or
`PLAINTEXT_ADDR` — stop photod first, or pass `--force` if that socket is not
this archive's daemon. And it refuses outright if `TLS_DIR` sits inside anything
it would remove, which is the configuration where a reset would take `ca.key`
with it and silently unpair every device.

There is no undo. Once the manifest is gone, nothing can reconstruct what was
archived — that is the point of the confirmation prompt, and `--yes` skips it
only for a scripted rebuild.

> **The phone will not re-upload on its own.** The app keeps its own queue in
> `photobackup-queue.db` and marks finished items `done` permanently, so after a
> server-side reset it reports a complete backup and sends nothing. Re-checking
> is a step on the phone: **Backup › Re-check the archive**, which sends every
> finished item back to be asked about again. Everything the archive still holds
> answers `have` in round one and costs nothing; only what the reset actually
> took is re-sent.
>
> Worth knowing even when no reset was run. `done` is the only state
> re-enumeration cannot reach — `enqueue` is `insert or ignore` on the local id
> and `due` never selects a finished row — so any item that reaches it while its
> bytes are not in the archive is invisible to every later run, and the phone
> reports "up to date" with photos missing.

## What is still open

The read endpoints are authenticated on 8787 and open on 8788. A phone reads the
gallery with the same device token it uploads with; the Next app on this machine
reads without a token, over loopback, which is what keeps a development browser
from having to trust a private CA to draw a thumbnail.

So 8788 is the only unauthenticated way into the archive, and everything that
protects it is the fact that it is bound to `127.0.0.1` and left out of the
firewall. Widening `PLAINTEXT_ADDR` puts the whole archive on the LAN for anyone
who asks. It still cannot leak a credential — the plaintext listener serves
neither pairing nor any authenticated route — but there is no browser-facing
authentication in front of it either: reaching the gallery from another machine
on the LAN is not something this deployment supports today.

`/health` is unauthenticated on both, on purpose: the app pings a remembered
address to see whether it still answers, which it has to be able to do before it
has a token and after one has been revoked.

## Media tooling

HEIC decoding needs RPM Fusion. Without it every thumbnail fails while uploads
keep working perfectly, which is the intended degradation but a confusing one to
debug.

```sh
sudo dnf install \
  https://mirrors.rpmfusion.org/free/fedora/rpmfusion-free-release-$(rpm -E %fedora).noarch.rpm \
  https://mirrors.rpmfusion.org/nonfree/fedora/rpmfusion-nonfree-release-$(rpm -E %fedora).noarch.rpm
sudo dnf install ImageMagick perl-Image-ExifTool ffmpeg

magick some-photo.HEIC /tmp/out.webp      # must succeed before trusting a backfill
```

photod logs each missing tool at startup and starts anyway.

## SELinux

Fedora enforces SELinux, and a service confined by `ProtectSystem=strict` writing
to a mounted external drive is exactly the combination that gets denied. If
photod starts but cannot write:

```sh
sudo ausearch -m avc -ts recent
sudo semanage fcontext -a -t var_lib_t "/mnt/photos(/.*)?"
sudo restorecon -Rv /mnt/photos
```

Check this before concluding the archive path is wrong — a permission denial and
a bad path look identical in the log.

## mDNS

Fedora runs Avahi, which owns port 5353, so photod's built-in responder cannot
bind. Set `MDNS_DISABLED=1` and let Avahi advertise:

```xml
<!-- /etc/avahi/services/photobackup.service -->
<service-group>
  <name replace-wildcards="yes">photobackup on %h</name>
  <service>
    <type>_photobackup._tcp</type>
    <port>8787</port>
  </service>
</service-group>
```

## Checking it works

```sh
systemctl status photod
journalctl -u photod -f
curl -s localhost:8788/health                        # the plaintext read listener
curl -s --cacert /var/lib/photod/tls/ca.crt https://localhost:8787/health

# The gallery reads without a token only on 8788. On 8787 they answer 401.
curl -s localhost:8788/v1/timeline?limit=1
curl -si --cacert /var/lib/photod/tls/ca.crt https://localhost:8787/v1/timeline | head -1

photobackup-admin verify                  # fast: stat only
photobackup-admin verify --deep           # slow: re-hashes the whole archive
photobackup-admin verify --retry-failed   # put jobs that gave up back in the queue
systemctl list-timers photobackup-verify
```

`--retry-failed` is deliberately not part of `--fix`, which the weekly timer
runs: a job in this state has already spent every attempt it was given, so
retrying it is a judgement about what changed — a new binary, a codec that was
missing before. Automatic, it would regrind the same impossible file forever.

`photobackup verify` exit codes, so the timer's failure means something:

| code | meaning |
|---|---|
| 0 | intact |
| 1 | findings, none of them lost or damaged originals |
| 2 | originals missing or no longer matching their hash |

## The one thing to test first

Before pointing the phone at this machine, prove the drive survives a reboot
with the service enabled:

```sh
sudo systemctl reboot
# after it comes back
systemctl status photod && curl -s localhost:8787/health
```

`RequiresMountsFor=/mnt/photos` exists so photod refuses to start when the drive
is absent. If that ordering is wrong, photod creates an empty blob tree at the
mount point and archives into it — the failure this deployment most needs to not
have.
