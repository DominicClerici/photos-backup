import { File, Paths } from 'expo-file-system';

export type LogLine = { seq: number; t: number; tag: string; msg: string };

// Persisted so that a run which ends in the app being suspended or force-quit
// still leaves evidence behind. Without this, Q2 has no client-side record.
const LOG_FILE = new File(Paths.document, 'phase0.log');

let seq = 0;
let lines: LogLine[] = [];
const subscribers = new Set<(lines: LogLine[]) => void>();

function ensureFile() {
  if (!LOG_FILE.exists) LOG_FILE.create({ intermediates: true, overwrite: false });
}

function emit() {
  const snapshot = lines;
  subscribers.forEach((fn) => fn(snapshot));
}

export function loadPersisted(): number {
  try {
    ensureFile();
    const raw = LOG_FILE.textSync();
    const parsed = raw
      .split('\n')
      .filter(Boolean)
      .map((l) => {
        try {
          return JSON.parse(l) as LogLine;
        } catch {
          return null;
        }
      })
      .filter((l): l is LogLine => l !== null);
    lines = parsed;
    seq = parsed.length ? parsed[parsed.length - 1].seq : 0;
    emit();
    return parsed.length;
  } catch {
    return 0;
  }
}

export function log(tag: string, msg: string) {
  const line: LogLine = { seq: ++seq, t: Date.now(), tag, msg };
  lines = [...lines, line];
  if (lines.length > 2000) lines = lines.slice(-2000);
  console.log(`[${tag}] ${msg}`);
  try {
    ensureFile();
    LOG_FILE.write(JSON.stringify(line) + '\n', { append: true });
  } catch {}
  emit();
}

export function clearLog() {
  lines = [];
  seq = 0;
  try {
    if (LOG_FILE.exists) LOG_FILE.delete();
  } catch {}
  emit();
}

export function subscribe(fn: (lines: LogLine[]) => void) {
  subscribers.add(fn);
  fn(lines);
  return () => {
    subscribers.delete(fn);
  };
}

export function formatTime(t: number) {
  const d = new Date(t);
  const pad = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
}
