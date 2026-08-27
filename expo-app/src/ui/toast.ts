import { installNotifier, type Notice } from '@photobackup/core';

/**
 * Somewhere for a failed write to be seen.
 *
 * `@photobackup/core` decides *what* is said — a delete that did not happen has
 * no other symptom, so `notify()` is the whole of the feedback — and leaves
 * *where* to the client. In the browser that is Base UI's toaster; here it is
 * this, a module-level list with subscribers, because `notify()` is called from
 * inside hooks and from module scope and has nowhere to reach a React context
 * from.
 *
 * The store is deliberately outside React. `<Toaster />` is a view of it.
 */
export interface Toast extends Notice {
  id: string;
}

/** Long enough to read a sentence, short enough not to sit over a photograph. */
const DEFAULT_MS = 5_000;

let toasts: Toast[] = [];
let nextId = 0;
const listeners = new Set<(toasts: Toast[]) => void>();
const timers = new Map<string, ReturnType<typeof setTimeout>>();

/**
 * At most this many at once, oldest dropped first.
 *
 * A failing run reports every item, and a stack of thirty notices is a screen
 * nobody can use rather than thirty things anybody reads.
 */
const MOST = 3;

function publish(): void {
  for (const listener of listeners) listener(toasts);
}

export function subscribeToasts(listener: (toasts: Toast[]) => void): () => void {
  listeners.add(listener);
  listener(toasts);
  return () => {
    listeners.delete(listener);
  };
}

export function closeToast(id: string): void {
  const timer = timers.get(id);
  if (timer) {
    clearTimeout(timer);
    timers.delete(id);
  }
  toasts = toasts.filter((toast) => toast.id !== id);
  publish();
}

function addToast(notice: Notice): string {
  const id = String(nextId++);
  toasts = [...toasts, { ...notice, id }].slice(-MOST);
  // Anything pushed off the end has a timer still running against an id that is
  // no longer on screen. Harmless, but it would fire and re-publish for nothing.
  for (const [key, timer] of timers) {
    if (!toasts.some((toast) => toast.id === key)) {
      clearTimeout(timer);
      timers.delete(key);
    }
  }

  const ms = notice.timeout ?? DEFAULT_MS;
  // A notice offering an action — an undo — is the one thing that must not
  // vanish while somebody is deciding, so core sets its own longer timeout and
  // this respects it. `0` means it stays until something closes it.
  if (ms > 0) timers.set(id, setTimeout(() => closeToast(id), ms));

  publish();
  return id;
}

/**
 * Called once, at the top of the root layout.
 *
 * Until Phase 2 this was `console.warn` in `src/archive.ts`, which was honest
 * about there being nowhere to put a notice yet. There is now.
 */
export function installToaster(): void {
  installNotifier({ add: addToast, close: closeToast });
}
