/**
 * The gallery's palette, on the phone.
 *
 * These are the same values as the `:root` block in `web/src/app/globals.css`,
 * carried across by hand because there is no CSS here to read them from. The
 * names are shadcn's, which is what the browser gallery bound its own colours
 * to, so a component ported from there can keep the token it was written
 * against instead of being re-coloured on the way.
 *
 * `web/AGENTS.md` says a literal `#16161a` in a component is a bug. It is the
 * same bug here, and worse: there is no `bg-card` utility to catch it, so the
 * only thing keeping the two apps the same colour is that everything reaches
 * for this file.
 */
export const color = {
  background: "#0b0b0d",
  foreground: "#ececed",
  card: "#16161a",
  cardForeground: "#ececed",
  popover: "#16161a",
  primary: "#6ea8fe",
  primaryForeground: "#0b0b0d",
  secondary: "#1c1c21",
  secondaryForeground: "#ececed",
  muted: "#1c1c21",
  mutedForeground: "#9a9aa4",
  /**
   * The hover/pressed surface for rows and menus, not a brand colour — the same
   * distinction `globals.css` draws. The blue is `primary`; putting it here
   * would tint every pressed list row.
   */
  accent: "#1c1c21",
  accentForeground: "#ececed",
  destructive: "#f0755f",
  border: "#26262d",
  input: "#26262d",
  ring: "#6ea8fe",

  /**
   * Degraded, as distinct from broken. Three health states where shadcn ships
   * two: a server missing ffmpeg is not a server that is down.
   */
  warning: "#e0a458",
  /** The dimmest text there is. Below `mutedForeground`. */
  faint: "#6a6a74",
  /** What a thumbnail sits on before it has loaded. */
  tile: "#1c1c21",
  tileSheen: "#24242b",
  /** The full-screen viewer's backdrop, darker than the app's own. */
  viewer: "#08080a",

  /**
   * Not in `globals.css`: the browser gets green for free from a `text-` class
   * nobody standardised, and the phone's diagnostics — the gallery-access
   * checklist, the archive summary — need a settled "this worked" colour.
   */
  success: "#7ed492",
} as const;

export type ColorToken = keyof typeof color;

/**
 * A four-step scale, because everything on these screens is either a label, a
 * line of body text, a heading, or a number being shown off.
 */
export const text = {
  /** Timestamps, counts under a number, the small print under a control. */
  caption: { fontSize: 11, lineHeight: 15 },
  /** Explanatory paragraphs — most of the words in this app. */
  small: { fontSize: 12, lineHeight: 17 },
  body: { fontSize: 14, lineHeight: 20 },
  /** A section title. */
  title: { fontSize: 16, lineHeight: 22, fontWeight: "600" },
  /** A screen's name, and the one number a card exists to show. */
  display: { fontSize: 20, lineHeight: 26, fontWeight: "600" },
} as const;

/**
 * Menlo on iOS, monospace elsewhere. Named rather than repeated because a
 * pairing code, a log line and a provenance dump all want the same face and
 * none of them wants to know what it is called.
 */
export const mono = "Menlo";

/** Multiples of four. `md` is the gap between two things in the same group. */
export const space = {
  xs: 4,
  sm: 8,
  md: 12,
  lg: 16,
  xl: 24,
  xxl: 32,
} as const;

/**
 * `--radius: 0.625rem` is 10px, and the rest of the browser's scale is that
 * times a constant. Kept in the same proportions so a card is a card.
 */
export const radius = {
  sm: 6,
  md: 8,
  lg: 10,
  xl: 14,
  pill: 999,
} as const;

export const theme = { color, text, mono, space, radius } as const;
