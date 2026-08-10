# loadgen

Pushes a directory of photos at photod over the same protocol the phone speaks,
so the archive can be run at real size without a phone in the loop.

**This is a test client, not a product surface.** Everything it does the app also
does — enumerate, ask what the server already has, hash only what the server
could not answer for, upload only what was asked for, and resume a large video
rather than restart it. If the two ever disagree, this one is wrong.

```sh
go run ./cmd/loadgen -root ../PHOTOS_TEST -server http://localhost:8790
```

| flag | default | |
|---|---|---|
| `-root` | `../PHOTOS_TEST` | directory of originals |
| `-server` | `http://localhost:8787` | photod base URL |
| `-device-id` | `loadgen` | upload as this device |
| `-concurrency` | `3` | simultaneous uploads, matching the app |
| `-check-batch` | `200` | items per `sync/check` |
| `-chunk-threshold` | `64MB` | size at which an upload becomes resumable |
| `-chunk-size` | `8MB` | bytes per chunk |
| `-limit` | `0` | stop after N files |
| `-abort-chunked-after` | `0` | abandon each chunked upload after N chunks |
| `-v` | | log every item |

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
