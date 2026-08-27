import { ApiError } from "./wire/api.ts";

/**
 * What a client can say to somebody, said in a way both clients can hear.
 *
 * A failed delete has no other symptom: the grid is unchanged, which is exactly
 * what it looks like when nothing was selected. So this is not a courtesy, it
 * is the whole of the feedback — which is why the hooks that do the writing
 * cannot be portable without it.
 *
 * Deliberately smaller than either client's toast. No React node anywhere: the
 * action is a label and a callback, because a phone renders it as a `Pressable`
 * and a browser as Base UI's `actionProps.children`, and a package a phone
 * imports must not have an opinion about which.
 */
export interface Notice {
  /** Absent is the neutral one. */
  type?: "success" | "error";
  title: string;
  description?: string;
  /** Milliseconds. Absent means the host's own default. */
  timeout?: number;
  action?: { label: string; onPress: () => void };
}

export interface Notifier {
  /** Returns an id, so that an action can close the notice that offered it. */
  add(notice: Notice): string;
  close(id: string): void;
}

let current: Notifier = {
  add: () => "",
  close: () => {},
};

/**
 * Installed once at startup, beside the transport.
 *
 * Unlike the transport this has a working default rather than a throwing one:
 * a client that has not installed one yet should still be able to delete a
 * photograph, silently, rather than throw inside the catch block that was
 * trying to report the first failure.
 */
export function installNotifier(notifier: Notifier): void {
  current = notifier;
}

export function notify(notice: Notice): string {
  return current.add(notice);
}

export function closeNotice(id: string): void {
  current.close(id);
}

/**
 * Says what went wrong, in the one place anybody is looking.
 */
export function notifyError(err: unknown, what: string): void {
  notify({
    type: "error",
    title: what,
    description:
      err instanceof ApiError || err instanceof Error ? err.message : "the server did not answer",
  });
}

/**
 * How long an undo stays on screen.
 *
 * Longer than the usual five seconds, and deliberately: the notice is the only
 * place a delete's batch is ever named, so once it goes the only way back is
 * the trash. Long enough to notice something is missing and reach for it; short
 * enough that it is gone before the next delete lands.
 */
export const UNDO_MS = 10_000;
