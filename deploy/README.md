# Deploying to the archive machine

Written during Phase 4 and revised in Phase 5. The systemd units and the SELinux
notes have been run on this Fedora box; the pairing and TLS steps below were
verified against a photod on scratch ports and paths, not against the deployed
service. Treat the install order as reliable and the exact paths as worth reading
before pasting.

## What runs where

| | where | why |
|---|---|---|
| `blobs/`, `manifest.jsonl`, `incoming/` | `/mnt/photos`, the 6TB drive | irreplaceable, and large |
| derivatives | `/var/lib/photod/derivatives`, the SSD | the gallery reads them constantly, and all of them can be rebuilt |
| Postgres | the SSD | ordinary database reasons |
| `ca.key`, `server.key` | `/var/lib/photod/tls`, the SSD | machine identity, not library — and losing `ca.key` means re-pairing every device |

`incoming/` has to be on the same filesystem as `blobs/`, because a completed
upload becomes a blob by rename. That is why it is under `PHOTOS_ROOT` and not
configurable separately.

## Install

```sh
sudo useradd --system --home-dir /var/lib/photod --shell /usr/sbin/nologin photod
sudo install -d -o photod -g photod /var/lib/photod /var/lib/photod/derivatives
# photod tightens this to 0700 on startup; creating it now just settles ownership.
sudo install -d -o photod -g photod -m 0700 /var/lib/photod/tls
sudo chown -R photod:photod /mnt/photos

go build -o /tmp/photod ./server/cmd/photod
go build -o /tmp/photobackup ./server/cmd/photobackup
sudo install -m 0755 /tmp/photod /tmp/photobackup /usr/local/bin/

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
sudo firewall-cmd --permanent --add-port=8787/tcp     # photod, HTTPS: uploads
sudo firewall-cmd --permanent --add-port=5353/udp     # mDNS, for Avahi to answer
sudo firewall-cmd --reload
```

Port 8788 stays closed. That is the read-only plaintext listener, bound to
127.0.0.1 for the Next app and the CLI — opening it would put the whole archive
on the LAN unauthenticated, which is a decision, not a step (see "What is still
open" below).

`photobackup ca --serve` needs one port open for as long as the transfer takes:

```sh
sudo firewall-cmd --add-port=8789/tcp                 # no --permanent: gone on reload
```

## Pairing the phone

Three steps, in this order. The first two are once per phone; the last is once
per phone forever.

**1. Get the CA onto the phone.** photod generated one on first start. It has to
be installed *and* trusted before the app can reach the upload path at all,
because iOS will not let an app decide for itself which certificates to accept.

```sh
sudo -u photod photobackup ca --serve --addr :8789
```

That prints the SHA-256 fingerprint and a URL. Open the URL in **Safari** on the
phone, then:

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
sudo -u photod photobackup pair
```

Type the eight-character code into the app's Pairing section. It is good for ten
minutes, works once, and hands the phone a token that lives in the keychain and
never expires.

**3. Check.**

```sh
sudo -u photod photobackup devices
```

### When the app cannot connect

In order of likelihood:

| symptom | cause |
|---|---|
| pairing fails as though the server were unreachable | the CA is installed but not *trusted*, or not installed |
| everything times out | firewalld — port 8787 is not open |
| pairing works, uploads 426 | the app is on an `http://` address; it reached the read-only listener |
| worked at home, fails away | the Tailscale address is not in the certificate — `photobackup ca` lists what is |

iOS reports a rejected certificate and a dead host identically, which is why the
first two look the same from the phone. Rule the firewall out from the server
first: `curl -sk https://<lan-ip>:8787/health`.

## What is still open

The gallery's read endpoints have no authentication. Anyone who can reach
photod's plaintext listener can page the whole archive and download originals,
which is why `PLAINTEXT_ADDR` defaults to `127.0.0.1` and why 8788 stays out of
the firewall. Phase 5 closed the write path; the read path is a known,
deliberate gap.

A device token can never cross the network in the clear regardless — the
plaintext listener does not serve pairing or any authenticated route, so widening
it exposes the archive but never a credential.

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

sudo -u photod photobackup verify          # fast: stat only
sudo -u photod photobackup verify --deep   # slow: re-hashes the whole archive
systemctl list-timers photobackup-verify
```

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
