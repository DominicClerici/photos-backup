# Porting the gallery to the phone

The browser gallery is finished. The Expo app is a backup utility with a
diagnostic screen bolted to it. This document plans the six phases that make
the phone a full gallery — the same timeline, the same viewer, the same
albums, the same vault — without either app growing a second copy of the
logic that decides what a timeline is.

It is a plan, not a record. `PROJECT.md` § 7 is the record; phases land there
when they are done.

---

## 1. What the survey found

Three things were established by reading the two apps and photod before any of
this was decided. They are why the plan is shaped the way it is.

### The phone needs no new authentication

`requireAuth` in `internal/api/websession.go` accepts **either** a passkey
session cookie **or** a `Bearer pbk_` device token, and the bearer branch wins
and returns before the cookie path or its CSRF check is ever reached. Every
read route and every route in `galleryRoutes` — trash, restore, purge, albums,
the vault — is behind that one guard.

The phone has held a device token in the keychain since Phase 5, and
`expo-app/src/gallery/client.ts` already reads the timeline and fetches
renditions with it. Phase 17's central constraint — *a browser attaches a
same-origin cookie to an `<img>` and will not attach a bearer header to one* —
is a browser's problem and not a phone's. `expo-image`, `expo-video` and
`File.downloadFileAsync` all take headers.

So: no passkeys on mobile, no cookie jar, no signed media URLs, no second
credential. The pairing flow that exists is the whole of it. The private CA is
already installed and trusted on the device, so native image and video loaders
validate the same certificate `fetch` does.

The one thing worth writing down because it is easy to get wrong later: the
vault unlock is **server-wide process state**, not per-caller and not
per-session. `POST /v1/vault/unlock` from the phone unlocks it for the browser
too, and locking it from the browser locks it on the phone. That is the
existing design — `internal/api/vault.go` says so — and the mobile UI must not
imply otherwise.

### The logic worth sharing is already pure

The gallery's difficult parts were written without a DOM in reach, and are
tested by node's own runner:

| Module | What it holds |
|---|---|
| `layout.ts` | The whole grid geometry: day model, zoom levels, `frameAt`, `tileRect`, `visibleItems`, `dayAt` |
| `view.ts` | The sort-and-filter rules, total and pure, a view in and a view out |
| `ranges.ts` | Selections as index ranges |
| `zoom.ts` | The continuous zoom position and its persistence |
| `search.ts` | Query parsing and the ask-for vocabulary |
| `format.ts` | Bytes, durations, capture times, coordinates |

`useTimeline.ts` — the sparse-array-over-a-day-table store with its own fetch
scheduler, 435 lines and the most load-bearing thing in the gallery — touches
`fetch`, `AbortController` and refs, and nothing else. It ports as-is.

A survey of the rest of `web/src/hooks`:

| Hook | Portability |
|---|---|
| `useTimeline`, `useTrash`, `useAlbums`, `useSearch`, `useView` | Clean. Nothing browser-bound. |
| `useVault`, `useLiveFade` | `window.setTimeout` / `setInterval` only. Drop the prefix. |
| `useSelection` | One keydown listener and one `document.querySelector`. Lift both to the web component; the state machine is portable. |
| `useViewer` | **Not portable, and should not be.** It encodes the open asset in the query string and uses `history.pushState`/`popstate` so the Back button closes the viewer. On the phone expo-router's own stack does exactly that job. Reimplement. |
| `useStatus`, `useTags`, `useMerges`, `useUpload`, `usePalette` | Out of scope — see § 6. |

What is *not* portable is `api.ts`'s transport head, and that is the subject of
§ 3.2. `Timeline.tsx`, `Viewer.tsx`, `Tile.tsx` and everything under
`components/ui/` are browser rendering and are rewritten, not moved.

### The mobile app today

One `App.tsx` of 1,538 lines. No navigation library, no `expo-image`, no
`expo-video`, no reanimated, no gesture-handler. Underneath it, a sync engine
that works: `src/sync/` (engine, chunked transport, SQLite queue, backoff,
conditions), `src/gallery/` (a read client with the right shape),
`src/sharedalbums/`, `pairing.ts`, `discovery.ts`.

None of that machinery is the problem. The problem is that it has one screen.

---

## 2. Decisions

