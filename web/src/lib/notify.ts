"use client";

/**
 * The gallery's toast, handed to the hooks that live in core.
 *
 * `notifyError` and `UNDO_MS` are core's — a failed delete has no symptom but
 * the message, so the hooks that do the writing cannot be portable without
 * somewhere to say so. What is here is the adapter: core's `Notice` is a title,
 * a description and an action that is a label and a callback, and Base UI's
 * toast wants that action as `children` and `onClick`.
 *
 * Installed at module scope, and imported for that side effect by
 * `@/hooks/useTrash` — the one hook that reports anything — as well as by the
 * components that call `notifyError` directly.
 */
import { installNotifier } from "@photobackup/core";
import { toast } from "@/components/ui/toast";

installNotifier({
  add: (notice) =>
    toast.add({
      type: notice.type,
      title: notice.title,
      description: notice.description,
      timeout: notice.timeout,
      actionProps: notice.action
        ? { children: notice.action.label, onClick: notice.action.onPress }
        : undefined,
    }),
  close: (id) => toast.close(id),
});

export { notifyError, UNDO_MS } from "@photobackup/core";
