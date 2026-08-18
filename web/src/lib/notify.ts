import { ApiError } from "./api";
import { toast } from "@/components/ui/toast";

/**
 * Says what went wrong, in the one place anybody is looking.
 *
 * A failed delete has no other symptom. The grid is unchanged, which is exactly
 * what it looks like when nothing was selected — so the toast is not a courtesy
 * here, it is the whole of the feedback.
 */
export function notifyError(err: unknown, what: string): void {
  toast.add({
    type: "error",
    title: what,
    description:
      err instanceof ApiError || err instanceof Error ? err.message : "the server did not answer",
  });
}

/**
 * How long an undo stays on screen.
 *
 * Longer than the default five seconds, and deliberately: the toast is the only
 * place a delete's batch is ever named, so once it goes the only way back is the
 * trash page. Long enough to notice something is missing and reach for the
 * mouse; short enough that it is gone before the next delete lands.
 */
export const UNDO_MS = 10_000;
