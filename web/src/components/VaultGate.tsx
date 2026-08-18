"use client";

import { useCallback, useEffect, useState } from "react";
import { KeyRound, Loader2, ShieldCheck } from "lucide-react";

import { closeGate, onGate, useVault } from "@/hooks/useVault";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";

/**
 * The password prompt, mounted once by the root layout.
 *
 * It is here rather than on the Archive page because of when it is needed:
 * archiving a photograph on an archive that has never had a vault has to be
 * able to ask for a password from the middle of the library's grid, and
 * browsing a locked bucket has to be able to ask from a page that has drawn
 * nothing. One dialog, opened from anywhere, is the only shape that serves both
 * without a prop threaded through every component in between.
 *
 * Two modes, because they are two different conversations. Unlocking is "prove
 * you are you"; creating is "choose the thing that will be the difference
 * between these photographs existing and not", and it says so.
 */
export function VaultGate() {
  const [reason, setReason] = useState<"unlock" | "setup" | null>(null);
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  const vault = useVault();

  useEffect(() => onGate(setReason), []);

  // Nothing typed survives the dialog closing. It is a password, and the one
  // place it is allowed to live is the request that spends it.
  useEffect(() => {
    if (reason === null) {
      setPassword("");
      setConfirm("");
      setFailed(null);
    }
  }, [reason]);

  const setup = reason === "setup";

  const submit = useCallback(async () => {
    setFailed(null);
    if (setup && password !== confirm) {
      setFailed("Those do not match.");
      return;
    }
    setBusy(true);
    try {
      if (setup) await vault.create(password);
      else await vault.unlock(password);
    } catch (err) {
      setFailed(err instanceof Error ? err.message : "That did not work.");
    } finally {
      setBusy(false);
    }
  }, [setup, password, confirm, vault]);

  const ready = setup ? password.length >= 8 && confirm.length > 0 : password.length > 0;

  return (
    <Dialog open={reason !== null} onOpenChange={(open) => (open ? null : closeGate())}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            {setup ? (
              <ShieldCheck className="size-[18px] text-muted-foreground" aria-hidden="true" />
            ) : (
              <KeyRound className="size-[18px] text-muted-foreground" aria-hidden="true" />
            )}
            {setup ? "Choose a vault password" : "Unlock the vault"}
          </DialogTitle>
          <DialogDescription>
            {setup
              ? "Archive and Hidden are encrypted with this password. There is no recovery: if you forget it, those photos are gone for good."
              : "Archive and Hidden are encrypted. The password decrypts them for the next fifteen minutes."}
          </DialogDescription>
        </DialogHeader>

        <form
          className="flex flex-col gap-3"
          onSubmit={(ev) => {
            ev.preventDefault();
            if (ready && !busy) void submit();
          }}
        >
          <Input
            // Two different fields as far as a password manager is concerned,
            // so it offers to save on the first and to fill on the second
            // rather than doing the wrong one of those.
            autoComplete={setup ? "new-password" : "current-password"}
            type="password"
            placeholder={setup ? "New password, at least 8 characters" : "Password"}
            value={password}
            autoFocus
            onChange={(ev) => setPassword(ev.target.value)}
          />
          {setup ? (
            <Input
              autoComplete="new-password"
              type="password"
              placeholder="Type it again"
              value={confirm}
              onChange={(ev) => setConfirm(ev.target.value)}
            />
          ) : null}

          {failed ? <p className="px-0.5 text-[13px] text-destructive">{failed}</p> : null}

          {setup ? (
            <p className="px-0.5 text-[11px] leading-relaxed text-faint">
              Hiding a photo works whether or not the vault is unlocked. Opening
              it — browsing, restoring, or seeing a single thumbnail — always
              needs this password.
            </p>
          ) : null}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={closeGate}>
              Cancel
            </Button>
            <Button type="submit" disabled={!ready || busy}>
              {busy ? <Loader2 className="animate-spin" aria-hidden="true" /> : null}
              {setup ? "Create vault" : "Unlock"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
