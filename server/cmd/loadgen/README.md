# loadgen

Pushes a directory of photos at photod over the same protocol the phone speaks,
so the archive can be run at real size without a phone in the loop.

**This is a test client, not a product surface.** Everything it does the app also
does — enumerate, ask what the server already has, hash only what the server
could not answer for, upload only what was asked for, and resume a large video
rather than restart it. If the two ever disagree, this one is wrong.

```sh
photobackup pair                                     # take the code
photobackup ca                                       # note where ca.crt is

export PHOTOBACKUP_TOKEN=pbk_...                     # redeem the code first; see below
export PHOTOBACKUP_CA=/var/lib/photod/tls/ca.crt
go run ./cmd/loadgen -root ../PHOTOS_TEST
```

Since Phase 5 it needs a device token and photod's CA, exactly as the phone does.
That is not a testing inconvenience to route around: the write path has no
unauthenticated entrance, and giving this client a private one would stop it being
a stand-in for the app. Redeeming a code is one request:

```sh
curl -s --cacert "$PHOTOBACKUP_CA" -X POST https://localhost:8787/v1/pair \
  -H 'content-type: application/json' \
  -d '{"code":"ABCD-EFGH","name":"loadgen","platform":"go"}'
```

| flag | default | |
|---|---|---|
| `-root` | `../PHOTOS_TEST` | directory of originals |
| `-server` | `https://localhost:8787` | photod base URL |
| `-token` | `$PHOTOBACKUP_TOKEN` | device token from `photobackup pair` |
| `-ca` | `$PHOTOBACKUP_CA` | photod's CA, so its self-signed TLS validates |
| `-insecure` | | skip TLS verification entirely |
| `-device-id` | unset | upload as this device; empty means whichever the token names |
| `-concurrency` | `3` | simultaneous uploads, matching the app |
| `-check-batch` | `200` | items per `sync/check` |
| `-chunk-threshold` | `64MB` | size at which an upload becomes resumable |
| `-chunk-size` | `8MB` | bytes per chunk |
| `-limit` | `0` | stop after N files |
| `-abort-chunked-after` | `0` | abandon each chunked upload after N chunks |
| `-v` | | log every item |

`-device-id` is empty by default because the token settles identity now. Setting
it to anything other than the paired device's id is a 403 — the server checks the
claim against the token rather than trusting it.

photod signs its own certificate, so a client either trusts that CA or does not
verify at all; trusting the system roots and hoping is not one of the options.
`-insecure` exists for a throwaway run against a server whose CA is not to hand,
and says so on stderr when used.

It skips `*.json` (Takeout's metadata sidecars) and dotfiles, uses the path
relative to `-root` as the local id, and reads capture times out of the Takeout
sidecar when there is one — falling back to the file's modification time.

## Proving resume works

`-abort-chunked-after` stops each chunked upload partway and leaves the partial
on the server on purpose. Re-run without it and the same files pick up where
they stopped, across a process boundary:

```sh
go run ./cmd/loadgen -root ../PHOTOS_TEST -abort-chunked-after 5   # abandons
go run ./cmd/loadgen -root ../PHOTOS_TEST                          # resumes
```

A session is scoped to the device that opened it, so resuming only works under the
same token. A different paired device asking about the same session is told 404,
and cannot abort it either — which matters, because a hostile abort would cost the
real uploader a multi-gigabyte re-send.

The second run reports `chunked uploads N (N resumed from a partial)` and sends
only the bytes still owed — for the 550MB video in the test corpus, 509.8MB
after 40MB had already landed.

## Reading the output

`bytes sent` is what went on the wire, not what the files weigh. A resumed
upload sends less than its own size, and that gap is the feature working.

A second run against an archived library should report **zero bytes and no
hashing** — that is PROJECT.md's second success criterion, and it is also the
canary for the timestamp-precision bug described in the server README. If a
second run hashes anything at all, something is wrong.
