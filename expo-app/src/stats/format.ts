/**
 * Rendering helpers for the backup card. Pure on purpose: everything else in
 * src/stats touches the filesystem and cannot be imported under jest, so the
 * logic worth testing lives here rather than beside the file it decorates.
 */

/**
 * Bytes as a person would say them.
 *
 * Decimal units, not binary. The archive is measured against a drive sold as
 * 6TB, and a card that called 100,000,000,000 bytes "93.1 GB" would disagree
 * with every other number in this project for no reader's benefit.
 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';

  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  const exponent = Math.min(Math.floor(Math.log10(bytes) / 3), units.length - 1);
  const value = bytes / 1000 ** exponent;

  // One decimal place below 100 and none above it, so the column stays about
  // three digits wide whichever unit it lands in.
  const digits = exponent === 0 ? 0 : value < 100 ? 1 : 0;
  return `${value.toFixed(digits)} ${units[exponent]}`;
}

/**
 * How long ago something happened, at the coarsest useful precision.
 *
 * "just now" for anything under a minute rather than a count of seconds: this
 * labels a backup, and a number that ticks every second on a screen nobody is
 * watching draws the eye for no reason.
 */
export function formatAge(then: number, now: number): string {
  const seconds = Math.round((now - then) / 1000);
  if (!Number.isFinite(seconds) || seconds < 60) return 'just now';

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;

  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

/**
 * When a device last got something into the archive, as the card says it.
 *
 * The server omits the field entirely for a phone that has backed up nothing,
 * which is a different thing from a backup that happened long ago and has to
 * read differently.
 */
export function formatLastBackup(iso: string | undefined, now: number): string {
  if (!iso) return 'never';
  const at = Date.parse(iso);
  if (!Number.isFinite(at)) return 'unknown';
  return formatAge(at, now);
}

/** "1,284" — the counts get big enough that the separator earns its place. */
export function formatCount(value: number): string {
  return Math.round(value).toLocaleString('en-US');
}