**Shared logic lives in a workspace package**, not copied and not
reimplemented. Two copies of `layout.ts` drift the first time a bug is fixed in
one of them, and the geometry is the part with tests worth keeping.

**The mobile app gets the gallery, the viewer, collections, search, trash and
the vault**, plus a real interface for the backup engine it already has. It
does not get the status dashboard, merge review or tag cleanup — see § 6.

**The timeline geometry is ported exactly**, not replaced with a sectioned
list at fixed column counts. Continuous zoom, an exact scroll height from the
first frame, and drag-selection are the properties that make the gallery feel
like this project's rather than like a template's, and they are all in
`layout.ts` already.

**The UI is a ported theme module and `StyleSheet`**, not NativeWind. shadcn
does not exist on React Native, so every control is hand-built under either
choice; what carries the look across is the palette, and a typed theme object
carries it without adding a Metro build step to an Expo 57 upgrade path.

**The gallery caches the day table and recent pages in SQLite.** The browser
has no offline story and needs none — it is on the machine's network or it is
not open. A phone is routinely out of reach of the archive, and a gallery that
is a blank screen on the train is not a gallery.

**Navigation is expo-router**, so routes map onto the Next app one for one and
deep links come free.

---

## 3. Architecture

### 3.1 The workspace

```
photos-backup/
  pnpm-workspace.yaml        new, at the root
  packages/core/             new
  web/
  expo-app/
  server/  photo-ml/  deploy/
```

`expo-app/pnpm-workspace.yaml` exists today only to carry an `allowBuilds`
entry; it is absorbed into the root file rather than left to nest.

`packages/core` has **two entry points**, and the split is the point:

- `@photobackup/core` — the wire client and the pure modules. **Zero runtime
  dependencies, and no React.** Tested with `node --test`, exactly as
  `web/src/lib` is today.
- `@photobackup/core/react` — the portable hooks, with `react` as a
  *peerDependency*.

That split is what makes the React version skew between the two apps (19.2.8
in web, 19.2.3 in expo-app) a non-issue rather than a resolution problem: the
half that both apps import unconditionally has no opinion about React at all.

### 3.2 The transport seam

`api.ts` today hardcodes three things a phone cannot use: `/api` as a
same-origin base, an implicit cookie, and `window.location.assign("/signin")`
on a 401. Everything else in those 1,765 lines is wire types and endpoint
functions that both clients want unchanged.

Three ways to cut it were considered.

**A client class** — `new ArchiveClient(transport)`, `client.fetchTimeline(…)`
— is the tidiest on paper and was rejected: it rewrites every one of the ~80
names imported from `@/lib/api` across 38 files, and threads an object through
every hook and every component, for no behaviour that differs.

**Endpoint descriptors** — core returns `{path, method, body}` and each app
brings its own executor — tests well and was rejected for splitting error
handling in two. `apiError` noticing a 401 exactly once, in one place, is a
property worth keeping whole.

**A configured module transport** is what this plan takes:

```ts
// packages/core/src/wire/transport.ts
export interface Transport {
  /** Read per request, never captured: discovery moves LAN → Tailscale. */
  baseUrl(): string;
  /** Likewise: a re-pairing must take effect without rebuilding a client. */
  headers(): Record<string, string>;
  onUnauthorized(): void;
}
export function configure(t: Transport): void;
```

Every existing call site — `fetchTimeline(start, size, filter, view, signal)`,
`deleteItems(target)` — stays byte-identical. Only the module head changes.

That functions-read-per-request shape is not invented here. It is what
`expo-app/src/gallery/client.ts` already does, and its own comment says why:
the address and the token are read per request rather than captured at
construction, so discovery moving from the LAN to Tailscale and a re-pairing
both take effect without anything holding a client being rebuilt. The global
is honest — there is one archive.

Installed at startup:

| | `baseUrl()` | `headers()` | `onUnauthorized()` |
|---|---|---|---|
| web | `"/api"` | `{}` — the cookie rides along | `location.assign("/signin?next=…")` |
| mobile | the address discovery settled on | `{ authorization: "Bearer pbk_…" }` | drop the dead token, route to pairing |

`MEDIA_BASE` and the eight `…Url(id)` helpers collapse into one function:

```ts
media(id, variant): { uri: string; headers: Record<string, string> }
```

