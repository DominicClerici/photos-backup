# @photobackup/core

What the browser gallery and the phone both are, minus the drawing.

Two entry points, and the split is the point:

- `@photobackup/core` — the wire client and the pure modules. Zero runtime
  dependencies and no React. Tested with `node --test`, which is what the
  gallery's geometry has always been tested with.
- `@photobackup/core/react` — the portable hooks, with `react` as a peer.

That split is why the React version skew between the two apps is a non-issue
rather than a resolution problem: the half both apps import unconditionally has
no opinion about React at all.

Nothing here knows which client it is inside. The three facts that differ — the
address, the credential, and what a 401 means — are `configure()` in
`src/wire/transport.ts`, installed once at startup. The fourth, what a toast is,
is `installNotifier()` in `src/notify.ts`.

`pnpm test` runs the geometry tests. `pnpm typecheck` runs `tsc --noEmit`; there
is no build, because the package ships TypeScript source and both consumers
compile it — Next through `transpilePackages`, Metro through Babel.

## The React Compiler

`expo-app` has `experiments.reactCompiler: true`; `web` does not. Once these
hooks resolved to a path outside `node_modules`, the app's Babel started
compiling them, so the compiler's verdict on them is worth writing down. Ten of
the thirteen functions under `src/react` compile. Three decline:

| Function | Why the compiler stopped |
|---|---|
| `useTimeline` | "cannot access variable before it is declared" |
| `useSearch` | the same |
| `useTrash` | an internal assertion, `Expected HoistedConst to have been pruned` |

None of them fails a build. `babel-preset-expo` leaves `panicThreshold` at the
plugin's default in development and sets it to `NONE` in production, so a
function the compiler cannot handle is left exactly as written.

Only `useTimeline`'s refusal is load-bearing, and it carries a `"use no memo"`
directive so that it stays refused: it writes refs during render and mutates a
sparse array of one slot per photograph in place, which is the shape memoizing
would break. The other two decline incidentally and would be fine compiled.
