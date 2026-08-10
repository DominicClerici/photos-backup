# Deploying to the archive machine

Everything in this directory is **untested**. It was written during Phase 4,
while the configuration surface was fresh, but the load testing for that phase
ran on a Mac against a local directory. Nothing here has been run on Fedora.
Treat it as a careful first draft, not a known-good deployment.

## What runs where

| | where | why |
|---|---|---|
| `blobs/`, `manifest.jsonl`, `incoming/` | `/mnt/photos`, the 6TB drive | irreplaceable, and large |
| derivatives | `/var/lib/photod/derivatives`, the SSD | the gallery reads them constantly, and all of them can be rebuilt |
| Postgres | the SSD | ordinary database reasons |

`incoming/` has to be on the same filesystem as `blobs/`, because a completed
upload becomes a blob by rename. That is why it is under `PHOTOS_ROOT` and not
configurable separately.

## Install

```sh
sudo useradd --system --home-dir /var/lib/photod --shell /usr/sbin/nologin photod
sudo install -d -o photod -g photod /var/lib/photod /var/lib/photod/derivatives
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
curl -s localhost:8787/health

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