Web spreads `{uri}` onto an `<img src>` and ignores the rest. Mobile hands the
whole object to `expo-image`. **That one line is the entire difference between
the two clients**, and it is the same difference Phase 17 wrote down.

**Migration tactic.** `web/src/lib/api.ts` becomes a one-line re-export shim on
day one, so the 38 files importing `@/lib/api` keep building untouched. The
imports get rewritten opportunistically, or never — the shim costs nothing.

### 3.3 The mobile app

```
expo-app/
  app/
    _layout.tsx                theme, transport config, the pairing gate
    (tabs)/_layout.tsx         Gallery · Collections · Backup, floating search
    (tabs)/index.tsx           the timeline
    (tabs)/collections/index.tsx
    (tabs)/collections/[kind]/[value].tsx
    (tabs)/backup.tsx          the sync engine, given a face
    viewer/[index].tsx         full-screen, presented modally
    search.tsx
    trash.tsx
    archive/index.tsx  archive/[kind]/[value].tsx
    hidden/index.tsx   hidden/[kind]/[value].tsx
    settings.tsx               pairing, discovery, shared-album survey, diagnostics
  src/
    sync/  gallery/  sharedalbums/  pairing.ts  discovery.ts   unchanged
    theme.ts                   new
    ui/                        new — Button, Sheet, Pill, Toast, ContextMenu
    grid/                      new — the timeline's rendering
```

The routes are the Next routes. `App.tsx` is dismantled rather than kept: the
pairing and discovery screens become the gate in `_layout.tsx`, the backup
progress becomes `(tabs)/backup.tsx`, and the shared-album survey and fetch
diagnostics move to `settings.tsx` where they belong. Nothing under `src/sync`
is touched by any of it.

New dependencies, and nothing beyond them: `expo-router`, `expo-image`,
`expo-video`, `react-native-reanimated`, `react-native-gesture-handler`.

### 3.4 The theme

`src/theme.ts` carries the `:root` block from `globals.css` across as a typed
object — `background #0b0b0d`, `card #16161a`, `primary #6ea8fe`,
`muted-foreground #9a9aa4`, `border #26262d`, `destructive #f0755f`, and the
four the gallery has that shadcn does not: `warning #e0a458`, `faint #6a6a74`,
`tile #1c1c21`, `viewer #08080a` — plus a type scale and a spacing scale.

A literal `#16161a` in a mobile component is the same bug there that
`web/AGENTS.md` says it is here.

### 3.5 The timeline

`layout.ts` is used **unmodified**. `useTimeline` moves to
`@photobackup/core/react` and is used unmodified.

The rendering is new and small. A `ScrollView` whose content height is
`frame.totalHeight` — exact before a single photograph has been fetched, which
is the property the day table exists to give and the reason the scrollbar never
jumps as pages land. Absolutely positioned tiles for whatever
`visibleItems(days, count, frame, scrollY, viewportHeight, overscan)` returns.
A pinch gesture drives the continuous `z` through gesture-handler and
reanimated, and `frameAt(levels, z)` blends the two bracketing layouts so tiles
slide between cell sizes instead of jumping. The sticky day heading comes from
`dayAt`. A long press starts a selection, expressed in `ranges.ts`.

Every one of those functions already exists and has a test beside it. What is
being written is the part that puts a `<View>` where the maths says.

### 3.6 The offline cache

`src/gallery/cache.ts`, on `expo-sqlite`, following the shape
`src/sync/sqliteStore.ts` already established in this app.

Two tables, both keyed by `(filterKey, viewKey)`: one holding a day table, one
holding timeline pages by page number. The read-through is a seam in
`useTimeline` — a `store` parameter that defaults to `null`, so the web passes
nothing and behaves exactly as it does today.

- The day table paints from cache instantly when present, and is replaced when
  the network answers. Geometry before connectivity.
- Pages serve from cache while their fetch is in flight, so a scroll through
  ground already visited is not a wall of placeholders.
- Invalidation is a whole-key drop on any write — delete, restore, album
  change, vault move — plus a TTL. Not a merge: the same reasoning the day
  table itself is refetched rather than reconciled.
- Thumbnails need nothing. `expo-image`'s disk cache already holds them.

The seam is built in Phase 3, with the hook. It is filled in Phase 6.
Retrofitting a read-through into a store this size afterwards is how you end up
with two of them.

