export function formatBytes(bytes: number): string {
  if (bytes < 1000) return `${bytes} B`;
  const units = ["kB", "MB", "GB", "TB"];
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit++;
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

export function formatDuration(seconds: number): string {
  const total = Math.round(seconds);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  return `${m}:${String(s).padStart(2, "0")}`;
}

export function formatOffset(minutes: number): string {
  const sign = minutes < 0 ? "−" : "+";
  const abs = Math.abs(minutes);
  return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, "0")}:${String(abs % 60).padStart(2, "0")}`;
}

/**
 * Renders a capture time in the timezone the camera was actually in.
 *
 * When the file recorded no offset there is nothing to shift by, and the result
 * is labelled as the viewer's local time rather than passed off as the
 * photographer's — the server stores those two cases differently and the panel
 * should not flatten them.
 */
export function formatCaptureTime(
  iso: string,
  offsetMinutes?: number | null,
): { text: string; zone: string } {
  const t = new Date(iso);
  const opts: Intl.DateTimeFormatOptions = {
    year: "numeric",
    month: "long",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  };

  if (offsetMinutes == null) {
    return { text: t.toLocaleString(undefined, opts), zone: "your local time" };
  }
  const shifted = new Date(t.getTime() + offsetMinutes * 60_000);
  return {
    text: shifted.toLocaleString(undefined, { ...opts, timeZone: "UTC" }),
    zone: formatOffset(offsetMinutes),
  };
}

export function formatCoords(lat: number, lon: number): string {
  const ns = lat >= 0 ? "N" : "S";
  const ew = lon >= 0 ? "E" : "W";
  return `${Math.abs(lat).toFixed(5)}° ${ns}, ${Math.abs(lon).toFixed(5)}° ${ew}`;
}

export function mapLink(lat: number, lon: number): string {
  return `https://www.openstreetmap.org/?mlat=${lat}&mlon=${lon}#map=15/${lat}/${lon}`;
}

/**
 * The noun a count is rendered in.
 *
 * Two words rather than one plus an "s", because the interesting cases are the
 * ones a rule gets wrong, and because what a thing is called is a decision the
 * caller is making — a right-click on a video knows it is about a video.
 */
export interface Noun {
  one: string;
  many: string;
}

/** What a selection is called when nothing more specific is known. */
export const ITEMS: Noun = { one: "photo", many: "photos" };

export function nounFor(kind: "image" | "video"): Noun {
  return kind === "video" ? { one: "video", many: "videos" } : ITEMS;
}

export function counted(n: number, noun: Noun = ITEMS): string {
  return `${n.toLocaleString()} ${n === 1 ? noun.one : noun.many}`;
}

/**
 * What a destructive or filing action is about, for the words on its button.
 *
 * Three shapes rather than a count and a noun, because the three read
 * differently and only one of them is countable. An album is called "album" and
 * not by its title — "Archive album" is unambiguous where "Archive Iceland
 * 2025" reads like a place. A person *is* called by their name, because that is
 * the only thing that makes "Archive Brody" mean what it means.
 */
export type Subject =
  | { kind: "items"; count: number; noun?: Noun }
  | { kind: "album" }
  | { kind: "person"; name: string };

/**
 * The label a menu item or a button carries, given what it is about.
 *
 * One photograph is called what it is — a photo or a video, which the grid
 * knows — and several are called items, because a selection of eleven
 * photographs and two videos is not eleven photos and it is not thirteen
 * photos. Every verb in the gallery goes through here, so Delete, Archive and
 * Hide cannot end up describing the same selection three different ways.
 */
export function describeAction(verb: string, subject: Subject): string {
  switch (subject.kind) {
    case "album":
      return `${verb} album`;
    case "person":
      return `${verb} ${subject.name}`;
    case "items":
      if (subject.count === 1) return `${verb} ${(subject.noun ?? ITEMS).one}`;
      return `${verb} ${subject.count.toLocaleString()} items`;
  }
}
