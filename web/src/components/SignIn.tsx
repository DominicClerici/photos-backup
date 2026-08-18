"use client";

import { useCallback, useEffect, useState } from "react";
import { Loader2, Lock } from "lucide-react";

import { useSession } from "@/hooks/useSession";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

/**
 * The gallery password, asked for once per browser.
 *
 * Mounted by the root layout and drawing nothing at all on a server that wants
 * no password — which is every development run, where the gallery talks to
 * photod's loopback listener directly.
 *
 * It covers the screen rather than opening as a dialog because it is not a
 * question with a Cancel: behind it there is nothing to go back to, since every
 * request the app makes is refused until it is answered. That is also why it is
 * a plain overlay rather than the vault's Dialog, which can be dismissed by
 * design.
 *
 * Part of the browser gate; see `@/lib/session` for what removing it means.
 */
export function SignIn() {
  const session = useSession();
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);

  // Nothing typed outlives the prompt. It is a password, and the one place it
  // is allowed to live is the request that spends it.
  useEffect(() => {
    if (!session.locked) {
      setPassword("");
      setFailed(null);
    }
  }, [session.locked]);

  const submit = useCallback(async () => {
    setFailed(null);
    setBusy(true);
    try {
      await session.signIn(password);
      // A full reload rather than a re-render. Everything on the other side of
      // this prompt was mounted while every request was being refused, so the
      // timeline, the collections and the vault's status are all holding an
      // error rather than data. Reloading is one line and leaves nothing
      // half-loaded; the alternative is a retry path threaded through every
      // hook in the app, for a moment that happens once a fortnight.
      window.location.reload();
    } catch (err) {
      setFailed(err instanceof Error ? err.message : "That did not work.");
      setBusy(false);
    }
  }, [password, session]);

  if (!session.locked) return null;

  return (
    <div className="fixed inset-0 z-100 flex items-center justify-center bg-background p-6">
      <div className="flex w-full max-w-sm flex-col gap-4">
        <div className="flex flex-col gap-1.5">
          <Lock className="size-5 text-muted-foreground" aria-hidden="true" />
          <h1 className="text-base font-medium">Photos</h1>
          <p className="text-[13px] leading-relaxed text-faint">
            This archive is on your network rather than the internet, and it is
            still private. Enter the gallery password to see it.
          </p>
        </div>

        <form
          className="flex flex-col gap-3"
          onSubmit={(ev) => {
            ev.preventDefault();
            if (password && !busy) void submit();
          }}
        >
          <Input
            type="password"
            // The gallery's password, not the vault's — named so a password
            // manager offers the right one of the two.
            name="gallery-password"
            autoComplete="current-password"
            placeholder="Gallery password"
            value={password}
            autoFocus
            onChange={(ev) => setPassword(ev.target.value)}
          />

          {failed ? <p className="px-0.5 text-[13px] text-destructive">{failed}</p> : null}

          <Button type="submit" disabled={!password || busy}>
            {busy ? <Loader2 className="animate-spin" aria-hidden="true" /> : null}
            Sign in
          </Button>
        </form>

        <p className="px-0.5 text-[11px] leading-relaxed text-faint">
          This browser stays signed in until it goes unused for a fortnight.
          Archive and Hidden keep their own password.
        </p>
      </div>
    </div>
  );
}
