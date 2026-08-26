"use client";

import { useCallback, useEffect, useState } from "react";
import { KeyRound, LogOut, Plus, ShieldCheck, Trash2, TriangleAlert } from "lucide-react";

import {
  fetchAuthStatus,
  fetchPasskeys,
  mintRecoveryCodes,
  registerPasskey,
  revokePasskey,
  signOut,
  type AuthStatus,
  type Passkey,
} from "@/lib/api";
import { formatSince } from "@/lib/format";
import { notifyError } from "@/lib/notify";
import { explainCeremonyError, supportsPasskeys } from "@/lib/webauthn";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { CopyButton } from "@/components/CopyButton";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";

/**
 * The credentials that open this archive, and the session currently holding it.
 *
 * It is on the status page rather than a settings page of its own because it is
 * the same kind of thing as everything else here: a reading of what the server
 * is doing, occasionally acted on. Signing in is not here at all — photod
 * serves that page itself, so an unauthenticated browser never receives this
 * bundle to render.
 *
 * The one thing this surface is careful about is the order it puts the two
 * risks in. Losing every passkey is recoverable only through a recovery code,
 * and a synced passkey is lost with the account that syncs it — so the warning
 * about having none is drawn before the list of the ones that exist.
 */
export function Security() {
  const [status, setStatus] = useState<AuthStatus | null>(null);
  const [passkeys, setPasskeys] = useState<Passkey[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [revoking, setRevoking] = useState<Passkey | null>(null);
  const [minted, setMinted] = useState<string[] | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    const [next, keys] = await Promise.all([fetchAuthStatus(signal), fetchPasskeys(signal)]);
    setStatus(next);
    setPasskeys(keys.passkeys);
  }, []);

  useEffect(() => {
    const abort = new AbortController();
    load(abort.signal).catch(() => {
      // A 401 has already sent the browser to sign in — see apiError in
      // lib/api.ts — and anything else is the same server outage the cards
      // above have already reported.
    });
    return () => abort.abort();
  }, [load]);

  const live = passkeys?.filter((key) => !key.revokedAt) ?? [];

  async function add() {
    setAdding(true);
    try {
      await registerPasskey();
      await load();
    } catch (err) {
      notifyError(new Error(explainCeremonyError(err)), "Could not add that passkey");
    } finally {
      setAdding(false);
    }
  }

  async function confirmRevoke() {
    const target = revoking;
    if (!target) return;
    setRevoking(null);
    try {
      await revokePasskey(target.id);
      await load();
    } catch (err) {
      notifyError(err, "Could not revoke that passkey");
    }
  }

  async function mint() {
    try {
      const { codes } = await mintRecoveryCodes();
      setMinted(codes);
      await load();
    } catch (err) {
      notifyError(err, "Could not mint recovery codes");
    }
  }

  return (
    <Card className="gap-0 px-0 py-0">
      <div className="flex items-center gap-2.5 px-4 py-3.5">
        <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-tile">
          <ShieldCheck className="size-4 text-muted-foreground" aria-hidden="true" />
        </span>
        <div className="flex-1">
          <h2 className="text-[13px] font-medium tracking-[0.01em]">Security</h2>
          <p className="text-[12px] text-faint">{sessionLine(status)}</p>
        </div>
        <Button variant="ghost" size="sm" onClick={() => void signOut()}>
          <LogOut aria-hidden="true" />
          Sign out
        </Button>
      </div>

      <Separator />

      {!passkeys ? (
        <div className="flex flex-col gap-2 p-4">
          <Skeleton className="h-10 rounded-lg" />
          <Skeleton className="h-10 rounded-lg" />
        </div>
      ) : (
        <>
          {/* Drawn first when it applies, because it is the only thing on this
              card that describes a way to lose the archive rather than a way
              into it. */}
          {status && status.recoveryRemaining === 0 ? (
            <div className="flex items-start gap-2.5 border-b border-warning/25 bg-warning/[0.07] px-4 py-3">
              <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden="true" />
              <div className="flex-1 text-[12px] leading-relaxed">
                <p className="font-medium text-foreground">No recovery codes</p>
                <p className="text-muted-foreground">
                  A passkey synced through iCloud is lost with the account that syncs it. Without a
                  recovery code, losing it means losing the way in.
                </p>
              </div>
              <Button variant="outline" size="sm" onClick={() => void mint()}>
                Mint codes
              </Button>
            </div>
          ) : null}

          <ul className="divide-y">
            {live.map((key) => (
              <li key={key.id} className="flex items-center gap-3 px-4 py-3">
                <KeyRound className="size-4 shrink-0 text-faint" aria-hidden="true" />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-[13px] font-medium">{key.label || "Passkey"}</p>
                  <p className="text-[12px] text-faint">
                    Added {formatSince(key.createdAt)}
                    {key.lastUsedAt ? ` · last used ${formatSince(key.lastUsedAt)}` : " · never used"}
                  </p>
                </div>
                {key.transports?.includes("internal") ? (
                  <Badge variant="outline" className="max-sm:sr-only">
                    This device
                  </Badge>
                ) : null}
                {/* The last one cannot be withdrawn: an archive reachable only
                    by recovery code is a state to get into deliberately from
                    the command line, not by accident from a menu. photod
                    refuses it too — this only saves the round trip. */}
                <Button
                  variant="ghost"
                  size="icon-sm"
                  disabled={live.length <= 1}
                  onClick={() => setRevoking(key)}
                  aria-label={`Revoke ${key.label || "this passkey"}`}
                >
                  <Trash2 aria-hidden="true" />
                </Button>
              </li>
            ))}
          </ul>

          <Separator />

          <div className="flex flex-wrap items-center gap-2 px-4 py-3">
            <Button
              variant="outline"
              size="sm"
              disabled={adding || !supportsPasskeys()}
              onClick={() => void add()}
            >
              <Plus aria-hidden="true" />
              {adding ? "Waiting for your passkey…" : "Add a passkey"}
            </Button>

            <span className="ml-auto text-[12px] text-faint">
              {recoveryLine(status)}{" "}
              <button
                type="button"
                className="text-muted-foreground underline underline-offset-2 hover:text-foreground"
                onClick={() => void mint()}
              >
                Mint a new set
              </button>
            </span>
          </div>
        </>
      )}

      <AlertDialog open={revoking !== null} onOpenChange={(open) => !open && setRevoking(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke {revoking?.label || "this passkey"}?</AlertDialogTitle>
            <AlertDialogDescription>
              It stops working immediately, and any browser signed in with it is signed out. The
              archive itself is untouched.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep it</AlertDialogCancel>
            <AlertDialogAction onClick={() => void confirmRevoke()}>Revoke</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <RecoveryCodes codes={minted} onClose={() => setMinted(null)} />
    </Card>
  );
}

/**
 * The new codes, shown once.
 *
 * "Once" is literal: photod keeps only their digests, so closing this dialog is
 * the last time they exist anywhere but wherever they get written down. The
 * copy button is there because the alternative is transcribing ten
 * twenty-two-character strings by hand and getting one of them wrong.
 */
function RecoveryCodes({ codes, onClose }: { codes: string[] | null; onClose: () => void }) {
  return (
    <Dialog open={codes !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Recovery codes</DialogTitle>
          <DialogDescription>
            Each opens the archive once, from the sign-in page. This is the only time they are
            shown — keep them somewhere that is neither this machine nor the account your passkey
            syncs through, because those are the two things they exist to survive the loss of.
          </DialogDescription>
        </DialogHeader>

        <ul className="grid gap-1 rounded-lg bg-muted p-3 font-mono text-[12px] sm:grid-cols-2">
          {(codes ?? []).map((code) => (
            <li key={code} className="tabular-nums">
              {code}
            </li>
          ))}
        </ul>

        <DialogFooter>
          <CopyButton text={() => (codes ?? []).join("\n")} label="Copy all" variant="outline" />
          <Button onClick={onClose}>I have saved them</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** How this browser got in, and how long it has. */
function sessionLine(status: AuthStatus | null): string {
  if (!status) return "Checking this session…";
  if (!status.signedIn) return "Not signed in";

  const how =
    status.method === "recovery"
      ? "Signed in with a recovery code"
      : "Signed in with a passkey";
  return status.expires ? `${how} · ends ${formatSince(status.expires)}` : how;
}

function recoveryLine(status: AuthStatus | null): string {
  const remaining = status?.recoveryRemaining;
  if (remaining === undefined) return "Recovery codes:";
  if (remaining === 0) return "No recovery codes left.";
  return `${remaining} recovery ${remaining === 1 ? "code" : "codes"} left.`;
}