---

## 4. The phases

Six sessions. Each one ends somewhere the app still runs.

### Phase 1 — Foundation

The workspace, `packages/core`, and the transport seam. No mobile UI at all.

Move `layout.ts`, `view.ts`, `ranges.ts`, `zoom.ts`, `search.ts`, `format.ts`
and their tests into core untouched. Cut `api.ts` at the transport head per
§ 3.2 and leave a re-export shim behind. Move the portable hooks to
`@photobackup/core/react`, lifting `useSelection`'s keydown listener and
`document.querySelector` into the web component on the way past, and dropping
the `window.` prefix from `useVault`'s and `useLiveFade`'s timers.

**Done when** `pnpm build` and `pnpm test` pass in web with no behaviour
changed, and the phone fetches and logs a page of the timeline through the
shared client with its device token.

This is the largest single diff in the plan and the only phase that ships
nothing anyone can see. It is also the one where hurrying costs the most —
everything after it is built on this seam being right.

#### What a root workspace touches

Surveyed before the phase was written, because two of these are silent
failures and one of them is in the deploy path. **These are findings, not
instructions** — whoever implements the phase picks the fix. What is recorded
here is what was measured, so the same ground does not have to be re-walked.

**Metro needs no configuration.** Expo 57's `getDefaultConfig` calls
`getWatchFolders` and `getModulesPaths`, which resolve the workspace root
through `resolve-workspace-root@2.0.1` — and that package reads
`pnpm-workspace.yaml`. Watch folders and module paths are set from the root
file's globs automatically. There is no `metro.config.js` in `expo-app` today
and this does not require one.

**`expo-app/pnpm-workspace.yaml` is a trap.** It exists only to carry an
`allowBuilds` entry. Left in place, the resolver walking up from `expo-app/`
finds it first, concludes that `expo-app/` *is* the workspace root, and never
sees `packages/core` — so Metro watches the wrong tree and says nothing. It
has to go up, not stay.

**`output: "standalone"` collides with the deploy script.** Tracing a package
that lives above `web/` means pointing `outputFileTracingRoot` at the repo
root, which relocates the build: the app nests under its path relative to that
root, so `web/.next/standalone/server.js` becomes
`web/.next/standalone/web/server.js` with a hoisted `node_modules` beside it.
Four things are written against the old layout:

- `deploy/photobackup-admin:440` — the assertion on
  `.next/standalone/server.js`, whose failure message points at
  `next.config.ts` and would send the reader somewhere the fault is not
- `:444`–`:445` — the `cp -a` pair that stages the tree and carries
  `.next/static` across by hand
- `web_install`'s guard `[ -f "$WEB_STAGE/server.js" ]` — the one whose own
  comment records that an unset stage would `rsync --delete` into `/`
- `deploy/photoweb.service` — `ExecStart=/usr/bin/node /opt/photoweb/server.js`

Absorbing the nesting inside `web_build` — staging `standalone/web/.` at the
stage root and `standalone/node_modules` beside it — leaves `/opt/photoweb`'s
layout, the unit file and that guard untouched, and confines the change to the
one function whose comment already explains that `.next/static` is carried by
hand. Updating all four to the nested paths is the other option. Either is
defensible; doing neither is a gallery that deploys to a `server.js` that
is not there.

**Next may want `transpilePackages: ["@photobackup/core"]`**, since core ships
TypeScript source rather than a build. One line, and it settles the question
of whether a symlinked workspace package gets compiled.

**The React Compiler is on for mobile and off for web.** `app.json` sets
`experiments.reactCompiler: true` and `babel-plugin-react-compiler` is a dev
dependency; `web` has neither the plugin nor the flag. Once core's hooks
resolve to a real path outside `node_modules`, the app's Babel compiles them —
so the compiler will process `useTimeline`, which writes refs during render
(`asked.current = filter`, `looking.current = view`) and holds a mutable sparse
array in one. That is the shape a compiler bails on. One build with the
bail-out log read here is worth an afternoon of confusion in Phase 3.

**The test toolchain survives intact.** Node 22.19.0 strips types natively, so
`TZ=UTC node --test src/*.test.ts` moves into `packages/core` unchanged. No
build step, no transpiler, nothing new to install.

### Phase 2 — Shell

