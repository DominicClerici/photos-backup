<!-- BEGIN:nextjs-agent-rules -->

# This is NOT the Next.js you know

This version has breaking changes — APIs, conventions, and file structure may all differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` (resolved from this file's directory; in monorepos the `next` package may not be visible from the repo root) before writing any code. Heed deprecation notices.

This block is written and re-added by `next dev` — verify at `node_modules/next/dist/server/lib/generate-agent-files.js`. Removing it from a diff only re-creates the uncommitted change; committing it with your work keeps the tree clean.

<!-- END:nextjs-agent-rules -->

# UI

## Build with shadcn components

**Use the shadcn components in `src/components/ui/` for anything they cover.** Do not
hand-roll a button, dialog, menu, toast, form field, or any other control that already
exists there. Reach for a raw element only when a shadcn component genuinely cannot do
the job, or when asked to.

Currently installed: accordion, alert-dialog, aspect-ratio, badge, button, calendar,
card, carousel, checkbox, combobox, command, context-menu, dialog, drawer,
dropdown-menu, field, input, input-group, kbd, label, pagination, progress,
scroll-area, select, separator, slider, switch, textarea, toast.

Add more with `pnpm dlx shadcn@latest add <name>` — do not write them by hand. Files
under `src/components/ui/` are generated; they are meant to be edited in place once
added, but prefer passing `className` from the call site over editing the primitive.

## Base UI, not Radix

These components are built on **`@base-ui/react`**, which shadcn made its default in
July 2026. Radix answers do not transfer — the props, the part names, and the
composition patterns differ. Check `src/components/ui/<name>.tsx` for the real exports
before importing; the parts are named `*Content` (`DialogContent`, `SelectContent`),
not `*Popup` as Base UI's own docs call them.

Two Base UI patterns worth knowing:

- **`render` swaps the underlying element** instead of Radix's `asChild`:
  `<DialogTrigger render={<Button />}>`.
- **`nativeButton={false}` is required when a button renders a non-`<button>`.**
  Without it Base UI logs a console error about lost button semantics. See the
  download link in `Viewer.tsx` and shadcn's own `PaginationLink`.

## Styling

Tailwind v4, configured entirely in `src/app/globals.css` — there is no
`tailwind.config`, and there are no CSS module or `.css` files besides that one.
Style with utility classes; add global CSS only for something utilities cannot express.

The app is **dark-only**. The palette lives at `:root` in `globals.css` and `<html>`
carries a permanent `dark` class so the components' own `dark:` variants resolve.
There is no light theme and no toggle.

**Use the design tokens, never raw colours.** `bg-background`, `bg-card`, `text-faint`,
`border`, `text-primary`, `text-destructive`, and so on. The gallery's original palette
is bound to shadcn's token names, so stock components already match the app. A literal
`#16161a` or `bg-neutral-900` in a component is a bug.

App-specific tokens beyond the shadcn set: `text-faint` (dimmest text), `bg-tile`
(thumbnail backing), `bg-viewer` (full-screen viewer backdrop).
