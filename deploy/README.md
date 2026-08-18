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
| Postgres | the SSD | ordinary database reasons |
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

sudo cp deploy/photod.service deploy/photobackup-verify.service \
        deploy/photobackup-verify.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now photod photobackup-verify.timer
```

## Firewall

Fedora runs firewalld, and none of these ports are open by default. Without this
step the phone's mDNS scan finds the server and every connection to it times out,
which looks exactly like photod not running.

```sh
sudo firewall-cmd --permanent --add-port=8787/tcp     # photod, HTTPS: uploads, and the gallery
sudo firewall-cmd --permanent --add-port=5353/udp     # mDNS, for Avahi to answer
sudo firewall-cmd --reload
```

Port 8788 stays closed. That is the read-only plaintext listener, bound to
127.0.0.1 for the Next app and the CLI — it is the one way into the archive that
asks for nothing, so opening it would put the whole archive on the LAN
unauthenticated, which is a decision, not a step (see "What is still open"
below).

One port, not two. The gallery is served *through* 8787 when it is served to the
network at all — see below — so there is nothing to open for it.

`photobackup ca --serve` needs one port open for as long as the transfer takes:

```sh
sudo firewall-cmd --add-port=8789/tcp                 # no --permanent: gone on reload
```

## Serving the gallery to the house

Optional, and off until it is configured. Without this, the gallery is a browser
on this machine talking to loopback, and every other device in the house has
nothing.

Two units. `photoweb` runs Next, bound to `127.0.0.1:3000` and reachable by
nothing else; `photod` serves it on to the network from the port it already
has, behind one shared password. The archive is never on an unauthenticated
socket the network can reach, and the browser gets the app, the JSON and the
thumbnails from a single origin — which is what lets one cookie authenticate an
`<img>`.

```sh
sudo useradd --system --home-dir /opt/photobackup --shell /usr/sbin/nologin photoweb

# Build with neither PHOTOD_URL nor NEXT_PUBLIC_MEDIA_BASE set: both are frozen
# into the build, and both defaults are the ones this deployment wants.
(cd web && pnpm install --prod=false && pnpm build)
sudo install -d -o photoweb -g photoweb /opt/photobackup
sudo rsync -a --delete --chown=photoweb:photoweb web/ /opt/photobackup/web/

sudoedit /etc/photod/photod.env      # GALLERY_PASSWORD, and WEB_URL=http://127.0.0.1:3000
sudo cp deploy/photoweb.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now photoweb
sudo systemctl restart photod
```

Then, on the laptop or the iPad: install the CA once, exactly as the phone did
(`photobackup ca --serve`), and open `https://<hostname>.local:8787`. The first
thing it draws is the password prompt.

photod **refuses to start** if `WEB_URL` is set and `GALLERY_PASSWORD` is empty.
The failure that guards against is the quiet one — a gallery that works
perfectly, for everyone on the Wi-Fi.

Two things worth not getting wrong:

- **`-H 127.0.0.1` in photoweb.service.** That server proxies `/api/*` straight
  to 8788, which has no authentication at all. Binding it to `0.0.0.0` puts an
  open door beside the locked one, and nothing about it looks broken.
- **Leave `NEXT_PUBLIC_MEDIA_BASE` unset.** Pointing thumbnails at photod
  directly was worth doing when the alternative was streaming them through Node;
  under this arrangement they are already coming from photod, and setting it to
  an address the cookie does not cover 401s every tile in the grid.

The password is a house key: one for the household, rate-limited per address,
exchanged for a cookie that photod keeps in memory and forgets when it restarts.
It is not the vault password and cannot be — Archive and Hidden are encrypted
against this server, and this one is a lock the server has to be able to check.

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
installed unit files that have drifted from `deploy/`, restarts photod, and
waits for `/health` to answer before reporting success. If it does not answer
within fifteen seconds it prints the unit status and the last thirty journal
lines and exits non-zero.

```sh
photobackup-admin redeploy --dry-run          # build and diff, change nothing
photobackup-admin redeploy --src ~/src/photos-backup
```

Three things worth knowing:

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
gallery with the same device token it uploads with, and a browser reads it with a
session cookie it got by typing the gallery password; the Next app on this
machine reads without either, over loopback, which is what keeps a development
browser from having to trust a private CA to draw a thumbnail.

So 8788 is now the only unauthenticated way into the archive, and everything that
protects it is the fact that it is bound to `127.0.0.1` and left out of the
firewall. Widening `PLAINTEXT_ADDR` puts the whole archive on the LAN for anyone
who asks. It still cannot leak a credential — the plaintext listener serves
neither pairing nor any authenticated route — but it is no longer a gap shared
with the port the phone dials.

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