expo-router, `theme.ts`, the `src/ui/` primitives, the tab bar, the pairing
gate. `App.tsx` dismantled into `(tabs)/backup.tsx` and `settings.tsx`.

**Done when** backup, pairing, discovery and the shared-album tools do
everything they did before, inside the new shell, and the Gallery and
Collections tabs exist and are empty.

### Phase 3 — Timeline

`useTimeline` wired up, the virtualized grid, tiles with their derivative
states and placeholders, day headings, pinch zoom, the jump-to-date scrubber,
the `store` seam left null.

**Done when** the whole archive scrolls at every zoom level, with correct
geometry from the first frame and no visible fetching seams. Read-only —
tapping a tile does nothing yet.

The largest piece of new code in the plan. If it overruns, the honest split is
to land the grid at fixed zoom levels here and move the pinch gesture to the
top of Phase 4.

### Phase 4 — Viewer

The modal route, a horizontal pager over timeline positions, preview then
original, video playback, Live Photos on hold, the Snapchat overlay toggle, and
the metadata panel as a bottom sheet.

`useViewer` is *not* reused: the route stack is what makes the Back gesture
close the viewer, which is what `history.pushState` is doing on the web.

**Done when** every asset opens from the grid and the panel shows every field
the browser's does.

### Phase 5 — Collections, search and actions

Albums, people and categories; the album grid and its covers; creating an album
and adding to one. Search, with the parsed query and the ask-for vocabulary.
Multi-select by long press and drag, and the actions on a selection: trash,
restore, add to album, remove from album.

**Done when** the phone can do everything to a selection that the browser
gallery can.

### Phase 6 — Vault, trash and the offline archive

The vault gate and its password, the archive and hidden buckets and their own
collections, the trash timeline with restore and purge. Then `cache.ts`, the
`store` seam filled, and every unreachable-server state drawn properly.

**Done when** the gallery opens, draws its geometry and shows recently-viewed
photographs with no server in reach, and says plainly what it cannot do.

---

## 5. Risks

**1. Phase 1 ships nothing visible, and touches the deploy path.** A
1,765-line file's transport head and a workspace migration, with the reward
deferred a whole session. The mitigation for the refactor is the re-export
shim: web keeps building throughout, so the phase can be verified continuously
rather than at the end.

The deploy path has no such shim, and `--dry-run` is not the check it looks
like: `web_build` returns early on a dry run and fabricates the stage with
`: >"$WEB_STAGE/server.js"`, so the one thing that moved is the one thing a dry
run never exercises. The honest check is a real `next build` locally, and
looking at where `standalone/server.js` actually landed, before any of it
reaches the archive machine.

**2. Phase 3 is the most new code.** A virtualized grid with a continuous zoom
blend is not a small component. Named fallback above: fixed zoom levels in 3,
pinch in 4.

**3. The vault unlock is server-wide.** Two clients now show a state that
either can change out from under the other. The mobile UI must poll it the way
`useVault` already does rather than assume its own unlock is still in force.

**4. Cache invalidation is a new problem for this project.** The browser has
never had one. It is deliberately last, deliberately coarse — a whole-key drop,
never a merge — and deliberately not load-bearing: everything works without it,
slower.

**5. The device token never expires.** That is the existing decision and it
holds, but a phone that is now a gallery rather than a backup client is a phone
where more of the archive is reachable from one revocable credential. Nothing
changes in the model; it is worth writing down that the blast radius grew.

---

## 6. Deliberately not ported

**The status dashboard, merge review and tag cleanup.** Three dense
administrative surfaces — health, storage, queues, duplicate groups with side
by side comparison, tag triage — shaped for a wide screen and a pointer. They
are things you do to the archive at a desk, not on a phone in a queue. They
stay in the browser.

**Passkey registration and the security page.** The phone authenticates with a
device token, which is the better credential in this direction. Adding a second
one to a client that does not need it is exactly the kind of thing Phase 17
spent a phase removing.

**Uploading arbitrary photographs from the phone.** The backup engine already
carries the entire camera roll; an ad-hoc picker would be a second path into
the archive for something the first path is about to do anyway. The Backup tab
surfaces the queue, its progress and its failures, and that is the whole of the
upload interface.

**A light theme.** The browser gallery does not have one. Neither does this.
